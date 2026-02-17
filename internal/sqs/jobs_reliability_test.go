package sqs

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/openjobspec/ojs-backend-sqs/internal/core"
	"github.com/openjobspec/ojs-backend-sqs/internal/state"
)

type storeMock struct {
	putJobFn         func(ctx context.Context, record *state.JobRecord) error
	getJobFn         func(ctx context.Context, jobID string) (*state.JobRecord, error)
	updateJobStateFn func(ctx context.Context, jobID, newState string, updates map[string]any) error
	registerQueueFn  func(ctx context.Context, name string) error
	closeFn          func() error
}

func (m *storeMock) PutJob(ctx context.Context, record *state.JobRecord) error {
	if m.putJobFn != nil {
		return m.putJobFn(ctx, record)
	}
	return nil
}

func (m *storeMock) GetJob(ctx context.Context, jobID string) (*state.JobRecord, error) {
	if m.getJobFn != nil {
		return m.getJobFn(ctx, jobID)
	}
	return nil, errors.New("job not found")
}

func (m *storeMock) UpdateJobState(ctx context.Context, jobID, newState string, updates map[string]any) error {
	if m.updateJobStateFn != nil {
		return m.updateJobStateFn(ctx, jobID, newState, updates)
	}
	return nil
}

func (m *storeMock) DeleteJob(ctx context.Context, jobID string) error { return nil }

func (m *storeMock) ListJobsByQueue(ctx context.Context, queue, state string, limit int) ([]*state.JobRecord, error) {
	return nil, nil
}

func (m *storeMock) ListJobsByState(ctx context.Context, state string, limit, offset int) ([]*state.JobRecord, int, error) {
	return nil, 0, nil
}

func (m *storeMock) CountJobsByQueueAndState(ctx context.Context, queue, state string) (int, error) {
	return 0, nil
}

func (m *storeMock) RegisterQueue(ctx context.Context, name string) error {
	if m.registerQueueFn != nil {
		return m.registerQueueFn(ctx, name)
	}
	return nil
}

func (m *storeMock) ListQueues(ctx context.Context) ([]string, error) { return nil, nil }

func (m *storeMock) SetQueuePaused(ctx context.Context, name string, paused bool) error { return nil }

func (m *storeMock) IsQueuePaused(ctx context.Context, name string) (bool, error) { return false, nil }

func (m *storeMock) IncrementQueueCompleted(ctx context.Context, name string) error { return nil }

func (m *storeMock) GetQueueCompletedCount(ctx context.Context, name string) (int, error) {
	return 0, nil
}

func (m *storeMock) SetUniqueKey(ctx context.Context, fingerprint, jobID string, ttlSeconds int64) error {
	return nil
}

func (m *storeMock) GetUniqueKey(ctx context.Context, fingerprint string) (string, error) {
	return "", errors.New("not found")
}

func (m *storeMock) DeleteUniqueKey(ctx context.Context, fingerprint string) error { return nil }

func (m *storeMock) PutWorkflow(ctx context.Context, wf *state.WorkflowRecord) error { return nil }

func (m *storeMock) GetWorkflow(ctx context.Context, id string) (*state.WorkflowRecord, error) {
	return nil, errors.New("not found")
}

func (m *storeMock) UpdateWorkflow(ctx context.Context, id string, updates map[string]any) error {
	return nil
}

func (m *storeMock) AddWorkflowJob(ctx context.Context, workflowID, jobID string) error { return nil }

func (m *storeMock) GetWorkflowJobs(ctx context.Context, workflowID string) ([]string, error) {
	return nil, nil
}

func (m *storeMock) SetWorkflowResult(ctx context.Context, workflowID string, stepIdx int, result string) error {
	return nil
}

func (m *storeMock) GetWorkflowResults(ctx context.Context, workflowID string) (map[int]string, error) {
	return map[int]string{}, nil
}

func (m *storeMock) PutCron(ctx context.Context, cron *state.CronRecord) error { return nil }

func (m *storeMock) GetCron(ctx context.Context, name string) (*state.CronRecord, error) {
	return nil, errors.New("not found")
}

func (m *storeMock) DeleteCron(ctx context.Context, name string) error { return nil }

func (m *storeMock) ListCrons(ctx context.Context) ([]*state.CronRecord, error) { return nil, nil }

func (m *storeMock) AcquireCronLock(ctx context.Context, name string, timestamp int64) (bool, error) {
	return true, nil
}

func (m *storeMock) GetCronInstance(ctx context.Context, name string) (string, error) { return "", nil }

func (m *storeMock) SetCronInstance(ctx context.Context, name, jobID string) error { return nil }

func (m *storeMock) PutWorker(ctx context.Context, workerID string, data map[string]string) error {
	return nil
}

func (m *storeMock) GetWorkerDirective(ctx context.Context, workerID string) (string, error) {
	return "", nil
}

func (m *storeMock) SetWorkerDirective(ctx context.Context, workerID, directive string) error {
	return nil
}

func (m *storeMock) ListAllJobs(ctx context.Context, filters core.JobListFilters, limit, offset int) ([]*core.Job, int, error) {
	return []*core.Job{}, 0, nil
}

func (m *storeMock) ListAllWorkers(ctx context.Context, limit, offset int) ([]*core.WorkerInfo, core.WorkerSummary, error) {
	return []*core.WorkerInfo{}, core.WorkerSummary{}, nil
}

func (m *storeMock) AddToDeadLetter(ctx context.Context, jobID string) error { return nil }

func (m *storeMock) RemoveFromDeadLetter(ctx context.Context, jobID string) error { return nil }

func (m *storeMock) IsInDeadLetter(ctx context.Context, jobID string) (bool, error) {
	return false, nil
}

func (m *storeMock) AddScheduledJob(ctx context.Context, jobID string, scheduledAtMs int64) error {
	return nil
}

func (m *storeMock) GetDueScheduledJobs(ctx context.Context, nowMs int64) ([]string, error) {
	return nil, nil
}

func (m *storeMock) RemoveScheduledJob(ctx context.Context, jobID string) error { return nil }

func (m *storeMock) AddRetryJob(ctx context.Context, jobID string, retryAtMs int64) error { return nil }

func (m *storeMock) GetDueRetryJobs(ctx context.Context, nowMs int64) ([]string, error) {
	return nil, nil
}

func (m *storeMock) RemoveRetryJob(ctx context.Context, jobID string) error { return nil }

func (m *storeMock) Ping(ctx context.Context) error { return nil }

func (m *storeMock) Close() error {
	if m.closeFn != nil {
		return m.closeFn()
	}
	return nil
}

func TestPush_ReturnsRegisterQueueError(t *testing.T) {
	backend := &SQSBackend{
		store:  &storeMock{registerQueueFn: func(context.Context, string) error { return errors.New("queue register failed") }},
		logger: slog.Default(),
	}

	_, err := backend.Push(context.Background(), &core.Job{
		Type:  "email.send",
		Args:  json.RawMessage(`[]`),
		Queue: "default",
	})
	if err == nil {
		t.Fatal("expected push error")
	}
	if !strings.Contains(err.Error(), "register queue") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPushBatch_ReturnsStoreError(t *testing.T) {
	backend := &SQSBackend{
		store: &storeMock{
			putJobFn: func(context.Context, *state.JobRecord) error {
				return errors.New("put failed")
			},
		},
	}

	_, err := backend.PushBatch(context.Background(), []*core.Job{
		{Type: "email.send", Args: json.RawMessage(`[]`), Queue: "default"},
	})
	if err == nil {
		t.Fatal("expected push batch error")
	}
	if !strings.Contains(err.Error(), "store batch job") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNack_RequeueReturnsUpdateError(t *testing.T) {
	backend := &SQSBackend{
		store: &storeMock{
			getJobFn: func(context.Context, string) (*state.JobRecord, error) {
				return &state.JobRecord{
					ID:      "job-1",
					SK:      "JOB",
					State:   core.StateActive,
					Queue:   "default",
					Attempt: 1,
				}, nil
			},
			updateJobStateFn: func(context.Context, string, string, map[string]any) error {
				return errors.New("update failed")
			},
		},
		logger: slog.Default(),
	}

	_, err := backend.Nack(context.Background(), "job-1", &core.JobError{Message: "fail"}, true)
	if err == nil {
		t.Fatal("expected nack error")
	}
	if !strings.Contains(err.Error(), "update requeue state") {
		t.Fatalf("unexpected error: %v", err)
	}
}
