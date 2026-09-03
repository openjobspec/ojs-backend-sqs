package state

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/smithy-go"

	"github.com/openjobspec/ojs-backend-sqs/internal/core"
)

const (
	deliveryOutboxSK  = "DELIVERY_OUTBOX"
	jobEffectSK       = "JOB_EFFECT"
	workflowAdvanceSK = "WORKFLOW_ADVANCE"
)

func requestToken(value string) *string {
	sum := sha256.Sum256([]byte(value))
	token := fmt.Sprintf("%x", sum[:18])
	return aws.String(token)
}

func requestTokenFor(prefix string, value any) *string {
	data, err := json.Marshal(value)
	if err != nil {
		return requestToken(prefix)
	}
	return requestToken(prefix + ":" + string(data))
}

func deliveryIntentKey(jobID string, generation int64) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{
		"PK": &types.AttributeValueMemberS{Value: fmt.Sprintf("DELIVERY#%s#%020d", jobID, generation)},
		"SK": &types.AttributeValueMemberS{Value: deliveryOutboxSK},
	}
}

func scheduledKey(jobID string) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{
		"PK": &types.AttributeValueMemberS{Value: "SCHEDULED#" + jobID},
		"SK": &types.AttributeValueMemberS{Value: "SCHEDULED"},
	}
}

func retryKey(jobID string) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{
		"PK": &types.AttributeValueMemberS{Value: "RETRY#" + jobID},
		"SK": &types.AttributeValueMemberS{Value: "RETRY"},
	}
}

func deadLetterKey(jobID string) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{
		"PK": &types.AttributeValueMemberS{Value: "DLQ#" + jobID},
		"SK": &types.AttributeValueMemberS{Value: "DLQ"},
	}
}

func jobKey(jobID string) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{
		"PK": &types.AttributeValueMemberS{Value: jobID},
		"SK": &types.AttributeValueMemberS{Value: "JOB"},
	}
}

func workflowKey(workflowID string) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{
		"PK": &types.AttributeValueMemberS{Value: workflowID},
		"SK": &types.AttributeValueMemberS{Value: "WORKFLOW"},
	}
}

func workflowAdvanceKey(workflowID, jobID string) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{
		"PK": &types.AttributeValueMemberS{Value: "WORKFLOW_ADVANCE#" + workflowID + "#" + jobID},
		"SK": &types.AttributeValueMemberS{Value: workflowAdvanceSK},
	}
}

func marshalTransactPut(item any, tableName string) (types.TransactWriteItem, error) {
	marshaled, err := attributevalue.MarshalMap(item)
	if err != nil {
		return types.TransactWriteItem{}, err
	}
	put := &types.Put{
		TableName:           aws.String(tableName),
		Item:                marshaled,
		ConditionExpression: aws.String("attribute_not_exists(PK)"),
	}
	return types.TransactWriteItem{Put: put}, nil
}

func mapConditionalError(err error) error {
	if err == nil {
		return nil
	}
	var conditional *types.ConditionalCheckFailedException
	if errors.As(err, &conditional) {
		return fmt.Errorf("%w: %s", ErrConditionFailed, conditional.ErrorMessage())
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) && apiErr.ErrorCode() == "ValidationException" {
		return fmt.Errorf("%w: %s", ErrCorruptItem, apiErr.ErrorMessage())
	}
	return err
}

func mapTransactionError(err error, alreadyAppliedIndex int) error {
	if err == nil {
		return nil
	}

	var cancelled *types.TransactionCanceledException
	if !errors.As(err, &cancelled) {
		return mapConditionalError(err)
	}

	for i, reason := range cancelled.CancellationReasons {
		code := aws.ToString(reason.Code)
		switch code {
		case "", "None":
			continue
		case "ConditionalCheckFailed":
			if i == alreadyAppliedIndex {
				return fmt.Errorf("%w: workflow completion", ErrAlreadyApplied)
			}
			return fmt.Errorf("%w: transaction item %d", ErrConditionFailed, i)
		case "ValidationError":
			return fmt.Errorf("%w: %s", ErrCorruptItem, aws.ToString(reason.Message))
		default:
			return fmt.Errorf("dynamodb transaction cancelled (%s): %s", code, aws.ToString(reason.Message))
		}
	}
	if strings.Contains(cancelled.ErrorMessage(), "ConditionalCheckFailed") {
		return fmt.Errorf("%w: transaction condition", ErrConditionFailed)
	}
	if strings.Contains(cancelled.ErrorMessage(), "ValidationError") {
		return fmt.Errorf("%w: %s", ErrCorruptItem, cancelled.ErrorMessage())
	}
	return fmt.Errorf("dynamodb transaction cancelled: %w", err)
}

// GetUniqueKeyRecord returns a strongly-consistent unique mapping, including TTL.
func (s *DynamoDBStore) GetUniqueKeyRecord(ctx context.Context, fingerprint string) (*UniqueKeyRecord, error) {
	result, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.tableName),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: "UNIQUE#" + fingerprint},
			"SK": &types.AttributeValueMemberS{Value: "UNIQUE"},
		},
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return nil, fmt.Errorf("get unique key: %w", err)
	}
	if len(result.Item) == 0 {
		return nil, nil
	}
	jobID, ok := result.Item["job_id"].(*types.AttributeValueMemberS)
	if !ok {
		return nil, fmt.Errorf("%w: unique job_id is not a string", ErrCorruptItem)
	}
	record := &UniqueKeyRecord{JobID: jobID.Value}
	if ttl, ok := result.Item["ttl"]; ok {
		number, numberOK := ttl.(*types.AttributeValueMemberN)
		if !numberOK {
			return nil, fmt.Errorf("%w: unique ttl is not a number", ErrCorruptItem)
		}
		record.ExpiresAtUnix, err = strconv.ParseInt(number.Value, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid unique ttl: %w", ErrCorruptItem, err)
		}
	}
	if guard, ok := result.Item["replacement_guard_until_ms"]; ok {
		number, numberOK := guard.(*types.AttributeValueMemberN)
		if !numberOK {
			return nil, fmt.Errorf("%w: unique replacement guard is not a number", ErrCorruptItem)
		}
		record.ReplacementGuardUntilMs, err = strconv.ParseInt(number.Value, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid unique replacement guard: %w", ErrCorruptItem, err)
		}
	}
	return record, nil
}

// CreateJobAtomic persists the job and its due marker or delivery intent in one transaction.
func (s *DynamoDBStore) CreateJobAtomic(ctx context.Context, plan *JobCreatePlan) error {
	if plan == nil || plan.Job == nil {
		return fmt.Errorf("create job plan is required")
	}

	jobItem, err := attributevalue.MarshalMap(plan.Job)
	if err != nil {
		return fmt.Errorf("marshal job: %w", err)
	}

	items := []types.TransactWriteItem{{
		Put: &types.Put{
			TableName:           aws.String(s.tableName),
			Item:                jobItem,
			ConditionExpression: aws.String("attribute_not_exists(PK)"),
		},
	}}

	items = append(items, types.TransactWriteItem{Update: &types.Update{
		TableName: aws.String(s.tableName),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: "QUEUE#" + plan.Job.Queue},
			"SK": &types.AttributeValueMemberS{Value: "META"},
		},
		UpdateExpression: aws.String("SET #name = if_not_exists(#name, :name), paused = if_not_exists(paused, :false), completed = if_not_exists(completed, :zero)"),
		ExpressionAttributeNames: map[string]string{
			"#name": "name",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":name":  &types.AttributeValueMemberS{Value: plan.Job.Queue},
			":false": &types.AttributeValueMemberBOOL{Value: false},
			":zero":  &types.AttributeValueMemberN{Value: "0"},
		},
	}})

	if plan.Job.WorkflowID != "" {
		items = append(items, types.TransactWriteItem{ConditionCheck: &types.ConditionCheck{
			TableName:           aws.String(s.tableName),
			Key:                 workflowKey(plan.Job.WorkflowID),
			ConditionExpression: aws.String("#state = :running"),
			ExpressionAttributeNames: map[string]string{
				"#state": "state",
			},
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":running": &types.AttributeValueMemberS{Value: "running"},
			},
		}})
		items = append(items, types.TransactWriteItem{Put: &types.Put{
			TableName: aws.String(s.tableName),
			Item: map[string]types.AttributeValue{
				"PK":     &types.AttributeValueMemberS{Value: plan.Job.WorkflowID},
				"SK":     &types.AttributeValueMemberS{Value: "JOB#" + plan.Job.ID},
				"job_id": &types.AttributeValueMemberS{Value: plan.Job.ID},
			},
			ConditionExpression: aws.String("attribute_not_exists(PK)"),
		}})
	}

	if plan.ScheduledAtMs != nil {
		items = append(items, scheduledMarkerPut(s.tableName, plan.Job.ID, *plan.ScheduledAtMs))
	}
	if plan.Intent != nil {
		intentPut, marshalErr := marshalTransactPut(plan.Intent, s.tableName)
		if marshalErr != nil {
			return fmt.Errorf("marshal delivery intent: %w", marshalErr)
		}
		items = append(items, intentPut)
	}

	if plan.Unique != nil {
		uniqueItems := s.uniqueCreateItems(plan.Unique, plan.Job.ID)
		items = append(items, uniqueItems...)
	}

	if len(items) > 100 {
		return fmt.Errorf("job transaction has %d items; maximum is 100", len(items))
	}
	_, err = s.client.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{
		TransactItems:      items,
		ClientRequestToken: requestTokenFor("create", plan),
	})
	if err != nil {
		return fmt.Errorf("create job transaction: %w", mapTransactionError(err, -1))
	}
	return nil
}

func (s *DynamoDBStore) uniqueCreateItems(plan *UniqueJobPlan, newJobID string) []types.TransactWriteItem {
	uniquePK := "UNIQUE#" + plan.Fingerprint
	item := map[string]types.AttributeValue{
		"PK":     &types.AttributeValueMemberS{Value: uniquePK},
		"SK":     &types.AttributeValueMemberS{Value: "UNIQUE"},
		"job_id": &types.AttributeValueMemberS{Value: newJobID},
		"ttl":    &types.AttributeValueMemberN{Value: strconv.FormatInt(plan.ExpiresAtUnix, 10)},
	}
	if plan.ReplacementGuardUntilMs > 0 {
		item["replacement_guard_until_ms"] = &types.AttributeValueMemberN{Value: strconv.FormatInt(plan.ReplacementGuardUntilMs, 10)}
	}
	put := &types.Put{
		TableName: aws.String(s.tableName),
		Item:      item,
		ExpressionAttributeNames: map[string]string{
			"#ttl": "ttl",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":now": &types.AttributeValueMemberN{Value: strconv.FormatInt(plan.NowUnix, 10)},
		},
	}
	if plan.ExpectedMappingJobID == "" {
		put.ConditionExpression = aws.String("attribute_not_exists(PK) OR #ttl <= :now")
	} else {
		put.ExpressionAttributeNames["#job"] = "job_id"
		put.ConditionExpression = aws.String("#job = :expected AND (attribute_not_exists(#ttl) OR #ttl > :now)")
		put.ExpressionAttributeValues[":expected"] = &types.AttributeValueMemberS{Value: plan.ExpectedMappingJobID}
	}

	items := []types.TransactWriteItem{{Put: put}}
	if !plan.CancelExisting {
		return items
	}

	gsi1sk := fmt.Sprintf("STATE#%s#%s", core.StateCancelled, plan.ExistingCreatedAt)
	items = append(items, types.TransactWriteItem{Update: &types.Update{
		TableName: aws.String(s.tableName),
		Key:       jobKey(plan.ExpectedMappingJobID),
		UpdateExpression: aws.String(
			"SET #state = :cancelled, cancelled_at = :cancelled_at, GSI1SK = :gsi1sk, GSI2PK = :gsi2pk, sqs_receipt_handle = :empty, worker_id = :empty, delivery_deadline_ms = :zero ADD #version :one",
		),
		ConditionExpression: aws.String("#state = :expected_state AND (#version = :version OR (attribute_not_exists(#version) AND :version = :zero))"),
		ExpressionAttributeNames: map[string]string{
			"#state":   "state",
			"#version": "version",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":cancelled":      &types.AttributeValueMemberS{Value: core.StateCancelled},
			":cancelled_at":   &types.AttributeValueMemberS{Value: plan.CancelledAt},
			":gsi1sk":         &types.AttributeValueMemberS{Value: gsi1sk},
			":gsi2pk":         &types.AttributeValueMemberS{Value: "STATE#" + core.StateCancelled},
			":empty":          &types.AttributeValueMemberS{Value: ""},
			":zero":           &types.AttributeValueMemberN{Value: "0"},
			":one":            &types.AttributeValueMemberN{Value: "1"},
			":expected_state": &types.AttributeValueMemberS{Value: plan.ExistingState},
			":version":        &types.AttributeValueMemberN{Value: strconv.FormatInt(plan.ExistingVersion, 10)},
		},
	}})

	switch plan.ExistingState {
	case core.StateScheduled:
		items = append(items, types.TransactWriteItem{Delete: &types.Delete{
			TableName: aws.String(s.tableName),
			Key:       scheduledKey(plan.ExpectedMappingJobID),
		}})
	case core.StateRetryable:
		items = append(items, types.TransactWriteItem{Delete: &types.Delete{
			TableName: aws.String(s.tableName),
			Key:       retryKey(plan.ExpectedMappingJobID),
		}})
	}
	if plan.ExistingDeliveryGeneration > 0 {
		items = append(items, types.TransactWriteItem{Delete: &types.Delete{
			TableName: aws.String(s.tableName),
			Key:       deliveryIntentKey(plan.ExpectedMappingJobID, plan.ExistingDeliveryGeneration),
		}})
	}
	if plan.ExistingWorkflowID != "" {
		items = append(items, types.TransactWriteItem{Put: &types.Put{
			TableName: aws.String(s.tableName),
			Item: map[string]types.AttributeValue{
				"PK":          &types.AttributeValueMemberS{Value: "WORKFLOW_ADVANCE#" + plan.ExistingWorkflowID + "#" + plan.ExpectedMappingJobID},
				"SK":          &types.AttributeValueMemberS{Value: workflowAdvanceSK},
				"workflow_id": &types.AttributeValueMemberS{Value: plan.ExistingWorkflowID},
				"job_id":      &types.AttributeValueMemberS{Value: plan.ExpectedMappingJobID},
				"failed":      &types.AttributeValueMemberBOOL{Value: true},
				"created_at":  &types.AttributeValueMemberS{Value: core.NowFormatted()},
			},
			ConditionExpression: aws.String("attribute_not_exists(PK)"),
		}})
	}
	return items
}

func scheduledMarkerPut(tableName, jobID string, scheduledAtMs int64) types.TransactWriteItem {
	return types.TransactWriteItem{Put: &types.Put{
		TableName: aws.String(tableName),
		Item: map[string]types.AttributeValue{
			"PK":              &types.AttributeValueMemberS{Value: "SCHEDULED#" + jobID},
			"SK":              &types.AttributeValueMemberS{Value: "SCHEDULED"},
			"job_id":          &types.AttributeValueMemberS{Value: jobID},
			"scheduled_at_ms": &types.AttributeValueMemberN{Value: strconv.FormatInt(scheduledAtMs, 10)},
			"GSI3PK":          &types.AttributeValueMemberS{Value: "DUE#scheduled"},
			"GSI3SK":          &types.AttributeValueMemberN{Value: strconv.FormatInt(scheduledAtMs, 10)},
		},
	}}
}

func retryMarkerPut(tableName, jobID string, retryAtMs int64) types.TransactWriteItem {
	return types.TransactWriteItem{Put: &types.Put{
		TableName: aws.String(tableName),
		Item: map[string]types.AttributeValue{
			"PK":          &types.AttributeValueMemberS{Value: "RETRY#" + jobID},
			"SK":          &types.AttributeValueMemberS{Value: "RETRY"},
			"job_id":      &types.AttributeValueMemberS{Value: jobID},
			"retry_at_ms": &types.AttributeValueMemberN{Value: strconv.FormatInt(retryAtMs, 10)},
			"GSI3PK":      &types.AttributeValueMemberS{Value: "DUE#retry"},
			"GSI3SK":      &types.AttributeValueMemberN{Value: strconv.FormatInt(retryAtMs, 10)},
		},
	}}
}

// ClaimJob conditionally performs available -> active, increments the attempt,
// and records the exact SQS delivery ownership.
func (s *DynamoDBStore) ClaimJob(ctx context.Context, claim JobClaim) (*JobRecord, error) {
	current, err := s.GetJob(ctx, claim.JobID)
	if err != nil {
		return nil, err
	}

	actualGeneration := claim.MessageGeneration
	generationCondition := "delivery_generation = :message_generation"
	if claim.MessageGeneration == 0 {
		if current.DeliveryGeneration != 0 {
			return nil, fmt.Errorf("%w: delivery generation mismatch", ErrConditionFailed)
		}
		actualGeneration = 1
		generationCondition = "attribute_not_exists(delivery_generation) OR delivery_generation = :zero"
	}

	gsi1sk := fmt.Sprintf("STATE#%s#%s", core.StateActive, current.CreatedAt)
	expressionValues := map[string]types.AttributeValue{
		":active":            &types.AttributeValueMemberS{Value: core.StateActive},
		":available":         &types.AttributeValueMemberS{Value: core.StateAvailable},
		":gsi1sk":            &types.AttributeValueMemberS{Value: gsi1sk},
		":gsi2pk":            &types.AttributeValueMemberS{Value: "STATE#" + core.StateActive},
		":started":           &types.AttributeValueMemberS{Value: claim.StartedAt},
		":worker":            &types.AttributeValueMemberS{Value: claim.WorkerID},
		":receipt":           &types.AttributeValueMemberS{Value: claim.ReceiptHandle},
		":message_id":        &types.AttributeValueMemberS{Value: claim.MessageID},
		":actual_generation": &types.AttributeValueMemberN{Value: strconv.FormatInt(actualGeneration, 10)},
		":deadline":          &types.AttributeValueMemberN{Value: strconv.FormatInt(claim.DeadlineMs, 10)},
		":one":               &types.AttributeValueMemberN{Value: "1"},
	}
	if claim.MessageGeneration == 0 {
		expressionValues[":zero"] = &types.AttributeValueMemberN{Value: "0"}
	} else {
		expressionValues[":message_generation"] = &types.AttributeValueMemberN{Value: strconv.FormatInt(claim.MessageGeneration, 10)}
	}

	result, err := s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.tableName),
		Key:       jobKey(claim.JobID),
		UpdateExpression: aws.String(
			"SET #state = :active, GSI1SK = :gsi1sk, GSI2PK = :gsi2pk, started_at = :started, worker_id = :worker, sqs_receipt_handle = :receipt, sqs_message_id = :message_id, delivery_generation = :actual_generation, delivery_deadline_ms = :deadline ADD attempt :one, #version :one",
		),
		ConditionExpression: aws.String("#state = :available AND (" + generationCondition + ")"),
		ExpressionAttributeNames: map[string]string{
			"#state":   "state",
			"#version": "version",
		},
		ExpressionAttributeValues: expressionValues,
		ReturnValues:              types.ReturnValueAllNew,
	})
	if err != nil {
		return nil, fmt.Errorf("claim job: %w", mapConditionalError(err))
	}

	var record JobRecord
	if err := attributevalue.UnmarshalMap(result.Attributes, &record); err != nil {
		return nil, fmt.Errorf("%w: unmarshal claimed job: %w", ErrCorruptItem, err)
	}
	return &record, nil
}

// TransitionJob atomically fences a state transition and all associated marker changes.
func (s *DynamoDBStore) TransitionJob(ctx context.Context, plan *JobTransitionPlan) (*JobRecord, error) {
	if plan == nil {
		return nil, fmt.Errorf("transition plan is required")
	}

	update, err := s.transitionUpdate(plan)
	if err != nil {
		return nil, err
	}
	items := []types.TransactWriteItem{{Update: update}}
	sideEffects, err := s.transitionSideEffects(plan)
	if err != nil {
		return nil, err
	}
	items = append(items, sideEffects...)

	_, err = s.client.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{
		TransactItems:      items,
		ClientRequestToken: requestTokenFor("transition", plan),
	})
	if err != nil {
		return nil, fmt.Errorf("transition job: %w", mapTransactionError(err, -1))
	}
	return s.GetJob(ctx, plan.JobID)
}

func (s *DynamoDBStore) transitionSideEffects(plan *JobTransitionPlan) ([]types.TransactWriteItem, error) {
	var items []types.TransactWriteItem
	if plan.Intent != nil {
		intentPut, marshalErr := marshalTransactPut(plan.Intent, s.tableName)
		if marshalErr != nil {
			return nil, fmt.Errorf("marshal delivery intent: %w", marshalErr)
		}
		items = append(items, intentPut)
	}
	if plan.ScheduledAtMs != nil {
		items = append(items, scheduledMarkerPut(s.tableName, plan.JobID, *plan.ScheduledAtMs))
	}
	if plan.RetryAtMs != nil {
		items = append(items, retryMarkerPut(s.tableName, plan.JobID, *plan.RetryAtMs))
	}
	if plan.DeleteScheduled {
		items = append(items, types.TransactWriteItem{Delete: &types.Delete{TableName: aws.String(s.tableName), Key: scheduledKey(plan.JobID)}})
	}
	if plan.DeleteRetry {
		items = append(items, types.TransactWriteItem{Delete: &types.Delete{TableName: aws.String(s.tableName), Key: retryKey(plan.JobID)}})
	}
	if plan.AddDeadLetter {
		items = append(items, types.TransactWriteItem{Put: &types.Put{
			TableName: aws.String(s.tableName),
			Item: map[string]types.AttributeValue{
				"PK":       &types.AttributeValueMemberS{Value: "DLQ#" + plan.JobID},
				"SK":       &types.AttributeValueMemberS{Value: "DLQ"},
				"job_id":   &types.AttributeValueMemberS{Value: plan.JobID},
				"added_at": &types.AttributeValueMemberS{Value: time.Now().UTC().Format(time.RFC3339)},
			},
		}})
	}
	if plan.DeleteDeadLetter {
		items = append(items, types.TransactWriteItem{Delete: &types.Delete{TableName: aws.String(s.tableName), Key: deadLetterKey(plan.JobID)}})
	}
	if plan.DeleteCurrentIntent && plan.ExpectedDeliveryGeneration > 0 {
		items = append(items, types.TransactWriteItem{Delete: &types.Delete{
			TableName: aws.String(s.tableName),
			Key:       deliveryIntentKey(plan.JobID, plan.ExpectedDeliveryGeneration),
		}})
	}
	if plan.WorkflowAdvance != nil {
		advancePut, marshalErr := marshalTransactPut(plan.WorkflowAdvance, s.tableName)
		if marshalErr != nil {
			return nil, fmt.Errorf("marshal workflow advancement: %w", marshalErr)
		}
		items = append(items, advancePut)
	}
	return items, nil
}

func (s *DynamoDBStore) transitionUpdate(plan *JobTransitionPlan) (*types.Update, error) {
	names := map[string]string{
		"#state":   "state",
		"#version": "version",
	}
	values := map[string]types.AttributeValue{
		":from": &types.AttributeValueMemberS{Value: plan.FromState},
		":to":   &types.AttributeValueMemberS{Value: plan.ToState},
		":gsi1": &types.AttributeValueMemberS{Value: fmt.Sprintf("STATE#%s#%s", plan.ToState, plan.CreatedAt)},
		":gsi2": &types.AttributeValueMemberS{Value: "STATE#" + plan.ToState},
		":one":  &types.AttributeValueMemberN{Value: "1"},
		":zero": &types.AttributeValueMemberN{Value: "0"},
	}
	setParts := []string{"#state = :to", "GSI1SK = :gsi1", "GSI2PK = :gsi2"}
	removeParts := make([]string, 0)

	keys := make([]string, 0, len(plan.Updates))
	for key := range plan.Updates {
		if isManagedStateAttribute(key) || key == "version" {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for i, key := range keys {
		name := fmt.Sprintf("#u%d", i)
		names[name] = key
		value := plan.Updates[key]
		if value == nil {
			removeParts = append(removeParts, name)
			continue
		}
		placeholder := fmt.Sprintf(":u%d", i)
		av, err := attributevalue.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("marshal transition value %s: %w", key, err)
		}
		values[placeholder] = av
		setParts = append(setParts, name+" = "+placeholder)
	}

	conditionParts := []string{"#state = :from"}
	if plan.MatchVersion {
		values[":expected_version"] = &types.AttributeValueMemberN{Value: strconv.FormatInt(plan.ExpectedVersion, 10)}
		conditionParts = append(conditionParts, "(#version = :expected_version OR (attribute_not_exists(#version) AND :expected_version = :zero))")
	}
	if plan.MatchDeliveryGeneration {
		values[":expected_generation"] = &types.AttributeValueMemberN{Value: strconv.FormatInt(plan.ExpectedDeliveryGeneration, 10)}
		conditionParts = append(conditionParts, "delivery_generation = :expected_generation")
	}
	if plan.ExpectedReceiptHandle != "" {
		values[":expected_receipt"] = &types.AttributeValueMemberS{Value: plan.ExpectedReceiptHandle}
		conditionParts = append(conditionParts, "sqs_receipt_handle = :expected_receipt")
	}
	if plan.ExpectedWorkerID != "" {
		values[":expected_worker"] = &types.AttributeValueMemberS{Value: plan.ExpectedWorkerID}
		conditionParts = append(conditionParts, "worker_id = :expected_worker")
	}
	if plan.DeadlineBeforeMs != nil {
		values[":deadline"] = &types.AttributeValueMemberN{Value: strconv.FormatInt(*plan.DeadlineBeforeMs, 10)}
		conditionParts = append(conditionParts, "delivery_deadline_ms <= :deadline")
	}

	updateExpression := "SET " + strings.Join(setParts, ", ")
	if len(removeParts) > 0 {
		updateExpression += " REMOVE " + strings.Join(removeParts, ", ")
	}
	updateExpression += " ADD #version :one"

	return &types.Update{
		TableName:                 aws.String(s.tableName),
		Key:                       jobKey(plan.JobID),
		UpdateExpression:          aws.String(updateExpression),
		ConditionExpression:       aws.String(strings.Join(conditionParts, " AND ")),
		ExpressionAttributeNames:  names,
		ExpressionAttributeValues: values,
	}, nil
}

// RefreshClaim updates the tracked deadline only when the same worker still owns
// the exact active delivery generation and receipt.
func (s *DynamoDBStore) RefreshClaim(ctx context.Context, jobID, workerID, receiptHandle string, generation, deadlineMs int64) error {
	_, err := s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.tableName),
		Key:       jobKey(jobID),
		UpdateExpression: aws.String(
			"SET delivery_deadline_ms = :deadline, last_heartbeat_at = :heartbeat",
		),
		ConditionExpression: aws.String("#state = :active AND delivery_generation = :generation AND sqs_receipt_handle = :receipt AND worker_id = :worker"),
		ExpressionAttributeNames: map[string]string{
			"#state": "state",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":deadline":   &types.AttributeValueMemberN{Value: strconv.FormatInt(deadlineMs, 10)},
			":heartbeat":  &types.AttributeValueMemberS{Value: core.NowFormatted()},
			":active":     &types.AttributeValueMemberS{Value: core.StateActive},
			":generation": &types.AttributeValueMemberN{Value: strconv.FormatInt(generation, 10)},
			":receipt":    &types.AttributeValueMemberS{Value: receiptHandle},
			":worker":     &types.AttributeValueMemberS{Value: workerID},
		},
	})
	if err != nil {
		return fmt.Errorf("refresh claim: %w", mapConditionalError(err))
	}
	return nil
}

// ListExpiredActiveJobs returns every active job whose tracked delivery lease expired.
func (s *DynamoDBStore) ListExpiredActiveJobs(ctx context.Context, nowMs int64) ([]*JobRecord, error) {
	input := &dynamodb.QueryInput{
		TableName:              aws.String(s.tableName),
		IndexName:              aws.String("GSI2"),
		KeyConditionExpression: aws.String("GSI2PK = :state"),
		FilterExpression:       aws.String("delivery_deadline_ms <= :now"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":state": &types.AttributeValueMemberS{Value: "STATE#" + core.StateActive},
			":now":   &types.AttributeValueMemberN{Value: strconv.FormatInt(nowMs, 10)},
		},
	}
	if s.pageLimit > 0 {
		input.Limit = aws.Int32(s.pageLimit)
	}

	var records []*JobRecord
	for {
		result, err := s.client.Query(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("query expired active jobs: %w", err)
		}
		for _, item := range result.Items {
			var record JobRecord
			if err := attributevalue.UnmarshalMap(item, &record); err != nil {
				return nil, fmt.Errorf("%w: expired active job: %w", ErrCorruptItem, err)
			}
			records = append(records, &record)
		}
		if len(result.LastEvaluatedKey) == 0 {
			break
		}
		input.ExclusiveStartKey = result.LastEvaluatedKey
	}
	return records, nil
}

// ListDeliveryIntents scans all pending delivery intents, following every page.
func (s *DynamoDBStore) ListDeliveryIntents(ctx context.Context, limit int) ([]*DeliveryIntent, error) {
	input := &dynamodb.ScanInput{
		TableName:        aws.String(s.tableName),
		FilterExpression: aws.String("SK = :sk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":sk": &types.AttributeValueMemberS{Value: deliveryOutboxSK},
		},
	}
	if s.pageLimit > 0 {
		input.Limit = aws.Int32(s.pageLimit)
	}

	intents := make([]*DeliveryIntent, 0)
	for {
		result, err := s.client.Scan(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("scan delivery outbox: %w", err)
		}
		for _, item := range result.Items {
			var intent DeliveryIntent
			if err := attributevalue.UnmarshalMap(item, &intent); err != nil {
				return nil, fmt.Errorf("%w: delivery intent: %w", ErrCorruptItem, err)
			}
			intents = append(intents, &intent)
			if limit > 0 && len(intents) >= limit {
				return intents, nil
			}
		}
		if len(result.LastEvaluatedKey) == 0 {
			break
		}
		input.ExclusiveStartKey = result.LastEvaluatedKey
	}
	return intents, nil
}

// CompleteDeliveryIntent retires an intent and its source due marker together.
func (s *DynamoDBStore) CompleteDeliveryIntent(ctx context.Context, intent *DeliveryIntent) error {
	if intent == nil {
		return nil
	}
	items := []types.TransactWriteItem{{Delete: &types.Delete{
		TableName: aws.String(s.tableName),
		Key:       deliveryIntentKey(intent.JobID, intent.Generation),
	}}}
	switch intent.SourceType {
	case "scheduled":
		items = append(items, types.TransactWriteItem{Delete: &types.Delete{TableName: aws.String(s.tableName), Key: scheduledKey(intent.JobID)}})
	case "retry":
		items = append(items, types.TransactWriteItem{Delete: &types.Delete{TableName: aws.String(s.tableName), Key: retryKey(intent.JobID)}})
	}

	if len(items) == 1 {
		_, err := s.client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
			TableName: aws.String(s.tableName),
			Key:       deliveryIntentKey(intent.JobID, intent.Generation),
		})
		return err
	}
	_, err := s.client.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{TransactItems: items})
	if err != nil {
		return fmt.Errorf("complete delivery intent: %w", mapTransactionError(err, -1))
	}
	return nil
}

// RecordDeliveryFailure retains the intent and records diagnostic retry metadata.
func (s *DynamoDBStore) RecordDeliveryFailure(ctx context.Context, intent *DeliveryIntent, message string) error {
	if intent == nil {
		return nil
	}
	_, err := s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:           aws.String(s.tableName),
		Key:                 deliveryIntentKey(intent.JobID, intent.Generation),
		UpdateExpression:    aws.String("SET last_error = :error, last_attempt_at = :now ADD attempts :one"),
		ConditionExpression: aws.String("attribute_exists(PK)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":error": &types.AttributeValueMemberS{Value: message},
			":now":   &types.AttributeValueMemberS{Value: core.NowFormatted()},
			":one":   &types.AttributeValueMemberN{Value: "1"},
		},
	})
	if err != nil {
		return fmt.Errorf("record delivery failure: %w", mapConditionalError(err))
	}
	return nil
}

// CreateWorkflowAtomic persists workflow metadata and all initial effects together.
func (s *DynamoDBStore) CreateWorkflowAtomic(ctx context.Context, plan *WorkflowCreatePlan) error {
	if plan == nil || plan.Workflow == nil {
		return fmt.Errorf("workflow create plan is required")
	}
	workflowPut, err := marshalTransactPut(plan.Workflow, s.tableName)
	if err != nil {
		return fmt.Errorf("marshal workflow: %w", err)
	}
	items := []types.TransactWriteItem{workflowPut}
	for _, effect := range plan.Effects {
		effectPut, marshalErr := marshalTransactPut(effect, s.tableName)
		if marshalErr != nil {
			return fmt.Errorf("marshal workflow effect: %w", marshalErr)
		}
		items = append(items, effectPut)
	}
	if len(items) > 100 {
		return fmt.Errorf("workflow has too many initial jobs: %d (maximum 99)", len(plan.Effects))
	}
	_, err = s.client.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{
		TransactItems:      items,
		ClientRequestToken: requestTokenFor("workflow-create", plan),
	})
	if err != nil {
		return fmt.Errorf("create workflow transaction: %w", mapTransactionError(err, -1))
	}
	return nil
}

// AdvanceWorkflowAtomic records one completion exactly once and updates counters/effects.
func (s *DynamoDBStore) AdvanceWorkflowAtomic(ctx context.Context, plan *WorkflowAdvancePlan) error {
	if plan == nil {
		return fmt.Errorf("workflow advance plan is required")
	}
	items := []types.TransactWriteItem{{Put: &types.Put{
		TableName: aws.String(s.tableName),
		Item: map[string]types.AttributeValue{
			"PK":          &types.AttributeValueMemberS{Value: plan.WorkflowID},
			"SK":          &types.AttributeValueMemberS{Value: "COMPLETION#" + plan.JobID},
			"job_id":      &types.AttributeValueMemberS{Value: plan.JobID},
			"step_idx":    &types.AttributeValueMemberN{Value: strconv.Itoa(plan.StepIndex)},
			"recorded_at": &types.AttributeValueMemberS{Value: core.NowFormatted()},
		},
		ConditionExpression: aws.String("attribute_not_exists(PK)"),
	}}}

	if plan.Result != "" {
		items = append(items, types.TransactWriteItem{Put: &types.Put{
			TableName: aws.String(s.tableName),
			Item: map[string]types.AttributeValue{
				"PK":       &types.AttributeValueMemberS{Value: plan.WorkflowID},
				"SK":       &types.AttributeValueMemberS{Value: fmt.Sprintf("RESULT#%d", plan.StepIndex)},
				"step_idx": &types.AttributeValueMemberN{Value: strconv.Itoa(plan.StepIndex)},
				"result":   &types.AttributeValueMemberS{Value: plan.Result},
			},
		}})
	}

	names := map[string]string{
		"#state":   "state",
		"#version": "version",
	}
	values := map[string]types.AttributeValue{
		":running":            &types.AttributeValueMemberS{Value: "running"},
		":state":              &types.AttributeValueMemberS{Value: plan.State},
		":expected_completed": &types.AttributeValueMemberN{Value: strconv.Itoa(plan.ExpectedCompleted)},
		":expected_failed":    &types.AttributeValueMemberN{Value: strconv.Itoa(plan.ExpectedFailed)},
		":completed":          &types.AttributeValueMemberN{Value: strconv.Itoa(plan.Completed)},
		":failed":             &types.AttributeValueMemberN{Value: strconv.Itoa(plan.Failed)},
		":current":            &types.AttributeValueMemberN{Value: strconv.Itoa(plan.CurrentStep)},
		":one":                &types.AttributeValueMemberN{Value: "1"},
	}
	condition := "#state = :running AND completed = :expected_completed AND failed = :expected_failed"
	if plan.MatchCurrentStep {
		values[":expected_current"] = &types.AttributeValueMemberN{Value: strconv.Itoa(plan.ExpectedCurrentStep)}
		condition += " AND (current_step = :expected_current OR (attribute_not_exists(current_step) AND :expected_current = :zero))"
		values[":zero"] = &types.AttributeValueMemberN{Value: "0"}
	}
	updateExpression := "SET completed = :completed, failed = :failed, current_step = :current, #state = :state"
	if plan.CompletedAt != "" {
		values[":completed_at"] = &types.AttributeValueMemberS{Value: plan.CompletedAt}
		updateExpression += ", completed_at = :completed_at"
	}
	updateExpression += " ADD #version :one"
	items = append(items, types.TransactWriteItem{Update: &types.Update{
		TableName:                 aws.String(s.tableName),
		Key:                       workflowKey(plan.WorkflowID),
		UpdateExpression:          aws.String(updateExpression),
		ConditionExpression:       aws.String(condition),
		ExpressionAttributeNames:  names,
		ExpressionAttributeValues: values,
	}})

	for _, effect := range plan.Effects {
		effectPut, err := marshalTransactPut(effect, s.tableName)
		if err != nil {
			return fmt.Errorf("marshal workflow effect: %w", err)
		}
		items = append(items, effectPut)
	}
	if len(items) > 100 {
		return fmt.Errorf("workflow advancement has too many effects: %d", len(plan.Effects))
	}

	_, err := s.client.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{
		TransactItems:      items,
		ClientRequestToken: requestTokenFor("workflow-advance", plan),
	})
	if err != nil {
		return fmt.Errorf("advance workflow transaction: %w", mapTransactionError(err, 0))
	}
	return nil
}

// IsWorkflowCompletionApplied checks the workflow completion deduplication record.
func (s *DynamoDBStore) IsWorkflowCompletionApplied(ctx context.Context, workflowID, jobID string) (bool, error) {
	result, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.tableName),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: workflowID},
			"SK": &types.AttributeValueMemberS{Value: "COMPLETION#" + jobID},
		},
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return false, fmt.Errorf("get workflow completion: %w", err)
	}
	return len(result.Item) > 0, nil
}

// ListJobEffects returns durable workflow and cron job effects across all scan pages.
func (s *DynamoDBStore) ListJobEffects(ctx context.Context, limit int) ([]*JobEffectRecord, error) {
	input := &dynamodb.ScanInput{
		TableName:        aws.String(s.tableName),
		FilterExpression: aws.String("SK = :sk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":sk": &types.AttributeValueMemberS{Value: jobEffectSK},
		},
	}
	if s.pageLimit > 0 {
		input.Limit = aws.Int32(s.pageLimit)
	}

	effects := make([]*JobEffectRecord, 0)
	for {
		result, err := s.client.Scan(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("scan job effects: %w", err)
		}
		for _, item := range result.Items {
			var effect JobEffectRecord
			if err := attributevalue.UnmarshalMap(item, &effect); err != nil {
				return nil, fmt.Errorf("%w: job effect: %w", ErrCorruptItem, err)
			}
			effects = append(effects, &effect)
			if limit > 0 && len(effects) >= limit {
				return effects, nil
			}
		}
		if len(result.LastEvaluatedKey) == 0 {
			break
		}
		input.ExclusiveStartKey = result.LastEvaluatedKey
	}
	return effects, nil
}

// CompleteJobEffect retires an effect after its job and delivery intent are durable.
func (s *DynamoDBStore) CompleteJobEffect(ctx context.Context, effect *JobEffectRecord) error {
	if effect == nil {
		return nil
	}
	_, err := s.client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(s.tableName),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: effect.PK},
			"SK": &types.AttributeValueMemberS{Value: jobEffectSK},
		},
	})
	if err != nil {
		return fmt.Errorf("complete job effect: %w", err)
	}
	return nil
}

// ListWorkflowAdvances returns durable terminal-job workflow accounting intents.
func (s *DynamoDBStore) ListWorkflowAdvances(ctx context.Context, limit int) ([]*WorkflowAdvanceIntent, error) {
	input := &dynamodb.ScanInput{
		TableName:        aws.String(s.tableName),
		FilterExpression: aws.String("SK = :sk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":sk": &types.AttributeValueMemberS{Value: workflowAdvanceSK},
		},
	}
	if s.pageLimit > 0 {
		input.Limit = aws.Int32(s.pageLimit)
	}
	var intents []*WorkflowAdvanceIntent
	for {
		result, err := s.client.Scan(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("scan workflow advancements: %w", err)
		}
		for _, item := range result.Items {
			var intent WorkflowAdvanceIntent
			if err := attributevalue.UnmarshalMap(item, &intent); err != nil {
				return nil, fmt.Errorf("%w: workflow advancement: %w", ErrCorruptItem, err)
			}
			intents = append(intents, &intent)
			if limit > 0 && len(intents) >= limit {
				return intents, nil
			}
		}
		if len(result.LastEvaluatedKey) == 0 {
			return intents, nil
		}
		input.ExclusiveStartKey = result.LastEvaluatedKey
	}
}

// CompleteWorkflowAdvance retires a workflow accounting intent after deduped application.
func (s *DynamoDBStore) CompleteWorkflowAdvance(ctx context.Context, intent *WorkflowAdvanceIntent) error {
	if intent == nil {
		return nil
	}
	_, err := s.client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(s.tableName),
		Key:       workflowAdvanceKey(intent.WorkflowID, intent.JobID),
	})
	if err != nil {
		return fmt.Errorf("complete workflow advancement: %w", err)
	}
	return nil
}

// CancelWorkflowAtomic fences future completions and effect materialization.
func (s *DynamoDBStore) CancelWorkflowAtomic(ctx context.Context, workflowID, completedAt string) error {
	_, err := s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.tableName),
		Key:       workflowKey(workflowID),
		UpdateExpression: aws.String(
			"SET #state = :cancelled, completed_at = :completed_at ADD #version :one",
		),
		ConditionExpression: aws.String("#state = :running"),
		ExpressionAttributeNames: map[string]string{
			"#state":   "state",
			"#version": "version",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":cancelled":    &types.AttributeValueMemberS{Value: "cancelled"},
			":completed_at": &types.AttributeValueMemberS{Value: completedAt},
			":running":      &types.AttributeValueMemberS{Value: "running"},
			":one":          &types.AttributeValueMemberN{Value: "1"},
		},
	})
	if err != nil {
		return fmt.Errorf("cancel workflow: %w", mapConditionalError(err))
	}
	return nil
}

// ClaimCronOccurrence advances one stored occurrence and persists its effect once.
func (s *DynamoDBStore) ClaimCronOccurrence(ctx context.Context, plan *CronOccurrencePlan) (bool, error) {
	if plan == nil {
		return false, fmt.Errorf("cron occurrence plan is required")
	}
	items := []types.TransactWriteItem{{Update: &types.Update{
		TableName: aws.String(s.tableName),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: "CRON#" + plan.Name},
			"SK": &types.AttributeValueMemberS{Value: "CRON"},
		},
		UpdateExpression:    aws.String("SET next_run_at = :next, last_run_at = :last"),
		ConditionExpression: aws.String("next_run_at = :expected AND enabled = :true"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":next":     &types.AttributeValueMemberS{Value: plan.NextRunAt},
			":last":     &types.AttributeValueMemberS{Value: plan.LastRunAt},
			":expected": &types.AttributeValueMemberS{Value: plan.ExpectedNextRunAt},
			":true":     &types.AttributeValueMemberBOOL{Value: true},
		},
	}}}
	items = append(items, types.TransactWriteItem{Put: &types.Put{
		TableName: aws.String(s.tableName),
		Item: map[string]types.AttributeValue{
			"PK":        &types.AttributeValueMemberS{Value: fmt.Sprintf("CRON_LOCK#%s#%d", plan.Name, plan.OccurrenceUnix)},
			"SK":        &types.AttributeValueMemberS{Value: "LOCK"},
			"timestamp": &types.AttributeValueMemberN{Value: strconv.FormatInt(plan.OccurrenceUnix, 10)},
			"ttl":       &types.AttributeValueMemberN{Value: strconv.FormatInt(time.Now().Unix()+3600, 10)},
		},
		ConditionExpression: aws.String("attribute_not_exists(PK)"),
	}})
	if plan.Effect != nil {
		effectPut, err := marshalTransactPut(plan.Effect, s.tableName)
		if err != nil {
			return false, fmt.Errorf("marshal cron effect: %w", err)
		}
		items = append(items, effectPut)
		items = append(items, types.TransactWriteItem{Put: &types.Put{
			TableName: aws.String(s.tableName),
			Item: map[string]types.AttributeValue{
				"PK":     &types.AttributeValueMemberS{Value: "CRON_INSTANCE#" + plan.Name},
				"SK":     &types.AttributeValueMemberS{Value: "INSTANCE"},
				"job_id": &types.AttributeValueMemberS{Value: plan.InstanceJobID},
			},
		}})
	}

	_, err := s.client.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{
		TransactItems:      items,
		ClientRequestToken: requestTokenFor("cron", plan),
	})
	if err != nil {
		mapped := mapTransactionError(err, -1)
		if errors.Is(mapped, ErrConditionFailed) {
			return false, nil
		}
		return false, fmt.Errorf("claim cron occurrence: %w", mapped)
	}
	return true, nil
}

// ConsumeWorkerDirective returns and removes a worker directive without replacing metadata.
func (s *DynamoDBStore) ConsumeWorkerDirective(ctx context.Context, workerID string) (string, error) {
	result, err := s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.tableName),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: "WORKER#" + workerID},
			"SK": &types.AttributeValueMemberS{Value: "WORKER"},
		},
		UpdateExpression: aws.String("REMOVE directive"),
		ReturnValues:     types.ReturnValueUpdatedOld,
	})
	if err != nil {
		return "", fmt.Errorf("consume worker directive: %w", err)
	}
	if directive, ok := result.Attributes["directive"].(*types.AttributeValueMemberS); ok {
		return directive.Value, nil
	}
	return "", nil
}
