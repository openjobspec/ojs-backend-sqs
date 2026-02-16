package state

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"

	"github.com/openjobspec/ojs-backend-sqs/internal/core"
)

func TestRecordToJob_BasicFields(t *testing.T) {
	record := &JobRecord{
		ID:        "job-123",
		SK:        "JOB",
		Type:      "email.send",
		State:     "available",
		Queue:     "default",
		Attempt:   0,
		CreatedAt: "2025-01-01T10:00:00.000Z",
	}

	job := RecordToJob(record)

	if job.ID != "job-123" {
		t.Errorf("ID = %q, want %q", job.ID, "job-123")
	}
	if job.Type != "email.send" {
		t.Errorf("Type = %q, want %q", job.Type, "email.send")
	}
	if job.State != "available" {
		t.Errorf("State = %q, want %q", job.State, "available")
	}
	if job.Queue != "default" {
		t.Errorf("Queue = %q, want %q", job.Queue, "default")
	}
}

func TestRecordToJob_WithArgs(t *testing.T) {
	record := &JobRecord{
		ID:    "job-123",
		Type:  "test",
		State: "available",
		Queue: "default",
		Args:  `["arg1","arg2"]`,
	}

	job := RecordToJob(record)

	if job.Args == nil {
		t.Fatal("expected non-nil args")
	}
	var args []string
	json.Unmarshal(job.Args, &args)
	if len(args) != 2 || args[0] != "arg1" {
		t.Errorf("args = %v, want [arg1, arg2]", args)
	}
}

func TestRecordToJob_WithMeta(t *testing.T) {
	record := &JobRecord{
		ID:    "job-123",
		Type:  "test",
		State: "available",
		Queue: "default",
		Meta:  `{"key":"value"}`,
	}

	job := RecordToJob(record)

	if job.Meta == nil {
		t.Fatal("expected non-nil meta")
	}
}

func TestRecordToJob_WithRetryPolicy(t *testing.T) {
	record := &JobRecord{
		ID:    "job-123",
		Type:  "test",
		State: "available",
		Queue: "default",
		Retry: `{"max_attempts":5,"initial_interval":"PT2S"}`,
	}

	job := RecordToJob(record)

	if job.Retry == nil {
		t.Fatal("expected non-nil retry policy")
	}
	if job.Retry.MaxAttempts != 5 {
		t.Errorf("retry max_attempts = %d, want 5", job.Retry.MaxAttempts)
	}
}

func TestRecordToJob_WithUniquePolicy(t *testing.T) {
	record := &JobRecord{
		ID:     "job-123",
		Type:   "test",
		State:  "available",
		Queue:  "default",
		Unique: `{"keys":["type","args"],"period":"PT1H"}`,
	}

	job := RecordToJob(record)

	if job.Unique == nil {
		t.Fatal("expected non-nil unique policy")
	}
	if len(job.Unique.Keys) != 2 {
		t.Errorf("unique keys count = %d, want 2", len(job.Unique.Keys))
	}
}

func TestRecordToJob_WithErrorHistory(t *testing.T) {
	record := &JobRecord{
		ID:           "job-123",
		Type:         "test",
		State:        "discarded",
		Queue:        "default",
		ErrorHistory: `[{"message":"error1"},{"message":"error2"}]`,
	}

	job := RecordToJob(record)

	if len(job.Errors) != 2 {
		t.Errorf("errors count = %d, want 2", len(job.Errors))
	}
}

func TestRecordToJob_WithUnknownFields(t *testing.T) {
	record := &JobRecord{
		ID:    "job-123",
		Type:  "test",
		State: "available",
		Queue: "default",
		UnknownFields: map[string]string{
			"custom_field": `"custom_value"`,
		},
	}

	job := RecordToJob(record)

	if len(job.UnknownFields) != 1 {
		t.Errorf("unknown fields count = %d, want 1", len(job.UnknownFields))
	}
	if string(job.UnknownFields["custom_field"]) != `"custom_value"` {
		t.Errorf("custom_field = %q, want %q", string(job.UnknownFields["custom_field"]), `"custom_value"`)
	}
}

func TestRecordToJob_EmptyOptionalFields(t *testing.T) {
	record := &JobRecord{
		ID:    "job-123",
		Type:  "test",
		State: "available",
		Queue: "default",
	}

	job := RecordToJob(record)

	if job.Args != nil {
		t.Error("expected nil args")
	}
	if job.Meta != nil {
		t.Error("expected nil meta")
	}
	if job.Result != nil {
		t.Error("expected nil result")
	}
	if job.Retry != nil {
		t.Error("expected nil retry")
	}
	if job.Unique != nil {
		t.Error("expected nil unique")
	}
}

func TestJobToRecord_BasicFields(t *testing.T) {
	job := &core.Job{
		ID:        "job-123",
		Type:      "email.send",
		State:     "available",
		Queue:     "default",
		Attempt:   0,
		CreatedAt: "2025-01-01T10:00:00.000Z",
	}

	record := JobToRecord(job)

	if record.ID != "job-123" {
		t.Errorf("ID = %q, want %q", record.ID, "job-123")
	}
	if record.SK != "JOB" {
		t.Errorf("SK = %q, want %q", record.SK, "JOB")
	}
	if record.GSI1PK != "QUEUE#default" {
		t.Errorf("GSI1PK = %q, want %q", record.GSI1PK, "QUEUE#default")
	}
	if record.GSI2PK != "STATE#available" {
		t.Errorf("GSI2PK = %q, want %q", record.GSI2PK, "STATE#available")
	}
}

func TestJobToRecord_WithArgs(t *testing.T) {
	job := &core.Job{
		ID:    "job-123",
		Type:  "test",
		State: "available",
		Queue: "default",
		Args:  json.RawMessage(`["arg1"]`),
	}

	record := JobToRecord(job)

	if record.Args != `["arg1"]` {
		t.Errorf("Args = %q, want %q", record.Args, `["arg1"]`)
	}
}

func TestJobToRecord_WithRetryPolicy(t *testing.T) {
	job := &core.Job{
		ID:    "job-123",
		Type:  "test",
		State: "available",
		Queue: "default",
		Retry: &core.RetryPolicy{MaxAttempts: 5},
	}

	record := JobToRecord(job)

	if record.Retry == "" {
		t.Fatal("expected non-empty retry JSON")
	}
	if !strings.Contains(record.Retry, "max_attempts") {
		t.Errorf("retry JSON missing max_attempts: %s", record.Retry)
	}
}

func TestJobToRecord_GSIAttributes(t *testing.T) {
	tests := []struct {
		name      string
		queue     string
		state     string
		createdAt string
		wantGSI1  string
		wantGSI2  string
	}{
		{
			name:      "default queue available",
			queue:     "default",
			state:     "available",
			createdAt: "2025-01-01T10:00:00.000Z",
			wantGSI1:  "QUEUE#default",
			wantGSI2:  "STATE#available",
		},
		{
			name:      "email queue active",
			queue:     "emails",
			state:     "active",
			createdAt: "2025-06-01T12:00:00.000Z",
			wantGSI1:  "QUEUE#emails",
			wantGSI2:  "STATE#active",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := &core.Job{
				ID:        "job-123",
				Type:      "test",
				State:     tt.state,
				Queue:     tt.queue,
				CreatedAt: tt.createdAt,
			}

			record := JobToRecord(job)

			if record.GSI1PK != tt.wantGSI1 {
				t.Errorf("GSI1PK = %q, want %q", record.GSI1PK, tt.wantGSI1)
			}
			if record.GSI2PK != tt.wantGSI2 {
				t.Errorf("GSI2PK = %q, want %q", record.GSI2PK, tt.wantGSI2)
			}
		})
	}
}

func TestJobToRecord_Roundtrip(t *testing.T) {
	priority := 3
	maxAttempts := 5
	timeoutMs := 30000
	retryDelay := int64(5000)

	original := &core.Job{
		ID:           "job-123",
		Type:         "email.send",
		State:        "available",
		Queue:        "default",
		Attempt:      1,
		Priority:     &priority,
		MaxAttempts:  &maxAttempts,
		TimeoutMs:    &timeoutMs,
		CreatedAt:    "2025-01-01T10:00:00.000Z",
		EnqueuedAt:   "2025-01-01T10:00:01.000Z",
		Args:         json.RawMessage(`["test@example.com"]`),
		Meta:         json.RawMessage(`{"source":"api"}`),
		Tags:         []string{"important", "email"},
		RetryDelayMs: &retryDelay,
		Retry: &core.RetryPolicy{
			MaxAttempts:     5,
			InitialInterval: "PT2S",
		},
	}

	record := JobToRecord(original)
	restored := RecordToJob(record)

	if restored.ID != original.ID {
		t.Errorf("ID = %q, want %q", restored.ID, original.ID)
	}
	if restored.Type != original.Type {
		t.Errorf("Type = %q, want %q", restored.Type, original.Type)
	}
	if restored.State != original.State {
		t.Errorf("State = %q, want %q", restored.State, original.State)
	}
	if restored.Queue != original.Queue {
		t.Errorf("Queue = %q, want %q", restored.Queue, original.Queue)
	}
	if *restored.Priority != *original.Priority {
		t.Errorf("Priority = %d, want %d", *restored.Priority, *original.Priority)
	}
	if *restored.MaxAttempts != *original.MaxAttempts {
		t.Errorf("MaxAttempts = %d, want %d", *restored.MaxAttempts, *original.MaxAttempts)
	}
	if restored.Retry == nil {
		t.Fatal("expected non-nil retry after roundtrip")
	}
	if restored.Retry.MaxAttempts != 5 {
		t.Errorf("retry.MaxAttempts = %d, want 5", restored.Retry.MaxAttempts)
	}
}

func TestJobToRecord_UnknownFieldsRoundtrip(t *testing.T) {
	original := &core.Job{
		ID:    "job-123",
		Type:  "test",
		State: "available",
		Queue: "default",
		UnknownFields: map[string]json.RawMessage{
			"x_custom": json.RawMessage(`"hello"`),
		},
	}

	record := JobToRecord(original)
	restored := RecordToJob(record)

	if len(restored.UnknownFields) != 1 {
		t.Fatalf("unknown fields count = %d, want 1", len(restored.UnknownFields))
	}
	if string(restored.UnknownFields["x_custom"]) != `"hello"` {
		t.Errorf("x_custom = %q, want %q", string(restored.UnknownFields["x_custom"]), `"hello"`)
	}
}

func TestGetDueScheduledJobs_EmptyResult(t *testing.T) {
	store := newTestDynamoStore(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-amz-json-1.0")
		_, _ = io.WriteString(w, `{"Items":[]}`)
	})

	ids, err := store.GetDueScheduledJobs(context.Background(), 1000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("expected 0 jobs, got %d", len(ids))
	}
}

func TestGetDueRetryJobs_QuerySuccess(t *testing.T) {
	var queryCount int32

	store := newTestDynamoStore(t, func(w http.ResponseWriter, r *http.Request) {
		target := r.Header.Get("X-Amz-Target")

		switch target {
		case "DynamoDB_20120810.Query":
			atomic.AddInt32(&queryCount, 1)
			w.Header().Set("Content-Type", "application/x-amz-json-1.0")
			_, _ = io.WriteString(w, `{"Items":[{"job_id":{"S":"retry-1"}},{"job_id":{"S":"retry-2"}}]}`)
		default:
			w.Header().Set("Content-Type", "application/x-amz-json-1.0")
			_, _ = io.WriteString(w, `{"Items":[]}`)
		}
	})

	ids, err := store.GetDueRetryJobs(context.Background(), 2000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ids) != 2 {
		t.Errorf("expected 2 retry jobs, got %d", len(ids))
	}
}

func TestPutJob_SendsCorrectTarget(t *testing.T) {
	var targetSeen string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetSeen = r.Header.Get("X-Amz-Target")
		w.Header().Set("Content-Type", "application/x-amz-json-1.0")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer server.Close()

	store := newTestDynamoStoreWithURL(t, server.URL)
	record := &JobRecord{
		ID:    "job-123",
		SK:    "JOB",
		Type:  "test",
		State: "available",
		Queue: "default",
	}

	_ = store.PutJob(context.Background(), record)

	if targetSeen != "DynamoDB_20120810.PutItem" {
		t.Errorf("target = %q, want %q", targetSeen, "DynamoDB_20120810.PutItem")
	}
}

// newTestDynamoStoreWithURL creates a test DynamoDB store pointing to a custom URL.
func newTestDynamoStoreWithURL(t *testing.T, serverURL string) *DynamoDBStore {
	t.Helper()

	cfg, err := config.LoadDefaultConfig(
		context.Background(),
		config.WithRegion("us-east-1"),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test", "test", "test")),
		config.WithEndpointResolverWithOptions(aws.EndpointResolverWithOptionsFunc(
			func(service, region string, options ...interface{}) (aws.Endpoint, error) {
				return aws.Endpoint{
					URL:               serverURL,
					HostnameImmutable: true,
					PartitionID:       "aws",
				}, nil
			},
		)),
	)
	if err != nil {
		t.Fatalf("load aws config: %v", err)
	}

	client := dynamodb.NewFromConfig(cfg, func(o *dynamodb.Options) {
		o.RetryMaxAttempts = 1
	})
	return NewDynamoDBStore(client, "ojs-jobs-test")
}
