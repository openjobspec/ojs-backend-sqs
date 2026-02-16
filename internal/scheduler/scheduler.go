package scheduler

import (
	"context"
	"log/slog"
	"sync"
	"time"

	sqsbackend "github.com/openjobspec/ojs-backend-sqs/internal/sqs"
)

// Scheduler runs background tasks for the OJS SQS server.
type Scheduler struct {
	backend  *sqsbackend.SQSBackend
	stop     chan struct{}
	stopOnce sync.Once
	logger   *slog.Logger
}

// New creates a new Scheduler.
func New(backend *sqsbackend.SQSBackend, logger *slog.Logger) *Scheduler {
	return &Scheduler{
		backend: backend,
		stop:    make(chan struct{}),
		logger:  logger,
	}
}

// Start begins all background scheduling goroutines.
func (s *Scheduler) Start() {
	// Promote scheduled jobs that are due (stored in state store for delays > 15min)
	go s.runLoop("scheduled-promoter", 1*time.Second, s.backend.PromoteScheduled)

	// Promote retryable jobs that are due for retry
	go s.runLoop("retry-promoter", 200*time.Millisecond, s.backend.PromoteRetries)

	// Note: SQS handles visibility timeout natively. No stalled reaper needed.
	// When a message's visibility timeout expires, SQS automatically makes it
	// visible again for other consumers.

	// Fire cron jobs on schedule
	go s.runLoop("cron-scheduler", 10*time.Second, s.backend.FireCronJobs)
}

// Stop signals all background goroutines to stop.
func (s *Scheduler) Stop() {
	s.stopOnce.Do(func() {
		close(s.stop)
	})
}

func (s *Scheduler) runLoop(name string, interval time.Duration, fn func(context.Context) error) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			if err := fn(ctx); err != nil {
				s.logger.Error("scheduler loop error", "loop", name, "error", err)
			}
			cancel()
		}
	}
}
