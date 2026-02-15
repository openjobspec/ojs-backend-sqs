package core

import (
	"testing"
	"time"
)

func TestCalculateBackoff_NilPolicy(t *testing.T) {
	d := CalculateBackoff(nil, 1)
	// Default policy: exponential with 1s initial, coefficient 2.0, jitter enabled
	// attempt 1: 1s * 2^0 = 1s, jitter range 0.5x-1.5x = 500ms-1500ms
	if d < 500*time.Millisecond || d > 1500*time.Millisecond {
		t.Errorf("CalculateBackoff(nil, 1) = %v, want between 500ms and 1500ms", d)
	}
}

func TestCalculateBackoff_Exponential(t *testing.T) {
	policy := &RetryPolicy{
		InitialInterval:  "PT1S",
		BackoffCoefficient: 2.0,
		BackoffType:      "exponential",
	}

	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{1, 1 * time.Second},  // 1 * 2^0
		{2, 2 * time.Second},  // 1 * 2^1
		{3, 4 * time.Second},  // 1 * 2^2
		{4, 8 * time.Second},  // 1 * 2^3
	}

	for _, tt := range tests {
		got := CalculateBackoff(policy, tt.attempt)
		if got != tt.want {
			t.Errorf("CalculateBackoff(exponential, %d) = %v, want %v", tt.attempt, got, tt.want)
		}
	}
}

func TestCalculateBackoff_Linear(t *testing.T) {
	policy := &RetryPolicy{
		InitialInterval: "PT2S",
		BackoffType:     "linear",
	}

	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{1, 2 * time.Second},
		{2, 4 * time.Second},
		{3, 6 * time.Second},
	}

	for _, tt := range tests {
		got := CalculateBackoff(policy, tt.attempt)
		if got != tt.want {
			t.Errorf("CalculateBackoff(linear, %d) = %v, want %v", tt.attempt, got, tt.want)
		}
	}
}

func TestCalculateBackoff_Constant(t *testing.T) {
	policy := &RetryPolicy{
		InitialInterval: "PT5S",
		BackoffType:     "constant",
	}

	for _, attempt := range []int{1, 2, 3, 10} {
		got := CalculateBackoff(policy, attempt)
		if got != 5*time.Second {
			t.Errorf("CalculateBackoff(constant, %d) = %v, want 5s", attempt, got)
		}
	}
}

func TestCalculateBackoff_MaxInterval(t *testing.T) {
	policy := &RetryPolicy{
		InitialInterval:  "PT1S",
		BackoffCoefficient: 2.0,
		BackoffType:      "exponential",
		MaxInterval:      "PT10S",
	}

	// attempt 5: 1 * 2^4 = 16s, capped at 10s
	got := CalculateBackoff(policy, 5)
	if got != 10*time.Second {
		t.Errorf("CalculateBackoff with max_interval, attempt 5 = %v, want 10s", got)
	}
}

func TestCalculateBackoff_Jitter(t *testing.T) {
	policy := &RetryPolicy{
		InitialInterval:  "PT10S",
		BackoffType:      "constant",
		Jitter:           true,
	}

	// Run multiple times - jitter should produce values between 5s and 15s (0.5x to 1.5x)
	for i := 0; i < 100; i++ {
		got := CalculateBackoff(policy, 1)
		if got < 5*time.Second || got > 15*time.Second {
			t.Errorf("CalculateBackoff with jitter = %v, expected between 5s and 15s", got)
		}
	}
}

func TestCalculateBackoff_BackoffStrategy(t *testing.T) {
	// BackoffStrategy is an alias for BackoffType
	policy := &RetryPolicy{
		InitialInterval: "PT3S",
		BackoffStrategy: "linear",
	}

	got := CalculateBackoff(policy, 2)
	if got != 6*time.Second {
		t.Errorf("CalculateBackoff(BackoffStrategy=linear, 2) = %v, want 6s", got)
	}
}
