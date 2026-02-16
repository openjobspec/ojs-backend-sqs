package sqs

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/openjobspec/ojs-backend-sqs/internal/core"
	"github.com/openjobspec/ojs-backend-sqs/internal/state"
)

// SQSBackend implements core.Backend using AWS SQS + DynamoDB state store.
type SQSBackend struct {
	sqsClient   *sqs.Client
	store       state.Store
	queueURLs   map[string]string // cache: OJS queue name -> SQS queue URL
	queueURLsMu sync.RWMutex
	queuePrefix string
	useFIFO     bool
	startTime   time.Time
	logger      *slog.Logger
}

// New creates a new SQSBackend.
func New(sqsClient *sqs.Client, store state.Store, queuePrefix string, useFIFO bool) *SQSBackend {
	return &SQSBackend{
		sqsClient:   sqsClient,
		store:       store,
		queueURLs:   make(map[string]string),
		queuePrefix: queuePrefix,
		useFIFO:     useFIFO,
		startTime:   time.Now(),
		logger:      slog.Default(),
	}
}

// SetLogger sets the logger for the backend.
func (b *SQSBackend) SetLogger(logger *slog.Logger) {
	b.logger = logger
}

// Close closes the backend connection.
func (b *SQSBackend) Close() error {
	return b.store.Close()
}

// Health returns the health status.
func (b *SQSBackend) Health(ctx context.Context) (*core.HealthResponse, error) {
	start := time.Now()

	// Ping SQS (lightweight operation)
	_, sqsErr := b.sqsClient.ListQueues(ctx, &sqs.ListQueuesInput{
		MaxResults: aws.Int32(1),
	})

	// Ping state store
	storeErr := b.store.Ping(ctx)

	latency := time.Since(start).Milliseconds()

	resp := &core.HealthResponse{
		Version:       core.OJSVersion,
		UptimeSeconds: int64(time.Since(b.startTime).Seconds()),
	}

	if sqsErr != nil || storeErr != nil {
		resp.Status = "degraded"
		errMsg := ""
		if sqsErr != nil {
			errMsg += "SQS: " + sqsErr.Error()
		}
		if storeErr != nil {
			if errMsg != "" {
				errMsg += "; "
			}
			errMsg += "Store: " + storeErr.Error()
		}
		resp.Backend = core.BackendHealth{
			Type:   "sqs+dynamodb",
			Status: "disconnected",
			Error:  errMsg,
		}
		return resp, fmt.Errorf("health check failed: %s", errMsg)
	}

	resp.Status = "ok"
	resp.Backend = core.BackendHealth{
		Type:      "sqs+dynamodb",
		Status:    "connected",
		LatencyMs: latency,
	}
	return resp, nil
}

// computeFingerprint creates a unique fingerprint for deduplication.
func computeFingerprint(job *core.Job) string {
	h := sha256.New()
	keys := job.Unique.Keys
	if len(keys) == 0 {
		// Default: hash type + args
		keys = []string{"type", "args"}
	}
	sort.Strings(keys)
	for _, key := range keys {
		switch key {
		case "type":
			h.Write([]byte("type:"))
			h.Write([]byte(job.Type))
		case "args":
			h.Write([]byte("args:"))
			if job.Args != nil {
				h.Write(job.Args)
			}
		case "queue":
			h.Write([]byte("queue:"))
			h.Write([]byte(job.Queue))
		}
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

// matchesPattern checks if a string matches a pattern (regex or exact).
func matchesPattern(s, pattern string) bool {
	// Try regex matching first (patterns like "Auth.*")
	re, err := regexp.Compile("^" + pattern + "$")
	if err == nil {
		return re.MatchString(s)
	}
	// Fallback to exact match
	return s == pattern
}
