package sqs

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/openjobspec/ojs-backend-sqs/internal/core"
	"github.com/openjobspec/ojs-backend-sqs/internal/state"
)

// Push enqueues a single job.
func (b *SQSBackend) Push(ctx context.Context, job *core.Job) (*core.Job, error) {
	now := time.Now()

	// Assign ID if not provided
	if job.ID == "" {
		job.ID = core.NewUUIDv7()
	}

	// Set system-managed fields
	job.CreatedAt = core.FormatTime(now)
	job.Attempt = 0

	// Handle unique jobs
	if job.Unique != nil {
		fingerprint := computeFingerprint(job)
		ttl := int64(3600) // default 1 hour
		if job.Unique.Period != "" {
			if d, err := core.ParseISO8601Duration(job.Unique.Period); err == nil {
				ttl = int64(d.Seconds())
			}
		}

		conflict := job.Unique.OnConflict
		if conflict == "" {
			conflict = "reject"
		}

		// Check for existing unique job
		existingID, err := b.store.GetUniqueKey(ctx, fingerprint)
		if err == nil && existingID != "" {
			// Check if existing job is in a relevant state
			existingRecord, stateErr := b.store.GetJob(ctx, existingID)
			if stateErr == nil {
				// Check state filtering: if unique.states is specified, only consider
				// the job a duplicate if its state is in the list
				isRelevant := false
				if len(job.Unique.States) > 0 {
					for _, s := range job.Unique.States {
						if s == existingRecord.State {
							isRelevant = true
							break
						}
					}
				} else {
					// Default: any non-terminal state is relevant
					isRelevant = !core.IsTerminalState(existingRecord.State)
				}

				if isRelevant {
					switch conflict {
					case "reject":
						return nil, &core.OJSError{
							Code:    core.ErrCodeDuplicate,
							Message: "A job with the same unique key already exists.",
							Details: map[string]any{
								"existing_job_id": existingID,
								"unique_key":      fingerprint,
							},
						}
					case "ignore":
						// Return existing job with IsExisting flag
						existing := state.RecordToJob(existingRecord)
						existing.IsExisting = true
						return existing, nil
					case "replace":
						// Cancel existing and create new
						if _, cancelErr := b.Cancel(ctx, existingID); cancelErr != nil {
							if ojsErr, ok := cancelErr.(*core.OJSError); !ok || ojsErr.Code != core.ErrCodeNotFound {
								return nil, fmt.Errorf("cancel existing unique job: %w", cancelErr)
							}
						}
					}
				}
			}
		}

		// Set unique key
		if err := b.store.SetUniqueKey(ctx, fingerprint, job.ID, ttl); err != nil {
			if strings.Contains(err.Error(), "ConditionalCheckFailedException") {
				return nil, &core.OJSError{
					Code:    core.ErrCodeDuplicate,
					Message: "A job with the same unique key already exists.",
					Details: map[string]any{
						"unique_key": fingerprint,
					},
				}
			}
			return nil, fmt.Errorf("set unique key: %w", err)
		}
	}

	// Determine initial state
	if job.ScheduledAt != "" {
		scheduledTime, err := time.Parse(time.RFC3339, job.ScheduledAt)
		if err == nil && scheduledTime.After(now) {
			job.State = core.StateScheduled
			job.EnqueuedAt = core.FormatTime(now)

			// Store job in state store
			record := state.JobToRecord(job)
			if err := b.store.PutJob(ctx, record); err != nil {
				return nil, fmt.Errorf("store scheduled job: %w", err)
			}

			// Add to scheduled set
			if err := b.store.AddScheduledJob(ctx, job.ID, scheduledTime.UnixMilli()); err != nil {
				return nil, fmt.Errorf("add to scheduled set: %w", err)
			}

			// Register queue
			if err := b.store.RegisterQueue(ctx, job.Queue); err != nil {
				return nil, fmt.Errorf("register queue: %w", err)
			}

			return job, nil
		}
		// Past scheduled_at - treat as immediate
	}

	job.State = core.StateAvailable
	job.EnqueuedAt = core.FormatTime(now)

	// Store job in state store
	record := state.JobToRecord(job)
	if err := b.store.PutJob(ctx, record); err != nil {
		return nil, fmt.Errorf("store job: %w", err)
	}

	// Register queue
	if err := b.store.RegisterQueue(ctx, job.Queue); err != nil {
		return nil, fmt.Errorf("register queue: %w", err)
	}

	// Send to SQS
	if _, err := b.sendToSQS(ctx, job); err != nil {
		return nil, fmt.Errorf("send to SQS: %w", err)
	}

	return job, nil
}

// Fetch claims jobs from the specified queues.
func (b *SQSBackend) Fetch(ctx context.Context, queues []string, count int, workerID string, visibilityTimeoutMs int) ([]*core.Job, error) {
	now := time.Now()
	var jobs []*core.Job

	for _, queue := range queues {
		if len(jobs) >= count {
			break
		}

		// Check if queue is paused
		paused, _ := b.store.IsQueuePaused(ctx, queue)
		if paused {
			continue
		}

		remaining := count - len(jobs)

		// SQS allows max 10 messages per ReceiveMessage call
		batchSize := remaining
		if batchSize > 10 {
			batchSize = 10
		}

		// Calculate visibility timeout
		effectiveVisTimeout := visibilityTimeoutMs
		if effectiveVisTimeout <= 0 {
			effectiveVisTimeout = 30000 // default 30s
		}
		visTimeoutSec := int32(effectiveVisTimeout / 1000)

		// Get queue URL
		queueURL, err := b.getOrCreateQueueURL(ctx, queue)
		if err != nil {
			continue
		}

		// Receive messages from SQS
		for len(jobs) < count && batchSize > 0 {
			resp, err := b.sqsClient.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
				QueueUrl:              aws.String(queueURL),
				MaxNumberOfMessages:   int32(batchSize),
				VisibilityTimeout:     visTimeoutSec,
				MessageAttributeNames: []string{"All"},
				WaitTimeSeconds:       0, // Short polling for fetch operations
			})
			if err != nil || len(resp.Messages) == 0 {
				break
			}

			for _, msg := range resp.Messages {
				if len(jobs) >= count {
					break
				}

				// Decode job from message body
				var job core.Job
				if err := json.Unmarshal([]byte(*msg.Body), &job); err != nil {
					continue
				}

				// Check if job has expired
				if job.ExpiresAt != "" {
					expTime, err := time.Parse(time.RFC3339, job.ExpiresAt)
					if err == nil && now.After(expTime) {
						// Discard expired job
						b.store.UpdateJobState(ctx, job.ID, core.StateDiscarded, map[string]any{})
						// Delete from SQS
						b.sqsClient.DeleteMessage(ctx, &sqs.DeleteMessageInput{
							QueueUrl:      aws.String(queueURL),
							ReceiptHandle: msg.ReceiptHandle,
						})
						continue
					}
				}

				// Update job state to active in state store
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
					continue
				}

				// Get the full job from state store
				record, err := b.store.GetJob(ctx, job.ID)
				if err != nil {
					continue
				}

				fullJob := state.RecordToJob(record)
				jobs = append(jobs, fullJob)
			}

			batchSize = count - len(jobs)
			if batchSize > 10 {
				batchSize = 10
			}
		}
	}

	return jobs, nil
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

	now := core.NowFormatted()

	// Delete SQS message
	if record.SQSReceiptHandle != "" {
		queueURL, err := b.getOrCreateQueueURL(ctx, record.Queue)
		if err == nil {
			b.sqsClient.DeleteMessage(ctx, &sqs.DeleteMessageInput{
				QueueUrl:      aws.String(queueURL),
				ReceiptHandle: aws.String(record.SQSReceiptHandle),
			})
		}
	}

	// Update job state
	updates := map[string]any{
		"state":        core.StateCompleted,
		"completed_at": now,
	}
	if result != nil && len(result) > 0 {
		updates["result"] = string(result)
	}
	updates["error_data"] = "" // Clear error field

	if err := b.store.UpdateJobState(ctx, jobID, core.StateCompleted, updates); err != nil {
		return nil, fmt.Errorf("ack job: %w", err)
	}

	// Increment completed count
	b.store.IncrementQueueCompleted(ctx, record.Queue)

	// Advance workflow if applicable
	b.advanceWorkflow(ctx, jobID, core.StateCompleted, result)

	// Fetch the full updated job
	job, _ := b.Info(ctx, jobID)

	return &core.AckResponse{
		Acknowledged: true,
		JobID:        jobID,
		State:        core.StateCompleted,
		CompletedAt:  now,
		Job:          job,
	}, nil
}

// Nack reports a job failure.
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

	now := time.Now()
	attempt := record.Attempt
	maxAttempts := 3
	if record.MaxAttempts != nil {
		maxAttempts = *record.MaxAttempts
	}

	// Handle requeue: return job to available state immediately
	if requeue {
		// Change visibility to 0 for immediate redelivery
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
			JobID:       jobID,
			State:       core.StateAvailable,
			Attempt:     attempt,
			MaxAttempts: maxAttempts,
			Job:         job,
		}, nil
	}

	// Increment attempt counter (counts completed attempt cycles)
	newAttempt := attempt + 1

	// Store error on job
	var errJSON []byte
	if jobErr != nil {
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
		errJSON, err = json.Marshal(errObj)
		if err != nil {
			return nil, fmt.Errorf("marshal job error: %w", err)
		}
	}

	// Update error history
	var errorHistory []json.RawMessage
	if record.ErrorHistory != "" {
		if err := json.Unmarshal([]byte(record.ErrorHistory), &errorHistory); err != nil {
			b.logger.Warn("failed to unmarshal error history", "job_id", jobID, "error", err)
		}
	}
	if errJSON != nil {
		errorHistory = append(errorHistory, json.RawMessage(errJSON))
	}
	histJSON, err := json.Marshal(errorHistory)
	if err != nil {
		return nil, fmt.Errorf("marshal error history: %w", err)
	}

	// Check if error is non-retryable
	isNonRetryable := false
	if jobErr != nil && jobErr.Retryable != nil && !*jobErr.Retryable {
		isNonRetryable = true
	}

	// Check non-retryable error patterns from retry policy
	var retryPolicy *core.RetryPolicy
	if record.Retry != "" {
		var rp core.RetryPolicy
		if err := json.Unmarshal([]byte(record.Retry), &rp); err != nil {
			b.logger.Warn("failed to unmarshal retry policy", "job_id", jobID, "error", err)
		} else {
			retryPolicy = &rp
		}
	}

	if !isNonRetryable && jobErr != nil && retryPolicy != nil {
		for _, pattern := range retryPolicy.NonRetryableErrors {
			errType := jobErr.Code
			if jobErr.Type != "" {
				errType = jobErr.Type
			}
			if matchesPattern(errType, pattern) || matchesPattern(jobErr.Message, pattern) {
				isNonRetryable = true
				break
			}
		}
	}

	// Determine on_exhaustion behavior
	onExhaustion := "discard"
	if retryPolicy != nil && retryPolicy.OnExhaustion != "" {
		onExhaustion = retryPolicy.OnExhaustion
	}

	// Determine next state
	if isNonRetryable || newAttempt >= maxAttempts {
		discardedAt := core.FormatTime(now)

		// Delete from SQS
		if record.SQSReceiptHandle != "" {
			queueURL, err := b.getOrCreateQueueURL(ctx, record.Queue)
			if err != nil {
				return nil, fmt.Errorf("resolve queue for discard: %w", err)
			}
			if _, err := b.sqsClient.DeleteMessage(ctx, &sqs.DeleteMessageInput{
				QueueUrl:      aws.String(queueURL),
				ReceiptHandle: aws.String(record.SQSReceiptHandle),
			}); err != nil {
				return nil, fmt.Errorf("delete discarded SQS message: %w", err)
			}
		}

		updates := map[string]any{
			"state":              core.StateDiscarded,
			"completed_at":       discardedAt,
			"error_history":      string(histJSON),
			"attempt":            newAttempt,
			"sqs_receipt_handle": "",
		}
		if errJSON != nil {
			updates["error_data"] = string(errJSON)
		}

		if err := b.store.UpdateJobState(ctx, jobID, core.StateDiscarded, updates); err != nil {
			return nil, fmt.Errorf("update discarded state: %w", err)
		}

		// Only add to DLQ if on_exhaustion is "dead_letter"
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
			JobID:       jobID,
			State:       core.StateDiscarded,
			Attempt:     newAttempt,
			MaxAttempts: maxAttempts,
			DiscardedAt: discardedAt,
			Job:         job,
		}, nil
	}

	// Retry
	backoff := core.CalculateBackoff(retryPolicy, newAttempt)
	backoffMs := backoff.Milliseconds()
	nextAttemptAt := now.Add(backoff)

	// Delete from SQS
	if record.SQSReceiptHandle != "" {
		queueURL, err := b.getOrCreateQueueURL(ctx, record.Queue)
		if err != nil {
			return nil, fmt.Errorf("resolve queue for retry: %w", err)
		}
		if _, err := b.sqsClient.DeleteMessage(ctx, &sqs.DeleteMessageInput{
			QueueUrl:      aws.String(queueURL),
			ReceiptHandle: aws.String(record.SQSReceiptHandle),
		}); err != nil {
			return nil, fmt.Errorf("delete retry SQS message: %w", err)
		}
	}

	updates := map[string]any{
		"state":              core.StateRetryable,
		"error_history":      string(histJSON),
		"attempt":            newAttempt,
		"retry_delay_ms":     backoffMs,
		"sqs_receipt_handle": "",
	}
	if errJSON != nil {
		updates["error_data"] = string(errJSON)
	}

	if err := b.store.UpdateJobState(ctx, jobID, core.StateRetryable, updates); err != nil {
		return nil, fmt.Errorf("update retryable state: %w", err)
	}

	// Add to retry set
	if err := b.store.AddRetryJob(ctx, jobID, nextAttemptAt.UnixMilli()); err != nil {
		return nil, fmt.Errorf("add retry schedule: %w", err)
	}

	retryJob, err := b.Info(ctx, jobID)
	if err != nil {
		return nil, fmt.Errorf("fetch retryable job: %w", err)
	}
	return &core.NackResponse{
		JobID:         jobID,
		State:         core.StateRetryable,
		Attempt:       newAttempt,
		MaxAttempts:   maxAttempts,
		NextAttemptAt: core.FormatTime(nextAttemptAt),
		Job:           retryJob,
	}, nil
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

	now := core.NowFormatted()

	// Delete from SQS if it has a receipt handle
	if record.SQSReceiptHandle != "" {
		queueURL, err := b.getOrCreateQueueURL(ctx, record.Queue)
		if err == nil {
			b.sqsClient.DeleteMessage(ctx, &sqs.DeleteMessageInput{
				QueueUrl:      aws.String(queueURL),
				ReceiptHandle: aws.String(record.SQSReceiptHandle),
			})
		}
	}

	// Remove from scheduled/retry sets
	b.store.RemoveScheduledJob(ctx, jobID)
	b.store.RemoveRetryJob(ctx, jobID)

	// Update job state
	updates := map[string]any{
		"state":              core.StateCancelled,
		"cancelled_at":       now,
		"sqs_receipt_handle": "",
	}
	b.store.UpdateJobState(ctx, jobID, core.StateCancelled, updates)

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
