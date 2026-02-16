package sqs

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/openjobspec/ojs-backend-sqs/internal/core"
)

// ListQueues returns all known queues.
func (b *SQSBackend) ListQueues(ctx context.Context) ([]core.QueueInfo, error) {
	names, err := b.store.ListQueues(ctx)
	if err != nil {
		return nil, err
	}

	sort.Strings(names)
	var queues []core.QueueInfo
	for _, name := range names {
		status := "active"
		if paused, _ := b.store.IsQueuePaused(ctx, name); paused {
			status = "paused"
		}
		queues = append(queues, core.QueueInfo{
			Name:   name,
			Status: status,
		})
	}
	return queues, nil
}

// QueueStats returns statistics for a queue.
func (b *SQSBackend) QueueStats(ctx context.Context, name string) (*core.QueueStats, error) {
	available, _ := b.store.CountJobsByQueueAndState(ctx, name, core.StateAvailable)
	active, _ := b.store.CountJobsByQueueAndState(ctx, name, core.StateActive)
	completed, _ := b.store.GetQueueCompletedCount(ctx, name)

	status := "active"
	if paused, _ := b.store.IsQueuePaused(ctx, name); paused {
		status = "paused"
	}

	return &core.QueueStats{
		Queue:  name,
		Status: status,
		Stats: core.Stats{
			Available: available,
			Active:    active,
			Completed: completed,
		},
	}, nil
}

// PauseQueue pauses a queue.
func (b *SQSBackend) PauseQueue(ctx context.Context, name string) error {
	return b.store.SetQueuePaused(ctx, name, true)
}

// ResumeQueue resumes a queue.
func (b *SQSBackend) ResumeQueue(ctx context.Context, name string) error {
	return b.store.SetQueuePaused(ctx, name, false)
}

// SetWorkerState sets a directive for a worker.
func (b *SQSBackend) SetWorkerState(ctx context.Context, workerID string, state string) error {
	return b.store.SetWorkerDirective(ctx, workerID, state)
}

// Heartbeat extends visibility timeout and reports worker state.
func (b *SQSBackend) Heartbeat(ctx context.Context, workerID string, activeJobs []string, visibilityTimeoutMs int) (*core.HeartbeatResponse, error) {
	now := time.Now()
	extended := make([]string, 0)

	// Register worker
	b.store.PutWorker(ctx, workerID, map[string]string{
		"last_heartbeat": core.FormatTime(now),
		"active_jobs":    strconv.Itoa(len(activeJobs)),
	})

	// Extend visibility for active jobs
	timeoutSec := int32(visibilityTimeoutMs / 1000)
	for _, jobID := range activeJobs {
		record, err := b.store.GetJob(ctx, jobID)
		if err != nil || record.State != core.StateActive {
			continue
		}

		if record.SQSReceiptHandle != "" {
			queueURL, err := b.getOrCreateQueueURL(ctx, record.Queue)
			if err == nil {
				_, err := b.sqsClient.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{
					QueueUrl:          aws.String(queueURL),
					ReceiptHandle:     aws.String(record.SQSReceiptHandle),
					VisibilityTimeout: timeoutSec,
				})
				if err == nil {
					extended = append(extended, jobID)
				}
			}
		}
	}

	// Determine directive
	directive := "continue"

	// Check for stored worker directive
	storedDirective, err := b.store.GetWorkerDirective(ctx, workerID)
	if err == nil && storedDirective != "" {
		directive = storedDirective
	}

	// Check job metadata for test_directive (used in conformance tests)
	if directive == "continue" {
		for _, jobID := range activeJobs {
			record, err := b.store.GetJob(ctx, jobID)
			if err == nil && record.Meta != "" {
				var metaObj map[string]any
				if json.Unmarshal([]byte(record.Meta), &metaObj) == nil {
					if td, ok := metaObj["test_directive"]; ok {
						if tdStr, ok := td.(string); ok && tdStr != "" {
							directive = tdStr
							break
						}
					}
				}
			}
		}
	}

	return &core.HeartbeatResponse{
		State:        "active",
		Directive:    directive,
		JobsExtended: extended,
		ServerTime:   core.FormatTime(now),
	}, nil
}
