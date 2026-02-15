package core

import "testing"

func TestIsValidTransition(t *testing.T) {
	tests := []struct {
		from, to string
		want     bool
	}{
		// Valid transitions
		{StateAvailable, StateActive, true},
		{StateAvailable, StateCancelled, true},
		{StateScheduled, StateAvailable, true},
		{StateScheduled, StateCancelled, true},
		{StatePending, StateAvailable, true},
		{StatePending, StateCancelled, true},
		{StateActive, StateCompleted, true},
		{StateActive, StateRetryable, true},
		{StateActive, StateDiscarded, true},
		{StateActive, StateCancelled, true},
		{StateRetryable, StateAvailable, true},
		{StateRetryable, StateCancelled, true},

		// Invalid transitions
		{StateAvailable, StateCompleted, false},
		{StateAvailable, StateScheduled, false},
		{StateCompleted, StateActive, false},
		{StateCompleted, StateCancelled, false},
		{StateCancelled, StateAvailable, false},
		{StateDiscarded, StateAvailable, false},
		{StateActive, StateAvailable, false},
		{StateActive, StateScheduled, false},

		// Unknown state
		{"unknown", StateActive, false},
	}

	for _, tt := range tests {
		t.Run(tt.from+"->"+tt.to, func(t *testing.T) {
			got := IsValidTransition(tt.from, tt.to)
			if got != tt.want {
				t.Errorf("IsValidTransition(%q, %q) = %v, want %v", tt.from, tt.to, got, tt.want)
			}
		})
	}
}

func TestIsTerminalState(t *testing.T) {
	terminal := []string{StateCompleted, StateCancelled, StateDiscarded}
	nonTerminal := []string{StateAvailable, StateScheduled, StatePending, StateActive, StateRetryable}

	for _, s := range terminal {
		if !IsTerminalState(s) {
			t.Errorf("IsTerminalState(%q) = false, want true", s)
		}
	}
	for _, s := range nonTerminal {
		if IsTerminalState(s) {
			t.Errorf("IsTerminalState(%q) = true, want false", s)
		}
	}
}

func TestIsCancellableState(t *testing.T) {
	cancellable := []string{StateAvailable, StateScheduled, StatePending, StateActive, StateRetryable}
	notCancellable := []string{StateCompleted, StateCancelled, StateDiscarded}

	for _, s := range cancellable {
		if !IsCancellableState(s) {
			t.Errorf("IsCancellableState(%q) = false, want true", s)
		}
	}
	for _, s := range notCancellable {
		if IsCancellableState(s) {
			t.Errorf("IsCancellableState(%q) = true, want false", s)
		}
	}
}
