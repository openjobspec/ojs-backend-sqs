package sqs

import (
	"context"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
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
		"VisibilityTimeout":             "30", // Default 30s
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

	// Also create DLQ
	go b.ensureDLQ(context.Background(), ojsQueue, url)

	// Cache the URL
	b.queueURLsMu.Lock()
	b.queueURLs[ojsQueue] = url
	b.queueURLsMu.Unlock()

	return url, nil
}

// ensureDLQ creates a dead letter queue and configures the redrive policy.
func (b *SQSBackend) ensureDLQ(ctx context.Context, ojsQueue, mainQueueURL string) {
	dlqName := b.sqsDLQName(ojsQueue)
	dlqAttrs := map[string]string{
		"MessageRetentionPeriod": "1209600", // 14 days
	}
	if b.useFIFO {
		dlqAttrs["FifoQueue"] = "true"
		dlqAttrs["ContentBasedDeduplication"] = "true"
	}

	dlqResult, err := b.sqsClient.CreateQueue(ctx, &sqs.CreateQueueInput{
		QueueName:  aws.String(dlqName),
		Attributes: dlqAttrs,
	})
	if err != nil {
		return
	}

	// Get the DLQ ARN
	dlqAttrsResult, err := b.sqsClient.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl:       dlqResult.QueueUrl,
		AttributeNames: []sqstypes.QueueAttributeName{sqstypes.QueueAttributeNameQueueArn},
	})
	if err != nil {
		return
	}

	dlqArn, ok := dlqAttrsResult.Attributes["QueueArn"]
	if !ok {
		return
	}

	// Set redrive policy on main queue (max 3 receives before DLQ)
	redrivePolicy := fmt.Sprintf(`{"deadLetterTargetArn":"%s","maxReceiveCount":"3"}`, dlqArn)
	b.sqsClient.SetQueueAttributes(ctx, &sqs.SetQueueAttributesInput{
		QueueUrl: aws.String(mainQueueURL),
		Attributes: map[string]string{
			"RedrivePolicy": redrivePolicy,
		},
	})
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
