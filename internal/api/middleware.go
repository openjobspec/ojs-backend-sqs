package api

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/openjobspec/ojs-backend-sqs/internal/core"
)

// OJSHeaders middleware adds required OJS response headers.
func OJSHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("OJS-Version", core.OJSVersion)
		w.Header().Set("Content-Type", core.OJSMediaType)

		// Generate or echo X-Request-Id
		reqID := r.Header.Get("X-Request-Id")
		if reqID == "" {
			reqID = "req_" + core.NewUUIDv7()
		}
		w.Header().Set("X-Request-Id", reqID)

		next.ServeHTTP(w, r)
	})
}

// statusCapture wraps http.ResponseWriter to capture the status code.
type statusCapture struct {
	http.ResponseWriter
	code int
}

func (s *statusCapture) WriteHeader(code int) {
	s.code = code
	s.ResponseWriter.WriteHeader(code)
}

// RequestLogger logs each HTTP request with method, path, status, and duration.
func RequestLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sc := &statusCapture{ResponseWriter: w, code: http.StatusOK}
			next.ServeHTTP(sc, r)
			logger.Info("http request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", sc.code,
				"duration_ms", time.Since(start).Milliseconds(),
				"request_id", w.Header().Get("X-Request-Id"),
			)
		})
	}
}

// ValidateContentType middleware validates the Content-Type header for POST requests.
func ValidateContentType(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodPatch {
			ct := r.Header.Get("Content-Type")
			if ct != "" {
				// Extract media type (ignore parameters like charset)
				mediaType := strings.Split(ct, ";")[0]
				mediaType = strings.TrimSpace(mediaType)
				if mediaType != core.OJSMediaType && mediaType != "application/json" {
					WriteError(w, http.StatusBadRequest, core.NewInvalidRequestError(
						"Unsupported Content-Type. Expected 'application/openjobspec+json' or 'application/json'.",
						map[string]any{
							"received": ct,
						},
					))
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}
