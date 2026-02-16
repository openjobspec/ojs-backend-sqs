package scheduler

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

func TestNew_CreatesScheduler(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := New(nil, logger)

	if s == nil {
		t.Fatal("expected non-nil scheduler")
	}
	if s.logger != logger {
		t.Error("expected logger to be set")
	}
	if s.stop == nil {
		t.Error("expected stop channel to be initialized")
	}
}

func TestStop_ClosesChannel(t *testing.T) {
	s := &Scheduler{
		stop: make(chan struct{}),
	}

	s.Stop()

	select {
	case <-s.stop:
		// OK — channel is closed
	default:
		t.Fatal("expected stop channel to be closed")
	}
}

func TestStop_IdempotentMultipleCalls(t *testing.T) {
	s := &Scheduler{
		stop: make(chan struct{}),
	}

	// Should not panic on multiple calls
	for i := 0; i < 10; i++ {
		s.Stop()
	}

	select {
	case <-s.stop:
	default:
		t.Fatal("expected stop channel to be closed")
	}
}

func TestStop_ConcurrentCalls(t *testing.T) {
	s := &Scheduler{
		stop: make(chan struct{}),
	}

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Stop()
		}()
	}

	wg.Wait()

	select {
	case <-s.stop:
	default:
		t.Fatal("expected stop channel to be closed")
	}
}

func TestRunLoop_StopsOnClose(t *testing.T) {
	callCount := 0
	s := &Scheduler{
		stop:   make(chan struct{}),
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	done := make(chan struct{})
	go func() {
		s.runLoop("test-loop", 10*time.Millisecond, func(ctx context.Context) error {
			callCount++
			return nil
		})
		close(done)
	}()

	// Let it tick a few times
	time.Sleep(50 * time.Millisecond)
	s.Stop()

	select {
	case <-done:
		// loop exited
	case <-time.After(2 * time.Second):
		t.Fatal("runLoop did not exit after Stop()")
	}

	if callCount == 0 {
		t.Error("expected at least one loop iteration")
	}
}

func TestRunLoop_HandlesErrors(t *testing.T) {
	s := &Scheduler{
		stop:   make(chan struct{}),
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	errorCount := 0
	done := make(chan struct{})
	go func() {
		s.runLoop("error-loop", 10*time.Millisecond, func(ctx context.Context) error {
			errorCount++
			return fmt.Errorf("test error %d", errorCount)
		})
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)
	s.Stop()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runLoop did not exit after Stop()")
	}

	if errorCount == 0 {
		t.Error("expected loop to handle errors without crashing")
	}
}
