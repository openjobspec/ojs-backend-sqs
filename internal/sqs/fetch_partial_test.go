package sqs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/openjobspec/ojs-backend-sqs/internal/core"
	"github.com/openjobspec/ojs-backend-sqs/internal/state"
)

type fetchFaultSQS struct {
	*fakeSQS
	mu              sync.Mutex
	receiveCalls    int
	receiveFn       func(int, *sqs.ReceiveMessageInput) (*sqs.ReceiveMessageOutput, error)
	visibilityFn    func(*sqs.ChangeMessageVisibilityInput) error
	deletedReceipts []string
}

func (f *fetchFaultSQS) ReceiveMessage(_ context.Context, input *sqs.ReceiveMessageInput, _ ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error) {
	f.mu.Lock()
	f.receiveCalls++
	call := f.receiveCalls
	fn := f.receiveFn
	f.mu.Unlock()
	if fn == nil {
		return &sqs.ReceiveMessageOutput{}, nil
	}
	return fn(call, input)
}

func (f *fetchFaultSQS) ChangeMessageVisibility(_ context.Context, input *sqs.ChangeMessageVisibilityInput, _ ...func(*sqs.Options)) (*sqs.ChangeMessageVisibilityOutput, error) {
	if f.visibilityFn != nil {
		if err := f.visibilityFn(input); err != nil {
			return nil, err
		}
	}
	return &sqs.ChangeMessageVisibilityOutput{}, nil
}

func (f *fetchFaultSQS) DeleteMessage(_ context.Context, input *sqs.DeleteMessageInput, _ ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error) {
	f.mu.Lock()
	if input.ReceiptHandle != nil {
		f.deletedReceipts = append(f.deletedReceipts, *input.ReceiptHandle)
	}
	f.mu.Unlock()
	return &sqs.DeleteMessageOutput{}, nil
}

type fetchFaultStore struct {
	*outboxStore
	claimFn      func(state.JobClaim) (*state.JobRecord, error)
	refreshCalls []string
}

func (s *fetchFaultStore) ClaimJob(_ context.Context, claim state.JobClaim) (*state.JobRecord, error) {
	return s.claimFn(claim)
}

func (s *fetchFaultStore) RefreshClaim(_ context.Context, jobID, _ string, receiptHandle string, _ int64, _ int64) error {
	s.mu.Lock()
	s.refreshCalls = append(s.refreshCalls, jobID+":"+receiptHandle)
	s.mu.Unlock()
	return nil
}

func newFetchFaultBackend(client *fetchFaultSQS, store *fetchFaultStore, logBuffer *bytes.Buffer) *SQSBackend {
	backend := newWithSQSClient(client, store, "ojs", false)
	backend.queueURLs["default"] = "queue-url"
	backend.logger = slog.New(slog.NewTextHandler(logBuffer, nil))
	return backend
}

func fetchTestRecord(id string, visibilityMs *int) *state.JobRecord {
	return &state.JobRecord{
		ID:                  id,
		SK:                  "JOB",
		Type:                "fetch." + id,
		Args:                "[]",
		State:               core.StateAvailable,
		Queue:               "default",
		CreatedAt:           core.NowFormatted(),
		DeliveryGeneration:  1,
		Version:             1,
		VisibilityTimeoutMs: visibilityMs,
	}
}

func fetchTestMessage(t *testing.T, id, receipt, generation string) sqstypes.Message {
	t.Helper()
	body, err := json.Marshal(&core.Job{
		ID:    id,
		Type:  "fetch." + id,
		Args:  json.RawMessage(`[]`),
		Queue: "default",
	})
	if err != nil {
		t.Fatalf("marshal message: %v", err)
	}
	return sqstypes.Message{
		Body:          aws.String(string(body)),
		MessageId:     aws.String("message-" + id),
		ReceiptHandle: aws.String(receipt),
		MessageAttributes: map[string]sqstypes.MessageAttributeValue{
			AttrOJSDeliveryGeneration: {
				DataType:    aws.String("Number"),
				StringValue: aws.String(generation),
			},
		},
	}
}

func claimFetchTestRecord(store *fetchFaultStore, claim state.JobClaim) (*state.JobRecord, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	record := store.jobs[claim.JobID]
	if record == nil {
		return nil, errors.New("missing job")
	}
	claimed := *record
	claimed.State = core.StateActive
	claimed.Attempt++
	claimed.WorkerID = claim.WorkerID
	claimed.SQSReceiptHandle = claim.ReceiptHandle
	claimed.DeliveryDeadlineMs = claim.DeadlineMs
	claimed.Version++
	store.jobs[claim.JobID] = &claimed
	return &claimed, nil
}

func TestFetchFromQueue_ReturnsSuccessBeforeCorruptGeneration(t *testing.T) {
	good := fetchTestMessage(t, "good", "receipt-good", "1")
	corrupt := fetchTestMessage(t, "corrupt", "receipt-corrupt", "not-a-number")
	client := &fetchFaultSQS{fakeSQS: &fakeSQS{}}
	client.receiveFn = func(call int, _ *sqs.ReceiveMessageInput) (*sqs.ReceiveMessageOutput, error) {
		switch call {
		case 1:
			return &sqs.ReceiveMessageOutput{Messages: []sqstypes.Message{good, corrupt}}, nil
		case 3:
			return &sqs.ReceiveMessageOutput{Messages: []sqstypes.Message{corrupt}}, nil
		default:
			return &sqs.ReceiveMessageOutput{}, nil
		}
	}
	store := &fetchFaultStore{outboxStore: &outboxStore{
		jobs: map[string]*state.JobRecord{
			"good":    fetchTestRecord("good", nil),
			"corrupt": fetchTestRecord("corrupt", nil),
		},
	}}
	store.claimFn = func(claim state.JobClaim) (*state.JobRecord, error) {
		return claimFetchTestRecord(store, claim)
	}
	var logs bytes.Buffer
	backend := newFetchFaultBackend(client, store, &logs)

	jobs, err := backend.fetchFromQueue(context.Background(), "queue-url", 2, "worker", 1)
	if err != nil {
		t.Fatalf("partial fetch error: %v", err)
	}
	if len(jobs) != 1 || jobs[0].ID != "good" {
		t.Fatalf("partial fetch jobs = %#v, want only good", jobs)
	}
	if !strings.Contains(logs.String(), "partial fetch failure") {
		t.Fatalf("partial failure was not logged: %s", logs.String())
	}

	retried, err := backend.fetchFromQueue(context.Background(), "queue-url", 1, "worker", 1)
	if err == nil || !strings.Contains(err.Error(), "invalid SQS delivery generation") {
		t.Fatalf("corrupt retry error = %v", err)
	}
	if len(retried) != 0 {
		t.Fatalf("corrupt retry returned jobs: %#v", retried)
	}
	if len(client.deletedReceipts) != 0 {
		t.Fatalf("corrupt message was deleted: %v", client.deletedReceipts)
	}
}

func TestFetchFromQueue_IsolatesVisibilityChangeThrottle(t *testing.T) {
	overrideMs := 5_000
	good := fetchTestMessage(t, "good", "receipt-good", "1")
	throttled := fetchTestMessage(t, "throttled", "receipt-throttled", "1")
	client := &fetchFaultSQS{
		fakeSQS: &fakeSQS{},
		receiveFn: func(call int, _ *sqs.ReceiveMessageInput) (*sqs.ReceiveMessageOutput, error) {
			if call == 1 {
				return &sqs.ReceiveMessageOutput{Messages: []sqstypes.Message{good, throttled}}, nil
			}
			return &sqs.ReceiveMessageOutput{}, nil
		},
		visibilityFn: func(input *sqs.ChangeMessageVisibilityInput) error {
			if input.ReceiptHandle != nil && *input.ReceiptHandle == "receipt-throttled" {
				return errors.New("visibility throttled")
			}
			return nil
		},
	}
	store := &fetchFaultStore{outboxStore: &outboxStore{
		jobs: map[string]*state.JobRecord{
			"good":      fetchTestRecord("good", nil),
			"throttled": fetchTestRecord("throttled", &overrideMs),
		},
	}}
	store.claimFn = func(claim state.JobClaim) (*state.JobRecord, error) {
		return claimFetchTestRecord(store, claim)
	}
	var logs bytes.Buffer
	backend := newFetchFaultBackend(client, store, &logs)

	jobs, err := backend.fetchFromQueue(context.Background(), "queue-url", 2, "worker", 1)
	if err != nil {
		t.Fatalf("partial fetch error: %v", err)
	}
	if len(jobs) != 1 || jobs[0].ID != "good" {
		t.Fatalf("partial fetch jobs = %#v, want only good", jobs)
	}
	if len(store.refreshCalls) != 1 || store.refreshCalls[0] != "throttled:receipt-throttled" {
		t.Fatalf("refresh calls = %v", store.refreshCalls)
	}
	if len(client.deletedReceipts) != 0 {
		t.Fatalf("visibility-throttled message was deleted: %v", client.deletedReceipts)
	}
}

func TestFetchFromQueue_RetriesConditionalStoreError(t *testing.T) {
	good := fetchTestMessage(t, "good", "receipt-good", "1")
	retry := fetchTestMessage(t, "retry", "receipt-retry", "1")
	client := &fetchFaultSQS{fakeSQS: &fakeSQS{}}
	client.receiveFn = func(call int, _ *sqs.ReceiveMessageInput) (*sqs.ReceiveMessageOutput, error) {
		switch call {
		case 1:
			return &sqs.ReceiveMessageOutput{Messages: []sqstypes.Message{good, retry}}, nil
		case 3:
			return &sqs.ReceiveMessageOutput{Messages: []sqstypes.Message{retry}}, nil
		default:
			return &sqs.ReceiveMessageOutput{}, nil
		}
	}
	store := &fetchFaultStore{outboxStore: &outboxStore{
		jobs: map[string]*state.JobRecord{
			"good":  fetchTestRecord("good", nil),
			"retry": fetchTestRecord("retry", nil),
		},
	}}
	claimCounts := map[string]int{}
	store.claimFn = func(claim state.JobClaim) (*state.JobRecord, error) {
		claimCounts[claim.JobID]++
		if claim.JobID == "retry" && claimCounts[claim.JobID] == 1 {
			return nil, errors.New("conditional store unavailable")
		}
		return claimFetchTestRecord(store, claim)
	}
	var logs bytes.Buffer
	backend := newFetchFaultBackend(client, store, &logs)

	first, err := backend.fetchFromQueue(context.Background(), "queue-url", 2, "worker", 1)
	if err != nil || len(first) != 1 || first[0].ID != "good" {
		t.Fatalf("first fetch = %#v, err=%v", first, err)
	}
	second, err := backend.fetchFromQueue(context.Background(), "queue-url", 1, "worker", 1)
	if err != nil || len(second) != 1 || second[0].ID != "retry" {
		t.Fatalf("retry fetch = %#v, err=%v", second, err)
	}
	if claimCounts["good"] != 1 || claimCounts["retry"] != 2 {
		t.Fatalf("claim counts = %v", claimCounts)
	}
	if len(client.deletedReceipts) != 0 {
		t.Fatalf("store-failed message was deleted: %v", client.deletedReceipts)
	}
}

func TestFetchFromQueue_ReturnsClaimsWhenSecondReceiveFails(t *testing.T) {
	good := fetchTestMessage(t, "good", "receipt-good", "1")
	client := &fetchFaultSQS{
		fakeSQS: &fakeSQS{},
		receiveFn: func(call int, _ *sqs.ReceiveMessageInput) (*sqs.ReceiveMessageOutput, error) {
			if call == 1 {
				return &sqs.ReceiveMessageOutput{Messages: []sqstypes.Message{good}}, nil
			}
			if call == 2 {
				return nil, errors.New("receive throttled")
			}
			return &sqs.ReceiveMessageOutput{}, nil
		},
	}
	store := &fetchFaultStore{outboxStore: &outboxStore{
		jobs: map[string]*state.JobRecord{"good": fetchTestRecord("good", nil)},
	}}
	claims := 0
	store.claimFn = func(claim state.JobClaim) (*state.JobRecord, error) {
		claims++
		return claimFetchTestRecord(store, claim)
	}
	var logs bytes.Buffer
	backend := newFetchFaultBackend(client, store, &logs)

	jobs, err := backend.fetchFromQueue(context.Background(), "queue-url", 2, "worker", 1)
	if err != nil {
		t.Fatalf("partial receive error leaked: %v", err)
	}
	if len(jobs) != 1 || jobs[0].ID != "good" || claims != 1 {
		t.Fatalf("jobs = %#v, claims=%d", jobs, claims)
	}
	if !strings.Contains(logs.String(), "receive throttled") {
		t.Fatalf("receive partial failure was not logged: %s", logs.String())
	}
}

func TestFetch_ReturnsEarlierQueueClaimsWhenLaterQueueFails(t *testing.T) {
	good := fetchTestMessage(t, "good", "receipt-good", "1")
	client := &fetchFaultSQS{
		fakeSQS: &fakeSQS{},
		receiveFn: func(call int, input *sqs.ReceiveMessageInput) (*sqs.ReceiveMessageOutput, error) {
			if input.QueueUrl != nil && *input.QueueUrl == "queue-one" {
				if call == 1 {
					return &sqs.ReceiveMessageOutput{Messages: []sqstypes.Message{good}}, nil
				}
				return &sqs.ReceiveMessageOutput{}, nil
			}
			return nil, errors.New("later queue unavailable")
		},
	}
	store := &fetchFaultStore{outboxStore: &outboxStore{
		jobs: map[string]*state.JobRecord{"good": fetchTestRecord("good", nil)},
	}}
	store.claimFn = func(claim state.JobClaim) (*state.JobRecord, error) {
		return claimFetchTestRecord(store, claim)
	}
	var logs bytes.Buffer
	backend := newFetchFaultBackend(client, store, &logs)
	backend.queueURLs = map[string]string{"one": "queue-one", "two": "queue-two"}

	jobs, err := backend.Fetch(context.Background(), []string{"one", "two"}, 2, "worker", 1_000)
	if err != nil {
		t.Fatalf("later queue error leaked: %v", err)
	}
	if len(jobs) != 1 || jobs[0].ID != "good" {
		t.Fatalf("jobs = %#v, want good", jobs)
	}
	if !strings.Contains(logs.String(), "later queue unavailable") {
		t.Fatalf("later queue partial failure was not logged: %s", logs.String())
	}
}
