package lambda

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/openjobspec/ojs-backend-sqs/internal/server"
)

func TestNewBuildsExistingRouterFromEnvironment(t *testing.T) {
	var (
		mu            sync.Mutex
		describeCalls int
		tableName     string
	)
	awsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.Header.Get("X-Amz-Target"), ".DescribeTable") {
			t.Errorf("unexpected AWS operation %q", r.Header.Get("X-Amz-Target"))
			http.Error(w, "unexpected operation", http.StatusBadRequest)
			return
		}

		var request struct {
			TableName string
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode DescribeTable request: %v", err)
		}
		mu.Lock()
		describeCalls++
		tableName = request.TableName
		mu.Unlock()

		w.Header().Set("Content-Type", "application/x-amz-json-1.0")
		_, _ = io.WriteString(w, `{"Table":{"TableName":"lambda-jobs","TableStatus":"ACTIVE"}}`)
	}))
	defer awsServer.Close()

	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_ENDPOINT_URL", awsServer.URL)
	t.Setenv("OJS_LOCALSTACK_ENDPOINT", awsServer.URL)
	t.Setenv("DYNAMODB_TABLE", "lambda-jobs")
	t.Setenv("SQS_QUEUE_PREFIX", "lambda")
	t.Setenv("SQS_USE_FIFO", "true")
	t.Setenv("OJS_API_KEY", "lambda-secret")
	t.Setenv("OJS_ALLOW_INSECURE_NO_AUTH", "false")

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	adapter, err := New(context.Background(), WithLogger(logger))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	t.Cleanup(func() {
		if err := adapter.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})

	mu.Lock()
	gotDescribeCalls := describeCalls
	gotTableName := tableName
	mu.Unlock()
	if gotDescribeCalls != 1 {
		t.Fatalf("DescribeTable calls = %d, want 1", gotDescribeCalls)
	}
	if gotTableName != "lambda-jobs" {
		t.Fatalf("DescribeTable table = %q, want lambda-jobs", gotTableName)
	}

	unauthorized := httptest.NewRecorder()
	adapter.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/ojs/manifest", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d, want %d", unauthorized.Code, http.StatusUnauthorized)
	}

	authorizedRequest := httptest.NewRequest(http.MethodGet, "/ojs/manifest", nil)
	authorizedRequest.Header.Set("Authorization", "Bearer lambda-secret")
	authorized := httptest.NewRecorder()
	adapter.Handler().ServeHTTP(authorized, authorizedRequest)
	if authorized.Code != http.StatusOK {
		t.Fatalf("authorized status = %d, want %d: %s", authorized.Code, http.StatusOK, authorized.Body.String())
	}
	if !bytes.Contains(authorized.Body.Bytes(), []byte(`"implementation"`)) {
		t.Fatalf("manifest response missing implementation metadata: %s", authorized.Body.String())
	}
	if err := adapter.Close(); err != nil {
		t.Fatalf("first Close() error = %v", err)
	}
	if err := adapter.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
}

func TestResolveAWSConfig_CustomEndpointPreservesCredentialChain(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "production-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "production-secret")
	t.Setenv("AWS_SESSION_TOKEN", "production-token")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")

	cfg, err := resolveAWSConfig(context.Background(), server.Config{
		AWSRegion:      "us-west-2",
		AWSEndpointURL: "https://aws-compatible.example",
	}, nil)
	if err != nil {
		t.Fatalf("resolveAWSConfig() error = %v", err)
	}
	if cfg.BaseEndpoint == nil || *cfg.BaseEndpoint != "https://aws-compatible.example" {
		t.Fatalf("BaseEndpoint = %v, want custom AWS endpoint", cfg.BaseEndpoint)
	}

	credentials, err := cfg.Credentials.Retrieve(context.Background())
	if err != nil {
		t.Fatalf("Retrieve() error = %v", err)
	}
	if credentials.AccessKeyID != "production-key" ||
		credentials.SecretAccessKey != "production-secret" ||
		credentials.SessionToken != "production-token" {
		t.Fatalf("credentials were replaced by endpoint override: %#v", credentials)
	}
}

func TestResolveAWSConfig_LocalStackExplicitlyUsesTestCredentials(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "production-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "production-secret")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")

	cfg, err := resolveAWSConfig(context.Background(), server.Config{
		AWSRegion:          "us-east-1",
		AWSEndpointURL:     "https://aws-compatible.example",
		LocalStackEndpoint: "http://localhost:4566",
	}, nil)
	if err != nil {
		t.Fatalf("resolveAWSConfig() error = %v", err)
	}
	if cfg.BaseEndpoint == nil || *cfg.BaseEndpoint != "http://localhost:4566" {
		t.Fatalf("BaseEndpoint = %v, want LocalStack endpoint", cfg.BaseEndpoint)
	}

	credentials, err := cfg.Credentials.Retrieve(context.Background())
	if err != nil {
		t.Fatalf("Retrieve() error = %v", err)
	}
	if credentials.AccessKeyID != "test" || credentials.SecretAccessKey != "test" {
		t.Fatalf("LocalStack credentials = %#v, want explicit test credentials", credentials)
	}
}
