package state

import (
	"context"
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
)

func newTestDynamoStore(t *testing.T, h http.HandlerFunc) *DynamoDBStore {
	t.Helper()

	server := httptest.NewServer(h)
	t.Cleanup(server.Close)

	cfg, err := config.LoadDefaultConfig(
		context.Background(),
		config.WithRegion("us-east-1"),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test", "test", "test")),
		config.WithEndpointResolverWithOptions(aws.EndpointResolverWithOptionsFunc(
			func(service, region string, options ...interface{}) (aws.Endpoint, error) {
				return aws.Endpoint{
					URL:               server.URL,
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

func TestGetDueScheduledJobs_UsesDueIndexQuery(t *testing.T) {
	var queryCount int32

	store := newTestDynamoStore(t, func(w http.ResponseWriter, r *http.Request) {
		target := r.Header.Get("X-Amz-Target")
		body, _ := io.ReadAll(r.Body)

		if target != "DynamoDB_20120810.Query" {
			t.Fatalf("unexpected target: %s", target)
		}

		atomic.AddInt32(&queryCount, 1)
		payload := string(body)
		if !strings.Contains(payload, `"IndexName":"GSI3"`) {
			t.Fatalf("query payload missing GSI3 index: %s", payload)
		}
		if !strings.Contains(payload, "DUE#scheduled") {
			t.Fatalf("query payload missing scheduled due partition: %s", payload)
		}

		w.Header().Set("Content-Type", "application/x-amz-json-1.0")
		_, _ = io.WriteString(w, `{"Items":[{"job_id":{"S":"job-1"}},{"job_id":{"S":"job-2"}}]}`)
	})

	ids, err := store.GetDueScheduledJobs(context.Background(), 1000)
	if err != nil {
		t.Fatalf("GetDueScheduledJobs returned error: %v", err)
	}

	if got, want := len(ids), 2; got != want {
		t.Fatalf("GetDueScheduledJobs count = %d, want %d", got, want)
	}
	if ids[0] != "job-1" || ids[1] != "job-2" {
		t.Fatalf("unexpected job IDs: %v", ids)
	}
	if got := atomic.LoadInt32(&queryCount); got != 1 {
		t.Fatalf("query count = %d, want 1", got)
	}
}

func TestGetDueRetryJobs_FallsBackToScanWithoutIndex(t *testing.T) {
	var queryCount int32
	var scanCount int32

	store := newTestDynamoStore(t, func(w http.ResponseWriter, r *http.Request) {
		target := r.Header.Get("X-Amz-Target")

		switch target {
		case "DynamoDB_20120810.Query":
			atomic.AddInt32(&queryCount, 1)
			w.Header().Set("Content-Type", "application/x-amz-json-1.0")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"__type":"com.amazonaws.dynamodb.v20120810#ValidationException","message":"The table does not have the specified index: GSI3"}`)
		case "DynamoDB_20120810.Scan":
			atomic.AddInt32(&scanCount, 1)
			w.Header().Set("Content-Type", "application/x-amz-json-1.0")
			_, _ = io.WriteString(w, `{"Items":[{"job_id":{"S":"retry-job-1"}}]}`)
		default:
			t.Fatalf("unexpected target: %s", target)
		}
	})

	ids, err := store.GetDueRetryJobs(context.Background(), 2000)
	if err != nil {
		t.Fatalf("GetDueRetryJobs returned error: %v", err)
	}

	if got, want := len(ids), 1; got != want {
		t.Fatalf("GetDueRetryJobs count = %d, want %d", got, want)
	}
	if ids[0] != "retry-job-1" {
		t.Fatalf("unexpected retry job IDs: %v", ids)
	}
	if got := atomic.LoadInt32(&queryCount); got != 1 {
		t.Fatalf("query count = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&scanCount); got != 1 {
		t.Fatalf("scan count = %d, want 1", got)
	}
}
