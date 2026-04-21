package sqs

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/openjobspec/ojs-backend-sqs/internal/core"
	"github.com/openjobspec/ojs-backend-sqs/internal/state"
)

type fakeSQS struct {
	mu          sync.Mutex
	receiveErr  error
	batchFn     func(*sqs.SendMessageBatchInput) (*sqs.SendMessageBatchOutput, error)
	batchInputs []*sqs.SendMessageBatchInput
}

func (f *fakeSQS) ListQueues(context.Context, *sqs.ListQueuesInput, ...func(*sqs.Options)) (*sqs.ListQueuesOutput, error) {
	return &sqs.ListQueuesOutput{}, nil
}

func (f *fakeSQS) CreateQueue(context.Context, *sqs.CreateQueueInput, ...func(*sqs.Options)) (*sqs.CreateQueueOutput, error) {
	return &sqs.CreateQueueOutput{QueueUrl: aws.String("queue-url")}, nil
}

func (f *fakeSQS) GetQueueUrl(context.Context, *sqs.GetQueueUrlInput, ...func(*sqs.Options)) (*sqs.GetQueueUrlOutput, error) {
	return &sqs.GetQueueUrlOutput{QueueUrl: aws.String("queue-url")}, nil
}

func (f *fakeSQS) SetQueueAttributes(context.Context, *sqs.SetQueueAttributesInput, ...func(*sqs.Options)) (*sqs.SetQueueAttributesOutput, error) {
	return &sqs.SetQueueAttributesOutput{}, nil
}

func (f *fakeSQS) SendMessage(context.Context, *sqs.SendMessageInput, ...func(*sqs.Options)) (*sqs.SendMessageOutput, error) {
	return &sqs.SendMessageOutput{}, nil
}

func (f *fakeSQS) SendMessageBatch(_ context.Context, input *sqs.SendMessageBatchInput, _ ...func(*sqs.Options)) (*sqs.SendMessageBatchOutput, error) {
	f.mu.Lock()
	f.batchInputs = append(f.batchInputs, input)
	fn := f.batchFn
	f.mu.Unlock()
	if fn != nil {
		return fn(input)
	}
	successful := make([]sqstypes.SendMessageBatchResultEntry, 0, len(input.Entries))
	for _, entry := range input.Entries {
		successful = append(successful, sqstypes.SendMessageBatchResultEntry{Id: entry.Id})
	}
	return &sqs.SendMessageBatchOutput{Successful: successful}, nil
}

func (f *fakeSQS) ReceiveMessage(context.Context, *sqs.ReceiveMessageInput, ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error) {
	if f.receiveErr != nil {
		return nil, f.receiveErr
	}
	return &sqs.ReceiveMessageOutput{}, nil
}

func (f *fakeSQS) DeleteMessage(context.Context, *sqs.DeleteMessageInput, ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error) {
	return &sqs.DeleteMessageOutput{}, nil
}

func (f *fakeSQS) ChangeMessageVisibility(context.Context, *sqs.ChangeMessageVisibilityInput, ...func(*sqs.Options)) (*sqs.ChangeMessageVisibilityOutput, error) {
	return &sqs.ChangeMessageVisibilityOutput{}, nil
}

type outboxStore struct {
	storeMock
	mu        sync.Mutex
	jobs      map[string]*state.JobRecord
	intents   []*state.DeliveryIntent
	completed []string
	failures  map[string]int
	unique    *state.UniqueKeyRecord
	createFn  func(*state.JobCreatePlan) error
}

func (s *outboxStore) GetJob(_ context.Context, jobID string) (*state.JobRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record := s.jobs[jobID]
	if record == nil {
		return nil, errors.New("not found")
	}
	copy := *record
	return &copy, nil
}

func (s *outboxStore) GetUniqueKeyRecord(context.Context, string) (*state.UniqueKeyRecord, error) {
	return s.unique, nil
}

func (s *outboxStore) CreateJobAtomic(_ context.Context, plan *state.JobCreatePlan) error {
	if s.createFn != nil {
		return s.createFn(plan)
	}
	return nil
}

func (s *outboxStore) ClaimJob(context.Context, state.JobClaim) (*state.JobRecord, error) {
	return nil, state.ErrConditionFailed
}

func (s *outboxStore) TransitionJob(context.Context, *state.JobTransitionPlan) (*state.JobRecord, error) {
	return nil, state.ErrConditionFailed
}

func (s *outboxStore) RefreshClaim(context.Context, string, string, string, int64, int64) error {
	return nil
}

func (s *outboxStore) ListExpiredActiveJobs(context.Context, int64) ([]*state.JobRecord, error) {
	return nil, nil
}

func (s *outboxStore) ListDeliveryIntents(context.Context, int) ([]*state.DeliveryIntent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*state.DeliveryIntent(nil), s.intents...), nil
}

func (s *outboxStore) CompleteDeliveryIntent(_ context.Context, intent *state.DeliveryIntent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.completed = append(s.completed, intent.ID)
	for i, candidate := range s.intents {
		if candidate.ID == intent.ID {
			s.intents = append(s.intents[:i], s.intents[i+1:]...)
			break
		}
	}
	return nil
}

func (s *outboxStore) RecordDeliveryFailure(_ context.Context, intent *state.DeliveryIntent, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures[intent.ID]++
	return nil
}

func newOutboxBackend(client *fakeSQS, store *outboxStore, fifo bool) *SQSBackend {
	backend := newWithSQSClient(client, store, "ojs", fifo)
	backend.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	backend.queueURLs["default"] = "queue-url"
	return backend
}

func TestDrainDeliveryOutbox_RetiresOnlySuccessfulBatchEntries(t *testing.T) {
	store := &outboxStore{
		jobs: map[string]*state.JobRecord{
			"job-1": {ID: "job-1", SK: "JOB", Type: "test.one", State: core.StateAvailable, Queue: "default", CreatedAt: core.NowFormatted(), DeliveryGeneration: 1},
			"job-2": {ID: "job-2", SK: "JOB", Type: "test.two", State: core.StateAvailable, Queue: "default", CreatedAt: core.NowFormatted(), DeliveryGeneration: 1},
		},
		intents: []*state.DeliveryIntent{
			newDeliveryIntent("job-1", "default", 1, "initial"),
			newDeliveryIntent("job-2", "default", 1, "initial"),
		},
		failures: make(map[string]int),
	}
	client := &fakeSQS{batchFn: func(input *sqs.SendMessageBatchInput) (*sqs.SendMessageBatchOutput, error) {
		return &sqs.SendMessageBatchOutput{
			Successful: []sqstypes.SendMessageBatchResultEntry{{Id: input.Entries[0].Id}},
			Failed: []sqstypes.BatchResultErrorEntry{{
				Id:      input.Entries[1].Id,
				Code:    aws.String("InternalError"),
				Message: aws.String("retry me"),
			}},
		}, nil
	}}

	err := newOutboxBackend(client, store, false).DrainDeliveryOutbox(context.Background())
	if err == nil {
		t.Fatal("expected partial batch error")
	}
	if len(store.completed) != 1 || store.completed[0] != "job-1:1" {
		t.Fatalf("completed intents = %v, want only job-1:1", store.completed)
	}
	if len(store.intents) != 1 || store.intents[0].ID != "job-2:1" {
		t.Fatalf("remaining intents = %v, want job-2:1", store.intents)
	}
	if store.failures["job-2:1"] != 1 {
		t.Fatalf("failed intent retry count = %d, want 1", store.failures["job-2:1"])
	}
}

func TestDrainDeliveryOutbox_RetainsAmbiguousSendAndRecovers(t *testing.T) {
	store := &outboxStore{
		jobs: map[string]*state.JobRecord{
			"job-1": {ID: "job-1", SK: "JOB", Type: "test", State: core.StateAvailable, Queue: "default", CreatedAt: core.NowFormatted(), DeliveryGeneration: 3},
		},
		intents:  []*state.DeliveryIntent{newDeliveryIntent("job-1", "default", 3, "retry")},
		failures: make(map[string]int),
	}
	calls := 0
	client := &fakeSQS{batchFn: func(input *sqs.SendMessageBatchInput) (*sqs.SendMessageBatchOutput, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("throttled after ambiguous write")
		}
		return &sqs.SendMessageBatchOutput{
			Successful: []sqstypes.SendMessageBatchResultEntry{{Id: input.Entries[0].Id}},
		}, nil
	}}
	backend := newOutboxBackend(client, store, true)

	if err := backend.DrainDeliveryOutbox(context.Background()); err == nil {
		t.Fatal("expected first ambiguous send to remain pending")
	}
	if len(store.intents) != 1 {
		t.Fatalf("intent was retired after ambiguous send")
	}
	if err := backend.DrainDeliveryOutbox(context.Background()); err != nil {
		t.Fatalf("recovery drain failed: %v", err)
	}
	if len(store.intents) != 0 {
		t.Fatalf("intent remained after confirmed retry")
	}
	if got := aws.ToString(client.batchInputs[1].Entries[0].MessageDeduplicationId); got != "job-1-3" {
		t.Fatalf("FIFO deduplication id = %q, want job-1-3", got)
	}
}

func TestFetch_PropagatesReceiveMessageError(t *testing.T) {
	client := &fakeSQS{receiveErr: errors.New("receive failed")}
	backend := newWithSQSClient(client, &storeMock{}, "ojs", false)
	backend.queueURLs["default"] = "queue-url"

	_, err := backend.Fetch(context.Background(), []string{"default"}, 1, "worker-1", 30_000)
	if err == nil || !errors.Is(err, client.receiveErr) {
		t.Fatalf("Fetch error = %v, want receive error", err)
	}
}

func TestUniqueReplace_TransactionFailureLeavesOriginalUnchanged(t *testing.T) {
	createdAt := core.NowFormatted()
	store := &outboxStore{
		jobs: map[string]*state.JobRecord{
			"original": {
				ID:                 "original",
				SK:                 "JOB",
				Type:               "unique.job",
				State:              core.StateAvailable,
				Queue:              "default",
				CreatedAt:          createdAt,
				Version:            1,
				DeliveryGeneration: 1,
			},
		},
		unique: &state.UniqueKeyRecord{
			JobID:         "original",
			ExpiresAtUnix: time.Now().Add(time.Hour).Unix(),
		},
		failures: make(map[string]int),
	}

	var planSeen *state.JobCreatePlan
	store.createFn = func(plan *state.JobCreatePlan) error {
		planSeen = plan
		return errors.New("transaction rejected")
	}
	backend := newOutboxBackend(&fakeSQS{}, store, false)

	_, err := backend.Push(context.Background(), &core.Job{
		Type:  "unique.job",
		Args:  json.RawMessage(`[]`),
		Queue: "default",
		Unique: &core.UniquePolicy{
			Keys:       []string{"type", "args"},
			Period:     "PT1H",
			OnConflict: "replace",
		},
	})
	if err == nil {
		t.Fatal("expected replacement transaction error")
	}
	if planSeen == nil || planSeen.Unique == nil || !planSeen.Unique.CancelExisting {
		t.Fatalf("replacement did not build atomic cancellation plan: %#v", planSeen)
	}
	original, getErr := store.GetJob(context.Background(), "original")
	if getErr != nil || original.State != core.StateAvailable {
		t.Fatalf("original changed after failed transaction: %#v err=%v", original, getErr)
	}
}

func TestUniqueReplace_RejectsActivePredecessorBeforeTransaction(t *testing.T) {
	store := &outboxStore{
		jobs: map[string]*state.JobRecord{
			"original": {
				ID:                 "original",
				SK:                 "JOB",
				Type:               "unique.job",
				State:              core.StateActive,
				Queue:              "default",
				CreatedAt:          core.NowFormatted(),
				Version:            2,
				DeliveryGeneration: 1,
				SQSReceiptHandle:   "receipt",
			},
		},
		unique: &state.UniqueKeyRecord{
			JobID:         "original",
			ExpiresAtUnix: time.Now().Add(time.Hour).Unix(),
		},
		failures: make(map[string]int),
		createFn: func(*state.JobCreatePlan) error {
			t.Fatal("active replacement reached transaction")
			return nil
		},
	}
	backend := newOutboxBackend(&fakeSQS{}, store, false)
	_, err := backend.Push(context.Background(), &core.Job{
		Type:  "unique.job",
		Args:  json.RawMessage(`[]`),
		Queue: "default",
		Unique: &core.UniquePolicy{
			Keys:       []string{"type", "args"},
			Period:     "PT1H",
			OnConflict: "replace",
		},
	})
	if err == nil {
		t.Fatal("active predecessor replacement unexpectedly succeeded")
	}
}

func TestUniqueReplace_HonorsExpiredTTLAndConfiguredStates(t *testing.T) {
	tests := []struct {
		name          string
		mapping       *state.UniqueKeyRecord
		existingState string
		states        []string
		wantExpected  string
	}{
		{
			name: "expired mapping is free",
			mapping: &state.UniqueKeyRecord{
				JobID:         "original",
				ExpiresAtUnix: time.Now().Add(-time.Minute).Unix(),
			},
			existingState: core.StateAvailable,
		},
		{
			name: "state outside policy swaps without cancellation",
			mapping: &state.UniqueKeyRecord{
				JobID:         "original",
				ExpiresAtUnix: time.Now().Add(time.Hour).Unix(),
			},
			existingState: core.StateAvailable,
			states:        []string{core.StateScheduled},
			wantExpected:  "original",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &outboxStore{
				jobs: map[string]*state.JobRecord{
					"original": {
						ID:        "original",
						SK:        "JOB",
						Type:      "unique.job",
						State:     test.existingState,
						Queue:     "default",
						CreatedAt: core.NowFormatted(),
						Version:   1,
					},
				},
				unique:   test.mapping,
				failures: make(map[string]int),
			}
			var planSeen *state.JobCreatePlan
			store.createFn = func(plan *state.JobCreatePlan) error {
				planSeen = plan
				return nil
			}
			backend := newOutboxBackend(&fakeSQS{}, store, false)
			_, err := backend.Push(context.Background(), &core.Job{
				Type:  "unique.job",
				Args:  json.RawMessage(`[]`),
				Queue: "default",
				Unique: &core.UniquePolicy{
					Keys:       []string{"type", "args"},
					Period:     "PT1H",
					OnConflict: "replace",
					States:     test.states,
				},
			})
			if err != nil {
				t.Fatalf("Push: %v", err)
			}
			if planSeen == nil || planSeen.Unique == nil {
				t.Fatal("missing unique transaction plan")
			}
			if planSeen.Unique.ExpectedMappingJobID != test.wantExpected {
				t.Fatalf("expected mapping = %q, want %q", planSeen.Unique.ExpectedMappingJobID, test.wantExpected)
			}
			if planSeen.Unique.CancelExisting {
				t.Fatal("expired/irrelevant predecessor was cancelled")
			}
		})
	}
}

func TestCreateWorkflow_RejectsZeroJobsBeforePersistence(t *testing.T) {
	backend := New(nil, &storeMock{}, "ojs", false)
	if _, err := backend.CreateWorkflow(context.Background(), &core.WorkflowRequest{Type: "chain"}); err == nil {
		t.Fatal("zero-job workflow unexpectedly persisted")
	}
}
