package sqs

import (
	"context"
	"time"

	"github.com/openjobspec/ojs-backend-sqs/internal/core"
	"github.com/openjobspec/ojs-backend-sqs/internal/state"
)

// ListDeadLetter returns dead letter jobs.
func (b *SQSBackend) ListDeadLetter(ctx context.Context, limit, offset int) ([]*core.Job, int, error) {
	records, total, err := b.store.ListJobsByState(ctx, core.StateDiscarded, limit, offset)
	if err != nil {
		return nil, 0, err
	}

	// Filter only jobs in dead letter queue
	var jobs []*core.Job
	for _, record := range records {
		inDLQ, _ := b.store.IsInDeadLetter(ctx, record.ID)
		if inDLQ {
			jobs = append(jobs, state.RecordToJob(record))
		}
	}

	return jobs, total, nil
}

// RetryDeadLetter retries a dead letter job.
func (b *SQSBackend) RetryDeadLetter(ctx context.Context, jobID string) (*core.Job, error) {
	inDLQ, err := b.store.IsInDeadLetter(ctx, jobID)
	if err != nil || !inDLQ {
		return nil, core.NewNotFoundError("Dead letter job", jobID)
	}

	now := time.Now()
	record, err := b.store.GetJob(ctx, jobID)
	if err != nil {
		return nil, core.NewNotFoundError("Job", jobID)
	}

	// Remove from dead letter queue
	b.store.RemoveFromDeadLetter(ctx, jobID)

	// Update job state
	updates := map[string]any{
		"state":          core.StateAvailable,
		"attempt":        0,
		"enqueued_at":    core.FormatTime(now),
		"error_data":     "",
		"error_history":  "",
		"completed_at":   "",
		"retry_delay_ms": nil,
	}
	b.store.UpdateJobState(ctx, jobID, core.StateAvailable, updates)

	// Re-enqueue to SQS
	job := state.RecordToJob(record)
	job.State = core.StateAvailable
	job.Attempt = 0
	job.EnqueuedAt = core.FormatTime(now)
	if _, err := b.sendToSQS(ctx, job); err != nil {
		b.logger.Error("failed to re-enqueue dead letter job to SQS",
			"job_id", jobID, "queue", job.Queue, "error", err)
	}

	return b.Info(ctx, jobID)
}

// DeleteDeadLetter removes a job from the dead letter queue.
func (b *SQSBackend) DeleteDeadLetter(ctx context.Context, jobID string) error {
	inDLQ, err := b.store.IsInDeadLetter(ctx, jobID)
	if err != nil || !inDLQ {
		return core.NewNotFoundError("Dead letter job", jobID)
	}
	return b.store.RemoveFromDeadLetter(ctx, jobID)
}
