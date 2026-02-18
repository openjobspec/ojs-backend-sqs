package api

import (
	"log/slog"
	"net/http"
	"time"

	commonmw "github.com/openjobspec/ojs-go-backend-common/middleware"
)

// OJSHeaders middleware adds required OJS response headers.
func OJSHeaders(next http.Handler) http.Handler {
	return commonmw.OJSHeaders(next)
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

// ValidateContentType middleware validates the Content-Type header for mutation requests.
func ValidateContentType(next http.Handler) http.Handler {
	return commonmw.ValidateContentType(next)
}
