package sqs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	ojsotel "github.com/openjobspec/ojs-go-backend-common/otel"

	"github.com/openjobspec/ojs-backend-sqs/internal/core"
	"github.com/openjobspec/ojs-backend-sqs/internal/metrics"
	"github.com/openjobspec/ojs-backend-sqs/internal/state"
)

// Push enqueues a single job.
func (b *SQSBackend) Push(ctx context.Context, job *core.Job) (*core.Job, error) {
	ctx, span := ojsotel.StartJobSpan(ctx, "push", job.ID, job.Type, job.Queue)
	defer span.End()

	now := time.Now()

	// Assign ID if not provided
	if job.ID == "" {
		job.ID = core.NewUUIDv7()
	}

	// Set system-managed fields
	job.CreatedAt = core.FormatTime(now)
	job.Attempt = 0

	if store, ok := b.reliabilityStore(); ok {
		return b.pushReliable(ctx, store, job, now)
	}

	// Handle unique jobs. A non-nil returned job (e.g. the "ignore" conflict
	// outcome) short-circuits the enqueue.
	if job.Unique != nil {
		if existing, err := b.reserveUnique(ctx, job); existing != nil || err != nil {
			return existing, err
		}
	}

	// Scheduled in the future: persist and let the scheduler promote it.
	if job.ScheduledAt != "" {
		if scheduledTime, err := time.Parse(time.RFC3339, job.ScheduledAt); err == nil && scheduledTime.After(now) {
			return b.enqueueScheduled(ctx, job, now, scheduledTime)
		}
		// Past scheduled_at - treat as immediate.
	}

	return b.enqueueAvailable(ctx, job, now)
}

func (b *SQSBackend) pushReliable(ctx context.Context, store state.JobReliabilityStore, job *core.Job, now time.Time) (*core.Job, error) {
	plan, existing, err := b.prepareReliableJobPlan(ctx, store, job, now)
	if err != nil || existing != nil {
		return existing, err
	}
	if err := b.createJobReliable(ctx, store, plan); err != nil {
		if errors.Is(err, state.ErrConditionFailed) {
			return nil, &core.OJSError{
				Code:    core.ErrCodeDuplicate,
				Message: "A concurrent enqueue won the unique or job ID claim.",
				Details: map[string]any{"job_id": job.ID},
			}
		}
		return nil, fmt.Errorf("create durable job: %w", err)
	}

	if plan.Unique != nil && plan.Unique.CancelExisting {
		b.advanceWorkflow(ctx, plan.Unique.ExpectedMappingJobID, core.StateCancelled, nil)
	}
	if plan.Intent != nil {
		b.drainDeliveryBestEffort(ctx)
	}
	return job, nil
}

func (b *SQSBackend) createJobReliable(ctx context.Context, store state.JobReliabilityStore, plan *state.JobCreatePlan) error {
	err := store.CreateJobAtomic(ctx, plan)
	if err == nil || errors.Is(err, state.ErrConditionFailed) || errors.Is(err, state.ErrCorruptItem) {
		return err
	}
	// Reconcile a timeout/throttle response after a possibly committed
	// transaction. Matching immutable creation fields prove this plan won.
	existing, getErr := b.store.GetJob(ctx, plan.Job.ID)
	if getErr == nil &&
		existing.Type == plan.Job.Type &&
		existing.Queue == plan.Job.Queue &&
		existing.CreatedAt == plan.Job.CreatedAt {
		return nil
	}
	return err
}

func (b *SQSBackend) prepareReliableJobPlan(ctx context.Context, store state.JobReliabilityStore, job *core.Job, now time.Time) (*state.JobCreatePlan, *core.Job, error) {
	scheduled := false
	var scheduledAtMs *int64
	if job.ScheduledAt != "" {
		if scheduledTime, err := time.Parse(time.RFC3339, job.ScheduledAt); err == nil && scheduledTime.After(now) {
			scheduled = true
			due := scheduledTime.UnixMilli()
			scheduledAtMs = &due
		}
	}

	if scheduled {
		job.State = core.StateScheduled
	} else {
		job.State = core.StateAvailable
	}
	job.EnqueuedAt = core.FormatTime(now)

	record := state.JobToRecord(job)
	record.Version = 1
	if !scheduled {
		record.DeliveryGeneration = 1
	}

	plan := &state.JobCreatePlan{
		Job:           record,
		ScheduledAtMs: scheduledAtMs,
	}
	if !scheduled {
		plan.Intent = newDeliveryIntent(job.ID, job.Queue, record.DeliveryGeneration, "initial")
	}

	if job.Unique != nil {
		var existing *core.Job
		var err error
		plan.Unique, existing, err = b.prepareUniqueCreate(ctx, store, job, now)
		if err != nil || existing != nil {
			return nil, existing, err
		}
	}
	return plan, nil, nil
}

func (b *SQSBackend) prepareUniqueCreate(ctx context.Context, store state.JobReliabilityStore, job *core.Job, now time.Time) (*state.UniqueJobPlan, *core.Job, error) {
	fingerprint := computeFingerprint(job)
	ttlSeconds := uniqueTTLSeconds(job.Unique)
	if ttlSeconds < 1 {
		ttlSeconds = 1
	}
	plan := &state.UniqueJobPlan{
		Fingerprint:   fingerprint,
		ExpiresAtUnix: now.Unix() + ttlSeconds,
		NowUnix:       now.Unix(),
	}

	mapping, err := store.GetUniqueKeyRecord(ctx, fingerprint)
	if err != nil {
		return nil, nil, fmt.Errorf("get unique key: %w", err)
	}
	if mapping == nil || (mapping.ExpiresAtUnix > 0 && mapping.ExpiresAtUnix <= now.Unix()) {
		return plan, nil, nil
	}
	plan.ExpectedMappingJobID = mapping.JobID

	existingRecord, err := b.store.GetJob(ctx, mapping.JobID)
	if err != nil {
		// A mapping whose job was independently purged can be replaced, but the
		// compare-and-swap still requires that exact mapping.
		return plan, nil, nil
	}
	if !uniqueConflictRelevant(job.Unique, existingRecord.State) {
		return plan, nil, nil
	}

	conflict := job.Unique.OnConflict
	if conflict == "" {
		conflict = "reject"
	}
	switch conflict {
	case "ignore":
		existing := state.RecordToJob(existingRecord)
		existing.IsExisting = true
		return nil, existing, nil
	case "replace":
		if mapping.ReplacementGuardUntilMs > now.UnixMilli() {
			return nil, nil, &core.OJSError{
				Code:    core.ErrCodeDuplicate,
				Message: "A concurrent unique replacement already won.",
				Details: map[string]any{"existing_job_id": mapping.JobID, "unique_key": fingerprint},
			}
		}
		if existingRecord.State == core.StateActive {
			return nil, nil, core.NewConflictError(
				"Cannot replace a unique job while its predecessor is active.",
				map[string]any{"existing_job_id": existingRecord.ID, "current_state": existingRecord.State},
			)
		}
		plan.ReplacementGuardUntilMs = now.Add(time.Second).UnixMilli()
		if !core.IsTerminalState(existingRecord.State) {
			plan.CancelExisting = true
			plan.ExistingState = existingRecord.State
			plan.ExistingVersion = existingRecord.Version
			plan.ExistingDeliveryGeneration = existingRecord.DeliveryGeneration
			plan.ExistingQueue = existingRecord.Queue
			plan.ExistingCreatedAt = existingRecord.CreatedAt
			plan.ExistingWorkflowID = existingRecord.WorkflowID
			plan.CancelledAt = core.FormatTime(now)
		}
		return plan, nil, nil
	default:
		return nil, nil, &core.OJSError{
			Code:    core.ErrCodeDuplicate,
			Message: "A job with the same unique key already exists.",
			Details: map[string]any{
				"existing_job_id": mapping.JobID,
				"unique_key":      fingerprint,
			},
		}
	}
}

// reserveUnique enforces a job's unique policy. It returns a non-nil job when the
// caller should return that job directly (the "ignore" outcome), a non-nil error
// to reject the enqueue, or (nil, nil) to proceed with a fresh unique claim.
func (b *SQSBackend) reserveUnique(ctx context.Context, job *core.Job) (*core.Job, error) {
	fingerprint := computeFingerprint(job)

	conflict := job.Unique.OnConflict
	if conflict == "" {
		conflict = "reject"
	}

	if existingID, err := b.store.GetUniqueKey(ctx, fingerprint); err == nil && existingID != "" {
		if existing, err := b.resolveUniqueConflict(ctx, job, fingerprint, existingID, conflict); existing != nil || err != nil {
			return existing, err
		}
	}

	if err := b.store.SetUniqueKey(ctx, fingerprint, job.ID, uniqueTTLSeconds(job.Unique)); err != nil {
		if strings.Contains(err.Error(), "ConditionalCheckFailedException") {
			return nil, &core.OJSError{
				Code:    core.ErrCodeDuplicate,
				Message: "A job with the same unique key already exists.",
				Details: map[string]any{"unique_key": fingerprint},
			}
		}
		return nil, fmt.Errorf("set unique key: %w", err)
	}
	return nil, nil
}

// resolveUniqueConflict decides what to do when an existing unique claim is found.
// It mirrors reserveUnique's return contract: (job, nil) to short-circuit, (nil,
// err) to reject/fail, or (nil, nil) to fall through and claim the key.
func (b *SQSBackend) resolveUniqueConflict(ctx context.Context, job *core.Job, fingerprint, existingID, conflict string) (*core.Job, error) {
	existingRecord, err := b.store.GetJob(ctx, existingID)
	if err != nil {
		// The claim points at a missing job; treat the key as free.
		return nil, nil
	}
	if !uniqueConflictRelevant(job.Unique, existingRecord.State) {
		return nil, nil
	}

	switch conflict {
	case "ignore":
		existing := state.RecordToJob(existingRecord)
		existing.IsExisting = true
		return existing, nil
	case "replace":
		if _, cancelErr := b.Cancel(ctx, existingID); cancelErr != nil {
			var ojsErr *core.OJSError
			if !errors.As(cancelErr, &ojsErr) || ojsErr.Code != core.ErrCodeNotFound {
				return nil, fmt.Errorf("cancel existing unique job: %w", cancelErr)
			}
		}
		return nil, nil
	default: // reject
		return nil, &core.OJSError{
			Code:    core.ErrCodeDuplicate,
			Message: "A job with the same unique key already exists.",
			Details: map[string]any{
				"existing_job_id": existingID,
				"unique_key":      fingerprint,
			},
		}
	}
}

// uniqueTTLSeconds resolves the unique-claim TTL, defaulting to one hour.
func uniqueTTLSeconds(u *core.UniquePolicy) int64 {
	if u.Period != "" {
		if d, err := core.ParseISO8601Duration(u.Period); err == nil {
			return int64(d.Seconds())
		}
	}
	return 3600
}

// uniqueConflictRelevant reports whether an existing job in the given state
// should be treated as a duplicate for the unique policy.
func uniqueConflictRelevant(u *core.UniquePolicy, existingState string) bool {
	if len(u.States) > 0 {
		for _, s := range u.States {
			if s == existingState {
				return true
			}
		}
		return false
	}
	// Default: any non-terminal state is relevant.
	return !core.IsTerminalState(existingState)
}

// enqueueScheduled persists a future-dated job and registers it with the
// scheduler's due index without sending it to SQS yet.
func (b *SQSBackend) enqueueScheduled(ctx context.Context, job *core.Job, now, scheduledTime time.Time) (*core.Job, error) {
	job.State = core.StateScheduled
	job.EnqueuedAt = core.FormatTime(now)

	record := state.JobToRecord(job)
	if err := b.store.PutJob(ctx, record); err != nil {
		return nil, fmt.Errorf("store scheduled job: %w", err)
	}
	if err := b.store.AddScheduledJob(ctx, job.ID, scheduledTime.UnixMilli()); err != nil {
		return nil, fmt.Errorf("add to scheduled set: %w", err)
	}
	if err := b.store.RegisterQueue(ctx, job.Queue); err != nil {
		return nil, fmt.Errorf("register queue: %w", err)
	}
	return job, nil
}

// enqueueAvailable persists an immediately-runnable job and sends it to SQS.
func (b *SQSBackend) enqueueAvailable(ctx context.Context, job *core.Job, now time.Time) (*core.Job, error) {
	job.State = core.StateAvailable
	job.EnqueuedAt = core.FormatTime(now)

	record := state.JobToRecord(job)
	if err := b.store.PutJob(ctx, record); err != nil {
		return nil, fmt.Errorf("store job: %w", err)
	}
	if err := b.store.RegisterQueue(ctx, job.Queue); err != nil {
		return nil, fmt.Errorf("register queue: %w", err)
	}
	if err := b.sendToSQS(ctx, job); err != nil {
		return nil, fmt.Errorf("send to SQS: %w", err)
	}
	return job, nil
}

// Fetch claims jobs from the specified queues. Its partial-success contract is
// deterministic: once any job is claimed, Fetch returns those jobs with a nil
// error. A later queue failure is logged and metered because HTTP and gRPC
// transports discard response jobs whenever an error is returned.
func (b *SQSBackend) Fetch(ctx context.Context, queues []string, count int, workerID string, visibilityTimeoutMs int) ([]*core.Job, error) {
	visTimeoutSec := clampVisibilityTimeoutSeconds(visibilityTimeoutMs)

	var jobs []*core.Job
	for _, queue := range queues {
		if len(jobs) >= count {
			break
		}

		// Skip paused queues.
		if paused, _ := b.store.IsQueuePaused(ctx, queue); paused {
			continue
		}

		queueURL, err := b.getOrCreateQueueURL(ctx, queue)
		if err != nil {
			if len(jobs) > 0 {
				b.reportPartialFetch("queue", queue, len(jobs), err)
				return jobs, nil
			}
			return nil, err
		}

		fetched, err := b.fetchFromQueue(ctx, queueURL, count-len(jobs), workerID, visTimeoutSec)
		jobs = append(jobs, fetched...)
		if err != nil {
			if len(jobs) > 0 {
				b.reportPartialFetch("queue", queue, len(jobs), err)
				return jobs, nil
			}
			return nil, err
		}
	}

	return jobs, nil
}

// fetchFromQueue receives and claims up to limit jobs from a single SQS queue,
// making repeated ReceiveMessage calls (each capped at SQS's 10-message limit).
func (b *SQSBackend) fetchFromQueue(ctx context.Context, queueURL string, limit int, workerID string, visTimeoutSec int32) ([]*core.Job, error) {
	var jobs []*core.Job
	var firstErr error
	var firstErrStage string

	for len(jobs) < limit {
		batchSize := limit - len(jobs)
		if batchSize > 10 {
			batchSize = 10
		}

		resp, err := b.sqsClient.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl:              aws.String(queueURL),
			MaxNumberOfMessages:   int32(batchSize),
			VisibilityTimeout:     visTimeoutSec,
			MessageAttributeNames: []string{"All"},
			WaitTimeSeconds:       0, // Short polling for fetch operations
		})
		if err != nil {
			receiveErr := fmt.Errorf("SQS ReceiveMessage: %w", err)
			if firstErr == nil {
				firstErr = receiveErr
				firstErrStage = "receive"
			}
			return b.finishQueueFetch(queueURL, jobs, firstErr, firstErrStage)
		}
		if len(resp.Messages) == 0 {
			break
		}

		for _, msg := range resp.Messages {
			if len(jobs) >= limit {
				break
			}
			job, err := b.claimFetchedMessage(ctx, queueURL, msg, workerID, visTimeoutSec, time.Now())
			if err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("claim SQS message %s: %w", receivedMessageID(msg), err)
					firstErrStage = "claim"
				}
				if ctx.Err() != nil {
					return b.finishQueueFetch(queueURL, jobs, firstErr, firstErrStage)
				}
				continue
			}
			if job != nil {
				jobs = append(jobs, job)
			}
		}
	}

	if firstErr != nil {
		return b.finishQueueFetch(queueURL, jobs, firstErr, firstErrStage)
	}
	return jobs, nil
}

func (b *SQSBackend) finishQueueFetch(queueURL string, jobs []*core.Job, err error, stage string) ([]*core.Job, error) {
	if len(jobs) == 0 {
		return nil, err
	}
	b.reportPartialFetch(stage, queueURL, len(jobs), err)
	return jobs, nil
}

func (b *SQSBackend) reportPartialFetch(stage, queue string, claimed int, err error) {
	metrics.FetchPartialFailures.WithLabelValues(stage).Inc()
	b.logger.Warn("partial fetch failure; returning already claimed jobs",
		"stage", stage,
		"queue", queue,
		"claimed_jobs", claimed,
		"error", err,
	)
}

func receivedMessageID(msg sqstypes.Message) string {
	if msg.MessageId == nil || *msg.MessageId == "" {
		return "<unknown>"
	}
	return *msg.MessageId
}

// claimFetchedMessage decodes a received SQS message and transitions the job to
// active. It returns nil when the message is malformed, expired, or cannot be
// claimed (leaving it to SQS visibility recovery).
func (b *SQSBackend) claimFetchedMessage(ctx context.Context, queueURL string, msg sqstypes.Message, workerID string, visibilitySeconds int32, now time.Time) (*core.Job, error) {
	if msg.Body == nil || msg.ReceiptHandle == nil {
		return nil, nil
	}
	var job core.Job
	if err := json.Unmarshal([]byte(*msg.Body), &job); err != nil {
		return nil, nil
	}

	if b.discardIfExpired(ctx, queueURL, &job, msg, now) {
		return nil, nil
	}

	if store, ok := b.reliabilityStore(); ok {
		return b.claimReliableMessage(ctx, store, queueURL, msg, &job, workerID, visibilitySeconds, now)
	}

	updates := map[string]any{
		"state":              core.StateActive,
		"started_at":         core.FormatTime(now),
		"worker_id":          workerID,
		"sqs_receipt_handle": *msg.ReceiptHandle,
	}
	if msg.MessageId != nil {
		updates["sqs_message_id"] = *msg.MessageId
	}
	if err := b.store.UpdateJobState(ctx, job.ID, core.StateActive, updates); err != nil {
		return nil, nil
	}

	record, err := b.store.GetJob(ctx, job.ID)
	if err != nil {
		return nil, nil
	}
	return state.RecordToJob(record), nil
}

func (b *SQSBackend) claimReliableMessage(ctx context.Context, store state.JobReliabilityStore, queueURL string, msg sqstypes.Message, job *core.Job, workerID string, receiveVisibilitySeconds int32, now time.Time) (*core.Job, error) {
	generation, err := deliveryGenerationFromMessage(msg)
	if err != nil {
		return nil, err
	}
	claimVisibilitySeconds, err := b.jobVisibilitySeconds(ctx, job.ID, receiveVisibilitySeconds)
	if err != nil {
		return nil, err
	}
	messageID := ""
	if msg.MessageId != nil {
		messageID = *msg.MessageId
	}
	record, err := store.ClaimJob(ctx, state.JobClaim{
		JobID:             job.ID,
		WorkerID:          workerID,
		ReceiptHandle:     *msg.ReceiptHandle,
		MessageID:         messageID,
		MessageGeneration: generation,
		StartedAt:         core.FormatTime(now),
		DeadlineMs:        now.Add(time.Duration(claimVisibilitySeconds) * time.Second).UnixMilli(),
	})
	if err == nil {
		if visibilityErr := b.applyClaimVisibility(ctx, store, queueURL, msg, record, workerID, receiveVisibilitySeconds, claimVisibilitySeconds, now); visibilityErr != nil {
			return nil, visibilityErr
		}
		return state.RecordToJob(record), nil
	}
	if !errors.Is(err, state.ErrConditionFailed) {
		return nil, err
	}
	if deleteErr := b.deleteReceivedMessage(ctx, queueURL, msg.ReceiptHandle); deleteErr != nil {
		b.logger.Warn("failed to delete unclaimable SQS message", "job_id", job.ID, "error", deleteErr)
	}
	return nil, nil
}

func (b *SQSBackend) jobVisibilitySeconds(ctx context.Context, jobID string, fallback int32) (int32, error) {
	record, err := b.store.GetJob(ctx, jobID)
	if err != nil {
		return 0, err
	}
	if record.VisibilityTimeoutMs == nil {
		return fallback, nil
	}
	return clampVisibilityTimeoutSeconds(*record.VisibilityTimeoutMs), nil
}

func (b *SQSBackend) applyClaimVisibility(ctx context.Context, store state.JobReliabilityStore, queueURL string, msg sqstypes.Message, record *state.JobRecord, workerID string, receiveSeconds, claimSeconds int32, now time.Time) error {
	if claimSeconds == receiveSeconds {
		return nil
	}
	_, err := b.sqsClient.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{
		QueueUrl:          aws.String(queueURL),
		ReceiptHandle:     msg.ReceiptHandle,
		VisibilityTimeout: claimSeconds,
	})
	if err == nil {
		return nil
	}
	_ = store.RefreshClaim(ctx, record.ID, workerID, *msg.ReceiptHandle, record.DeliveryGeneration, now.UnixMilli())
	return fmt.Errorf("set per-job visibility: %w", err)
}

func deliveryGenerationFromMessage(msg sqstypes.Message) (int64, error) {
	attr, ok := msg.MessageAttributes[AttrOJSDeliveryGeneration]
	if !ok || attr.StringValue == nil || *attr.StringValue == "" {
		return 0, nil
	}
	generation, err := strconv.ParseInt(*attr.StringValue, 10, 64)
	if err != nil || generation < 1 {
		return 0, fmt.Errorf("invalid SQS delivery generation %q", *attr.StringValue)
	}
	return generation, nil
}

func (b *SQSBackend) deleteReceivedMessage(ctx context.Context, queueURL string, receiptHandle *string) error {
	if receiptHandle == nil || *receiptHandle == "" {
		return nil
	}
	_, err := b.sqsClient.DeleteMessage(ctx, &sqs.DeleteMessageInput{
		QueueUrl:      aws.String(queueURL),
		ReceiptHandle: receiptHandle,
	})
	return err
}

// discardIfExpired discards and deletes an expired job's message. It reports
// whether the message was consumed (expired).
func (b *SQSBackend) discardIfExpired(ctx context.Context, queueURL string, job *core.Job, msg sqstypes.Message, now time.Time) bool {
	if job.ExpiresAt == "" {
		return false
	}
	expTime, err := time.Parse(time.RFC3339, job.ExpiresAt)
	if err != nil || !now.After(expTime) {
		return false
	}

	if store, ok := b.reliabilityStore(); ok {
		record, getErr := b.store.GetJob(ctx, job.ID)
		if getErr == nil && record.State == core.StateAvailable {
			generation, generationErr := deliveryGenerationFromMessage(msg)
			if generationErr == nil {
				_, transitionErr := store.TransitionJob(ctx, &state.JobTransitionPlan{
					JobID:                      record.ID,
					Queue:                      record.Queue,
					CreatedAt:                  record.CreatedAt,
					FromState:                  core.StateAvailable,
					ToState:                    core.StateDiscarded,
					ExpectedVersion:            record.Version,
					MatchVersion:               true,
					ExpectedDeliveryGeneration: generation,
					MatchDeliveryGeneration:    generation > 0,
					DeleteCurrentIntent:        true,
					WorkflowAdvance:            newWorkflowAdvanceIntent(record, nil, true),
					Updates: map[string]any{
						"completed_at": core.FormatTime(now),
						"error_data":   `{"type":"expired","message":"job expired before it was claimed"}`,
					},
				})
				if transitionErr != nil && !errors.Is(transitionErr, state.ErrConditionFailed) {
					b.logger.Warn("failed to conditionally discard expired job", "job_id", job.ID, "error", transitionErr)
				}
			}
		}
		if delErr := b.deleteReceivedMessage(ctx, queueURL, msg.ReceiptHandle); delErr != nil {
			b.logger.Warn("failed to delete expired job from SQS", "job_id", job.ID, "error", delErr)
		}
		return true
	}

	// Discard expired job (best-effort state update).
	if updErr := b.store.UpdateJobState(ctx, job.ID, core.StateDiscarded, map[string]any{}); updErr != nil {
		slog.Warn("failed to mark expired job discarded", "job_id", job.ID, "error", updErr)
	}
	if _, delErr := b.sqsClient.DeleteMessage(ctx, &sqs.DeleteMessageInput{
		QueueUrl:      aws.String(queueURL),
		ReceiptHandle: msg.ReceiptHandle,
	}); delErr != nil {
		slog.Warn("failed to delete expired job from SQS", "job_id", job.ID, "error", delErr)
	}
	return true
}

// Ack acknowledges a job as completed.
func (b *SQSBackend) Ack(ctx context.Context, jobID string, result []byte) (*core.AckResponse, error) {
	// Get current job record
	record, err := b.store.GetJob(ctx, jobID)
	if err != nil {
		return nil, core.NewNotFoundError("Job", jobID)
	}

	if record.State != core.StateActive {
		return nil, core.NewConflictError(
			fmt.Sprintf("Cannot acknowledge job not in 'active' state. Current state: '%s'.", record.State),
			map[string]any{
				"job_id":         jobID,
				"current_state":  record.State,
				"expected_state": "active",
			},
		)
	}

	if store, ok := b.reliabilityStore(); ok {
		return b.ackReliable(ctx, store, record, result)
	}

	now := core.NowFormatted()

	// Delete SQS message
	if record.SQSReceiptHandle != "" {
		queueURL, err := b.getOrCreateQueueURL(ctx, record.Queue)
		if err == nil {
			if _, delErr := b.sqsClient.DeleteMessage(ctx, &sqs.DeleteMessageInput{
				QueueUrl:      aws.String(queueURL),
				ReceiptHandle: aws.String(record.SQSReceiptHandle),
			}); delErr != nil {
				slog.Warn("failed to delete acked job from SQS", "job_id", jobID, "error", delErr)
			}
		}
	}
	return b.ackLegacyFinalize(ctx, record, result, now)
}

func (b *SQSBackend) ackReliable(ctx context.Context, store state.JobReliabilityStore, record *state.JobRecord, result []byte) (*core.AckResponse, error) {
	now := core.NowFormatted()
	updates := map[string]any{
		"completed_at":         now,
		"error_data":           "",
		"sqs_receipt_handle":   "",
		"worker_id":            "",
		"delivery_deadline_ms": int64(0),
	}
	if len(result) > 0 {
		updates["result"] = string(result)
	}
	updated, err := b.transitionReliable(ctx, store, &state.JobTransitionPlan{
		JobID:                      record.ID,
		Queue:                      record.Queue,
		CreatedAt:                  record.CreatedAt,
		FromState:                  core.StateActive,
		ToState:                    core.StateCompleted,
		ExpectedVersion:            record.Version,
		MatchVersion:               true,
		ExpectedDeliveryGeneration: record.DeliveryGeneration,
		MatchDeliveryGeneration:    true,
		ExpectedReceiptHandle:      record.SQSReceiptHandle,
		Updates:                    updates,
		WorkflowAdvance:            newWorkflowAdvanceIntent(record, result, false),
	})
	if err != nil {
		return nil, transitionConflict("acknowledge", record, err)
	}

	b.deleteOwnedMessageBestEffort(ctx, record, "acked")
	if err := b.store.IncrementQueueCompleted(ctx, record.Queue); err != nil {
		b.logger.Warn("ack: failed to increment completed count", "job_id", record.ID, "queue", record.Queue, "error", err)
	}
	b.advanceWorkflow(ctx, record.ID, core.StateCompleted, result)

	return &core.AckResponse{
		Acknowledged: true,
		ID:           record.ID,
		State:        core.StateCompleted,
		CompletedAt:  now,
		Job:          state.RecordToJob(updated),
	}, nil
}

func (b *SQSBackend) transitionReliable(ctx context.Context, store state.JobReliabilityStore, plan *state.JobTransitionPlan) (*state.JobRecord, error) {
	updated, err := store.TransitionJob(ctx, plan)
	if err == nil || errors.Is(err, state.ErrConditionFailed) || errors.Is(err, state.ErrCorruptItem) {
		return updated, err
	}

	// A timeout after DynamoDB accepted a transaction is ambiguous. Reconcile
	// against the authoritative job state before allowing the caller to retry.
	current, getErr := b.store.GetJob(ctx, plan.JobID)
	expectedGeneration := plan.ExpectedDeliveryGeneration
	if plan.Intent != nil {
		expectedGeneration = plan.Intent.Generation
	}
	if getErr == nil &&
		current.State == plan.ToState &&
		(!plan.MatchDeliveryGeneration || current.DeliveryGeneration == expectedGeneration) {
		return current, nil
	}
	return nil, err
}

func transitionConflict(action string, record *state.JobRecord, err error) error {
	if errors.Is(err, state.ErrConditionFailed) {
		return core.NewConflictError(
			fmt.Sprintf("Cannot %s job because another delivery owner already changed it.", action),
			map[string]any{
				"job_id":              record.ID,
				"expected_state":      core.StateActive,
				"delivery_generation": record.DeliveryGeneration,
			},
		)
	}
	return fmt.Errorf("%s job: %w", action, err)
}

func (b *SQSBackend) deleteOwnedMessageBestEffort(ctx context.Context, record *state.JobRecord, purpose string) {
	if record.SQSReceiptHandle == "" {
		return
	}
	if err := b.deleteJobMessage(ctx, record, purpose); err != nil {
		b.logger.Warn("failed to delete owned SQS message", "job_id", record.ID, "purpose", purpose, "error", err)
	}
}

func (b *SQSBackend) ackLegacyFinalize(ctx context.Context, record *state.JobRecord, result []byte, now string) (*core.AckResponse, error) {
	jobID := record.ID

	// Update job state
	updates := map[string]any{
		"state":        core.StateCompleted,
		"completed_at": now,
	}
	if len(result) > 0 {
		updates["result"] = string(result)
	}
	updates["error_data"] = "" // Clear error field

	if err := b.store.UpdateJobState(ctx, jobID, core.StateCompleted, updates); err != nil {
		return nil, fmt.Errorf("ack job: %w", err)
	}

	// Increment completed count (best-effort statistic)
	if err := b.store.IncrementQueueCompleted(ctx, record.Queue); err != nil {
		b.logger.Warn("ack: failed to increment completed count", "job_id", jobID, "queue", record.Queue, "error", err)
	}

	// Advance workflow if applicable
	b.advanceWorkflow(ctx, jobID, core.StateCompleted, result)

	// Fetch the full updated job
	job, _ := b.Info(ctx, jobID)

	return &core.AckResponse{
		Acknowledged: true,
		ID:           jobID,
		State:        core.StateCompleted,
		CompletedAt:  now,
		Job:          job,
	}, nil
}

// Nack reports a job failure. requeue returns the job to available immediately;
// otherwise it is either retried (with backoff) or discarded once attempts are
// exhausted or the error is non-retryable.
func (b *SQSBackend) Nack(ctx context.Context, jobID string, jobErr *core.JobError, requeue bool) (*core.NackResponse, error) {
	record, err := b.store.GetJob(ctx, jobID)
	if err != nil {
		return nil, core.NewNotFoundError("Job", jobID)
	}

	if record.State != core.StateActive {
		return nil, core.NewConflictError(
			fmt.Sprintf("Cannot fail job not in 'active' state. Current state: '%s'.", record.State),
			map[string]any{
				"job_id":         jobID,
				"current_state":  record.State,
				"expected_state": "active",
			},
		)
	}

	if store, ok := b.reliabilityStore(); ok {
		return b.nackReliable(ctx, store, record, jobErr, requeue)
	}

	now := time.Now()
	attempt := record.Attempt
	maxAttempts := 3
	if record.MaxAttempts != nil {
		maxAttempts = *record.MaxAttempts
	}
	return b.nackLegacy(ctx, jobID, record, jobErr, requeue, now, attempt, maxAttempts)
}

func (b *SQSBackend) nackReliable(ctx context.Context, store state.JobReliabilityStore, record *state.JobRecord, jobErr *core.JobError, requeue bool) (*core.NackResponse, error) {
	now := time.Now()
	attempt := record.Attempt
	if attempt < 1 {
		attempt = 1
	}
	maxAttempts := 3
	if record.MaxAttempts != nil {
		maxAttempts = *record.MaxAttempts
	}

	if requeue {
		generation := record.DeliveryGeneration + 1
		intent := newDeliveryIntent(record.ID, record.Queue, generation, "requeue")
		updated, err := b.transitionReliable(ctx, store, &state.JobTransitionPlan{
			JobID:                      record.ID,
			Queue:                      record.Queue,
			CreatedAt:                  record.CreatedAt,
			FromState:                  core.StateActive,
			ToState:                    core.StateAvailable,
			ExpectedVersion:            record.Version,
			MatchVersion:               true,
			ExpectedDeliveryGeneration: record.DeliveryGeneration,
			MatchDeliveryGeneration:    true,
			ExpectedReceiptHandle:      record.SQSReceiptHandle,
			Intent:                     intent,
			Updates: map[string]any{
				"started_at":           "",
				"worker_id":            "",
				"enqueued_at":          core.FormatTime(now),
				"sqs_receipt_handle":   "",
				"delivery_deadline_ms": int64(0),
				"delivery_generation":  generation,
			},
		})
		if err != nil {
			return nil, transitionConflict("requeue", record, err)
		}
		b.deleteOwnedMessageBestEffort(ctx, record, "requeued")
		b.drainDeliveryBestEffort(ctx)
		return &core.NackResponse{
			ID:          record.ID,
			State:       core.StateAvailable,
			Attempt:     attempt,
			MaxAttempts: maxAttempts,
			Job:         state.RecordToJob(updated),
		}, nil
	}

	errJSON, err := buildNackErrorJSON(jobErr, attempt)
	if err != nil {
		return nil, err
	}
	history, err := b.appendErrorHistory(record.ID, record.ErrorHistory, errJSON)
	if err != nil {
		return nil, err
	}
	retryPolicy := b.parseRetryPolicy(record.ID, record.Retry)
	if isNonRetryable(jobErr, retryPolicy) || attempt >= maxAttempts {
		return b.nackDiscardReliable(ctx, store, record, now, attempt, maxAttempts, errJSON, history, onExhaustionPolicy(retryPolicy))
	}
	return b.nackRetryReliable(ctx, store, record, now, attempt, maxAttempts, errJSON, history, retryPolicy)
}

func (b *SQSBackend) nackDiscardReliable(ctx context.Context, store state.JobReliabilityStore, record *state.JobRecord, now time.Time, attempt, maxAttempts int, errJSON []byte, history, exhaustion string) (*core.NackResponse, error) {
	discardedAt := core.FormatTime(now)
	updates := map[string]any{
		"completed_at":         discardedAt,
		"error_history":        history,
		"sqs_receipt_handle":   "",
		"worker_id":            "",
		"delivery_deadline_ms": int64(0),
	}
	if errJSON != nil {
		updates["error_data"] = string(errJSON)
	}
	updated, err := b.transitionReliable(ctx, store, &state.JobTransitionPlan{
		JobID:                      record.ID,
		Queue:                      record.Queue,
		CreatedAt:                  record.CreatedAt,
		FromState:                  core.StateActive,
		ToState:                    core.StateDiscarded,
		ExpectedVersion:            record.Version,
		MatchVersion:               true,
		ExpectedDeliveryGeneration: record.DeliveryGeneration,
		MatchDeliveryGeneration:    true,
		ExpectedReceiptHandle:      record.SQSReceiptHandle,
		Updates:                    updates,
		AddDeadLetter:              exhaustion == "dead_letter",
		WorkflowAdvance:            newWorkflowAdvanceIntent(record, nil, true),
	})
	if err != nil {
		return nil, transitionConflict("discard", record, err)
	}
	b.deleteOwnedMessageBestEffort(ctx, record, "discarded")
	b.advanceWorkflow(ctx, record.ID, core.StateDiscarded, nil)
	return &core.NackResponse{
		ID:          record.ID,
		State:       core.StateDiscarded,
		Attempt:     attempt,
		MaxAttempts: maxAttempts,
		DiscardedAt: discardedAt,
		Job:         state.RecordToJob(updated),
	}, nil
}

func (b *SQSBackend) nackRetryReliable(ctx context.Context, store state.JobReliabilityStore, record *state.JobRecord, now time.Time, attempt, maxAttempts int, errJSON []byte, history string, retryPolicy *core.RetryPolicy) (*core.NackResponse, error) {
	backoff := core.CalculateBackoff(retryPolicy, attempt)
	nextAttemptAt := now.Add(backoff)
	retryAt := nextAttemptAt.UnixMilli()
	updates := map[string]any{
		"error_history":        history,
		"retry_delay_ms":       backoff.Milliseconds(),
		"sqs_receipt_handle":   "",
		"worker_id":            "",
		"delivery_deadline_ms": int64(0),
	}
	if errJSON != nil {
		updates["error_data"] = string(errJSON)
	}
	updated, err := b.transitionReliable(ctx, store, &state.JobTransitionPlan{
		JobID:                      record.ID,
		Queue:                      record.Queue,
		CreatedAt:                  record.CreatedAt,
		FromState:                  core.StateActive,
		ToState:                    core.StateRetryable,
		ExpectedVersion:            record.Version,
		MatchVersion:               true,
		ExpectedDeliveryGeneration: record.DeliveryGeneration,
		MatchDeliveryGeneration:    true,
		ExpectedReceiptHandle:      record.SQSReceiptHandle,
		Updates:                    updates,
		RetryAtMs:                  &retryAt,
	})
	if err != nil {
		return nil, transitionConflict("retry", record, err)
	}
	b.deleteOwnedMessageBestEffort(ctx, record, "retryable")
	return &core.NackResponse{
		ID:            record.ID,
		State:         core.StateRetryable,
		Attempt:       attempt,
		MaxAttempts:   maxAttempts,
		NextAttemptAt: core.FormatTime(nextAttemptAt),
		Job:           state.RecordToJob(updated),
	}, nil
}

func (b *SQSBackend) nackLegacy(ctx context.Context, jobID string, record *state.JobRecord, jobErr *core.JobError, requeue bool, now time.Time, attempt, maxAttempts int) (*core.NackResponse, error) {
	if requeue {
		return b.nackRequeue(ctx, jobID, record, now, attempt, maxAttempts)
	}

	// Increment attempt counter (counts completed attempt cycles).
	newAttempt := attempt + 1

	errJSON, err := buildNackErrorJSON(jobErr, attempt)
	if err != nil {
		return nil, err
	}
	histJSON, err := b.appendErrorHistory(jobID, record.ErrorHistory, errJSON)
	if err != nil {
		return nil, err
	}

	retryPolicy := b.parseRetryPolicy(jobID, record.Retry)

	if isNonRetryable(jobErr, retryPolicy) || newAttempt >= maxAttempts {
		return b.nackDiscard(ctx, jobID, record, now, newAttempt, maxAttempts, errJSON, histJSON, onExhaustionPolicy(retryPolicy))
	}
	return b.nackRetry(ctx, jobID, record, now, newAttempt, maxAttempts, errJSON, histJSON, retryPolicy)
}

// nackRequeue returns a nacked job to the available state for immediate redelivery.
func (b *SQSBackend) nackRequeue(ctx context.Context, jobID string, record *state.JobRecord, now time.Time, attempt, maxAttempts int) (*core.NackResponse, error) {
	// Change visibility to 0 for immediate redelivery.
	if record.SQSReceiptHandle != "" {
		queueURL, err := b.getOrCreateQueueURL(ctx, record.Queue)
		if err != nil {
			return nil, fmt.Errorf("resolve queue for requeue: %w", err)
		}
		if _, err := b.sqsClient.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{
			QueueUrl:          aws.String(queueURL),
			ReceiptHandle:     aws.String(record.SQSReceiptHandle),
			VisibilityTimeout: 0,
		}); err != nil {
			return nil, fmt.Errorf("requeue SQS message: %w", err)
		}
	}

	updates := map[string]any{
		"state":              core.StateAvailable,
		"started_at":         "",
		"worker_id":          "",
		"enqueued_at":        core.FormatTime(now),
		"sqs_receipt_handle": "",
	}
	if err := b.store.UpdateJobState(ctx, jobID, core.StateAvailable, updates); err != nil {
		return nil, fmt.Errorf("update requeue state: %w", err)
	}

	job, err := b.Info(ctx, jobID)
	if err != nil {
		return nil, fmt.Errorf("fetch requeued job: %w", err)
	}
	return &core.NackResponse{
		ID:          jobID,
		State:       core.StateAvailable,
		Attempt:     attempt,
		MaxAttempts: maxAttempts,
		Job:         job,
	}, nil
}

// nackDiscard moves a job to the discarded terminal state, optionally routing it
// to the dead-letter queue, and advances any owning workflow.
func (b *SQSBackend) nackDiscard(ctx context.Context, jobID string, record *state.JobRecord, now time.Time, newAttempt, maxAttempts int, errJSON []byte, histJSON, onExhaustion string) (*core.NackResponse, error) {
	discardedAt := core.FormatTime(now)

	if err := b.deleteJobMessage(ctx, record, "discard"); err != nil {
		return nil, err
	}

	updates := map[string]any{
		"state":              core.StateDiscarded,
		"completed_at":       discardedAt,
		"error_history":      histJSON,
		"attempt":            newAttempt,
		"sqs_receipt_handle": "",
	}
	if errJSON != nil {
		updates["error_data"] = string(errJSON)
	}
	if err := b.store.UpdateJobState(ctx, jobID, core.StateDiscarded, updates); err != nil {
		return nil, fmt.Errorf("update discarded state: %w", err)
	}

	// Only add to DLQ if on_exhaustion is "dead_letter".
	if onExhaustion == "dead_letter" {
		if err := b.store.AddToDeadLetter(ctx, jobID); err != nil {
			return nil, fmt.Errorf("add to dead letter: %w", err)
		}
	}

	b.advanceWorkflow(ctx, jobID, core.StateDiscarded, nil)

	job, err := b.Info(ctx, jobID)
	if err != nil {
		return nil, fmt.Errorf("fetch discarded job: %w", err)
	}
	return &core.NackResponse{
		ID:          jobID,
		State:       core.StateDiscarded,
		Attempt:     newAttempt,
		MaxAttempts: maxAttempts,
		DiscardedAt: discardedAt,
		Job:         job,
	}, nil
}

// nackRetry schedules a job for a future retry with backoff.
func (b *SQSBackend) nackRetry(ctx context.Context, jobID string, record *state.JobRecord, now time.Time, newAttempt, maxAttempts int, errJSON []byte, histJSON string, retryPolicy *core.RetryPolicy) (*core.NackResponse, error) {
	backoff := core.CalculateBackoff(retryPolicy, newAttempt)
	nextAttemptAt := now.Add(backoff)

	if err := b.deleteJobMessage(ctx, record, "retry"); err != nil {
		return nil, err
	}

	updates := map[string]any{
		"state":              core.StateRetryable,
		"error_history":      histJSON,
		"attempt":            newAttempt,
		"retry_delay_ms":     backoff.Milliseconds(),
		"sqs_receipt_handle": "",
	}
	if errJSON != nil {
		updates["error_data"] = string(errJSON)
	}
	if err := b.store.UpdateJobState(ctx, jobID, core.StateRetryable, updates); err != nil {
		return nil, fmt.Errorf("update retryable state: %w", err)
	}

	if err := b.store.AddRetryJob(ctx, jobID, nextAttemptAt.UnixMilli()); err != nil {
		return nil, fmt.Errorf("add retry schedule: %w", err)
	}

	retryJob, err := b.Info(ctx, jobID)
	if err != nil {
		return nil, fmt.Errorf("fetch retryable job: %w", err)
	}
	return &core.NackResponse{
		ID:            jobID,
		State:         core.StateRetryable,
		Attempt:       newAttempt,
		MaxAttempts:   maxAttempts,
		NextAttemptAt: core.FormatTime(nextAttemptAt),
		Job:           retryJob,
	}, nil
}

// deleteJobMessage removes a job's in-flight SQS message if it holds a receipt
// handle. purpose is used only for error context.
func (b *SQSBackend) deleteJobMessage(ctx context.Context, record *state.JobRecord, purpose string) error {
	if record.SQSReceiptHandle == "" {
		return nil
	}
	queueURL, err := b.getOrCreateQueueURL(ctx, record.Queue)
	if err != nil {
		return fmt.Errorf("resolve queue for %s: %w", purpose, err)
	}
	if _, err := b.sqsClient.DeleteMessage(ctx, &sqs.DeleteMessageInput{
		QueueUrl:      aws.String(queueURL),
		ReceiptHandle: aws.String(record.SQSReceiptHandle),
	}); err != nil {
		return fmt.Errorf("delete %s SQS message: %w", purpose, err)
	}
	return nil
}

// buildNackErrorJSON serializes the reported job error, or returns nil when no
// error was supplied.
func buildNackErrorJSON(jobErr *core.JobError, attempt int) ([]byte, error) {
	if jobErr == nil {
		return nil, nil
	}
	errObj := map[string]any{
		"message": jobErr.Message,
		"attempt": attempt,
	}
	if jobErr.Code != "" {
		errObj["type"] = jobErr.Code
	}
	if jobErr.Type != "" {
		errObj["type"] = jobErr.Type
	}
	if jobErr.Retryable != nil {
		errObj["retryable"] = *jobErr.Retryable
	}
	if jobErr.Details != nil {
		errObj["details"] = jobErr.Details
	}
	data, err := json.Marshal(errObj)
	if err != nil {
		return nil, fmt.Errorf("marshal job error: %w", err)
	}
	return data, nil
}

// appendErrorHistory appends the latest error to the job's existing history and
// returns the serialized result.
func (b *SQSBackend) appendErrorHistory(jobID, existing string, errJSON []byte) (string, error) {
	var history []json.RawMessage
	if existing != "" {
		if err := json.Unmarshal([]byte(existing), &history); err != nil {
			b.logger.Warn("failed to unmarshal error history", "job_id", jobID, "error", err)
		}
	}
	if errJSON != nil {
		history = append(history, json.RawMessage(errJSON))
	}
	data, err := json.Marshal(history)
	if err != nil {
		return "", fmt.Errorf("marshal error history: %w", err)
	}
	return string(data), nil
}

// parseRetryPolicy decodes the job's stored retry policy, returning nil when
// absent or malformed.
func (b *SQSBackend) parseRetryPolicy(jobID, raw string) *core.RetryPolicy {
	if raw == "" {
		return nil
	}
	var rp core.RetryPolicy
	if err := json.Unmarshal([]byte(raw), &rp); err != nil {
		b.logger.Warn("failed to unmarshal retry policy", "job_id", jobID, "error", err)
		return nil
	}
	return &rp
}

// isNonRetryable reports whether a failure must not be retried, honoring an
// explicit retryable=false flag and the retry policy's non-retryable patterns.
func isNonRetryable(jobErr *core.JobError, retryPolicy *core.RetryPolicy) bool {
	if jobErr == nil {
		return false
	}
	if jobErr.Retryable != nil && !*jobErr.Retryable {
		return true
	}
	if retryPolicy == nil {
		return false
	}
	errType := jobErr.Code
	if jobErr.Type != "" {
		errType = jobErr.Type
	}
	for _, pattern := range retryPolicy.NonRetryableErrors {
		if matchesPattern(errType, pattern) || matchesPattern(jobErr.Message, pattern) {
			return true
		}
	}
	return false
}

// onExhaustionPolicy resolves the on-exhaustion behavior, defaulting to "discard".
func onExhaustionPolicy(retryPolicy *core.RetryPolicy) string {
	if retryPolicy != nil && retryPolicy.OnExhaustion != "" {
		return retryPolicy.OnExhaustion
	}
	return "discard"
}

// Info retrieves job details.
func (b *SQSBackend) Info(ctx context.Context, jobID string) (*core.Job, error) {
	record, err := b.store.GetJob(ctx, jobID)
	if err != nil {
		return nil, core.NewNotFoundError("Job", jobID)
	}
	return state.RecordToJob(record), nil
}

// Cancel cancels a job.
func (b *SQSBackend) Cancel(ctx context.Context, jobID string) (*core.Job, error) {
	record, err := b.store.GetJob(ctx, jobID)
	if err != nil {
		return nil, core.NewNotFoundError("Job", jobID)
	}

	currentState := record.State

	if core.IsTerminalState(currentState) {
		return nil, core.NewConflictError(
			fmt.Sprintf("Cannot cancel job in terminal state '%s'.", currentState),
			map[string]any{
				"job_id":        jobID,
				"current_state": currentState,
			},
		)
	}

	if store, ok := b.reliabilityStore(); ok {
		return b.cancelReliable(ctx, store, record)
	}

	now := core.NowFormatted()

	// Delete from SQS if it has a receipt handle
	if record.SQSReceiptHandle != "" {
		queueURL, err := b.getOrCreateQueueURL(ctx, record.Queue)
		if err == nil {
			if _, delErr := b.sqsClient.DeleteMessage(ctx, &sqs.DeleteMessageInput{
				QueueUrl:      aws.String(queueURL),
				ReceiptHandle: aws.String(record.SQSReceiptHandle),
			}); delErr != nil {
				slog.Warn("failed to delete cancelled job from SQS", "job_id", jobID, "error", delErr)
			}
		}
	}
	return b.cancelLegacyFinalize(ctx, record, now)
}

func (b *SQSBackend) cancelReliable(ctx context.Context, store state.JobReliabilityStore, record *state.JobRecord) (*core.Job, error) {
	cancelledAt := core.NowFormatted()
	plan := &state.JobTransitionPlan{
		JobID:                      record.ID,
		Queue:                      record.Queue,
		CreatedAt:                  record.CreatedAt,
		FromState:                  record.State,
		ToState:                    core.StateCancelled,
		ExpectedVersion:            record.Version,
		MatchVersion:               true,
		ExpectedDeliveryGeneration: record.DeliveryGeneration,
		MatchDeliveryGeneration:    record.State == core.StateActive,
		DeleteScheduled:            record.State == core.StateScheduled,
		DeleteRetry:                record.State == core.StateRetryable,
		DeleteCurrentIntent:        record.DeliveryGeneration > 0,
		WorkflowAdvance:            newWorkflowAdvanceIntent(record, nil, true),
		Updates: map[string]any{
			"cancelled_at":         cancelledAt,
			"sqs_receipt_handle":   "",
			"worker_id":            "",
			"delivery_deadline_ms": int64(0),
		},
	}
	if record.State == core.StateActive {
		plan.ExpectedReceiptHandle = record.SQSReceiptHandle
	}
	updated, err := b.transitionReliable(ctx, store, plan)
	if err != nil {
		if errors.Is(err, state.ErrConditionFailed) {
			return nil, core.NewConflictError(
				"Cannot cancel job because another owner already transitioned it.",
				map[string]any{"job_id": record.ID, "current_state": record.State},
			)
		}
		return nil, fmt.Errorf("cancel job: %w", err)
	}

	b.deleteOwnedMessageBestEffort(ctx, record, "cancelled")
	b.advanceWorkflow(ctx, record.ID, core.StateCancelled, nil)
	return state.RecordToJob(updated), nil
}

func (b *SQSBackend) cancelLegacyFinalize(ctx context.Context, record *state.JobRecord, now string) (*core.Job, error) {
	jobID := record.ID

	// Remove from scheduled/retry sets (best-effort cleanup)
	if err := b.store.RemoveScheduledJob(ctx, jobID); err != nil {
		b.logger.Warn("cancel: failed to remove scheduled marker", "job_id", jobID, "error", err)
	}
	if err := b.store.RemoveRetryJob(ctx, jobID); err != nil {
		b.logger.Warn("cancel: failed to remove retry marker", "job_id", jobID, "error", err)
	}

	// Update job state
	updates := map[string]any{
		"state":              core.StateCancelled,
		"cancelled_at":       now,
		"sqs_receipt_handle": "",
	}
	if err := b.store.UpdateJobState(ctx, jobID, core.StateCancelled, updates); err != nil {
		return nil, fmt.Errorf("cancel job: %w", err)
	}

	job := state.RecordToJob(record)
	job.State = core.StateCancelled
	job.CancelledAt = now

	return job, nil
}

// PushBatch atomically enqueues multiple jobs.
func (b *SQSBackend) PushBatch(ctx context.Context, jobs []*core.Job) ([]*core.Job, error) {
	if len(jobs) == 0 {
		return []*core.Job{}, nil
	}

	// Pre-validate all jobs before any writes to avoid partial batch failures
	for _, job := range jobs {
		if err := core.ValidateEnqueueRequest(&core.EnqueueRequest{
			Type: job.Type,
			Args: job.Args,
		}); err != nil {
			return nil, err
		}
	}

	if store, ok := b.reliabilityStore(); ok {
		return b.pushBatchReliable(ctx, store, jobs)
	}

	now := time.Now()

	for _, job := range jobs {
		if job.ID == "" {
			job.ID = core.NewUUIDv7()
		}
		job.State = core.StateAvailable
		job.Attempt = 0
		job.CreatedAt = core.FormatTime(now)
		job.EnqueuedAt = core.FormatTime(now)

		record := state.JobToRecord(job)
		if err := b.store.PutJob(ctx, record); err != nil {
			return nil, fmt.Errorf("store batch job %s: %w", job.ID, err)
		}

		if err := b.store.RegisterQueue(ctx, job.Queue); err != nil {
			return nil, fmt.Errorf("register queue %s: %w", job.Queue, err)
		}
	}

	if err := b.sendBatchToSQS(ctx, jobs); err != nil {
		return nil, fmt.Errorf("send batch to SQS: %w", err)
	}

	return jobs, nil
}

func (b *SQSBackend) pushBatchReliable(ctx context.Context, store state.JobReliabilityStore, jobs []*core.Job) ([]*core.Job, error) {
	now := time.Now()
	plans := make([]*state.JobCreatePlan, 0, len(jobs))
	for _, job := range jobs {
		if job.ID == "" {
			job.ID = core.NewUUIDv7()
		}
		job.CreatedAt = core.FormatTime(now)
		job.Attempt = 0
		plan, existing, err := b.prepareReliableJobPlan(ctx, store, job, now)
		if err != nil {
			return nil, err
		}
		if existing != nil {
			return nil, &core.OJSError{
				Code:    core.ErrCodeDuplicate,
				Message: "A batch entry conflicts with an existing unique job.",
				Details: map[string]any{"existing_job_id": existing.ID},
			}
		}
		plans = append(plans, plan)
	}

	for i, plan := range plans {
		if err := b.createJobReliable(ctx, store, plan); err != nil {
			return nil, fmt.Errorf("store batch job %s at index %d: %w", plan.Job.ID, i, err)
		}
	}
	b.drainDeliveryBestEffort(ctx)
	return jobs, nil
}
