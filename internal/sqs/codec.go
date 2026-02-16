package sqs

import (
	"encoding/json"
	"fmt"

	"github.com/openjobspec/ojs-backend-sqs/internal/core"
)

// MaxSQSMessageSize is the maximum SQS message size (256 KB).
const MaxSQSMessageSize = 256 * 1024

// EncodeJob serializes a Job to JSON for SQS message body.
// Returns an error if the encoded payload exceeds 256KB.
func EncodeJob(job *core.Job) (string, error) {
	data, err := json.Marshal(job)
	if err != nil {
		return "", fmt.Errorf("marshal job: %w", err)
	}

	if len(data) > MaxSQSMessageSize {
		return "", &core.OJSError{
			Code:    core.ErrCodeInvalidRequest,
			Message: fmt.Sprintf("Job payload size (%d bytes) exceeds SQS maximum of %d bytes. Consider using S3 for large payloads.", len(data), MaxSQSMessageSize),
			Details: map[string]any{
				"payload_size": len(data),
				"max_size":     MaxSQSMessageSize,
				"job_id":       job.ID,
			},
		}
	}

	return string(data), nil
}

// DecodeJob deserializes a Job from an SQS message body.
func DecodeJob(body string) (*core.Job, error) {
	var job core.Job

	// First unmarshal known fields
	if err := json.Unmarshal([]byte(body), &job); err != nil {
		return nil, fmt.Errorf("unmarshal job: %w", err)
	}

	// Then extract unknown fields
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		return nil, fmt.Errorf("unmarshal raw job: %w", err)
	}

	job.UnknownFields = make(map[string]json.RawMessage)
	for k, v := range raw {
		if !core.IsKnownJobField(k) {
			job.UnknownFields[k] = v
		}
	}
	if len(job.UnknownFields) == 0 {
		job.UnknownFields = nil
	}

	return &job, nil
}
