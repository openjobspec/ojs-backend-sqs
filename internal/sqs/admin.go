package sqs

import (
	"context"

	"github.com/openjobspec/ojs-backend-sqs/internal/core"
)

// ListJobs returns a paginated, filtered list of jobs.
func (b *SQSBackend) ListJobs(ctx context.Context, filters core.JobListFilters, limit, offset int) ([]*core.Job, int, error) {
	return b.store.ListAllJobs(ctx, filters, limit, offset)
}

// ListWorkers returns a paginated list of workers and summary counts.
func (b *SQSBackend) ListWorkers(ctx context.Context, limit, offset int) ([]*core.WorkerInfo, core.WorkerSummary, error) {
	return b.store.ListAllWorkers(ctx, limit, offset)
}
