package scheduler

import "testing"

func TestStopIsIdempotent(t *testing.T) {
	s := &Scheduler{
		stop: make(chan struct{}),
	}

	s.Stop()
	s.Stop()

	select {
	case <-s.stop:
	default:
		t.Fatal("expected scheduler stop channel to be closed")
	}
}
