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
	wg       sync.WaitGroup
	stopOnce sync.Once
	logger   *slog.Logger
	ctx      context.Context
	cancel   context.CancelFunc
}

// New creates a new Scheduler.
func New(backend *sqsbackend.SQSBackend, logger *slog.Logger) *Scheduler {
	ctx, cancel := context.WithCancel(context.Background())
	return &Scheduler{
		backend: backend,
		stop:    make(chan struct{}),
		logger:  logger,
		ctx:     ctx,
		cancel:  cancel,
	}
}

// Start begins all background scheduling goroutines.
func (s *Scheduler) Start() {
	// Promote scheduled jobs that are due (stored in state store for delays > 15min)
	s.launch("scheduled-promoter", 1*time.Second, s.backend.PromoteScheduled)

	// Promote retryable jobs that are due for retry
	s.launch("retry-promoter", 200*time.Millisecond, s.backend.PromoteRetries)

	// Publish all durable job/workflow/cron effects and delivery intents.
	s.launch("workflow-advance-drainer", 100*time.Millisecond, s.backend.DrainWorkflowAdvancements)
	s.launch("job-effect-drainer", 200*time.Millisecond, s.backend.DrainJobEffects)
	s.launch("delivery-outbox-drainer", 100*time.Millisecond, s.backend.DrainDeliveryOutbox)

	// Apply OJS retry/dead-letter policy to expired worker delivery leases.
	s.launch("delivery-reaper", 500*time.Millisecond, s.backend.ReapExpiredClaims)

	// Fire cron jobs on schedule
	s.launch("cron-scheduler", 10*time.Second, s.backend.FireCronJobs)
}

// launch starts a single background loop goroutine and tracks it in the
// WaitGroup. Keeping the Add/Done lifecycle here (rather than inside runLoop)
// lets runLoop be called directly, e.g. from tests, without corrupting the
// counter.
func (s *Scheduler) launch(name string, interval time.Duration, fn func(context.Context) error) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.runLoop(name, interval, fn)
	}()
}

// Stop signals all background goroutines to stop and waits for them to finish.
func (s *Scheduler) Stop() {
	s.stopOnce.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
		close(s.stop)
	})
	s.wg.Wait()
}

func (s *Scheduler) runLoop(name string, interval time.Duration, fn func(context.Context) error) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-s.stop:
			return
		case <-s.context().Done():
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(s.context(), 10*time.Second)
			if err := fn(ctx); err != nil {
				s.logger.Error("scheduler loop error", "loop", name, "error", err)
			}
			cancel()
		}
	}
}

func (s *Scheduler) context() context.Context {
	if s.ctx != nil {
		return s.ctx
	}
	return context.Background()
}
