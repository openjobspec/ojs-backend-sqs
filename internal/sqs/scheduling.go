package sqs

import (
	"context"
	"fmt"
	"time"

	"github.com/openjobspec/ojs-backend-sqs/internal/core"
	"github.com/openjobspec/ojs-backend-sqs/internal/state"
)

// PromoteScheduled promotes scheduled jobs that are due to SQS.
func (b *SQSBackend) PromoteScheduled(ctx context.Context) error {
	nowMs := time.Now().UnixMilli()
	jobIDs, err := b.store.GetDueScheduledJobs(ctx, nowMs)
	if err != nil {
		return err
	}

	var firstErr error
	for _, jobID := range jobIDs {
		record, err := b.store.GetJob(ctx, jobID)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("load scheduled job %s: %w", jobID, err)
			}
			b.logger.Error("failed to load scheduled job", "job_id", jobID, "error", err)
			continue
		}

		// Remove from scheduled set
		if err := b.store.RemoveScheduledJob(ctx, jobID); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("remove scheduled job %s: %w", jobID, err)
			}
			b.logger.Error("failed to remove scheduled marker", "job_id", jobID, "error", err)
			continue
		}

		// Update state to available
		now := time.Now()
		updates := map[string]any{
			"state":       core.StateAvailable,
			"enqueued_at": core.FormatTime(now),
		}
		if err := b.store.UpdateJobState(ctx, jobID, core.StateAvailable, updates); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("update scheduled job state %s: %w", jobID, err)
			}
			b.logger.Error("failed to update promoted scheduled job state", "job_id", jobID, "error", err)
			continue
		}

		// Send to SQS
		job := state.RecordToJob(record)
		job.State = core.StateAvailable
		job.EnqueuedAt = core.FormatTime(now)
		if _, err := b.sendToSQS(ctx, job); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("send promoted scheduled job %s: %w", jobID, err)
			}
			b.logger.Error("failed to send promoted scheduled job to SQS",
				"job_id", jobID, "queue", job.Queue, "error", err)
		}
	}

	return firstErr
}

// PromoteRetries promotes retryable jobs that are due for retry.
func (b *SQSBackend) PromoteRetries(ctx context.Context) error {
	nowMs := time.Now().UnixMilli()
	jobIDs, err := b.store.GetDueRetryJobs(ctx, nowMs)
	if err != nil {
		return err
	}

	var firstErr error
	for _, jobID := range jobIDs {
		record, err := b.store.GetJob(ctx, jobID)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("load retry job %s: %w", jobID, err)
			}
			b.logger.Error("failed to load retry job", "job_id", jobID, "error", err)
			continue
		}

		// Remove from retry set
		if err := b.store.RemoveRetryJob(ctx, jobID); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("remove retry marker %s: %w", jobID, err)
			}
			b.logger.Error("failed to remove retry marker", "job_id", jobID, "error", err)
			continue
		}

		// Update state to available
		now := time.Now()
		updates := map[string]any{
			"state":       core.StateAvailable,
			"enqueued_at": core.FormatTime(now),
		}
		if err := b.store.UpdateJobState(ctx, jobID, core.StateAvailable, updates); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("update retry job state %s: %w", jobID, err)
			}
			b.logger.Error("failed to update promoted retry job state", "job_id", jobID, "error", err)
			continue
		}

		// Send to SQS
		job := state.RecordToJob(record)
		job.State = core.StateAvailable
		job.EnqueuedAt = core.FormatTime(now)
		if _, err := b.sendToSQS(ctx, job); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("send promoted retry job %s: %w", jobID, err)
			}
			b.logger.Error("failed to send promoted retry job to SQS",
				"job_id", jobID, "queue", job.Queue, "error", err)
		}
	}

	return firstErr
}
