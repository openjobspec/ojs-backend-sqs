package sqs

import (
	"context"
	"fmt"
	"time"

	"github.com/openjobspec/ojs-backend-sqs/internal/core"
	"github.com/openjobspec/ojs-backend-sqs/internal/state"
)

const outboxDrainLimit = 500

func newDeliveryIntent(jobID, queue string, generation int64, sourceType string) *state.DeliveryIntent {
	return &state.DeliveryIntent{
		PK:         fmt.Sprintf("DELIVERY#%s#%020d", jobID, generation),
		SK:         "DELIVERY_OUTBOX",
		ID:         fmt.Sprintf("%s:%d", jobID, generation),
		JobID:      jobID,
		Queue:      queue,
		Generation: generation,
		SourceType: sourceType,
		CreatedAt:  core.FormatTime(time.Now()),
	}
}

func (b *SQSBackend) reliabilityStore() (state.JobReliabilityStore, bool) {
	store, ok := b.store.(state.JobReliabilityStore)
	return store, ok
}

// DrainDeliveryOutbox publishes durable intents in per-queue SQS batches. Only
// successful entries are retired; failed and ambiguous entries remain durable.
func (b *SQSBackend) DrainDeliveryOutbox(ctx context.Context) error {
	store, ok := b.reliabilityStore()
	if !ok {
		return nil
	}

	intents, err := store.ListDeliveryIntents(ctx, outboxDrainLimit)
	if err != nil {
		return err
	}

	byQueue, err := b.collectPendingDeliveries(ctx, store, intents)
	if err != nil {
		return err
	}
	var firstErr error
	for queue, deliveries := range byQueue {
		if err := b.drainDeliveryQueue(ctx, store, queue, deliveries); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (b *SQSBackend) collectPendingDeliveries(ctx context.Context, store state.JobReliabilityStore, intents []*state.DeliveryIntent) (map[string][]deliveryMessage, error) {
	byQueue := make(map[string][]deliveryMessage)
	for _, intent := range intents {
		record, getErr := b.store.GetJob(ctx, intent.JobID)
		if getErr != nil {
			b.logger.Warn("outbox: retiring orphaned delivery intent", "intent_id", intent.ID, "error", getErr)
			if completeErr := store.CompleteDeliveryIntent(ctx, intent); completeErr != nil {
				return nil, completeErr
			}
			continue
		}

		// A claim proves an ambiguous send reached SQS. Terminal or newer
		// generations also make this intent stale. In every case it is safe to
		// retire the intent and its source marker without another send.
		if record.DeliveryGeneration != intent.Generation || record.State != core.StateAvailable {
			if completeErr := store.CompleteDeliveryIntent(ctx, intent); completeErr != nil {
				return nil, completeErr
			}
			continue
		}
		byQueue[intent.Queue] = append(byQueue[intent.Queue], deliveryMessage{
			intent: intent,
			job:    state.RecordToJob(record),
		})
	}
	return byQueue, nil
}

func (b *SQSBackend) drainDeliveryQueue(ctx context.Context, store state.JobReliabilityStore, queue string, deliveries []deliveryMessage) error {
	var firstErr error
	for start := 0; start < len(deliveries); start += 10 {
		end := start + 10
		if end > len(deliveries) {
			end = len(deliveries)
		}
		chunk := deliveries[start:end]
		result, sendErr := b.sendDeliveryBatch(ctx, queue, chunk)
		if sendErr != nil {
			if firstErr == nil {
				firstErr = sendErr
			}
			b.recordBatchSendFailure(ctx, store, chunk, sendErr)
			continue
		}
		if err := b.applyDeliveryBatchResult(ctx, store, chunk, result); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (b *SQSBackend) recordBatchSendFailure(ctx context.Context, store state.JobReliabilityStore, deliveries []deliveryMessage, sendErr error) {
	for _, delivery := range deliveries {
		if recordErr := store.RecordDeliveryFailure(ctx, delivery.intent, sendErr.Error()); recordErr != nil {
			b.logger.Warn("outbox: failed to record send error", "intent_id", delivery.intent.ID, "error", recordErr)
		}
	}
}

func (b *SQSBackend) applyDeliveryBatchResult(ctx context.Context, store state.JobReliabilityStore, deliveries []deliveryMessage, result *deliveryBatchResult) error {
	var firstErr error
	for _, delivery := range deliveries {
		if result.successful[delivery.intent.ID] {
			if completeErr := store.CompleteDeliveryIntent(ctx, delivery.intent); completeErr != nil && firstErr == nil {
				firstErr = completeErr
			}
			continue
		}
		failure := result.failed[delivery.intent.ID]
		if failure == nil {
			failure = fmt.Errorf("delivery was not confirmed")
		}
		if firstErr == nil {
			firstErr = failure
		}
		if recordErr := store.RecordDeliveryFailure(ctx, delivery.intent, failure.Error()); recordErr != nil {
			b.logger.Warn("outbox: failed to retain batch entry failure", "intent_id", delivery.intent.ID, "error", recordErr)
		}
	}
	return firstErr
}

func (b *SQSBackend) drainDeliveryBestEffort(ctx context.Context) {
	if err := b.DrainDeliveryOutbox(ctx); err != nil {
		b.logger.Warn("delivery outbox drain deferred", "error", err)
	}
}
