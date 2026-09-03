package sqs

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/openjobspec/ojs-backend-sqs/internal/core"
)

// maxSQSVisibilityTimeoutSeconds is the SQS maximum visibility timeout (12 hours).
const maxSQSVisibilityTimeoutSeconds int32 = 43200

// clampVisibilityTimeoutSeconds converts a millisecond visibility timeout into
// the SQS-supported seconds range. Non-positive values fall back to the default
// visibility timeout (a heartbeat must never set 0, which would immediately
// release the message to other consumers); values above SQS's 12-hour maximum
// are capped.
func clampVisibilityTimeoutSeconds(visibilityTimeoutMs int) int32 {
	if visibilityTimeoutMs <= 0 {
		visibilityTimeoutMs = core.DefaultVisibilityTimeoutMs
	}
	if visibilityTimeoutMs >= int(maxSQSVisibilityTimeoutSeconds)*1000 {
		return maxSQSVisibilityTimeoutSeconds
	}
	timeoutSec := (int64(visibilityTimeoutMs) + 999) / 1000
	if timeoutSec > int64(maxSQSVisibilityTimeoutSeconds) {
		timeoutSec = int64(maxSQSVisibilityTimeoutSeconds)
	}
	if timeoutSec < 1 {
		timeoutSec = 1
	}
	return int32(timeoutSec)
}

// changeMessageVisibility extends or resets the visibility timeout of an SQS message.
// timeoutSeconds: the new visibility timeout in seconds.
// Set to 0 for immediate redelivery (nack with requeue).
func (b *SQSBackend) changeMessageVisibility(ctx context.Context, ojsQueue, receiptHandle string, timeoutSeconds int32) error {
	queueURL, err := b.getOrCreateQueueURL(ctx, ojsQueue)
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
