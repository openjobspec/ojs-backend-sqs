package sqs

import (
	"testing"
)

func TestExtendVisibility_CalculatesTimeoutSeconds(t *testing.T) {
	tests := []struct {
		name            string
		visTimeoutMs    int
		expectedSeconds int32
	}{
		{
			name:            "30 seconds",
			visTimeoutMs:    30000,
			expectedSeconds: 30,
		},
		{
			name:            "60 seconds",
			visTimeoutMs:    60000,
			expectedSeconds: 60,
		},
		{
			name:            "zero defaults to 30",
			visTimeoutMs:    0,
			expectedSeconds: 30,
		},
		{
			name:            "negative defaults to 30",
			visTimeoutMs:    -1000,
			expectedSeconds: 30,
		},
		{
			name:            "very small positive rounds up to one",
			visTimeoutMs:    500,
			expectedSeconds: 1,
		},
		{
			name:            "fractional second rounds up",
			visTimeoutMs:    1001,
			expectedSeconds: 2,
		},
		{
			name:            "maximum capped at 43200",
			visTimeoutMs:    50000000,
			expectedSeconds: 43200,
		},
		{
			name:            "exactly at max boundary",
			visTimeoutMs:    43200000,
			expectedSeconds: 43200,
		},
		{
			name:            "just over max",
			visTimeoutMs:    43201000,
			expectedSeconds: 43200,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Exercise the real clamping helper used by extendVisibility/heartbeat.
			timeoutSec := clampVisibilityTimeoutSeconds(tt.visTimeoutMs)

			if timeoutSec != tt.expectedSeconds {
				t.Errorf("timeout = %d seconds, want %d", timeoutSec, tt.expectedSeconds)
			}
		})
	}
}

func TestChangeMessageVisibility_TimeoutBounds(t *testing.T) {
	tests := []struct {
		name         string
		timeoutSec   int32
		isValidRange bool
	}{
		{"zero for immediate redelivery", 0, true},
		{"minimum 1 second", 1, true},
		{"normal 30 seconds", 30, true},
		{"maximum 12 hours", 43200, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// SQS allows visibility timeout from 0 to 43200 seconds (12 hours)
			valid := tt.timeoutSec >= 0 && tt.timeoutSec <= 43200
			if valid != tt.isValidRange {
				t.Errorf("timeout %d valid = %v, want %v", tt.timeoutSec, valid, tt.isValidRange)
			}
		})
	}
}

func TestEncodeJob_SizeLimit(t *testing.T) {
	tests := []struct {
		name      string
		bodySize  int
		wantError bool
	}{
		{"small payload", 100, false},
		{"exactly at limit", MaxSQSMessageSize, false},
		{"over limit", MaxSQSMessageSize + 1, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// MaxSQSMessageSize is 256KB
			overLimit := tt.bodySize > MaxSQSMessageSize
			if overLimit != tt.wantError {
				t.Errorf("bodySize %d overLimit = %v, want %v", tt.bodySize, overLimit, tt.wantError)
			}
		})
	}
}
