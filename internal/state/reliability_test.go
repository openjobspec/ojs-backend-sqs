package state

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/openjobspec/ojs-backend-sqs/internal/core"
)

func TestMapTransactionError_MapsCancellationReasons(t *testing.T) {
	conditional := &types.TransactionCanceledException{
		CancellationReasons: []types.CancellationReason{
			{Code: aws.String("None")},
			{Code: aws.String("ConditionalCheckFailed"), Message: aws.String("lost race")},
		},
	}
	if err := mapTransactionError(conditional, -1); !errors.Is(err, ErrConditionFailed) {
		t.Fatalf("conditional transaction error = %v, want ErrConditionFailed", err)
	}

	duplicate := &types.TransactionCanceledException{
		CancellationReasons: []types.CancellationReason{
			{Code: aws.String("ConditionalCheckFailed")},
		},
	}
	if err := mapTransactionError(duplicate, 0); !errors.Is(err, ErrAlreadyApplied) {
		t.Fatalf("dedup transaction error = %v, want ErrAlreadyApplied", err)
	}

	validation := &types.TransactionCanceledException{
		CancellationReasons: []types.CancellationReason{
			{Code: aws.String("ValidationError"), Message: aws.String("wrong attribute type")},
		},
	}
	if err := mapTransactionError(validation, -1); !errors.Is(err, ErrCorruptItem) {
		t.Fatalf("validation transaction error = %v, want ErrCorruptItem", err)
	}
}

func TestQueryDueJobs_FollowsLastEvaluatedKey(t *testing.T) {
	var calls int32
	store := newTestDynamoStore(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"Limit":1`) {
			t.Fatalf("forced page limit missing: %s", body)
		}
		w.Header().Set("Content-Type", "application/x-amz-json-1.0")
		if atomic.AddInt32(&calls, 1) == 1 {
			_, _ = io.WriteString(w, `{"Items":[{"job_id":{"S":"job-1"}}],"LastEvaluatedKey":{"PK":{"S":"cursor"},"SK":{"S":"cursor"}}}`)
			return
		}
		if !strings.Contains(string(body), "ExclusiveStartKey") {
			t.Fatalf("second query did not include cursor: %s", body)
		}
		_, _ = io.WriteString(w, `{"Items":[{"job_id":{"S":"job-2"}}]}`)
	})
	store.pageLimit = 1

	ids, err := store.GetDueScheduledJobs(context.Background(), 1000)
	if err != nil {
		t.Fatalf("GetDueScheduledJobs: %v", err)
	}
	if strings.Join(ids, ",") != "job-1,job-2" {
		t.Fatalf("due job ids = %v", ids)
	}
}

func TestListCrons_FollowsLastEvaluatedKey(t *testing.T) {
	var calls int32
	store := newTestDynamoStore(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-amz-json-1.0")
		if atomic.AddInt32(&calls, 1) == 1 {
			_, _ = io.WriteString(w, `{"Items":[{"PK":{"S":"CRON#one"},"SK":{"S":"CRON"},"name":{"S":"one"},"expression":{"S":"* * * * *"},"enabled":{"BOOL":true}}],"LastEvaluatedKey":{"PK":{"S":"cursor"},"SK":{"S":"cursor"}}}`)
			return
		}
		_, _ = io.WriteString(w, `{"Items":[{"PK":{"S":"CRON#two"},"SK":{"S":"CRON"},"name":{"S":"two"},"expression":{"S":"* * * * *"},"enabled":{"BOOL":true}}]}`)
	})
	store.pageLimit = 1

	crons, err := store.ListCrons(context.Background())
	if err != nil {
		t.Fatalf("ListCrons: %v", err)
	}
	if len(crons) != 2 {
		t.Fatalf("cron count = %d, want 2", len(crons))
	}
}

func TestCountJobsByQueueAndState_FollowsAllPages(t *testing.T) {
	var calls int32
	store := newTestDynamoStore(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-amz-json-1.0")
		if atomic.AddInt32(&calls, 1) == 1 {
			_, _ = io.WriteString(w, `{"Count":2,"ScannedCount":2,"LastEvaluatedKey":{"PK":{"S":"cursor"},"SK":{"S":"cursor"}}}`)
			return
		}
		_, _ = io.WriteString(w, `{"Count":3,"ScannedCount":3}`)
	})
	store.pageLimit = 2

	count, err := store.CountJobsByQueueAndState(context.Background(), "default", "available")
	if err != nil {
		t.Fatalf("CountJobsByQueueAndState: %v", err)
	}
	if count != 5 {
		t.Fatalf("count = %d, want 5", count)
	}
}

func TestClaimJob_WrongAttributeTypeMapsToCorruptItem(t *testing.T) {
	store := newTestDynamoStore(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-amz-json-1.0")
		switch r.Header.Get("X-Amz-Target") {
		case "DynamoDB_20120810.GetItem":
			_, _ = io.WriteString(w, `{"Item":{"PK":{"S":"job-1"},"SK":{"S":"JOB"},"type":{"S":"test"},"state":{"S":"available"},"queue":{"S":"default"},"attempt":{"S":"wrong-type"},"created_at":{"S":"2026-01-01T00:00:00Z"},"delivery_generation":{"N":"1"}}}`)
		case "DynamoDB_20120810.UpdateItem":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"__type":"com.amazonaws.dynamodb.v20120810#ValidationException","message":"An operand in the update expression has an incorrect data type"}`)
		default:
			t.Fatalf("unexpected target %s", r.Header.Get("X-Amz-Target"))
		}
	})

	_, err := store.ClaimJob(context.Background(), JobClaim{
		JobID:             "job-1",
		WorkerID:          "worker-1",
		ReceiptHandle:     "receipt",
		MessageGeneration: 1,
		StartedAt:         "2026-01-01T00:00:01Z",
		DeadlineMs:        1000,
	})
	if !errors.Is(err, ErrCorruptItem) {
		t.Fatalf("ClaimJob error = %v, want ErrCorruptItem", err)
	}
}

func TestPutWorker_UsesUpdateAndPreservesUnmentionedDirective(t *testing.T) {
	var requestBody []byte
	store := newTestDynamoStore(t, func(w http.ResponseWriter, r *http.Request) {
		if target := r.Header.Get("X-Amz-Target"); target != "DynamoDB_20120810.UpdateItem" {
			t.Fatalf("target = %q, want UpdateItem", target)
		}
		requestBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/x-amz-json-1.0")
		_, _ = io.WriteString(w, `{}`)
	})

	if err := store.PutWorker(context.Background(), "worker-1", map[string]string{
		"last_heartbeat": "now",
		"active_jobs":    "2",
	}); err != nil {
		t.Fatalf("PutWorker: %v", err)
	}
	var request struct {
		UpdateExpression         string            `json:"UpdateExpression"`
		ExpressionAttributeNames map[string]string `json:"ExpressionAttributeNames"`
	}
	if err := json.Unmarshal(requestBody, &request); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	for _, name := range request.ExpressionAttributeNames {
		if name == "directive" {
			t.Fatalf("heartbeat update unexpectedly replaced directive: %s", requestBody)
		}
	}
	if !strings.HasPrefix(request.UpdateExpression, "SET ") {
		t.Fatalf("update expression = %q", request.UpdateExpression)
	}
}

func TestAdminScans_FollowAllPages(t *testing.T) {
	t.Run("jobs", func(t *testing.T) {
		var calls int32
		store := newTestDynamoStore(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/x-amz-json-1.0")
			if atomic.AddInt32(&calls, 1) == 1 {
				_, _ = io.WriteString(w, `{"Items":[{"PK":{"S":"job-1"},"SK":{"S":"JOB"},"type":{"S":"wanted"},"state":{"S":"available"},"queue":{"S":"default"},"attempt":{"N":"0"},"created_at":{"S":"2026-01-01T00:00:00Z"}}],"LastEvaluatedKey":{"PK":{"S":"cursor"},"SK":{"S":"cursor"}}}`)
				return
			}
			_, _ = io.WriteString(w, `{"Items":[{"PK":{"S":"job-2"},"SK":{"S":"JOB"},"type":{"S":"wanted"},"state":{"S":"available"},"queue":{"S":"default"},"attempt":{"N":"0"},"created_at":{"S":"2026-01-02T00:00:00Z"}}]}`)
		})
		store.pageLimit = 1
		jobs, total, err := store.ListAllJobs(context.Background(), core.JobListFilters{Type: "wanted"}, 10, 0)
		if err != nil {
			t.Fatalf("ListAllJobs: %v", err)
		}
		if total != 2 || len(jobs) != 2 {
			t.Fatalf("jobs total=%d page=%d, want 2", total, len(jobs))
		}
	})

	t.Run("workers", func(t *testing.T) {
		var calls int32
		store := newTestDynamoStore(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/x-amz-json-1.0")
			if atomic.AddInt32(&calls, 1) == 1 {
				_, _ = io.WriteString(w, `{"Items":[{"PK":{"S":"WORKER#one"},"SK":{"S":"WORKER"},"last_heartbeat":{"S":"now"}}],"LastEvaluatedKey":{"PK":{"S":"cursor"},"SK":{"S":"cursor"}}}`)
				return
			}
			_, _ = io.WriteString(w, `{"Items":[{"PK":{"S":"WORKER#two"},"SK":{"S":"WORKER"},"last_heartbeat":{"S":"now"}}]}`)
		})
		store.pageLimit = 1
		workers, summary, err := store.ListAllWorkers(context.Background(), 10, 0)
		if err != nil {
			t.Fatalf("ListAllWorkers: %v", err)
		}
		if summary.Total != 2 || len(workers) != 2 {
			t.Fatalf("workers total=%d page=%d, want 2", summary.Total, len(workers))
		}
	})
}
