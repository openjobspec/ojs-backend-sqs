package sqs

import (
	"strconv"

	"github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/openjobspec/ojs-backend-sqs/internal/core"
)

// OJS message attribute names. SQS allows max 10 message attributes per message.
const (
	AttrOJSSpecVersion = "ojs.specversion"
	AttrOJSType        = "ojs.type"
	AttrOJSQueue       = "ojs.queue"
	AttrOJSPriority    = "ojs.priority"
	AttrOJSID          = "ojs.id"
	AttrOJSAttempt     = "ojs.attempt"
	AttrOJSScheduledAt = "ojs.scheduled_at"
	AttrOJSUniqueKey   = "ojs.unique_key"
	AttrOJSTraceID     = "ojs.trace_id"
	AttrOJSCreatedAt   = "ojs.created_at"
)

// BuildMessageAttributes creates SQS message attributes from a Job.
// SQS allows a maximum of 10 message attributes per message.
func BuildMessageAttributes(job *core.Job) map[string]types.MessageAttributeValue {
	attrs := make(map[string]types.MessageAttributeValue)

	attrs[AttrOJSSpecVersion] = types.MessageAttributeValue{
		DataType:    strPtr("String"),
		StringValue: strPtr(core.OJSVersion),
	}

	attrs[AttrOJSID] = types.MessageAttributeValue{
		DataType:    strPtr("String"),
		StringValue: strPtr(job.ID),
	}

	attrs[AttrOJSType] = types.MessageAttributeValue{
		DataType:    strPtr("String"),
		StringValue: strPtr(job.Type),
	}

	attrs[AttrOJSQueue] = types.MessageAttributeValue{
		DataType:    strPtr("String"),
		StringValue: strPtr(job.Queue),
	}

	if job.Priority != nil {
		attrs[AttrOJSPriority] = types.MessageAttributeValue{
			DataType:    strPtr("Number"),
			StringValue: strPtr(strconv.Itoa(*job.Priority)),
		}
	}

	attrs[AttrOJSAttempt] = types.MessageAttributeValue{
		DataType:    strPtr("Number"),
		StringValue: strPtr(strconv.Itoa(job.Attempt)),
	}

	if job.ScheduledAt != "" {
		attrs[AttrOJSScheduledAt] = types.MessageAttributeValue{
			DataType:    strPtr("String"),
			StringValue: strPtr(job.ScheduledAt),
		}
	}

	if job.CreatedAt != "" {
		attrs[AttrOJSCreatedAt] = types.MessageAttributeValue{
			DataType:    strPtr("String"),
			StringValue: strPtr(job.CreatedAt),
		}
	}

	return attrs
}

func strPtr(s string) *string {
	return &s
}
