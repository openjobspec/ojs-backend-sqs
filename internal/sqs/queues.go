package sqs

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/openjobspec/ojs-backend-sqs/internal/core"
)

// SQS queue naming convention:
//   ojs-{queue_name}           -- standard queue
//   ojs-{queue_name}.fifo      -- FIFO queue variant
//   ojs-{queue_name}-dlq       -- dead letter queue
//   ojs-{queue_name}-dlq.fifo  -- FIFO dead letter queue

// sqsQueueName returns the SQS queue name for an OJS queue.
func (b *SQSBackend) sqsQueueName(ojsQueue string) string {
	name := b.queuePrefix + "-" + sanitizeQueueName(ojsQueue)
	if b.useFIFO {
		name += ".fifo"
	}
	return name
}

// sqsDLQName returns the SQS DLQ name for an OJS queue.
func (b *SQSBackend) sqsDLQName(ojsQueue string) string {
	name := b.queuePrefix + "-" + sanitizeQueueName(ojsQueue) + "-dlq"
	if b.useFIFO {
		name += ".fifo"
	}
	return name
}

// sanitizeQueueName converts OJS queue name to SQS-compatible name.
// SQS allows alphanumeric, hyphens, and underscores (and .fifo suffix).
func sanitizeQueueName(name string) string {
	return strings.ReplaceAll(name, ".", "-")
}

// getOrCreateQueueURL gets (from cache) or creates an SQS queue and returns its URL.
func (b *SQSBackend) getOrCreateQueueURL(ctx context.Context, ojsQueue string) (string, error) {
	// Check cache first
	b.queueURLsMu.RLock()
	if url, ok := b.queueURLs[ojsQueue]; ok {
		b.queueURLsMu.RUnlock()
		return url, nil
	}
	b.queueURLsMu.RUnlock()

	// Create or get queue
	sqsName := b.sqsQueueName(ojsQueue)
	attrs := map[string]string{
		"ReceiveMessageWaitTimeSeconds": "20", // Long polling
		"VisibilityTimeout":             strconv.Itoa(core.DefaultVisibilityTimeoutMs / 1000),
		"MessageRetentionPeriod":        "1209600", // 14 days
	}

	if b.useFIFO {
		attrs["FifoQueue"] = "true"
		attrs["ContentBasedDeduplication"] = "true"
	}

	result, err := b.sqsClient.CreateQueue(ctx, &sqs.CreateQueueInput{
		QueueName:  aws.String(sqsName),
		Attributes: attrs,
	})
	if err != nil {
		return "", fmt.Errorf("create SQS queue %s: %w", sqsName, err)
	}

	url := *result.QueueUrl

	// Preserve the historical DLQ queue name, but keep retries under OJS state
	// management rather than SQS's fixed maxReceiveCount redrive policy.
	b.ensureManagedQueueConfiguration(ctx, ojsQueue, url)

	// Cache the URL
	b.queueURLsMu.Lock()
	b.queueURLs[ojsQueue] = url
	b.queueURLsMu.Unlock()

	return url, nil
}

// ensureManagedQueueConfiguration preserves the existing native DLQ resource
// while clearing any legacy redrive policy that bypasses per-job OJS retry rules.
func (b *SQSBackend) ensureManagedQueueConfiguration(ctx context.Context, ojsQueue, mainQueueURL string) {
	dlqName := b.sqsDLQName(ojsQueue)
	dlqAttrs := map[string]string{
		"MessageRetentionPeriod": "1209600", // 14 days
	}
	if b.useFIFO {
		dlqAttrs["FifoQueue"] = "true"
		dlqAttrs["ContentBasedDeduplication"] = "true"
	}

	if _, err := b.sqsClient.CreateQueue(ctx, &sqs.CreateQueueInput{
		QueueName:  aws.String(dlqName),
		Attributes: dlqAttrs,
	}); err != nil {
		b.logger.Warn("failed to ensure compatibility DLQ", "queue", ojsQueue, "error", err)
	}

	// AWS removes RedrivePolicy when it is set to an empty string. LocalStack
	// versions that do not support removal return an error, which is safe to log:
	// new queues never receive a redrive policy and existing AWS queues are
	// updated whenever supported.
	if _, err := b.sqsClient.SetQueueAttributes(ctx, &sqs.SetQueueAttributesInput{
		QueueUrl: aws.String(mainQueueURL),
		Attributes: map[string]string{
			"RedrivePolicy": "",
		},
	}); err != nil {
		b.logger.Warn("failed to clear legacy SQS redrive policy", "queue", ojsQueue, "error", err)
	}
}

// getQueueURL gets an existing queue URL without creating it.
func (b *SQSBackend) getQueueURL(ctx context.Context, ojsQueue string) (string, error) {
	// Check cache first
	b.queueURLsMu.RLock()
	if url, ok := b.queueURLs[ojsQueue]; ok {
		b.queueURLsMu.RUnlock()
		return url, nil
	}
	b.queueURLsMu.RUnlock()

	sqsName := b.sqsQueueName(ojsQueue)
	result, err := b.sqsClient.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{
		QueueName: aws.String(sqsName),
	})
	if err != nil {
		return "", fmt.Errorf("get SQS queue URL for %s: %w", sqsName, err)
	}

	url := *result.QueueUrl

	b.queueURLsMu.Lock()
	b.queueURLs[ojsQueue] = url
	b.queueURLsMu.Unlock()

	return url, nil
}
