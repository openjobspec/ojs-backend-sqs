package sqs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/openjobspec/ojs-backend-sqs/internal/core"
	"github.com/openjobspec/ojs-backend-sqs/internal/state"
)

// ReapExpiredClaims applies OJS retry/dead-letter policy to workers that crash
// without ACK/NACK. SQS visibility alone never decides retry exhaustion.
func (b *SQSBackend) ReapExpiredClaims(ctx context.Context) error {
	store, ok := b.reliabilityStore()
	if !ok {
		return nil
	}
	now := time.Now()
	nowMs := now.UnixMilli()
	records, err := store.ListExpiredActiveJobs(ctx, nowMs)
	if err != nil {
		return err
	}

	var firstErr error
	for _, record := range records {
		if err := b.reapExpiredClaim(ctx, store, record, now, nowMs); err != nil &&
			!errors.Is(err, state.ErrConditionFailed) {
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func (b *SQSBackend) reapExpiredClaim(ctx context.Context, store state.JobReliabilityStore, record *state.JobRecord, now time.Time, nowMs int64) error {
	attempt := record.Attempt
	if attempt < 1 {
		attempt = 1
	}
	maxAttempts := 3
	if record.MaxAttempts != nil {
		maxAttempts = *record.MaxAttempts
	}
	retryPolicy := b.parseRetryPolicy(record.ID, record.Retry)
	errJSON, err := buildNackErrorJSON(&core.JobError{
		Message: "worker delivery visibility timeout expired",
		Code:    "delivery_timeout",
	}, attempt)
	if err != nil {
		return err
	}
	history, err := b.appendErrorHistory(record.ID, record.ErrorHistory, errJSON)
	if err != nil {
		return err
	}

	common := &state.JobTransitionPlan{
		JobID:                      record.ID,
		Queue:                      record.Queue,
		CreatedAt:                  record.CreatedAt,
		FromState:                  core.StateActive,
		ExpectedVersion:            record.Version,
		MatchVersion:               true,
		ExpectedDeliveryGeneration: record.DeliveryGeneration,
		MatchDeliveryGeneration:    true,
		ExpectedReceiptHandle:      record.SQSReceiptHandle,
		DeadlineBeforeMs:           &nowMs,
		Updates: map[string]any{
			"error_data":           string(errJSON),
			"error_history":        history,
			"sqs_receipt_handle":   "",
			"worker_id":            "",
			"delivery_deadline_ms": int64(0),
		},
	}

	if attempt >= maxAttempts {
		common.ToState = core.StateDiscarded
		common.Updates["completed_at"] = core.FormatTime(now)
		common.AddDeadLetter = onExhaustionPolicy(retryPolicy) == "dead_letter"
		common.WorkflowAdvance = newWorkflowAdvanceIntent(record, nil, true)
		if _, err := store.TransitionJob(ctx, common); err != nil {
			return fmt.Errorf("discard expired claim %s: %w", record.ID, err)
		}
		b.deleteOwnedMessageBestEffort(ctx, record, "expired")
		b.advanceWorkflow(ctx, record.ID, core.StateDiscarded, nil)
		return nil
	}

	generation := record.DeliveryGeneration + 1
	common.ToState = core.StateAvailable
	common.Intent = newDeliveryIntent(record.ID, record.Queue, generation, "visibility_timeout")
	common.Updates["enqueued_at"] = core.FormatTime(now)
	common.Updates["delivery_generation"] = generation
	if _, err := store.TransitionJob(ctx, common); err != nil {
		return fmt.Errorf("requeue expired claim %s: %w", record.ID, err)
	}
	b.deleteOwnedMessageBestEffort(ctx, record, "expired")
	b.drainDeliveryBestEffort(ctx)
	return nil
}
