package sqs

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/openjobspec/ojs-backend-sqs/internal/core"
	"github.com/openjobspec/ojs-backend-sqs/internal/state"
)

// receiveFromSQS receives messages from an SQS queue.
// SQS ReceiveMessage returns max 10 messages per call.
// For count > 10, multiple calls are made.
func (b *SQSBackend) receiveFromSQS(ctx context.Context, ojsQueue string, count int, visibilityTimeoutSec int32) ([]*core.Job, error) {
	queueURL, err := b.getQueueURL(ctx, ojsQueue)
	if err != nil {
		// Queue might not exist yet
		return nil, nil
	}

	var jobs []*core.Job
	remaining := count

	for remaining > 0 {
		maxMessages := int32(remaining)
		if maxMessages > 10 {
			maxMessages = 10
		}

		input := &sqs.ReceiveMessageInput{
			QueueUrl:              aws.String(queueURL),
			MaxNumberOfMessages:   maxMessages,
			VisibilityTimeout:     visibilityTimeoutSec,
			WaitTimeSeconds:       0, // Don't block on fetch - return immediately
			MessageAttributeNames: []string{"All"},
		}

		result, err := b.sqsClient.ReceiveMessage(ctx, input)
		if err != nil {
			return jobs, fmt.Errorf("SQS ReceiveMessage: %w", err)
		}

		if len(result.Messages) == 0 {
			break // No more messages available
		}

		for _, msg := range result.Messages {
			job, err := DecodeJob(*msg.Body)
			if err != nil {
				// Skip malformed messages
				continue
			}

			// Set the receipt handle for later ack/nack
			job.SQSReceiptHandle = *msg.ReceiptHandle

			// Check if job has expired
			if job.ExpiresAt != "" {
				expTime, parseErr := time.Parse(time.RFC3339, job.ExpiresAt)
				if parseErr == nil && time.Now().After(expTime) {
					// Discard expired job - delete from SQS
					b.deleteFromSQS(ctx, ojsQueue, *msg.ReceiptHandle)
					// Update state store
					b.store.UpdateJobState(ctx, job.ID, core.StateDiscarded, map[string]any{
						"state": core.StateDiscarded,
					})
					continue
				}
			}

			jobs = append(jobs, job)
		}

		remaining -= len(result.Messages)

		// If we got fewer messages than requested, no point in trying again
		if len(result.Messages) < int(maxMessages) {
			break
		}
	}

	return jobs, nil
}

// activateJob updates a job's state to active in the state store.
func (b *SQSBackend) activateJob(ctx context.Context, job *core.Job, workerID string, visibilityTimeoutMs int) error {
	now := core.NowFormatted()

	updates := map[string]any{
		"state":              core.StateActive,
		"started_at":         now,
		"worker_id":          workerID,
		"sqs_receipt_handle": job.SQSReceiptHandle,
	}

	record, _ := b.store.GetJob(ctx, job.ID)
	if record == nil {
		// Job not in state store yet (first time seeing it from SQS)
		// Create it from the decoded message
		job.State = core.StateActive
		job.StartedAt = now
		r := state.JobToRecord(job)
		r.WorkerID = workerID
		r.SQSReceiptHandle = job.SQSReceiptHandle
		return b.store.PutJob(ctx, r)
	}

	return b.store.UpdateJobState(ctx, job.ID, core.StateActive, updates)
}

// deleteFromSQS deletes a message from an SQS queue.
func (b *SQSBackend) deleteFromSQS(ctx context.Context, ojsQueue, receiptHandle string) error {
	queueURL, err := b.getQueueURL(ctx, ojsQueue)
	if err != nil {
		return err
	}

	_, err = b.sqsClient.DeleteMessage(ctx, &sqs.DeleteMessageInput{
		QueueUrl:      aws.String(queueURL),
		ReceiptHandle: aws.String(receiptHandle),
	})
	return err
}
