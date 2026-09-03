package sqs

import (
	"context"
	"fmt"
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

	if store, ok := b.reliabilityStore(); ok {
		generation := record.DeliveryGeneration + 1
		intent := newDeliveryIntent(jobID, record.Queue, generation, "dead_letter_replay")
		updated, transitionErr := b.transitionReliable(ctx, store, &state.JobTransitionPlan{
			JobID:                      jobID,
			Queue:                      record.Queue,
			CreatedAt:                  record.CreatedAt,
			FromState:                  core.StateDiscarded,
			ToState:                    core.StateAvailable,
			ExpectedVersion:            record.Version,
			MatchVersion:               true,
			ExpectedDeliveryGeneration: record.DeliveryGeneration,
			Intent:                     intent,
			DeleteDeadLetter:           true,
			Updates: map[string]any{
				"attempt":              0,
				"enqueued_at":          core.FormatTime(now),
				"error_data":           "",
				"error_history":        "",
				"completed_at":         "",
				"retry_delay_ms":       nil,
				"delivery_generation":  generation,
				"sqs_receipt_handle":   "",
				"worker_id":            "",
				"delivery_deadline_ms": int64(0),
			},
		})
		if transitionErr != nil {
			return nil, fmt.Errorf("retry dead letter job: %w", transitionErr)
		}
		b.drainDeliveryBestEffort(ctx)
		return state.RecordToJob(updated), nil
	}

	// Remove from dead letter queue (best-effort; the state update below is the
	// authoritative transition).
	if err := b.store.RemoveFromDeadLetter(ctx, jobID); err != nil {
		b.logger.Warn("retry dead letter: failed to remove DLQ marker", "job_id", jobID, "error", err)
	}

	// Update job state. This must succeed before we re-enqueue to SQS, otherwise
	// the state store and SQS would diverge (job re-delivered but not marked
	// available).
	updates := map[string]any{
		"state":          core.StateAvailable,
		"attempt":        0,
		"enqueued_at":    core.FormatTime(now),
		"error_data":     "",
		"error_history":  "",
		"completed_at":   "",
		"retry_delay_ms": nil,
	}
	if err := b.store.UpdateJobState(ctx, jobID, core.StateAvailable, updates); err != nil {
		return nil, fmt.Errorf("retry dead letter job: %w", err)
	}

	// Re-enqueue to SQS
	job := state.RecordToJob(record)
	job.State = core.StateAvailable
	job.Attempt = 0
	job.EnqueuedAt = core.FormatTime(now)
	if err := b.sendToSQS(ctx, job); err != nil {
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
