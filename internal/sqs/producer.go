package sqs

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/openjobspec/ojs-backend-sqs/internal/core"
)

// sendToSQS sends a job as an SQS message.
func (b *SQSBackend) sendToSQS(ctx context.Context, job *core.Job) (string, error) {
	queueURL, err := b.getOrCreateQueueURL(ctx, job.Queue)
	if err != nil {
		return "", err
	}

	body, err := EncodeJob(job)
	if err != nil {
		return "", err
	}

	input := &sqs.SendMessageInput{
		QueueUrl:          aws.String(queueURL),
		MessageBody:       aws.String(body),
		MessageAttributes: BuildMessageAttributes(job),
	}

	// For FIFO queues, set message group ID (use queue name for ordering)
	if b.useFIFO {
		input.MessageGroupId = aws.String(job.Queue)
		// Use job ID as deduplication ID
		input.MessageDeduplicationId = aws.String(job.ID)
	}

	// Apply SQS DelaySeconds for short delays (max 900 seconds = 15 minutes)
	// Longer delays are handled by the scheduler via the state store
	if job.ScheduledAt != "" {
		// Short delays handled at Push level, not here
		// This function is called for immediate sends only
	}

	result, err := b.sqsClient.SendMessage(ctx, input)
	if err != nil {
		return "", fmt.Errorf("SQS SendMessage: %w", err)
	}

	return *result.MessageId, nil
}

// sendBatchToSQS sends multiple jobs to SQS using SendMessageBatch.
// SQS allows max 10 messages per batch. This chunks larger batches.
func (b *SQSBackend) sendBatchToSQS(ctx context.Context, jobs []*core.Job) error {
	// Group jobs by queue
	byQueue := make(map[string][]*core.Job)
	for _, job := range jobs {
		byQueue[job.Queue] = append(byQueue[job.Queue], job)
	}

	for queue, queueJobs := range byQueue {
		queueURL, err := b.getOrCreateQueueURL(ctx, queue)
		if err != nil {
			return err
		}

		// Process in chunks of 10 (SQS batch limit)
		for i := 0; i < len(queueJobs); i += 10 {
			end := i + 10
			if end > len(queueJobs) {
				end = len(queueJobs)
			}
			chunk := queueJobs[i:end]

			entries := make([]sqstypes.SendMessageBatchRequestEntry, 0, len(chunk))
			for idx, job := range chunk {
				body, err := EncodeJob(job)
				if err != nil {
					return err
				}

				entry := sqstypes.SendMessageBatchRequestEntry{
					Id:                aws.String(fmt.Sprintf("%d", idx)),
					MessageBody:       aws.String(body),
					MessageAttributes: BuildMessageAttributes(job),
				}

				if b.useFIFO {
					entry.MessageGroupId = aws.String(job.Queue)
					entry.MessageDeduplicationId = aws.String(job.ID)
				}

				entries = append(entries, entry)
			}

			result, err := b.sqsClient.SendMessageBatch(ctx, &sqs.SendMessageBatchInput{
				QueueUrl: aws.String(queueURL),
				Entries:  entries,
			})
			if err != nil {
				return fmt.Errorf("SQS SendMessageBatch: %w", err)
			}

			if len(result.Failed) > 0 {
				return fmt.Errorf("SQS SendMessageBatch: %d messages failed, first error: %s",
					len(result.Failed), aws.ToString(result.Failed[0].Message))
			}
		}
	}

	return nil
}
