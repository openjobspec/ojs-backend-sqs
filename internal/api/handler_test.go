package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/openjobspec/ojs-backend-sqs/internal/core"
)

// mockBackend implements core.Backend for testing.
type mockBackend struct {
	pushFunc           func(ctx context.Context, job *core.Job) (*core.Job, error)
	infoFunc           func(ctx context.Context, jobID string) (*core.Job, error)
	cancelFunc         func(ctx context.Context, jobID string) (*core.Job, error)
	fetchFunc          func(ctx context.Context, queues []string, count int, workerID string, vis int) ([]*core.Job, error)
	ackFunc            func(ctx context.Context, jobID string, result []byte) (*core.AckResponse, error)
	nackFunc           func(ctx context.Context, jobID string, jobErr *core.JobError, requeue bool) (*core.NackResponse, error)
	healthFunc         func(ctx context.Context) (*core.HealthResponse, error)
	heartbeatFunc      func(ctx context.Context, workerID string, activeJobs []string, vis int) (*core.HeartbeatResponse, error)
	listQueuesFunc     func(ctx context.Context) ([]core.QueueInfo, error)
	listDeadLetterFunc func(ctx context.Context, limit, offset int) ([]*core.Job, int, error)
	queueStatsFunc     func(ctx context.Context, name string) (*core.QueueStats, error)
}

func (m *mockBackend) Push(ctx context.Context, job *core.Job) (*core.Job, error) {
	if m.pushFunc != nil {
		return m.pushFunc(ctx, job)
	}
	job.ID = "test-job-id"
	job.State = "available"
	return job, nil
}
func (m *mockBackend) Fetch(ctx context.Context, queues []string, count int, workerID string, vis int) ([]*core.Job, error) {
	if m.fetchFunc != nil {
		return m.fetchFunc(ctx, queues, count, workerID, vis)
	}
	return []*core.Job{}, nil
}
func (m *mockBackend) Ack(ctx context.Context, jobID string, result []byte) (*core.AckResponse, error) {
	if m.ackFunc != nil {
		return m.ackFunc(ctx, jobID, result)
	}
	return &core.AckResponse{Acknowledged: true, JobID: jobID, State: "completed"}, nil
}
func (m *mockBackend) Nack(ctx context.Context, jobID string, jobErr *core.JobError, requeue bool) (*core.NackResponse, error) {
	if m.nackFunc != nil {
		return m.nackFunc(ctx, jobID, jobErr, requeue)
	}
	return &core.NackResponse{JobID: jobID, State: "retryable"}, nil
}
func (m *mockBackend) Info(ctx context.Context, jobID string) (*core.Job, error) {
	if m.infoFunc != nil {
		return m.infoFunc(ctx, jobID)
	}
	return nil, core.NewNotFoundError("Job", jobID)
}
func (m *mockBackend) Cancel(ctx context.Context, jobID string) (*core.Job, error) {
	if m.cancelFunc != nil {
		return m.cancelFunc(ctx, jobID)
	}
	return nil, core.NewNotFoundError("Job", jobID)
}
func (m *mockBackend) ListQueues(ctx context.Context) ([]core.QueueInfo, error) {
	if m.listQueuesFunc != nil {
		return m.listQueuesFunc(ctx)
	}
	return []core.QueueInfo{}, nil
}
func (m *mockBackend) Health(ctx context.Context) (*core.HealthResponse, error) {
	if m.healthFunc != nil {
		return m.healthFunc(ctx)
	}
	return &core.HealthResponse{Status: "ok", Version: core.OJSVersion}, nil
}
func (m *mockBackend) Heartbeat(ctx context.Context, workerID string, activeJobs []string, vis int) (*core.HeartbeatResponse, error) {
	if m.heartbeatFunc != nil {
		return m.heartbeatFunc(ctx, workerID, activeJobs, vis)
	}
	return &core.HeartbeatResponse{State: "active", Directive: "continue"}, nil
}
func (m *mockBackend) ListDeadLetter(ctx context.Context, limit, offset int) ([]*core.Job, int, error) {
	if m.listDeadLetterFunc != nil {
		return m.listDeadLetterFunc(ctx, limit, offset)
	}
	return []*core.Job{}, 0, nil
}
func (m *mockBackend) RetryDeadLetter(ctx context.Context, jobID string) (*core.Job, error) {
	return nil, core.NewNotFoundError("Dead letter job", jobID)
}
func (m *mockBackend) DeleteDeadLetter(ctx context.Context, jobID string) error {
	return core.NewNotFoundError("Dead letter job", jobID)
}
func (m *mockBackend) RegisterCron(ctx context.Context, cron *core.CronJob) (*core.CronJob, error) {
	return cron, nil
}
func (m *mockBackend) ListCron(ctx context.Context) ([]*core.CronJob, error) {
	return []*core.CronJob{}, nil
}
func (m *mockBackend) DeleteCron(ctx context.Context, name string) (*core.CronJob, error) {
	return nil, core.NewNotFoundError("Cron job", name)
}
func (m *mockBackend) CreateWorkflow(ctx context.Context, req *core.WorkflowRequest) (*core.Workflow, error) {
	return &core.Workflow{ID: "wf-1", Type: req.Type, State: "running"}, nil
}
func (m *mockBackend) GetWorkflow(ctx context.Context, id string) (*core.Workflow, error) {
	return nil, core.NewNotFoundError("Workflow", id)
}
func (m *mockBackend) CancelWorkflow(ctx context.Context, id string) (*core.Workflow, error) {
	return nil, core.NewNotFoundError("Workflow", id)
}
func (m *mockBackend) AdvanceWorkflow(ctx context.Context, workflowID string, jobID string, result json.RawMessage, failed bool) error {
	return nil
}
func (m *mockBackend) PushBatch(ctx context.Context, jobs []*core.Job) ([]*core.Job, error) {
	return jobs, nil
}
func (m *mockBackend) QueueStats(ctx context.Context, name string) (*core.QueueStats, error) {
	if m.queueStatsFunc != nil {
		return m.queueStatsFunc(ctx, name)
	}
	return &core.QueueStats{Queue: name, Status: "active"}, nil
}
func (m *mockBackend) PauseQueue(ctx context.Context, name string) error  { return nil }
func (m *mockBackend) ResumeQueue(ctx context.Context, name string) error { return nil }
func (m *mockBackend) SetWorkerState(ctx context.Context, workerID string, state string) error {
	return nil
}
func (m *mockBackend) Close() error { return nil }

// --- Job Handler Tests ---

func TestJobCreate_Success(t *testing.T) {
	backend := &mockBackend{}
	h := NewJobHandler(backend)

	body := `{"type":"email.send","args":["test@example.com"]}`
	req := httptest.NewRequest(http.MethodPost, "/ojs/v1/jobs", bytes.NewBufferString(body))
	w := httptest.NewRecorder()

	h.Create(w, req)

	if w.Code != http.StatusCreated {
		t.Errorf("status = %d, want %d", w.Code, http.StatusCreated)
	}
	if ct := w.Header().Get("Content-Type"); ct != core.OJSMediaType {
		t.Errorf("Content-Type = %q, want %q", ct, core.OJSMediaType)
	}
	if loc := w.Header().Get("Location"); loc == "" {
		t.Error("expected Location header")
	}
}

func TestJobCreate_MissingType(t *testing.T) {
	backend := &mockBackend{}
	h := NewJobHandler(backend)

	body := `{"args":["arg1"]}`
	req := httptest.NewRequest(http.MethodPost, "/ojs/v1/jobs", bytes.NewBufferString(body))
	w := httptest.NewRecorder()

	h.Create(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestJobCreate_InvalidJSON(t *testing.T) {
	backend := &mockBackend{}
	h := NewJobHandler(backend)

	req := httptest.NewRequest(http.MethodPost, "/ojs/v1/jobs", bytes.NewBufferString("{invalid"))
	w := httptest.NewRecorder()

	h.Create(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestJobCreate_DuplicateReturnsConflict(t *testing.T) {
	backend := &mockBackend{
		pushFunc: func(ctx context.Context, job *core.Job) (*core.Job, error) {
			return nil, &core.OJSError{Code: core.ErrCodeDuplicate, Message: "duplicate"}
		},
	}
	h := NewJobHandler(backend)

	body := `{"type":"email.send","args":["arg"]}`
	req := httptest.NewRequest(http.MethodPost, "/ojs/v1/jobs", bytes.NewBufferString(body))
	w := httptest.NewRecorder()

	h.Create(w, req)

	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want %d", w.Code, http.StatusConflict)
	}
}

func TestJobGet_NotFound(t *testing.T) {
	backend := &mockBackend{}
	h := NewJobHandler(backend)

	// Set up chi URL params
	req := httptest.NewRequest(http.MethodGet, "/ojs/v1/jobs/nonexistent", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", "nonexistent")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()

	h.Get(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestJobGet_Found(t *testing.T) {
	backend := &mockBackend{
		infoFunc: func(ctx context.Context, jobID string) (*core.Job, error) {
			return &core.Job{ID: jobID, Type: "test", State: "active", Queue: "default"}, nil
		},
	}
	h := NewJobHandler(backend)

	req := httptest.NewRequest(http.MethodGet, "/ojs/v1/jobs/abc", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", "abc")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()

	h.Get(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestJobCancel_NotFound(t *testing.T) {
	backend := &mockBackend{}
	h := NewJobHandler(backend)

	req := httptest.NewRequest(http.MethodDelete, "/ojs/v1/jobs/nonexistent", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", "nonexistent")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()

	h.Cancel(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestJobCancel_Conflict(t *testing.T) {
	backend := &mockBackend{
		cancelFunc: func(ctx context.Context, jobID string) (*core.Job, error) {
			return nil, core.NewConflictError("already completed", nil)
		},
	}
	h := NewJobHandler(backend)

	req := httptest.NewRequest(http.MethodDelete, "/ojs/v1/jobs/abc", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", "abc")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()

	h.Cancel(w, req)

	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want %d", w.Code, http.StatusConflict)
	}
}

// --- Worker Handler Tests ---

func TestWorkerFetch_MissingQueues(t *testing.T) {
	backend := &mockBackend{}
	h := NewWorkerHandler(backend)

	body := `{}`
	req := httptest.NewRequest(http.MethodPost, "/ojs/v1/workers/fetch", bytes.NewBufferString(body))
	w := httptest.NewRecorder()

	h.Fetch(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestWorkerFetch_Success(t *testing.T) {
	backend := &mockBackend{
		fetchFunc: func(ctx context.Context, queues []string, count int, workerID string, vis int) ([]*core.Job, error) {
			return []*core.Job{{ID: "j1", Type: "test", State: "active", Queue: "default"}}, nil
		},
	}
	h := NewWorkerHandler(backend)

	body := `{"queues":["default"],"worker_id":"w1"}`
	req := httptest.NewRequest(http.MethodPost, "/ojs/v1/workers/fetch", bytes.NewBufferString(body))
	w := httptest.NewRecorder()

	h.Fetch(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp map[string]json.RawMessage
	json.Unmarshal(w.Body.Bytes(), &resp)
	if _, ok := resp["jobs"]; !ok {
		t.Error("response missing 'jobs' field")
	}
	if _, ok := resp["job"]; !ok {
		t.Error("response missing 'job' field when jobs returned")
	}
}

func TestWorkerFetch_EmptyResult(t *testing.T) {
	backend := &mockBackend{}
	h := NewWorkerHandler(backend)

	body := `{"queues":["default"]}`
	req := httptest.NewRequest(http.MethodPost, "/ojs/v1/workers/fetch", bytes.NewBufferString(body))
	w := httptest.NewRecorder()

	h.Fetch(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp map[string]json.RawMessage
	json.Unmarshal(w.Body.Bytes(), &resp)
	if _, ok := resp["job"]; ok {
		t.Error("response should not have 'job' field when empty")
	}
}

func TestWorkerAck_MissingJobID(t *testing.T) {
	backend := &mockBackend{}
	h := NewWorkerHandler(backend)

	body := `{}`
	req := httptest.NewRequest(http.MethodPost, "/ojs/v1/workers/ack", bytes.NewBufferString(body))
	w := httptest.NewRecorder()

	h.Ack(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestWorkerAck_NotFound(t *testing.T) {
	backend := &mockBackend{
		ackFunc: func(ctx context.Context, jobID string, result []byte) (*core.AckResponse, error) {
			return nil, core.NewNotFoundError("Job", jobID)
		},
	}
	h := NewWorkerHandler(backend)

	body := `{"job_id":"nonexistent"}`
	req := httptest.NewRequest(http.MethodPost, "/ojs/v1/workers/ack", bytes.NewBufferString(body))
	w := httptest.NewRecorder()

	h.Ack(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestWorkerAck_Conflict(t *testing.T) {
	backend := &mockBackend{
		ackFunc: func(ctx context.Context, jobID string, result []byte) (*core.AckResponse, error) {
			return nil, core.NewConflictError("not active", nil)
		},
	}
	h := NewWorkerHandler(backend)

	body := `{"job_id":"abc"}`
	req := httptest.NewRequest(http.MethodPost, "/ojs/v1/workers/ack", bytes.NewBufferString(body))
	w := httptest.NewRecorder()

	h.Ack(w, req)

	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want %d", w.Code, http.StatusConflict)
	}
}

func TestWorkerNack_MissingJobID(t *testing.T) {
	backend := &mockBackend{}
	h := NewWorkerHandler(backend)

	body := `{"error":{"message":"failed"}}`
	req := httptest.NewRequest(http.MethodPost, "/ojs/v1/workers/nack", bytes.NewBufferString(body))
	w := httptest.NewRecorder()

	h.Nack(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestWorkerHeartbeat_MissingWorkerID(t *testing.T) {
	backend := &mockBackend{}
	h := NewWorkerHandler(backend)

	body := `{}`
	req := httptest.NewRequest(http.MethodPost, "/ojs/v1/workers/heartbeat", bytes.NewBufferString(body))
	w := httptest.NewRecorder()

	h.Heartbeat(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestWorkerHeartbeat_Success(t *testing.T) {
	backend := &mockBackend{}
	h := NewWorkerHandler(backend)

	body := `{"worker_id":"w1","active_jobs":["j1"]}`
	req := httptest.NewRequest(http.MethodPost, "/ojs/v1/workers/heartbeat", bytes.NewBufferString(body))
	w := httptest.NewRecorder()

	h.Heartbeat(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
}

// --- System Handler Tests ---

func TestSystemManifest(t *testing.T) {
	backend := &mockBackend{}
	h := NewSystemHandler(backend)

	req := httptest.NewRequest(http.MethodGet, "/ojs/manifest", nil)
	w := httptest.NewRecorder()

	h.Manifest(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["specversion"] != core.OJSVersion {
		t.Errorf("specversion = %v, want %v", resp["specversion"], core.OJSVersion)
	}
}

func TestSystemHealth_OK(t *testing.T) {
	backend := &mockBackend{}
	h := NewSystemHandler(backend)

	req := httptest.NewRequest(http.MethodGet, "/ojs/v1/health", nil)
	w := httptest.NewRecorder()

	h.Health(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestSystemHealth_Degraded(t *testing.T) {
	backend := &mockBackend{
		healthFunc: func(ctx context.Context) (*core.HealthResponse, error) {
			return &core.HealthResponse{
				Status:  "degraded",
				Version: core.OJSVersion,
			}, nil
		},
	}
	h := NewSystemHandler(backend)

	req := httptest.NewRequest(http.MethodGet, "/ojs/v1/health", nil)
	w := httptest.NewRecorder()

	h.Health(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
}
