package core

import "testing"

func TestOJSErrorCodes(t *testing.T) {
	tests := []struct {
		name     string
		err      *OJSError
		wantCode string
	}{
		{"InvalidRequest", NewInvalidRequestError("bad input", nil), ErrCodeInvalidRequest},
		{"NotFound", NewNotFoundError("Job", "123"), ErrCodeNotFound},
		{"Conflict", NewConflictError("already done", nil), ErrCodeConflict},
		{"Validation", NewValidationError("invalid field", nil), ErrCodeValidationError},
		{"Internal", NewInternalError("something broke"), ErrCodeInternalError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.err.Code != tt.wantCode {
				t.Errorf("got code %q, want %q", tt.err.Code, tt.wantCode)
			}
		})
	}
}

func TestOJSErrorRetryable(t *testing.T) {
	if NewInvalidRequestError("x", nil).Retryable {
		t.Error("InvalidRequestError should not be retryable")
	}
	if NewNotFoundError("Job", "1").Retryable {
		t.Error("NotFoundError should not be retryable")
	}
	if NewConflictError("x", nil).Retryable {
		t.Error("ConflictError should not be retryable")
	}
	if !NewInternalError("x").Retryable {
		t.Error("InternalError should be retryable")
	}
}

func TestOJSErrorString(t *testing.T) {
	err := NewNotFoundError("Job", "abc-123")
	got := err.Error()
	if got != "[not_found] Job 'abc-123' not found." {
		t.Errorf("Error() = %q, unexpected format", got)
	}
}

func TestNotFoundErrorDetails(t *testing.T) {
	err := NewNotFoundError("Workflow", "wf-1")
	if err.Details["resource_type"] != "Workflow" {
		t.Errorf("resource_type = %v, want Workflow", err.Details["resource_type"])
	}
	if err.Details["resource_id"] != "wf-1" {
		t.Errorf("resource_id = %v, want wf-1", err.Details["resource_id"])
	}
}
