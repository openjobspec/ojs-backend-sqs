package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOTELHTTPMiddleware_PreservesFlusher(t *testing.T) {
	handler := otelHTTPMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, ok := w.(http.Flusher); !ok {
			t.Fatal("tracing middleware removed http.Flusher")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/events", nil))
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusNoContent)
	}
}
