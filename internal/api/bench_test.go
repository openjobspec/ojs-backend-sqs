package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/openjobspec/ojs-backend-sqs/internal/core"
)

func newTestRouter(backend core.Backend) *chi.Mux {
	r := chi.NewRouter()

	jobH := NewJobHandler(backend)
	workerH := NewWorkerHandler(backend)
	systemH := NewSystemHandler(backend)
	queueH := NewQueueHandler(backend)
	deadLetterH := NewDeadLetterHandler(backend)
	cronH := NewCronHandler(backend)
	workflowH := NewWorkflowHandler(backend)
	batchH := NewBatchHandler(backend)

	r.Get("/ojs/v1/health", systemH.Health)
	r.Post("/ojs/v1/jobs", jobH.Create)
	r.Get("/ojs/v1/jobs/{id}", jobH.Get)
	r.Delete("/ojs/v1/jobs/{id}", jobH.Cancel)
	r.Post("/ojs/v1/jobs/batch", batchH.Create)
	r.Post("/ojs/v1/workers/fetch", workerH.Fetch)
	r.Post("/ojs/v1/workers/ack", workerH.Ack)
	r.Post("/ojs/v1/workers/nack", workerH.Nack)
	r.Get("/ojs/v1/queues", queueH.List)
	r.Get("/ojs/v1/queues/{name}/stats", queueH.Stats)
	r.Get("/ojs/v1/dead-letter", deadLetterH.List)
	r.Post("/ojs/v1/cron", cronH.Register)
	r.Get("/ojs/v1/cron", cronH.List)
	r.Post("/ojs/v1/workflows", workflowH.Create)
	_ = deadLetterH
	_ = cronH

	return r
}

func BenchmarkJobCreate(b *testing.B) {
	router := newTestRouter(&mockBackend{})
	body := `{"type":"email.send","args":["user@example.com"],"options":{"queue":"default"}}`

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest(http.MethodPost, "/ojs/v1/jobs", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
	}
}

func BenchmarkJobGet(b *testing.B) {
	router := newTestRouter(&mockBackend{})

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest(http.MethodGet, "/ojs/v1/jobs/test-id", nil)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
	}
}

func BenchmarkWorkerFetch(b *testing.B) {
	backend := &mockBackend{
		fetchFunc: func(ctx context.Context, queues []string, count int, workerID string, visMs int) ([]*core.Job, error) {
			return []*core.Job{{ID: "job-1", Type: "test", State: "active", Queue: "default"}}, nil
		},
	}
	router := newTestRouter(backend)
	body := `{"queues":["default"],"worker_id":"w-1"}`

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest(http.MethodPost, "/ojs/v1/workers/fetch", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
	}
}

func BenchmarkWorkerAck(b *testing.B) {
	router := newTestRouter(&mockBackend{})
	body := `{"job_id":"job-1","result":{"ok":true}}`

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest(http.MethodPost, "/ojs/v1/workers/ack", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
	}
}

func BenchmarkBatchCreate(b *testing.B) {
	router := newTestRouter(&mockBackend{})
	body := `{"jobs":[{"type":"email.send","args":["a@b.com"]},{"type":"email.send","args":["c@d.com"]},{"type":"email.send","args":["e@f.com"]}]}`

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest(http.MethodPost, "/ojs/v1/jobs/batch", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
	}
}

func BenchmarkWorkerNack(b *testing.B) {
	router := newTestRouter(&mockBackend{})
	body := `{"job_id":"job-1","error":{"message":"timeout"}}`

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest(http.MethodPost, "/ojs/v1/workers/nack", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
	}
}

func BenchmarkQueueStats(b *testing.B) {
	router := newTestRouter(&mockBackend{})

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest(http.MethodGet, "/ojs/v1/queues/default/stats", nil)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
	}
}

func BenchmarkHealthCheck(b *testing.B) {
	router := newTestRouter(&mockBackend{})

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest(http.MethodGet, "/ojs/v1/health", nil)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
	}
}
