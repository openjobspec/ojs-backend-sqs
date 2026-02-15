package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openjobspec/ojs-backend-sqs/internal/core"
)

func TestOJSHeaders_SetsVersionAndContentType(t *testing.T) {
	handler := OJSHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/ojs/v1/health", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if got := w.Header().Get("OJS-Version"); got != core.OJSVersion {
		t.Errorf("OJS-Version = %q, want %q", got, core.OJSVersion)
	}
	if got := w.Header().Get("Content-Type"); got != core.OJSMediaType {
		t.Errorf("Content-Type = %q, want %q", got, core.OJSMediaType)
	}
}

func TestOJSHeaders_GeneratesRequestID(t *testing.T) {
	handler := OJSHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	reqID := w.Header().Get("X-Request-Id")
	if reqID == "" {
		t.Fatal("expected X-Request-Id header to be set")
	}
	if !strings.HasPrefix(reqID, "req_") {
		t.Errorf("X-Request-Id = %q, expected prefix 'req_'", reqID)
	}
}

func TestOJSHeaders_EchoesRequestID(t *testing.T) {
	handler := OJSHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("X-Request-Id", "custom-req-42")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if got := w.Header().Get("X-Request-Id"); got != "custom-req-42" {
		t.Errorf("X-Request-Id = %q, want %q", got, "custom-req-42")
	}
}

func TestValidateContentType_AcceptsOJSMediaType(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		method      string
		wantStatus  int
	}{
		{
			name:        "accepts OJS media type",
			contentType: core.OJSMediaType,
			method:      http.MethodPost,
			wantStatus:  http.StatusOK,
		},
		{
			name:        "accepts application/json",
			contentType: "application/json",
			method:      http.MethodPost,
			wantStatus:  http.StatusOK,
		},
		{
			name:        "accepts application/json with charset",
			contentType: "application/json; charset=utf-8",
			method:      http.MethodPost,
			wantStatus:  http.StatusOK,
		},
		{
			name:        "rejects text/plain on POST",
			contentType: "text/plain",
			method:      http.MethodPost,
			wantStatus:  http.StatusBadRequest,
		},
		{
			name:        "rejects text/xml on PUT",
			contentType: "text/xml",
			method:      http.MethodPut,
			wantStatus:  http.StatusBadRequest,
		},
		{
			name:        "rejects text/html on PATCH",
			contentType: "text/html",
			method:      http.MethodPatch,
			wantStatus:  http.StatusBadRequest,
		},
		{
			name:        "allows GET without content-type",
			contentType: "",
			method:      http.MethodGet,
			wantStatus:  http.StatusOK,
		},
		{
			name:        "allows POST without content-type",
			contentType: "",
			method:      http.MethodPost,
			wantStatus:  http.StatusOK,
		},
		{
			name:        "allows DELETE without content-type",
			contentType: "",
			method:      http.MethodDelete,
			wantStatus:  http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := ValidateContentType(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))

			req := httptest.NewRequest(tt.method, "/test", nil)
			if tt.contentType != "" {
				req.Header.Set("Content-Type", tt.contentType)
			}
			w := httptest.NewRecorder()

			handler.ServeHTTP(w, req)

			if w.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", w.Code, tt.wantStatus)
			}
		})
	}
}

func TestValidateContentType_ErrorFormat(t *testing.T) {
	handler := ValidateContentType(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/test", nil)
	req.Header.Set("Content-Type", "text/plain")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}

	var errResp ErrorResponse
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}
	if errResp.Error == nil {
		t.Fatal("expected error in response")
	}
	if errResp.Error.Code != core.ErrCodeInvalidRequest {
		t.Errorf("error code = %q, want %q", errResp.Error.Code, core.ErrCodeInvalidRequest)
	}
}

func TestStatusCapture_CapturesWriteHeader(t *testing.T) {
	w := httptest.NewRecorder()
	sc := &statusCapture{ResponseWriter: w, code: http.StatusOK}

	sc.WriteHeader(http.StatusNotFound)

	if sc.code != http.StatusNotFound {
		t.Errorf("captured code = %d, want %d", sc.code, http.StatusNotFound)
	}
	if w.Code != http.StatusNotFound {
		t.Errorf("underlying code = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestRequestLogger_LogsRequest(t *testing.T) {
	// Verify the middleware passes through without error
	logger := newTestLogger()
	handler := RequestLogger(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))

	req := httptest.NewRequest(http.MethodPost, "/ojs/v1/jobs", nil)
	w := httptest.NewRecorder()
	// Set request ID so logger has it available
	w.Header().Set("X-Request-Id", "test-req-id")

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Errorf("status = %d, want %d", w.Code, http.StatusCreated)
	}
}

func TestMiddlewareChain_HeadersAndContentType(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := OJSHeaders(ValidateContentType(inner))

	req := httptest.NewRequest(http.MethodPost, "/test", nil)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
	if got := w.Header().Get("OJS-Version"); got != core.OJSVersion {
		t.Errorf("OJS-Version = %q, want %q", got, core.OJSVersion)
	}
	if got := w.Header().Get("X-Request-Id"); got == "" {
		t.Error("expected X-Request-Id to be set")
	}
}

// newTestLogger returns a slog.Logger that discards output (for tests).
func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
