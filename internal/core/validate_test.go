package core

import (
	"encoding/json"
	"testing"
)

func TestValidateEnqueueRequest_Valid(t *testing.T) {
	req := &EnqueueRequest{
		Type: "email.send",
		Args: json.RawMessage(`["hello@example.com", "subject"]`),
	}
	if err := ValidateEnqueueRequest(req); err != nil {
		t.Errorf("expected no error, got %v", err)
	}
}

func TestValidateEnqueueRequest_MissingType(t *testing.T) {
	req := &EnqueueRequest{
		Args: json.RawMessage(`["arg1"]`),
	}
	err := ValidateEnqueueRequest(req)
	if err == nil {
		t.Fatal("expected error for missing type")
	}
	if err.Code != ErrCodeInvalidRequest {
		t.Errorf("expected code %q, got %q", ErrCodeInvalidRequest, err.Code)
	}
}

func TestValidateEnqueueRequest_InvalidTypeFormat(t *testing.T) {
	invalid := []string{
		"UPPERCASE",
		"with spaces",
		"123starts-with-number",
		"",
	}

	for _, typ := range invalid {
		req := &EnqueueRequest{
			Type: typ,
			Args: json.RawMessage(`["arg"]`),
		}
		err := ValidateEnqueueRequest(req)
		if err == nil {
			t.Errorf("expected error for type %q", typ)
		}
	}
}

func TestValidateEnqueueRequest_ValidTypeFormats(t *testing.T) {
	valid := []string{
		"email",
		"email-send",
		"email.send",
		"my-app.send-email",
		"a",
		"a-b.c-d",
	}

	for _, typ := range valid {
		req := &EnqueueRequest{
			Type: typ,
			Args: json.RawMessage(`["arg"]`),
		}
		if err := ValidateEnqueueRequest(req); err != nil {
			t.Errorf("expected no error for type %q, got %v", typ, err)
		}
	}
}

func TestValidateEnqueueRequest_MissingArgs(t *testing.T) {
	req := &EnqueueRequest{
		Type: "test.job",
	}
	err := ValidateEnqueueRequest(req)
	if err == nil {
		t.Fatal("expected error for missing args")
	}
}

func TestValidateEnqueueRequest_NullArgs(t *testing.T) {
	req := &EnqueueRequest{
		Type: "test.job",
		Args: json.RawMessage(`null`),
	}
	err := ValidateEnqueueRequest(req)
	if err == nil {
		t.Fatal("expected error for null args")
	}
}

func TestValidateEnqueueRequest_NonArrayArgs(t *testing.T) {
	req := &EnqueueRequest{
		Type: "test.job",
		Args: json.RawMessage(`{"key": "value"}`),
	}
	err := ValidateEnqueueRequest(req)
	if err == nil {
		t.Fatal("expected error for non-array args")
	}
}

func TestValidateEnqueueRequest_InvalidID(t *testing.T) {
	req := &EnqueueRequest{
		Type:  "test.job",
		Args:  json.RawMessage(`["arg"]`),
		ID:    "not-a-uuid",
		HasID: true,
	}
	err := ValidateEnqueueRequest(req)
	if err == nil {
		t.Fatal("expected error for invalid ID")
	}
}

func TestValidateEnqueueRequest_ValidID(t *testing.T) {
	req := &EnqueueRequest{
		Type:  "test.job",
		Args:  json.RawMessage(`["arg"]`),
		ID:    NewUUIDv7(),
		HasID: true,
	}
	if err := ValidateEnqueueRequest(req); err != nil {
		t.Errorf("expected no error for valid ID, got %v", err)
	}
}

func TestValidateOptions_InvalidQueue(t *testing.T) {
	req := &EnqueueRequest{
		Type: "test.job",
		Args: json.RawMessage(`["arg"]`),
		Options: &EnqueueOptions{
			Queue: "INVALID QUEUE",
		},
	}
	err := ValidateEnqueueRequest(req)
	if err == nil {
		t.Fatal("expected error for invalid queue name")
	}
}

func TestValidateOptions_ValidQueue(t *testing.T) {
	valid := []string{"default", "emails", "high-priority", "my.queue", "queue-1"}
	for _, q := range valid {
		req := &EnqueueRequest{
			Type: "test.job",
			Args: json.RawMessage(`["arg"]`),
			Options: &EnqueueOptions{
				Queue: q,
			},
		}
		if err := ValidateEnqueueRequest(req); err != nil {
			t.Errorf("expected no error for queue %q, got %v", q, err)
		}
	}
}

func TestValidateOptions_PriorityRange(t *testing.T) {
	tests := []struct {
		priority int
		wantErr  bool
	}{
		{0, false},
		{-100, false},
		{100, false},
		{50, false},
		{-101, true},
		{101, true},
		{999, true},
	}

	for _, tt := range tests {
		p := tt.priority
		req := &EnqueueRequest{
			Type: "test.job",
			Args: json.RawMessage(`["arg"]`),
			Options: &EnqueueOptions{
				Priority: &p,
			},
		}
		err := ValidateEnqueueRequest(req)
		if (err != nil) != tt.wantErr {
			t.Errorf("priority %d: error = %v, wantErr %v", tt.priority, err, tt.wantErr)
		}
	}
}

func TestValidateRetryPolicy_InvalidMaxAttempts(t *testing.T) {
	req := &EnqueueRequest{
		Type: "test.job",
		Args: json.RawMessage(`["arg"]`),
		Options: &EnqueueOptions{
			Retry: &RetryPolicy{
				MaxAttempts: -1,
			},
		},
	}
	err := ValidateEnqueueRequest(req)
	if err == nil {
		t.Fatal("expected error for negative max_attempts")
	}
}

func TestValidateRetryPolicy_InvalidBackoffCoefficient(t *testing.T) {
	req := &EnqueueRequest{
		Type: "test.job",
		Args: json.RawMessage(`["arg"]`),
		Options: &EnqueueOptions{
			Retry: &RetryPolicy{
				MaxAttempts:        3,
				BackoffCoefficient: 0.5,
			},
		},
	}
	err := ValidateEnqueueRequest(req)
	if err == nil {
		t.Fatal("expected error for backoff_coefficient < 1.0")
	}
}

func TestValidateRetryPolicy_InvalidInitialInterval(t *testing.T) {
	req := &EnqueueRequest{
		Type: "test.job",
		Args: json.RawMessage(`["arg"]`),
		Options: &EnqueueOptions{
			Retry: &RetryPolicy{
				MaxAttempts:     3,
				InitialInterval: "invalid",
			},
		},
	}
	err := ValidateEnqueueRequest(req)
	if err == nil {
		t.Fatal("expected error for invalid initial_interval")
	}
}

func TestValidateRetryPolicy_InvalidMaxInterval(t *testing.T) {
	req := &EnqueueRequest{
		Type: "test.job",
		Args: json.RawMessage(`["arg"]`),
		Options: &EnqueueOptions{
			Retry: &RetryPolicy{
				MaxAttempts: 3,
				MaxInterval: "bad",
			},
		},
	}
	err := ValidateEnqueueRequest(req)
	if err == nil {
		t.Fatal("expected error for invalid max_interval")
	}
}
