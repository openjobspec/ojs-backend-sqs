package sqs

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"testing"

	"github.com/openjobspec/ojs-backend-sqs/internal/core"
)

func TestNew_CreatesBackend(t *testing.T) {
	backend := New(nil, &storeMock{}, "ojs", false)

	if backend == nil {
		t.Fatal("expected non-nil backend")
	}
	if backend.queuePrefix != "ojs" {
		t.Errorf("queuePrefix = %q, want %q", backend.queuePrefix, "ojs")
	}
	if backend.useFIFO {
		t.Error("expected useFIFO to be false")
	}
	if backend.queueURLs == nil {
		t.Error("expected queueURLs map to be initialized")
	}
}

func TestNew_FIFO(t *testing.T) {
	backend := New(nil, &storeMock{}, "ojs", true)

	if !backend.useFIFO {
		t.Error("expected useFIFO to be true")
	}
}

func TestSetLogger(t *testing.T) {
	backend := New(nil, &storeMock{}, "ojs", false)
	logger := slog.Default()

	backend.SetLogger(logger)

	if backend.logger != logger {
		t.Error("expected logger to be set")
	}
}

func TestClose_DelegatestoStore(t *testing.T) {
	closed := false
	store := &storeMock{
		closeFn: func() error {
			closed = true
			return nil
		},
	}
	backend := New(nil, store, "ojs", false)

	err := backend.Close()

	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if !closed {
		t.Error("expected store Close to be called")
	}
}

func TestSQSQueueName_Standard(t *testing.T) {
	tests := []struct {
		name     string
		prefix   string
		queue    string
		fifo     bool
		expected string
	}{
		{
			name:     "standard queue",
			prefix:   "ojs",
			queue:    "default",
			fifo:     false,
			expected: "ojs-default",
		},
		{
			name:     "FIFO queue",
			prefix:   "ojs",
			queue:    "default",
			fifo:     true,
			expected: "ojs-default.fifo",
		},
		{
			name:     "queue with dots converted to hyphens",
			prefix:   "myapp",
			queue:    "email.send",
			fifo:     false,
			expected: "myapp-email-send",
		},
		{
			name:     "FIFO queue with dots",
			prefix:   "myapp",
			queue:    "email.send",
			fifo:     true,
			expected: "myapp-email-send.fifo",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend := &SQSBackend{queuePrefix: tt.prefix, useFIFO: tt.fifo}
			got := backend.sqsQueueName(tt.queue)
			if got != tt.expected {
				t.Errorf("sqsQueueName(%q) = %q, want %q", tt.queue, got, tt.expected)
			}
		})
	}
}

func TestSQSDLQName(t *testing.T) {
	tests := []struct {
		name     string
		prefix   string
		queue    string
		fifo     bool
		expected string
	}{
		{
			name:     "standard DLQ",
			prefix:   "ojs",
			queue:    "default",
			fifo:     false,
			expected: "ojs-default-dlq",
		},
		{
			name:     "FIFO DLQ",
			prefix:   "ojs",
			queue:    "default",
			fifo:     true,
			expected: "ojs-default-dlq.fifo",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend := &SQSBackend{queuePrefix: tt.prefix, useFIFO: tt.fifo}
			got := backend.sqsDLQName(tt.queue)
			if got != tt.expected {
				t.Errorf("sqsDLQName(%q) = %q, want %q", tt.queue, got, tt.expected)
			}
		})
	}
}

func TestSanitizeQueueName(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"default", "default"},
		{"email.send", "email-send"},
		{"my.queue.name", "my-queue-name"},
		{"no-dots", "no-dots"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := sanitizeQueueName(tt.input)
			if got != tt.expected {
				t.Errorf("sanitizeQueueName(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestComputeFingerprint_DefaultKeys(t *testing.T) {
	job := &core.Job{
		Type:   "email.send",
		Args:   json.RawMessage(`["test@example.com"]`),
		Queue:  "default",
		Unique: &core.UniquePolicy{},
	}

	fp := computeFingerprint(job)

	if fp == "" {
		t.Fatal("expected non-empty fingerprint")
	}

	// Same job should produce same fingerprint
	fp2 := computeFingerprint(job)
	if fp != fp2 {
		t.Errorf("fingerprint not deterministic: %q != %q", fp, fp2)
	}
}

func TestComputeFingerprint_CustomKeys(t *testing.T) {
	job := &core.Job{
		Type:  "email.send",
		Args:  json.RawMessage(`["test@example.com"]`),
		Queue: "default",
		Unique: &core.UniquePolicy{
			Keys: []string{"type", "queue"},
		},
	}

	fp := computeFingerprint(job)

	// Manually compute expected hash
	h := sha256.New()
	h.Write([]byte("queue:"))
	h.Write([]byte("default"))
	h.Write([]byte("type:"))
	h.Write([]byte("email.send"))
	expected := fmt.Sprintf("%x", h.Sum(nil))

	if fp != expected {
		t.Errorf("fingerprint = %q, want %q", fp, expected)
	}
}

func TestComputeFingerprint_DifferentArgs(t *testing.T) {
	job1 := &core.Job{
		Type:   "email.send",
		Args:   json.RawMessage(`["a@example.com"]`),
		Unique: &core.UniquePolicy{},
	}
	job2 := &core.Job{
		Type:   "email.send",
		Args:   json.RawMessage(`["b@example.com"]`),
		Unique: &core.UniquePolicy{},
	}

	fp1 := computeFingerprint(job1)
	fp2 := computeFingerprint(job2)

	if fp1 == fp2 {
		t.Error("expected different fingerprints for different args")
	}
}

func TestComputeFingerprint_NilArgs(t *testing.T) {
	job := &core.Job{
		Type:   "email.send",
		Unique: &core.UniquePolicy{},
	}

	fp := computeFingerprint(job)
	if fp == "" {
		t.Fatal("expected non-empty fingerprint even with nil args")
	}
}

func TestMatchesPattern_ExactMatch(t *testing.T) {
	tests := []struct {
		s       string
		pattern string
		want    bool
	}{
		{"AuthError", "AuthError", true},
		{"AuthError", "OtherError", false},
		{"AuthError", "Auth.*", true},
		{"NetworkTimeout", ".*Timeout", true},
		{"SomeError", ".*", true},
		{"", "", true},
		{"test", "^test$", true}, // ^test$ is a valid regex sub-pattern
	}

	for _, tt := range tests {
		t.Run(tt.s+"_"+tt.pattern, func(t *testing.T) {
			got := matchesPattern(tt.s, tt.pattern)
			if got != tt.want {
				t.Errorf("matchesPattern(%q, %q) = %v, want %v", tt.s, tt.pattern, got, tt.want)
			}
		})
	}
}

func TestGetQueueURL_CachesResult(t *testing.T) {
	backend := &SQSBackend{
		queueURLs:   make(map[string]string),
		queuePrefix: "ojs",
	}

	// Pre-populate cache
	backend.queueURLs["default"] = "https://sqs.us-east-1.amazonaws.com/123/ojs-default"

	url, err := backend.getQueueURL(context.Background(), "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if url != "https://sqs.us-east-1.amazonaws.com/123/ojs-default" {
		t.Errorf("url = %q, want cached URL", url)
	}
}

func TestInfo_NotFound(t *testing.T) {
	backend := &SQSBackend{
		store:  &storeMock{},
		logger: slog.Default(),
	}

	_, err := backend.Info(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent job")
	}

	ojsErr, ok := err.(*core.OJSError)
	if !ok {
		t.Fatalf("expected OJSError, got %T", err)
	}
	if ojsErr.Code != core.ErrCodeNotFound {
		t.Errorf("error code = %q, want %q", ojsErr.Code, core.ErrCodeNotFound)
	}
}

func TestCancel_NotFound(t *testing.T) {
	backend := &SQSBackend{
		store:  &storeMock{},
		logger: slog.Default(),
	}

	_, err := backend.Cancel(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error")
	}

	ojsErr, ok := err.(*core.OJSError)
	if !ok {
		t.Fatalf("expected OJSError, got %T", err)
	}
	if ojsErr.Code != core.ErrCodeNotFound {
		t.Errorf("error code = %q, want %q", ojsErr.Code, core.ErrCodeNotFound)
	}
}

func TestAck_NotFound(t *testing.T) {
	backend := &SQSBackend{
		store:  &storeMock{},
		logger: slog.Default(),
	}

	_, err := backend.Ack(context.Background(), "nonexistent", nil)
	if err == nil {
		t.Fatal("expected error")
	}

	ojsErr, ok := err.(*core.OJSError)
	if !ok {
		t.Fatalf("expected OJSError, got %T", err)
	}
	if ojsErr.Code != core.ErrCodeNotFound {
		t.Errorf("error code = %q, want %q", ojsErr.Code, core.ErrCodeNotFound)
	}
}

func TestNack_NotFound(t *testing.T) {
	backend := &SQSBackend{
		store:  &storeMock{},
		logger: slog.Default(),
	}

	_, err := backend.Nack(context.Background(), "nonexistent", nil, false)
	if err == nil {
		t.Fatal("expected error")
	}

	ojsErr, ok := err.(*core.OJSError)
	if !ok {
		t.Fatalf("expected OJSError, got %T", err)
	}
	if ojsErr.Code != core.ErrCodeNotFound {
		t.Errorf("error code = %q, want %q", ojsErr.Code, core.ErrCodeNotFound)
	}
}

func TestPushBatch_EmptyList(t *testing.T) {
	backend := &SQSBackend{
		store:  &storeMock{},
		logger: slog.Default(),
	}

	jobs, err := backend.PushBatch(context.Background(), []*core.Job{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(jobs) != 0 {
		t.Errorf("expected empty result, got %d jobs", len(jobs))
	}
}

