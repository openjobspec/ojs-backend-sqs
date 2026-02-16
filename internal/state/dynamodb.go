package state

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// DynamoDBStore implements the Store interface using AWS DynamoDB.
// Single-table design with PK/SK pattern:
//   - Jobs: PK=jobID, SK="JOB"
//   - Workflows: PK=workflowID, SK="WORKFLOW"
//   - Crons: PK="CRON#<name>", SK="CRON"
//   - Unique keys: PK="UNIQUE#<fingerprint>", SK="UNIQUE"
//   - Queue metadata: PK="QUEUE#<name>", SK="META"
//   - Dead letter: PK="DLQ#<jobID>", SK="DLQ"
//   - Workers: PK="WORKER#<id>", SK="WORKER"
//   - Scheduled: PK="SCHEDULED#<jobID>", SK="SCHEDULED"
//   - Retry: PK="RETRY#<jobID>", SK="RETRY"
//   - Workflow jobs: PK=workflowID, SK="JOB#<jobID>"
//   - Workflow results: PK=workflowID, SK="RESULT#<stepIdx>"
//   - Cron instances: PK="CRON_INSTANCE#<name>", SK="INSTANCE"
//   - Cron locks: PK="CRON_LOCK#<name>#<timestamp>", SK="LOCK"
//
// GSI1: GSI1PK (QUEUE#<name>) + GSI1SK (STATE#<state>#<created_at>)
// GSI2: GSI2PK (STATE#<state>) + GSI2SK (<created_at>)
// GSI3: GSI3PK (DUE#scheduled|DUE#retry) + GSI3SK (<due_at_ms>)
type DynamoDBStore struct {
	client    *dynamodb.Client
	tableName string
}

// NewDynamoDBStore creates a new DynamoDB store.
func NewDynamoDBStore(client *dynamodb.Client, tableName string) *DynamoDBStore {
	return &DynamoDBStore{
		client:    client,
		tableName: tableName,
	}
}

// EnsureTable creates the table with GSIs if it doesn't exist.
func (s *DynamoDBStore) EnsureTable(ctx context.Context) error {
	// Check if table exists
	_, err := s.client.DescribeTable(ctx, &dynamodb.DescribeTableInput{
		TableName: aws.String(s.tableName),
	})
	if err == nil {
		// Table already exists
		return nil
	}

	// Create table
	_, err = s.client.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName: aws.String(s.tableName),
		AttributeDefinitions: []types.AttributeDefinition{
			{AttributeName: aws.String("PK"), AttributeType: types.ScalarAttributeTypeS},
			{AttributeName: aws.String("SK"), AttributeType: types.ScalarAttributeTypeS},
			{AttributeName: aws.String("GSI1PK"), AttributeType: types.ScalarAttributeTypeS},
			{AttributeName: aws.String("GSI1SK"), AttributeType: types.ScalarAttributeTypeS},
			{AttributeName: aws.String("GSI2PK"), AttributeType: types.ScalarAttributeTypeS},
			{AttributeName: aws.String("GSI2SK"), AttributeType: types.ScalarAttributeTypeS},
			{AttributeName: aws.String("GSI3PK"), AttributeType: types.ScalarAttributeTypeS},
			{AttributeName: aws.String("GSI3SK"), AttributeType: types.ScalarAttributeTypeN},
		},
		KeySchema: []types.KeySchemaElement{
			{AttributeName: aws.String("PK"), KeyType: types.KeyTypeHash},
			{AttributeName: aws.String("SK"), KeyType: types.KeyTypeRange},
		},
		GlobalSecondaryIndexes: []types.GlobalSecondaryIndex{
			{
				IndexName: aws.String("GSI1"),
				KeySchema: []types.KeySchemaElement{
					{AttributeName: aws.String("GSI1PK"), KeyType: types.KeyTypeHash},
					{AttributeName: aws.String("GSI1SK"), KeyType: types.KeyTypeRange},
				},
				Projection: &types.Projection{ProjectionType: types.ProjectionTypeAll},
				ProvisionedThroughput: &types.ProvisionedThroughput{
					ReadCapacityUnits:  aws.Int64(5),
					WriteCapacityUnits: aws.Int64(5),
				},
			},
			{
				IndexName: aws.String("GSI2"),
				KeySchema: []types.KeySchemaElement{
					{AttributeName: aws.String("GSI2PK"), KeyType: types.KeyTypeHash},
					{AttributeName: aws.String("GSI2SK"), KeyType: types.KeyTypeRange},
				},
				Projection: &types.Projection{ProjectionType: types.ProjectionTypeAll},
				ProvisionedThroughput: &types.ProvisionedThroughput{
					ReadCapacityUnits:  aws.Int64(5),
					WriteCapacityUnits: aws.Int64(5),
				},
			},
			{
				IndexName: aws.String("GSI3"),
				KeySchema: []types.KeySchemaElement{
					{AttributeName: aws.String("GSI3PK"), KeyType: types.KeyTypeHash},
					{AttributeName: aws.String("GSI3SK"), KeyType: types.KeyTypeRange},
				},
				Projection: &types.Projection{ProjectionType: types.ProjectionTypeAll},
				ProvisionedThroughput: &types.ProvisionedThroughput{
					ReadCapacityUnits:  aws.Int64(5),
					WriteCapacityUnits: aws.Int64(5),
				},
			},
		},
		BillingMode: types.BillingModeProvisioned,
		ProvisionedThroughput: &types.ProvisionedThroughput{
			ReadCapacityUnits:  aws.Int64(5),
			WriteCapacityUnits: aws.Int64(5),
		},
	})
	if err != nil {
		return fmt.Errorf("failed to create table: %w", err)
	}

	// Wait for table to be active
	waiter := dynamodb.NewTableExistsWaiter(s.client)
	if err := waiter.Wait(ctx, &dynamodb.DescribeTableInput{
		TableName: aws.String(s.tableName),
	}, 2*time.Minute); err != nil {
		return fmt.Errorf("failed waiting for table: %w", err)
	}

	// Enable TTL
	_, err = s.client.UpdateTimeToLive(ctx, &dynamodb.UpdateTimeToLiveInput{
		TableName: aws.String(s.tableName),
		TimeToLiveSpecification: &types.TimeToLiveSpecification{
			Enabled:       aws.Bool(true),
			AttributeName: aws.String("ttl"),
		},
	})
	if err != nil {
		return fmt.Errorf("failed to enable TTL: %w", err)
	}

	return nil
}

// PutJob stores a job record.
func (s *DynamoDBStore) PutJob(ctx context.Context, record *JobRecord) error {
	item, err := attributevalue.MarshalMap(record)
	if err != nil {
		return fmt.Errorf("failed to marshal job: %w", err)
	}

	_, err = s.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.tableName),
		Item:      item,
	})
	if err != nil {
		return fmt.Errorf("failed to put job: %w", err)
	}

	return nil
}

// GetJob retrieves a job by ID.
func (s *DynamoDBStore) GetJob(ctx context.Context, jobID string) (*JobRecord, error) {
	result, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.tableName),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: jobID},
			"SK": &types.AttributeValueMemberS{Value: "JOB"},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get job: %w", err)
	}

	if result.Item == nil {
		return nil, fmt.Errorf("job not found: %s", jobID)
	}

	var record JobRecord
	if err := attributevalue.UnmarshalMap(result.Item, &record); err != nil {
		return nil, fmt.Errorf("failed to unmarshal job: %w", err)
	}

	return &record, nil
}

// UpdateJobState updates a job's state and additional fields.
func (s *DynamoDBStore) UpdateJobState(ctx context.Context, jobID, newState string, updates map[string]any) error {
	updateExpr := "SET #state = :state"
	exprAttrNames := map[string]string{
		"#state": "state",
	}
	exprAttrValues := map[string]types.AttributeValue{
		":state": &types.AttributeValueMemberS{Value: newState},
	}

	// Apply additional updates
	for key, value := range updates {
		placeholder := fmt.Sprintf(":val%d", len(exprAttrValues))
		attrName := fmt.Sprintf("#attr%d", len(exprAttrNames))
		updateExpr += fmt.Sprintf(", %s = %s", attrName, placeholder)
		exprAttrNames[attrName] = key

		av, err := attributevalue.Marshal(value)
		if err != nil {
			return fmt.Errorf("failed to marshal update value for %s: %w", key, err)
		}
		exprAttrValues[placeholder] = av
	}

	// Update GSI attributes for state change
	job, err := s.GetJob(ctx, jobID)
	if err != nil {
		return fmt.Errorf("failed to get job for GSI update: %w", err)
	}

	gsi1sk := fmt.Sprintf("STATE#%s#%s", newState, job.CreatedAt)
	gsi2pk := fmt.Sprintf("STATE#%s", newState)

	updateExpr += ", GSI1SK = :gsi1sk, GSI2PK = :gsi2pk"
	exprAttrValues[":gsi1sk"] = &types.AttributeValueMemberS{Value: gsi1sk}
	exprAttrValues[":gsi2pk"] = &types.AttributeValueMemberS{Value: gsi2pk}

	_, err = s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.tableName),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: jobID},
			"SK": &types.AttributeValueMemberS{Value: "JOB"},
		},
		UpdateExpression:          aws.String(updateExpr),
		ExpressionAttributeNames:  exprAttrNames,
		ExpressionAttributeValues: exprAttrValues,
	})
	if err != nil {
		return fmt.Errorf("failed to update job state: %w", err)
	}

	return nil
}

// DeleteJob removes a job.
func (s *DynamoDBStore) DeleteJob(ctx context.Context, jobID string) error {
	_, err := s.client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(s.tableName),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: jobID},
			"SK": &types.AttributeValueMemberS{Value: "JOB"},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to delete job: %w", err)
	}

	return nil
}

// ListJobsByQueue returns jobs in a queue with a specific state.
func (s *DynamoDBStore) ListJobsByQueue(ctx context.Context, queue, state string, limit int) ([]*JobRecord, error) {
	gsi1pk := fmt.Sprintf("QUEUE#%s", queue)
	gsi1skPrefix := fmt.Sprintf("STATE#%s#", state)

	queryInput := &dynamodb.QueryInput{
		TableName:              aws.String(s.tableName),
		IndexName:              aws.String("GSI1"),
		KeyConditionExpression: aws.String("GSI1PK = :pk AND begins_with(GSI1SK, :sk)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk": &types.AttributeValueMemberS{Value: gsi1pk},
			":sk": &types.AttributeValueMemberS{Value: gsi1skPrefix},
		},
		Limit: aws.Int32(int32(limit)),
	}

	result, err := s.client.Query(ctx, queryInput)
	if err != nil {
		return nil, fmt.Errorf("failed to query jobs by queue: %w", err)
	}

	jobs := make([]*JobRecord, 0, len(result.Items))
	for _, item := range result.Items {
		var job JobRecord
		if err := attributevalue.UnmarshalMap(item, &job); err != nil {
			return nil, fmt.Errorf("failed to unmarshal job: %w", err)
		}
		jobs = append(jobs, &job)
	}

	return jobs, nil
}

// ListJobsByState returns jobs with a specific state (paginated).
func (s *DynamoDBStore) ListJobsByState(ctx context.Context, state string, limit, offset int) ([]*JobRecord, int, error) {
	gsi2pk := fmt.Sprintf("STATE#%s", state)

	// First, get total count
	countInput := &dynamodb.QueryInput{
		TableName:              aws.String(s.tableName),
		IndexName:              aws.String("GSI2"),
		KeyConditionExpression: aws.String("GSI2PK = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk": &types.AttributeValueMemberS{Value: gsi2pk},
		},
		Select: types.SelectCount,
	}

	countResult, err := s.client.Query(ctx, countInput)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to count jobs by state: %w", err)
	}

	total := int(countResult.Count)

	// Now get the actual items with pagination
	queryInput := &dynamodb.QueryInput{
		TableName:              aws.String(s.tableName),
		IndexName:              aws.String("GSI2"),
		KeyConditionExpression: aws.String("GSI2PK = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk": &types.AttributeValueMemberS{Value: gsi2pk},
		},
		Limit: aws.Int32(int32(limit + offset)),
	}

	result, err := s.client.Query(ctx, queryInput)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to query jobs by state: %w", err)
	}

	// Apply offset manually (DynamoDB doesn't support offset directly)
	items := result.Items
	if offset >= len(items) {
		return []*JobRecord{}, total, nil
	}
	if offset+limit > len(items) {
		items = items[offset:]
	} else {
		items = items[offset : offset+limit]
	}

	jobs := make([]*JobRecord, 0, len(items))
	for _, item := range items {
		var job JobRecord
		if err := attributevalue.UnmarshalMap(item, &job); err != nil {
			return nil, 0, fmt.Errorf("failed to unmarshal job: %w", err)
		}
		jobs = append(jobs, &job)
	}

	return jobs, total, nil
}

// CountJobsByQueueAndState counts jobs in a queue with a specific state.
func (s *DynamoDBStore) CountJobsByQueueAndState(ctx context.Context, queue, state string) (int, error) {
	gsi1pk := fmt.Sprintf("QUEUE#%s", queue)
	gsi1skPrefix := fmt.Sprintf("STATE#%s#", state)

	result, err := s.client.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.tableName),
		IndexName:              aws.String("GSI1"),
		KeyConditionExpression: aws.String("GSI1PK = :pk AND begins_with(GSI1SK, :sk)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk": &types.AttributeValueMemberS{Value: gsi1pk},
			":sk": &types.AttributeValueMemberS{Value: gsi1skPrefix},
		},
		Select: types.SelectCount,
	})
	if err != nil {
		return 0, fmt.Errorf("failed to count jobs: %w", err)
	}

	return int(result.Count), nil
}

// RegisterQueue creates a queue metadata record.
func (s *DynamoDBStore) RegisterQueue(ctx context.Context, name string) error {
	pk := fmt.Sprintf("QUEUE#%s", name)

	_, err := s.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.tableName),
		Item: map[string]types.AttributeValue{
			"PK":        &types.AttributeValueMemberS{Value: pk},
			"SK":        &types.AttributeValueMemberS{Value: "META"},
			"name":      &types.AttributeValueMemberS{Value: name},
			"paused":    &types.AttributeValueMemberBOOL{Value: false},
			"completed": &types.AttributeValueMemberN{Value: "0"},
		},
		ConditionExpression: aws.String("attribute_not_exists(PK)"),
	})
	if err != nil {
		if strings.Contains(err.Error(), "ConditionalCheckFailedException") {
			// Queue already exists, which is fine
			return nil
		}
		return fmt.Errorf("failed to register queue: %w", err)
	}

	return nil
}

// ListQueues returns all registered queue names.
func (s *DynamoDBStore) ListQueues(ctx context.Context) ([]string, error) {
	result, err := s.client.Scan(ctx, &dynamodb.ScanInput{
		TableName:        aws.String(s.tableName),
		FilterExpression: aws.String("begins_with(PK, :prefix) AND SK = :sk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":prefix": &types.AttributeValueMemberS{Value: "QUEUE#"},
			":sk":     &types.AttributeValueMemberS{Value: "META"},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list queues: %w", err)
	}

	queues := make([]string, 0, len(result.Items))
	for _, item := range result.Items {
		if nameAttr, ok := item["name"]; ok {
			if nameVal, ok := nameAttr.(*types.AttributeValueMemberS); ok {
				queues = append(queues, nameVal.Value)
			}
		}
	}

	return queues, nil
}

// SetQueuePaused sets the paused status of a queue.
func (s *DynamoDBStore) SetQueuePaused(ctx context.Context, name string, paused bool) error {
	pk := fmt.Sprintf("QUEUE#%s", name)

	_, err := s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.tableName),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: pk},
			"SK": &types.AttributeValueMemberS{Value: "META"},
		},
		UpdateExpression: aws.String("SET paused = :paused"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":paused": &types.AttributeValueMemberBOOL{Value: paused},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to set queue paused: %w", err)
	}

	return nil
}

// IsQueuePaused checks if a queue is paused.
func (s *DynamoDBStore) IsQueuePaused(ctx context.Context, name string) (bool, error) {
	pk := fmt.Sprintf("QUEUE#%s", name)

	result, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.tableName),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: pk},
			"SK": &types.AttributeValueMemberS{Value: "META"},
		},
	})
	if err != nil {
		return false, fmt.Errorf("failed to get queue: %w", err)
	}

	if result.Item == nil {
		return false, fmt.Errorf("queue not found: %s", name)
	}

	if pausedAttr, ok := result.Item["paused"]; ok {
		if pausedVal, ok := pausedAttr.(*types.AttributeValueMemberBOOL); ok {
			return pausedVal.Value, nil
		}
	}

	return false, nil
}

// IncrementQueueCompleted increments the completed count for a queue.
func (s *DynamoDBStore) IncrementQueueCompleted(ctx context.Context, name string) error {
	pk := fmt.Sprintf("QUEUE#%s", name)

	_, err := s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.tableName),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: pk},
			"SK": &types.AttributeValueMemberS{Value: "META"},
		},
		UpdateExpression: aws.String("ADD completed :inc"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":inc": &types.AttributeValueMemberN{Value: "1"},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to increment queue completed: %w", err)
	}

	return nil
}

// GetQueueCompletedCount gets the completed count for a queue.
func (s *DynamoDBStore) GetQueueCompletedCount(ctx context.Context, name string) (int, error) {
	pk := fmt.Sprintf("QUEUE#%s", name)

	result, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.tableName),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: pk},
			"SK": &types.AttributeValueMemberS{Value: "META"},
		},
	})
	if err != nil {
		return 0, fmt.Errorf("failed to get queue: %w", err)
	}

	if result.Item == nil {
		return 0, nil
	}

	if completedAttr, ok := result.Item["completed"]; ok {
		if completedVal, ok := completedAttr.(*types.AttributeValueMemberN); ok {
			count, _ := strconv.Atoi(completedVal.Value)
			return count, nil
		}
	}

	return 0, nil
}

// SetUniqueKey stores a unique key mapping with TTL.
func (s *DynamoDBStore) SetUniqueKey(ctx context.Context, fingerprint, jobID string, ttlSeconds int64) error {
	pk := fmt.Sprintf("UNIQUE#%s", fingerprint)
	ttl := time.Now().Unix() + ttlSeconds

	_, err := s.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.tableName),
		Item: map[string]types.AttributeValue{
			"PK":     &types.AttributeValueMemberS{Value: pk},
			"SK":     &types.AttributeValueMemberS{Value: "UNIQUE"},
			"job_id": &types.AttributeValueMemberS{Value: jobID},
			"ttl":    &types.AttributeValueMemberN{Value: strconv.FormatInt(ttl, 10)},
		},
		ConditionExpression: aws.String("attribute_not_exists(PK)"),
	})
	if err != nil {
		return fmt.Errorf("failed to set unique key: %w", err)
	}

	return nil
}

// GetUniqueKey retrieves the job ID for a unique fingerprint.
func (s *DynamoDBStore) GetUniqueKey(ctx context.Context, fingerprint string) (string, error) {
	pk := fmt.Sprintf("UNIQUE#%s", fingerprint)

	result, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.tableName),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: pk},
			"SK": &types.AttributeValueMemberS{Value: "UNIQUE"},
		},
	})
	if err != nil {
		return "", fmt.Errorf("failed to get unique key: %w", err)
	}

	if result.Item == nil {
		return "", fmt.Errorf("unique key not found")
	}

	if jobIDAttr, ok := result.Item["job_id"]; ok {
		if jobIDVal, ok := jobIDAttr.(*types.AttributeValueMemberS); ok {
			return jobIDVal.Value, nil
		}
	}

	return "", fmt.Errorf("job_id not found in unique key record")
}

// DeleteUniqueKey removes a unique key.
func (s *DynamoDBStore) DeleteUniqueKey(ctx context.Context, fingerprint string) error {
	pk := fmt.Sprintf("UNIQUE#%s", fingerprint)

	_, err := s.client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(s.tableName),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: pk},
			"SK": &types.AttributeValueMemberS{Value: "UNIQUE"},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to delete unique key: %w", err)
	}

	return nil
}

// PutWorkflow stores a workflow record.
func (s *DynamoDBStore) PutWorkflow(ctx context.Context, wf *WorkflowRecord) error {
	item, err := attributevalue.MarshalMap(wf)
	if err != nil {
		return fmt.Errorf("failed to marshal workflow: %w", err)
	}

	_, err = s.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.tableName),
		Item:      item,
	})
	if err != nil {
		return fmt.Errorf("failed to put workflow: %w", err)
	}

	return nil
}

// GetWorkflow retrieves a workflow by ID.
func (s *DynamoDBStore) GetWorkflow(ctx context.Context, id string) (*WorkflowRecord, error) {
	result, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.tableName),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: id},
			"SK": &types.AttributeValueMemberS{Value: "WORKFLOW"},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get workflow: %w", err)
	}

	if result.Item == nil {
		return nil, fmt.Errorf("workflow not found: %s", id)
	}

	var wf WorkflowRecord
	if err := attributevalue.UnmarshalMap(result.Item, &wf); err != nil {
		return nil, fmt.Errorf("failed to unmarshal workflow: %w", err)
	}

	return &wf, nil
}

// UpdateWorkflow updates workflow fields.
func (s *DynamoDBStore) UpdateWorkflow(ctx context.Context, id string, updates map[string]any) error {
	if len(updates) == 0 {
		return nil
	}

	updateExpr := "SET"
	exprAttrNames := make(map[string]string)
	exprAttrValues := make(map[string]types.AttributeValue)

	first := true
	for key, value := range updates {
		if !first {
			updateExpr += ","
		}
		first = false

		attrName := fmt.Sprintf("#attr%d", len(exprAttrNames))
		placeholder := fmt.Sprintf(":val%d", len(exprAttrValues))
		updateExpr += fmt.Sprintf(" %s = %s", attrName, placeholder)
		exprAttrNames[attrName] = key

		av, err := attributevalue.Marshal(value)
		if err != nil {
			return fmt.Errorf("failed to marshal update value for %s: %w", key, err)
		}
		exprAttrValues[placeholder] = av
	}

	_, err := s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.tableName),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: id},
			"SK": &types.AttributeValueMemberS{Value: "WORKFLOW"},
		},
		UpdateExpression:          aws.String(updateExpr),
		ExpressionAttributeNames:  exprAttrNames,
		ExpressionAttributeValues: exprAttrValues,
	})
	if err != nil {
		return fmt.Errorf("failed to update workflow: %w", err)
	}

	return nil
}

// AddWorkflowJob adds a job to a workflow's job list.
func (s *DynamoDBStore) AddWorkflowJob(ctx context.Context, workflowID, jobID string) error {
	sk := fmt.Sprintf("JOB#%s", jobID)

	_, err := s.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.tableName),
		Item: map[string]types.AttributeValue{
			"PK":     &types.AttributeValueMemberS{Value: workflowID},
			"SK":     &types.AttributeValueMemberS{Value: sk},
			"job_id": &types.AttributeValueMemberS{Value: jobID},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add workflow job: %w", err)
	}

	return nil
}

// GetWorkflowJobs returns all job IDs for a workflow.
func (s *DynamoDBStore) GetWorkflowJobs(ctx context.Context, workflowID string) ([]string, error) {
	result, err := s.client.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.tableName),
		KeyConditionExpression: aws.String("PK = :pk AND begins_with(SK, :sk)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk": &types.AttributeValueMemberS{Value: workflowID},
			":sk": &types.AttributeValueMemberS{Value: "JOB#"},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get workflow jobs: %w", err)
	}

	jobIDs := make([]string, 0, len(result.Items))
	for _, item := range result.Items {
		if jobIDAttr, ok := item["job_id"]; ok {
			if jobIDVal, ok := jobIDAttr.(*types.AttributeValueMemberS); ok {
				jobIDs = append(jobIDs, jobIDVal.Value)
			}
		}
	}

	return jobIDs, nil
}

// SetWorkflowResult stores a workflow step result.
func (s *DynamoDBStore) SetWorkflowResult(ctx context.Context, workflowID string, stepIdx int, result string) error {
	sk := fmt.Sprintf("RESULT#%d", stepIdx)

	_, err := s.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.tableName),
		Item: map[string]types.AttributeValue{
			"PK":       &types.AttributeValueMemberS{Value: workflowID},
			"SK":       &types.AttributeValueMemberS{Value: sk},
			"step_idx": &types.AttributeValueMemberN{Value: strconv.Itoa(stepIdx)},
			"result":   &types.AttributeValueMemberS{Value: result},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to set workflow result: %w", err)
	}

	return nil
}

// GetWorkflowResults retrieves all step results for a workflow.
func (s *DynamoDBStore) GetWorkflowResults(ctx context.Context, workflowID string) (map[int]string, error) {
	result, err := s.client.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.tableName),
		KeyConditionExpression: aws.String("PK = :pk AND begins_with(SK, :sk)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk": &types.AttributeValueMemberS{Value: workflowID},
			":sk": &types.AttributeValueMemberS{Value: "RESULT#"},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get workflow results: %w", err)
	}

	results := make(map[int]string)
	for _, item := range result.Items {
		var stepIdx int
		var resultStr string

		if idxAttr, ok := item["step_idx"]; ok {
			if idxVal, ok := idxAttr.(*types.AttributeValueMemberN); ok {
				stepIdx, _ = strconv.Atoi(idxVal.Value)
			}
		}

		if resultAttr, ok := item["result"]; ok {
			if resultVal, ok := resultAttr.(*types.AttributeValueMemberS); ok {
				resultStr = resultVal.Value
			}
		}

		results[stepIdx] = resultStr
	}

	return results, nil
}

// PutCron stores a cron record.
func (s *DynamoDBStore) PutCron(ctx context.Context, cron *CronRecord) error {
	cron.PK = fmt.Sprintf("CRON#%s", cron.Name)
	cron.SK = "CRON"

	item, err := attributevalue.MarshalMap(cron)
	if err != nil {
		return fmt.Errorf("failed to marshal cron: %w", err)
	}

	_, err = s.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.tableName),
		Item:      item,
	})
	if err != nil {
		return fmt.Errorf("failed to put cron: %w", err)
	}

	return nil
}

// GetCron retrieves a cron by name.
func (s *DynamoDBStore) GetCron(ctx context.Context, name string) (*CronRecord, error) {
	pk := fmt.Sprintf("CRON#%s", name)

	result, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.tableName),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: pk},
			"SK": &types.AttributeValueMemberS{Value: "CRON"},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get cron: %w", err)
	}

	if result.Item == nil {
		return nil, fmt.Errorf("cron not found: %s", name)
	}

	var cron CronRecord
	if err := attributevalue.UnmarshalMap(result.Item, &cron); err != nil {
		return nil, fmt.Errorf("failed to unmarshal cron: %w", err)
	}

	return &cron, nil
}

// DeleteCron removes a cron.
func (s *DynamoDBStore) DeleteCron(ctx context.Context, name string) error {
	pk := fmt.Sprintf("CRON#%s", name)

	_, err := s.client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(s.tableName),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: pk},
			"SK": &types.AttributeValueMemberS{Value: "CRON"},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to delete cron: %w", err)
	}

	return nil
}

// ListCrons returns all cron records.
func (s *DynamoDBStore) ListCrons(ctx context.Context) ([]*CronRecord, error) {
	result, err := s.client.Scan(ctx, &dynamodb.ScanInput{
		TableName:        aws.String(s.tableName),
		FilterExpression: aws.String("begins_with(PK, :prefix) AND SK = :sk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":prefix": &types.AttributeValueMemberS{Value: "CRON#"},
			":sk":     &types.AttributeValueMemberS{Value: "CRON"},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list crons: %w", err)
	}

	crons := make([]*CronRecord, 0, len(result.Items))
	for _, item := range result.Items {
		var cron CronRecord
		if err := attributevalue.UnmarshalMap(item, &cron); err != nil {
			return nil, fmt.Errorf("failed to unmarshal cron: %w", err)
		}
		crons = append(crons, &cron)
	}

	return crons, nil
}

// AcquireCronLock attempts to acquire a distributed lock for cron execution.
func (s *DynamoDBStore) AcquireCronLock(ctx context.Context, name string, timestamp int64) (bool, error) {
	pk := fmt.Sprintf("CRON_LOCK#%s#%d", name, timestamp)

	_, err := s.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.tableName),
		Item: map[string]types.AttributeValue{
			"PK":        &types.AttributeValueMemberS{Value: pk},
			"SK":        &types.AttributeValueMemberS{Value: "LOCK"},
			"timestamp": &types.AttributeValueMemberN{Value: strconv.FormatInt(timestamp, 10)},
			"ttl":       &types.AttributeValueMemberN{Value: strconv.FormatInt(time.Now().Unix()+3600, 10)},
		},
		ConditionExpression: aws.String("attribute_not_exists(PK)"),
	})
	if err != nil {
		if strings.Contains(err.Error(), "ConditionalCheckFailedException") {
			return false, nil
		}
		return false, fmt.Errorf("failed to acquire cron lock: %w", err)
	}

	return true, nil
}

// GetCronInstance retrieves the current job ID for a cron instance.
func (s *DynamoDBStore) GetCronInstance(ctx context.Context, name string) (string, error) {
	pk := fmt.Sprintf("CRON_INSTANCE#%s", name)

	result, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.tableName),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: pk},
			"SK": &types.AttributeValueMemberS{Value: "INSTANCE"},
		},
	})
	if err != nil {
		return "", fmt.Errorf("failed to get cron instance: %w", err)
	}

	if result.Item == nil {
		return "", nil
	}

	if jobIDAttr, ok := result.Item["job_id"]; ok {
		if jobIDVal, ok := jobIDAttr.(*types.AttributeValueMemberS); ok {
			return jobIDVal.Value, nil
		}
	}

	return "", nil
}

// SetCronInstance stores the current job ID for a cron instance.
func (s *DynamoDBStore) SetCronInstance(ctx context.Context, name, jobID string) error {
	pk := fmt.Sprintf("CRON_INSTANCE#%s", name)

	_, err := s.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.tableName),
		Item: map[string]types.AttributeValue{
			"PK":     &types.AttributeValueMemberS{Value: pk},
			"SK":     &types.AttributeValueMemberS{Value: "INSTANCE"},
			"job_id": &types.AttributeValueMemberS{Value: jobID},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to set cron instance: %w", err)
	}

	return nil
}

// PutWorker stores worker metadata.
func (s *DynamoDBStore) PutWorker(ctx context.Context, workerID string, data map[string]string) error {
	pk := fmt.Sprintf("WORKER#%s", workerID)

	item := map[string]types.AttributeValue{
		"PK": &types.AttributeValueMemberS{Value: pk},
		"SK": &types.AttributeValueMemberS{Value: "WORKER"},
	}

	for key, value := range data {
		item[key] = &types.AttributeValueMemberS{Value: value}
	}

	_, err := s.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.tableName),
		Item:      item,
	})
	if err != nil {
		return fmt.Errorf("failed to put worker: %w", err)
	}

	return nil
}

// GetWorkerDirective retrieves the directive for a worker.
func (s *DynamoDBStore) GetWorkerDirective(ctx context.Context, workerID string) (string, error) {
	pk := fmt.Sprintf("WORKER#%s", workerID)

	result, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.tableName),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: pk},
			"SK": &types.AttributeValueMemberS{Value: "WORKER"},
		},
	})
	if err != nil {
		return "", fmt.Errorf("failed to get worker: %w", err)
	}

	if result.Item == nil {
		return "", nil
	}

	if directiveAttr, ok := result.Item["directive"]; ok {
		if directiveVal, ok := directiveAttr.(*types.AttributeValueMemberS); ok {
			return directiveVal.Value, nil
		}
	}

	return "", nil
}

// SetWorkerDirective sets the directive for a worker.
func (s *DynamoDBStore) SetWorkerDirective(ctx context.Context, workerID, directive string) error {
	pk := fmt.Sprintf("WORKER#%s", workerID)

	_, err := s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.tableName),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: pk},
			"SK": &types.AttributeValueMemberS{Value: "WORKER"},
		},
		UpdateExpression: aws.String("SET directive = :directive"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":directive": &types.AttributeValueMemberS{Value: directive},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to set worker directive: %w", err)
	}

	return nil
}

// AddToDeadLetter adds a job to the dead letter queue.
func (s *DynamoDBStore) AddToDeadLetter(ctx context.Context, jobID string) error {
	pk := fmt.Sprintf("DLQ#%s", jobID)

	_, err := s.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.tableName),
		Item: map[string]types.AttributeValue{
			"PK":       &types.AttributeValueMemberS{Value: pk},
			"SK":       &types.AttributeValueMemberS{Value: "DLQ"},
			"job_id":   &types.AttributeValueMemberS{Value: jobID},
			"added_at": &types.AttributeValueMemberS{Value: time.Now().UTC().Format(time.RFC3339)},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add to dead letter: %w", err)
	}

	return nil
}

// RemoveFromDeadLetter removes a job from the dead letter queue.
func (s *DynamoDBStore) RemoveFromDeadLetter(ctx context.Context, jobID string) error {
	pk := fmt.Sprintf("DLQ#%s", jobID)

	_, err := s.client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(s.tableName),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: pk},
			"SK": &types.AttributeValueMemberS{Value: "DLQ"},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to remove from dead letter: %w", err)
	}

	return nil
}

// IsInDeadLetter checks if a job is in the dead letter queue.
func (s *DynamoDBStore) IsInDeadLetter(ctx context.Context, jobID string) (bool, error) {
	pk := fmt.Sprintf("DLQ#%s", jobID)

	result, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.tableName),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: pk},
			"SK": &types.AttributeValueMemberS{Value: "DLQ"},
		},
	})
	if err != nil {
		return false, fmt.Errorf("failed to check dead letter: %w", err)
	}

	return result.Item != nil, nil
}

// AddScheduledJob adds a job to the scheduled index.
func (s *DynamoDBStore) AddScheduledJob(ctx context.Context, jobID string, scheduledAtMs int64) error {
	pk := fmt.Sprintf("SCHEDULED#%s", jobID)

	_, err := s.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.tableName),
		Item: map[string]types.AttributeValue{
			"PK":              &types.AttributeValueMemberS{Value: pk},
			"SK":              &types.AttributeValueMemberS{Value: "SCHEDULED"},
			"job_id":          &types.AttributeValueMemberS{Value: jobID},
			"scheduled_at_ms": &types.AttributeValueMemberN{Value: strconv.FormatInt(scheduledAtMs, 10)},
			"GSI3PK":          &types.AttributeValueMemberS{Value: "DUE#scheduled"},
			"GSI3SK":          &types.AttributeValueMemberN{Value: strconv.FormatInt(scheduledAtMs, 10)},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add scheduled job: %w", err)
	}

	return nil
}

// GetDueScheduledJobs returns scheduled jobs that are due.
func (s *DynamoDBStore) GetDueScheduledJobs(ctx context.Context, nowMs int64) ([]string, error) {
	jobIDs, err := s.queryDueJobs(ctx, "DUE#scheduled", nowMs)
	if err == nil {
		return jobIDs, nil
	}
	if !isMissingDueIndexError(err) {
		return nil, fmt.Errorf("failed to query due scheduled jobs: %w", err)
	}

	// Compatibility fallback for tables without GSI3.
	result, err := s.client.Scan(ctx, &dynamodb.ScanInput{
		TableName:        aws.String(s.tableName),
		FilterExpression: aws.String("begins_with(PK, :prefix) AND SK = :sk AND scheduled_at_ms <= :now"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":prefix": &types.AttributeValueMemberS{Value: "SCHEDULED#"},
			":sk":     &types.AttributeValueMemberS{Value: "SCHEDULED"},
			":now":    &types.AttributeValueMemberN{Value: strconv.FormatInt(nowMs, 10)},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get due scheduled jobs: %w", err)
	}

	jobIDs = make([]string, 0, len(result.Items))
	for _, item := range result.Items {
		if jobIDAttr, ok := item["job_id"]; ok {
			if jobIDVal, ok := jobIDAttr.(*types.AttributeValueMemberS); ok {
				jobIDs = append(jobIDs, jobIDVal.Value)
			}
		}
	}

	return jobIDs, nil
}

// RemoveScheduledJob removes a job from the scheduled index.
func (s *DynamoDBStore) RemoveScheduledJob(ctx context.Context, jobID string) error {
	pk := fmt.Sprintf("SCHEDULED#%s", jobID)

	_, err := s.client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(s.tableName),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: pk},
			"SK": &types.AttributeValueMemberS{Value: "SCHEDULED"},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to remove scheduled job: %w", err)
	}

	return nil
}

// AddRetryJob adds a job to the retry index.
func (s *DynamoDBStore) AddRetryJob(ctx context.Context, jobID string, retryAtMs int64) error {
	pk := fmt.Sprintf("RETRY#%s", jobID)

	_, err := s.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.tableName),
		Item: map[string]types.AttributeValue{
			"PK":          &types.AttributeValueMemberS{Value: pk},
			"SK":          &types.AttributeValueMemberS{Value: "RETRY"},
			"job_id":      &types.AttributeValueMemberS{Value: jobID},
			"retry_at_ms": &types.AttributeValueMemberN{Value: strconv.FormatInt(retryAtMs, 10)},
			"GSI3PK":      &types.AttributeValueMemberS{Value: "DUE#retry"},
			"GSI3SK":      &types.AttributeValueMemberN{Value: strconv.FormatInt(retryAtMs, 10)},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add retry job: %w", err)
	}

	return nil
}

// GetDueRetryJobs returns retry jobs that are due.
func (s *DynamoDBStore) GetDueRetryJobs(ctx context.Context, nowMs int64) ([]string, error) {
	jobIDs, err := s.queryDueJobs(ctx, "DUE#retry", nowMs)
	if err == nil {
		return jobIDs, nil
	}
	if !isMissingDueIndexError(err) {
		return nil, fmt.Errorf("failed to query due retry jobs: %w", err)
	}

	// Compatibility fallback for tables without GSI3.
	result, err := s.client.Scan(ctx, &dynamodb.ScanInput{
		TableName:        aws.String(s.tableName),
		FilterExpression: aws.String("begins_with(PK, :prefix) AND SK = :sk AND retry_at_ms <= :now"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":prefix": &types.AttributeValueMemberS{Value: "RETRY#"},
			":sk":     &types.AttributeValueMemberS{Value: "RETRY"},
			":now":    &types.AttributeValueMemberN{Value: strconv.FormatInt(nowMs, 10)},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get due retry jobs: %w", err)
	}

	jobIDs = make([]string, 0, len(result.Items))
	for _, item := range result.Items {
		if jobIDAttr, ok := item["job_id"]; ok {
			if jobIDVal, ok := jobIDAttr.(*types.AttributeValueMemberS); ok {
				jobIDs = append(jobIDs, jobIDVal.Value)
			}
		}
	}

	return jobIDs, nil
}

func (s *DynamoDBStore) queryDueJobs(ctx context.Context, dueType string, nowMs int64) ([]string, error) {
	result, err := s.client.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.tableName),
		IndexName:              aws.String("GSI3"),
		KeyConditionExpression: aws.String("GSI3PK = :pk AND GSI3SK <= :now"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk":  &types.AttributeValueMemberS{Value: dueType},
			":now": &types.AttributeValueMemberN{Value: strconv.FormatInt(nowMs, 10)},
		},
	})
	if err != nil {
		return nil, err
	}

	jobIDs := make([]string, 0, len(result.Items))
	for _, item := range result.Items {
		if jobIDAttr, ok := item["job_id"]; ok {
			if jobIDVal, ok := jobIDAttr.(*types.AttributeValueMemberS); ok {
				jobIDs = append(jobIDs, jobIDVal.Value)
			}
		}
	}

	return jobIDs, nil
}

func isMissingDueIndexError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "The table does not have the specified index") ||
		strings.Contains(msg, "Cannot read from backfilling global secondary index")
}

// RemoveRetryJob removes a job from the retry index.
func (s *DynamoDBStore) RemoveRetryJob(ctx context.Context, jobID string) error {
	pk := fmt.Sprintf("RETRY#%s", jobID)

	_, err := s.client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(s.tableName),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: pk},
			"SK": &types.AttributeValueMemberS{Value: "RETRY"},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to remove retry job: %w", err)
	}

	return nil
}

// Ping checks the connection to DynamoDB.
func (s *DynamoDBStore) Ping(ctx context.Context) error {
	_, err := s.client.DescribeTable(ctx, &dynamodb.DescribeTableInput{
		TableName: aws.String(s.tableName),
	})
	if err != nil {
		return fmt.Errorf("failed to ping DynamoDB: %w", err)
	}

	return nil
}

// Close closes the store (no-op for DynamoDB client).
func (s *DynamoDBStore) Close() error {
	return nil
}
