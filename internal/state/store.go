package state

import (
	"context"
	"encoding/json"

	"github.com/openjobspec/ojs-backend-sqs/internal/core"
)

// JobRecord represents a job stored in the state store (DynamoDB).
type JobRecord struct {
	ID                  string            `dynamodbav:"PK"`
	SK                  string            `dynamodbav:"SK"`
	Type                string            `dynamodbav:"type"`
	State               string            `dynamodbav:"state"`
	Queue               string            `dynamodbav:"queue"`
	Args                string            `dynamodbav:"args,omitempty"`
	Meta                string            `dynamodbav:"meta,omitempty"`
	Priority            *int              `dynamodbav:"priority,omitempty"`
	Attempt             int               `dynamodbav:"attempt"`
	MaxAttempts         *int              `dynamodbav:"max_attempts,omitempty"`
	TimeoutMs           *int              `dynamodbav:"timeout_ms,omitempty"`
	CreatedAt           string            `dynamodbav:"created_at"`
	EnqueuedAt          string            `dynamodbav:"enqueued_at,omitempty"`
	StartedAt           string            `dynamodbav:"started_at,omitempty"`
	CompletedAt         string            `dynamodbav:"completed_at,omitempty"`
	CancelledAt         string            `dynamodbav:"cancelled_at,omitempty"`
	ScheduledAt         string            `dynamodbav:"scheduled_at,omitempty"`
	Result              string            `dynamodbav:"result,omitempty"`
	Error               string            `dynamodbav:"error_data,omitempty"`
	ErrorHistory        string            `dynamodbav:"error_history,omitempty"`
	Tags                []string          `dynamodbav:"tags,omitempty"`
	Retry               string            `dynamodbav:"retry,omitempty"`
	Unique              string            `dynamodbav:"unique_policy,omitempty"`
	ExpiresAt           string            `dynamodbav:"expires_at,omitempty"`
	RetryDelayMs        *int64            `dynamodbav:"retry_delay_ms,omitempty"`
	VisibilityTimeoutMs *int              `dynamodbav:"visibility_timeout_ms,omitempty"`
	WorkflowID          string            `dynamodbav:"workflow_id,omitempty"`
	WorkflowStep        int               `dynamodbav:"workflow_step,omitempty"`
	WorkerID            string            `dynamodbav:"worker_id,omitempty"`
	SQSReceiptHandle    string            `dynamodbav:"sqs_receipt_handle,omitempty"`
	SQSMessageID        string            `dynamodbav:"sqs_message_id,omitempty"`
	UnknownFields       map[string]string `dynamodbav:"unknown_fields,omitempty"`
	ParentResults       string            `dynamodbav:"parent_results,omitempty"`

	// GSI attributes for queries
	GSI1PK string `dynamodbav:"GSI1PK,omitempty"` // QUEUE#<name>
	GSI1SK string `dynamodbav:"GSI1SK,omitempty"` // STATE#<state>#<created_at>
	GSI2PK string `dynamodbav:"GSI2PK,omitempty"` // STATE#<state>
	GSI2SK string `dynamodbav:"GSI2SK,omitempty"` // <created_at>
	TTL    *int64 `dynamodbav:"ttl,omitempty"`     // DynamoDB TTL
}

// Store defines the interface for the external state store.
type Store interface {
	// Job operations
	PutJob(ctx context.Context, record *JobRecord) error
	GetJob(ctx context.Context, jobID string) (*JobRecord, error)
	UpdateJobState(ctx context.Context, jobID, newState string, updates map[string]any) error
	DeleteJob(ctx context.Context, jobID string) error

	// Query operations
	ListJobsByQueue(ctx context.Context, queue, state string, limit int) ([]*JobRecord, error)
	ListJobsByState(ctx context.Context, state string, limit, offset int) ([]*JobRecord, int, error)
	CountJobsByQueueAndState(ctx context.Context, queue, state string) (int, error)

	// Queue metadata
	RegisterQueue(ctx context.Context, name string) error
	ListQueues(ctx context.Context) ([]string, error)
	SetQueuePaused(ctx context.Context, name string, paused bool) error
	IsQueuePaused(ctx context.Context, name string) (bool, error)
	IncrementQueueCompleted(ctx context.Context, name string) error
	GetQueueCompletedCount(ctx context.Context, name string) (int, error)

	// Unique job tracking
	SetUniqueKey(ctx context.Context, fingerprint, jobID string, ttlSeconds int64) error
	GetUniqueKey(ctx context.Context, fingerprint string) (string, error)
	DeleteUniqueKey(ctx context.Context, fingerprint string) error

	// Workflow operations
	PutWorkflow(ctx context.Context, wf *WorkflowRecord) error
	GetWorkflow(ctx context.Context, id string) (*WorkflowRecord, error)
	UpdateWorkflow(ctx context.Context, id string, updates map[string]any) error
	AddWorkflowJob(ctx context.Context, workflowID, jobID string) error
	GetWorkflowJobs(ctx context.Context, workflowID string) ([]string, error)
	SetWorkflowResult(ctx context.Context, workflowID string, stepIdx int, result string) error
	GetWorkflowResults(ctx context.Context, workflowID string) (map[int]string, error)

	// Cron operations
	PutCron(ctx context.Context, cron *CronRecord) error
	GetCron(ctx context.Context, name string) (*CronRecord, error)
	DeleteCron(ctx context.Context, name string) error
	ListCrons(ctx context.Context) ([]*CronRecord, error)
	AcquireCronLock(ctx context.Context, name string, timestamp int64) (bool, error)
	GetCronInstance(ctx context.Context, name string) (string, error)
	SetCronInstance(ctx context.Context, name, jobID string) error

	// Worker operations
	PutWorker(ctx context.Context, workerID string, data map[string]string) error
	GetWorkerDirective(ctx context.Context, workerID string) (string, error)
	SetWorkerDirective(ctx context.Context, workerID, directive string) error

	// Dead letter operations
	AddToDeadLetter(ctx context.Context, jobID string) error
	RemoveFromDeadLetter(ctx context.Context, jobID string) error
	IsInDeadLetter(ctx context.Context, jobID string) (bool, error)

	// Scheduled job operations
	AddScheduledJob(ctx context.Context, jobID string, scheduledAtMs int64) error
	GetDueScheduledJobs(ctx context.Context, nowMs int64) ([]string, error)
	RemoveScheduledJob(ctx context.Context, jobID string) error

	// Retry operations
	AddRetryJob(ctx context.Context, jobID string, retryAtMs int64) error
	GetDueRetryJobs(ctx context.Context, nowMs int64) ([]string, error)
	RemoveRetryJob(ctx context.Context, jobID string) error

	// Health check
	Ping(ctx context.Context) error

	// Close the store
	Close() error
}

// WorkflowRecord represents a workflow in the state store.
type WorkflowRecord struct {
	ID          string `dynamodbav:"PK"`
	SK          string `dynamodbav:"SK"`
	Type        string `dynamodbav:"type"`
	Name        string `dynamodbav:"name,omitempty"`
	State       string `dynamodbav:"state"`
	Total       int    `dynamodbav:"total"`
	Completed   int    `dynamodbav:"completed"`
	Failed      int    `dynamodbav:"failed"`
	CreatedAt   string `dynamodbav:"created_at"`
	CompletedAt string `dynamodbav:"completed_at,omitempty"`
	Callbacks   string `dynamodbav:"callbacks,omitempty"`
	JobDefs     string `dynamodbav:"job_defs,omitempty"`
}

// CronRecord represents a cron job definition in the state store.
type CronRecord struct {
	PK            string `dynamodbav:"PK"`
	SK            string `dynamodbav:"SK"`
	Name          string `dynamodbav:"name"`
	Expression    string `dynamodbav:"expression"`
	Timezone      string `dynamodbav:"timezone,omitempty"`
	OverlapPolicy string `dynamodbav:"overlap_policy,omitempty"`
	Enabled       bool   `dynamodbav:"enabled"`
	JobTemplate   string `dynamodbav:"job_template,omitempty"`
	CreatedAt     string `dynamodbav:"created_at,omitempty"`
	NextRunAt     string `dynamodbav:"next_run_at,omitempty"`
	LastRunAt     string `dynamodbav:"last_run_at,omitempty"`
	Queue         string `dynamodbav:"queue,omitempty"`
}

// RecordToJob converts a JobRecord to a core.Job.
func RecordToJob(r *JobRecord) *core.Job {
	job := &core.Job{
		ID:          r.ID,
		Type:        r.Type,
		State:       r.State,
		Queue:       r.Queue,
		Attempt:     r.Attempt,
		MaxAttempts: r.MaxAttempts,
		Priority:    r.Priority,
		TimeoutMs:   r.TimeoutMs,
		CreatedAt:   r.CreatedAt,
		EnqueuedAt:  r.EnqueuedAt,
		StartedAt:   r.StartedAt,
		CompletedAt: r.CompletedAt,
		CancelledAt: r.CancelledAt,
		ScheduledAt: r.ScheduledAt,
		ExpiresAt:   r.ExpiresAt,
		Tags:        r.Tags,
		RetryDelayMs:        r.RetryDelayMs,
		VisibilityTimeoutMs: r.VisibilityTimeoutMs,
		WorkflowID:          r.WorkflowID,
		WorkflowStep:        r.WorkflowStep,
	}

	if r.Args != "" {
		job.Args = json.RawMessage(r.Args)
	}
	if r.Meta != "" {
		job.Meta = json.RawMessage(r.Meta)
	}
	if r.Result != "" {
		job.Result = json.RawMessage(r.Result)
	}
	if r.Error != "" {
		job.Error = json.RawMessage(r.Error)
	}
	if r.ErrorHistory != "" {
		var errors []json.RawMessage
		if json.Unmarshal([]byte(r.ErrorHistory), &errors) == nil {
			job.Errors = errors
		}
	}
	if r.Retry != "" {
		var retry core.RetryPolicy
		if json.Unmarshal([]byte(r.Retry), &retry) == nil {
			job.Retry = &retry
		}
	}
	if r.Unique != "" {
		var unique core.UniquePolicy
		if json.Unmarshal([]byte(r.Unique), &unique) == nil {
			job.Unique = &unique
		}
	}
	if r.ParentResults != "" {
		var pr []json.RawMessage
		if json.Unmarshal([]byte(r.ParentResults), &pr) == nil {
			job.ParentResults = pr
		}
	}

	// Restore unknown fields
	if len(r.UnknownFields) > 0 {
		job.UnknownFields = make(map[string]json.RawMessage)
		for k, v := range r.UnknownFields {
			job.UnknownFields[k] = json.RawMessage(v)
		}
	}

	return job
}

// JobToRecord converts a core.Job to a JobRecord for storage.
func JobToRecord(job *core.Job) *JobRecord {
	r := &JobRecord{
		ID:                  job.ID,
		SK:                  "JOB",
		Type:                job.Type,
		State:               job.State,
		Queue:               job.Queue,
		Attempt:             job.Attempt,
		MaxAttempts:         job.MaxAttempts,
		Priority:            job.Priority,
		TimeoutMs:           job.TimeoutMs,
		CreatedAt:           job.CreatedAt,
		EnqueuedAt:          job.EnqueuedAt,
		StartedAt:           job.StartedAt,
		CompletedAt:         job.CompletedAt,
		CancelledAt:         job.CancelledAt,
		ScheduledAt:         job.ScheduledAt,
		ExpiresAt:           job.ExpiresAt,
		Tags:                job.Tags,
		RetryDelayMs:        job.RetryDelayMs,
		VisibilityTimeoutMs: job.VisibilityTimeoutMs,
		WorkflowID:          job.WorkflowID,
		WorkflowStep:        job.WorkflowStep,
		WorkerID:            "",
		GSI1PK:              "QUEUE#" + job.Queue,
		GSI1SK:              "STATE#" + job.State + "#" + job.CreatedAt,
		GSI2PK:              "STATE#" + job.State,
		GSI2SK:              job.CreatedAt,
	}

	if job.Args != nil {
		r.Args = string(job.Args)
	}
	if job.Meta != nil {
		r.Meta = string(job.Meta)
	}
	if job.Result != nil {
		r.Result = string(job.Result)
	}
	if job.Error != nil {
		r.Error = string(job.Error)
	}
	if len(job.Errors) > 0 {
		histJSON, _ := json.Marshal(job.Errors)
		r.ErrorHistory = string(histJSON)
	}
	if job.Retry != nil {
		retryJSON, _ := json.Marshal(job.Retry)
		r.Retry = string(retryJSON)
	}
	if job.Unique != nil {
		uniqueJSON, _ := json.Marshal(job.Unique)
		r.Unique = string(uniqueJSON)
	}
	if len(job.ParentResults) > 0 {
		prJSON, _ := json.Marshal(job.ParentResults)
		r.ParentResults = string(prJSON)
	}

	// Store unknown fields
	if len(job.UnknownFields) > 0 {
		r.UnknownFields = make(map[string]string)
		for k, v := range job.UnknownFields {
			r.UnknownFields[k] = string(v)
		}
	}

	return r
}
