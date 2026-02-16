package sqs

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/openjobspec/ojs-backend-sqs/internal/core"
)

// changeMessageVisibility extends or resets the visibility timeout of an SQS message.
// timeoutSeconds: the new visibility timeout in seconds.
// Set to 0 for immediate redelivery (nack with requeue).
func (b *SQSBackend) changeMessageVisibility(ctx context.Context, ojsQueue, receiptHandle string, timeoutSeconds int32) error {
	queueURL, err := b.getQueueURL(ctx, ojsQueue)
	if err != nil {
		return err
	}

	_, err = b.sqsClient.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{
		QueueUrl:          aws.String(queueURL),
		ReceiptHandle:     aws.String(receiptHandle),
		VisibilityTimeout: timeoutSeconds,
	})
	if err != nil {
		return fmt.Errorf("SQS ChangeMessageVisibility: %w", err)
	}

	return nil
}

// extendVisibility extends the visibility timeout for a job's SQS message.
// Used by heartbeat to prevent message from becoming visible again.
func (b *SQSBackend) extendVisibility(ctx context.Context, ojsQueue, receiptHandle string, visibilityTimeoutMs int) error {
	timeoutSec := int32(visibilityTimeoutMs / 1000)
	if timeoutSec < 1 {
		timeoutSec = int32(core.DefaultVisibilityTimeoutMs / 1000)
	}
	// SQS max visibility timeout is 12 hours (43200 seconds)
	if timeoutSec > 43200 {
		timeoutSec = 43200
	}
	return b.changeMessageVisibility(ctx, ojsQueue, receiptHandle, timeoutSec)
}
