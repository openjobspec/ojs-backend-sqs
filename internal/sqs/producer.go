package sqs

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/openjobspec/ojs-backend-sqs/internal/core"
	"github.com/openjobspec/ojs-backend-sqs/internal/state"
)

type deliveryMessage struct {
	intent *state.DeliveryIntent
	job    *core.Job
}

type deliveryBatchResult struct {
	successful map[string]bool
	failed     map[string]error
}

// sendToSQS is retained for lightweight Store test doubles. Production enqueue
// paths persist and drain a DeliveryIntent instead.
func (b *SQSBackend) sendToSQS(ctx context.Context, job *core.Job) error {
	return b.sendDelivery(ctx, job, 1)
}

func (b *SQSBackend) sendDelivery(ctx context.Context, job *core.Job, generation int64) error {
	queueURL, err := b.getOrCreateQueueURL(ctx, job.Queue)
	if err != nil {
		return err
	}

	body, err := EncodeJob(job)
	if err != nil {
		return err
	}

	input := &sqs.SendMessageInput{
		QueueUrl:          aws.String(queueURL),
		MessageBody:       aws.String(body),
		MessageAttributes: buildDeliveryMessageAttributes(job, generation),
	}
	if b.useFIFO {
		input.MessageGroupId = aws.String(job.Queue)
		input.MessageDeduplicationId = aws.String(deliveryDeduplicationID(job.ID, generation))
	}

	if _, err := b.sqsClient.SendMessage(ctx, input); err != nil {
		return fmt.Errorf("SQS SendMessage: %w", err)
	}
	return nil
}

func deliveryDeduplicationID(jobID string, generation int64) string {
	return fmt.Sprintf("%s-%d", jobID, generation)
}

// sendDeliveryBatch sends one queue's intents and maps SQS's per-entry result
// back to stable intent IDs. Omitted entries are treated as failures.
func (b *SQSBackend) sendDeliveryBatch(ctx context.Context, queue string, deliveries []deliveryMessage) (*deliveryBatchResult, error) {
	result := &deliveryBatchResult{
		successful: make(map[string]bool, len(deliveries)),
		failed:     make(map[string]error),
	}
	if len(deliveries) == 0 {
		return result, nil
	}
	if len(deliveries) > 10 {
		return nil, fmt.Errorf("SQS batch contains %d entries; maximum is 10", len(deliveries))
	}

	queueURL, err := b.getOrCreateQueueURL(ctx, queue)
	if err != nil {
		return nil, err
	}

	entries, entryToIntent := buildBatchEntries(deliveries, b.useFIFO, result)
	if len(entries) == 0 {
		return result, nil
	}

	response, err := b.sqsClient.SendMessageBatch(ctx, &sqs.SendMessageBatchInput{
		QueueUrl: aws.String(queueURL),
		Entries:  entries,
	})
	if err != nil {
		return nil, fmt.Errorf("SQS SendMessageBatch: %w", err)
	}

	applyBatchResponse(response, entryToIntent, result)
	return result, nil
}

func buildBatchEntries(deliveries []deliveryMessage, fifo bool, result *deliveryBatchResult) ([]sqstypes.SendMessageBatchRequestEntry, map[string]string) {
	entries := make([]sqstypes.SendMessageBatchRequestEntry, 0, len(deliveries))
	entryToIntent := make(map[string]string, len(deliveries))
	for i, delivery := range deliveries {
		body, err := EncodeJob(delivery.job)
		if err != nil {
			result.failed[delivery.intent.ID] = err
			continue
		}
		entryID := fmt.Sprintf("e%d", i)
		entryToIntent[entryID] = delivery.intent.ID
		entry := sqstypes.SendMessageBatchRequestEntry{
			Id:                aws.String(entryID),
			MessageBody:       aws.String(body),
			MessageAttributes: buildDeliveryMessageAttributes(delivery.job, delivery.intent.Generation),
		}
		if fifo {
			entry.MessageGroupId = aws.String(delivery.job.Queue)
			entry.MessageDeduplicationId = aws.String(deliveryDeduplicationID(delivery.job.ID, delivery.intent.Generation))
		}
		entries = append(entries, entry)
	}
	return entries, entryToIntent
}

func applyBatchResponse(response *sqs.SendMessageBatchOutput, entryToIntent map[string]string, result *deliveryBatchResult) {
	for _, success := range response.Successful {
		if intentID, ok := entryToIntent[aws.ToString(success.Id)]; ok {
			result.successful[intentID] = true
		}
	}
	for _, failure := range response.Failed {
		if intentID, ok := entryToIntent[aws.ToString(failure.Id)]; ok {
			result.failed[intentID] = fmt.Errorf("%s: %s", aws.ToString(failure.Code), aws.ToString(failure.Message))
		}
	}
	for _, intentID := range entryToIntent {
		if result.successful[intentID] {
			continue
		}
		if _, ok := result.failed[intentID]; !ok {
			result.failed[intentID] = fmt.Errorf("SQS batch response omitted entry")
		}
	}
}

// sendBatchToSQS is the compatibility path for non-transactional Store test
// doubles. Production batches are drained from durable intents.
func (b *SQSBackend) sendBatchToSQS(ctx context.Context, jobs []*core.Job) error {
	byQueue := make(map[string][]deliveryMessage)
	for _, job := range jobs {
		intent := &state.DeliveryIntent{ID: job.ID, JobID: job.ID, Queue: job.Queue, Generation: 1}
		byQueue[job.Queue] = append(byQueue[job.Queue], deliveryMessage{intent: intent, job: job})
	}

	for queue, deliveries := range byQueue {
		for start := 0; start < len(deliveries); start += 10 {
			end := start + 10
			if end > len(deliveries) {
				end = len(deliveries)
			}
			result, err := b.sendDeliveryBatch(ctx, queue, deliveries[start:end])
			if err != nil {
				return err
			}
			if len(result.failed) > 0 {
				return fmt.Errorf("SQS SendMessageBatch: %d messages failed", len(result.failed))
			}
		}
	}
	return nil
}
