package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/openjobspec/ojs-backend-sqs/internal/core"
)

func TestWriteJSON(t *testing.T) {
	type payload struct {
		Name  string `json:"name"`
		Count int    `json:"count"`
	}

	tests := []struct {
		name       string
		status     int
		data       any
		wantStatus int
	}{
		{
			name:       "200 OK with struct body",
			status:     http.StatusOK,
			data:       payload{Name: "test", Count: 42},
			wantStatus: http.StatusOK,
		},
		{
			name:       "201 Created with map body",
			status:     http.StatusCreated,
			data:       map[string]string{"id": "abc-123"},
			wantStatus: http.StatusCreated,
		},
		{
			name:       "200 OK with slice body",
			status:     http.StatusOK,
			data:       []string{"a", "b", "c"},
			wantStatus: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			WriteJSON(w, tt.status, tt.data)

			resp := w.Result()
			defer resp.Body.Close()

			if resp.StatusCode != tt.wantStatus {
				t.Errorf("status code = %d, want %d", resp.StatusCode, tt.wantStatus)
			}
			ct := resp.Header.Get("Content-Type")
			if ct != core.OJSMediaType {
				t.Errorf("Content-Type = %q, want %q", ct, core.OJSMediaType)
			}
			var decoded any
			if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
				t.Errorf("failed to decode response body as JSON: %v", err)
			}
		})
	}
}

func TestWriteError(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		ojsErr     *core.OJSError
		wantStatus int
		wantCode   string
	}{
		{
			name:   "400 invalid_request",
			status: http.StatusBadRequest,
			ojsErr: &core.OJSError{
				Code:    core.ErrCodeInvalidRequest,
				Message: "missing required field",
			},
			wantStatus: http.StatusBadRequest,
			wantCode:   core.ErrCodeInvalidRequest,
		},
		{
			name:   "404 not_found",
			status: http.StatusNotFound,
			ojsErr: &core.OJSError{
				Code:    core.ErrCodeNotFound,
				Message: "job not found",
			},
			wantStatus: http.StatusNotFound,
			wantCode:   core.ErrCodeNotFound,
		},
		{
			name:   "500 internal_error with retryable",
			status: http.StatusInternalServerError,
			ojsErr: &core.OJSError{
				Code:      core.ErrCodeInternalError,
				Message:   "connection lost",
				Retryable: true,
			},
			wantStatus: http.StatusInternalServerError,
			wantCode:   core.ErrCodeInternalError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			WriteError(w, tt.status, tt.ojsErr)

			resp := w.Result()
			defer resp.Body.Close()

			if resp.StatusCode != tt.wantStatus {
				t.Errorf("status code = %d, want %d", resp.StatusCode, tt.wantStatus)
			}
			ct := resp.Header.Get("Content-Type")
			if ct != core.OJSMediaType {
				t.Errorf("Content-Type = %q, want %q", ct, core.OJSMediaType)
			}
			var errResp ErrorResponse
			if err := json.NewDecoder(resp.Body).Decode(&errResp); err != nil {
				t.Fatalf("failed to decode error response: %v", err)
			}
			if errResp.Error == nil {
				t.Fatal("expected error field in response, got nil")
			}
			if errResp.Error.Code != tt.wantCode {
				t.Errorf("error.code = %q, want %q", errResp.Error.Code, tt.wantCode)
			}
		})
	}
}

func TestWriteError_IncludesRequestID(t *testing.T) {
	w := httptest.NewRecorder()
	w.Header().Set("X-Request-Id", "req-abc-123")

	ojsErr := &core.OJSError{
		Code:    core.ErrCodeInternalError,
		Message: "something went wrong",
	}
	WriteError(w, http.StatusInternalServerError, ojsErr)

	resp := w.Result()
	defer resp.Body.Close()

	var errResp ErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&errResp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}
	if errResp.Error.RequestID != "req-abc-123" {
		t.Errorf("error.request_id = %q, want %q", errResp.Error.RequestID, "req-abc-123")
	}
}
