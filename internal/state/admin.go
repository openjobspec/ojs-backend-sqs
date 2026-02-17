package state

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/openjobspec/ojs-backend-sqs/internal/core"
)

// ListAllJobs scans all jobs from DynamoDB with filtering and pagination.
func (s *DynamoDBStore) ListAllJobs(ctx context.Context, filters core.JobListFilters, limit, offset int) ([]*core.Job, int, error) {
	// If state filter is set, use GSI2 for efficient query
	if filters.State != "" && filters.Queue == "" && filters.Type == "" && filters.WorkerID == "" {
		records, total, err := s.ListJobsByState(ctx, filters.State, limit, offset)
		if err != nil {
			return nil, 0, err
		}
		jobs := make([]*core.Job, 0, len(records))
		for _, r := range records {
			jobs = append(jobs, RecordToJob(r))
		}
		return jobs, total, nil
	}

	// Otherwise, scan all jobs and filter in-memory
	scanInput := &dynamodb.ScanInput{
		TableName:        aws.String(s.tableName),
		FilterExpression: aws.String("SK = :sk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":sk": &types.AttributeValueMemberS{Value: "JOB"},
		},
	}

	var allRecords []*JobRecord
	var lastKey map[string]types.AttributeValue

	for {
		if lastKey != nil {
			scanInput.ExclusiveStartKey = lastKey
		}
		result, err := s.client.Scan(ctx, scanInput)
		if err != nil {
			return nil, 0, fmt.Errorf("failed to scan jobs: %w", err)
		}

		for _, item := range result.Items {
			var record JobRecord
			if err := attributevalue.UnmarshalMap(item, &record); err != nil {
				continue
			}
			allRecords = append(allRecords, &record)
		}

		if result.LastEvaluatedKey == nil {
			break
		}
		lastKey = result.LastEvaluatedKey
	}

	// Apply filters
	var filtered []*core.Job
	for _, r := range allRecords {
		if filters.State != "" && r.State != filters.State {
			continue
		}
		if filters.Queue != "" && r.Queue != filters.Queue {
			continue
		}
		if filters.Type != "" && r.Type != filters.Type {
			continue
		}
		if filters.WorkerID != "" && r.WorkerID != filters.WorkerID {
			continue
		}
		filtered = append(filtered, RecordToJob(r))
	}

	total := len(filtered)
	sort.Slice(filtered, func(i, j int) bool {
		return filtered[i].CreatedAt > filtered[j].CreatedAt
	})

	if offset >= len(filtered) {
		return []*core.Job{}, total, nil
	}
	end := offset + limit
	if end > len(filtered) {
		end = len(filtered)
	}
	return filtered[offset:end], total, nil
}

// ListAllWorkers scans workers from DynamoDB with pagination.
func (s *DynamoDBStore) ListAllWorkers(ctx context.Context, limit, offset int) ([]*core.WorkerInfo, core.WorkerSummary, error) {
	scanInput := &dynamodb.ScanInput{
		TableName:        aws.String(s.tableName),
		FilterExpression: aws.String("SK = :sk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":sk": &types.AttributeValueMemberS{Value: "WORKER"},
		},
	}

	var workers []*core.WorkerInfo
	var lastKey map[string]types.AttributeValue

	for {
		if lastKey != nil {
			scanInput.ExclusiveStartKey = lastKey
		}
		result, err := s.client.Scan(ctx, scanInput)
		if err != nil {
			return nil, core.WorkerSummary{}, fmt.Errorf("failed to scan workers: %w", err)
		}

		for _, item := range result.Items {
			w := &core.WorkerInfo{}

			if pk, ok := item["PK"].(*types.AttributeValueMemberS); ok {
				w.ID = strings.TrimPrefix(pk.Value, "WORKER#")
			}
			if d, ok := item["directive"]; ok {
				if dv, ok := d.(*types.AttributeValueMemberS); ok {
					w.Directive = dv.Value
				}
			}
			if lh, ok := item["last_heartbeat"]; ok {
				if lhv, ok := lh.(*types.AttributeValueMemberS); ok {
					w.LastHeartbeat = lhv.Value
				}
			}
			if aj, ok := item["active_jobs"]; ok {
				if ajv, ok := aj.(*types.AttributeValueMemberS); ok {
					w.ActiveJobs, _ = strconv.Atoi(ajv.Value)
				}
			}

			// Derive state from directive
			switch w.Directive {
			case "quiet":
				w.State = "quiet"
			default:
				w.State = "running"
			}

			workers = append(workers, w)
		}

		if result.LastEvaluatedKey == nil {
			break
		}
		lastKey = result.LastEvaluatedKey
	}

	summary := core.WorkerSummary{Total: len(workers)}
	for _, w := range workers {
		switch w.State {
		case "running":
			summary.Running++
		case "quiet":
			summary.Quiet++
		default:
			summary.Stale++
		}
	}

	if offset >= len(workers) {
		return []*core.WorkerInfo{}, summary, nil
	}
	end := offset + limit
	if end > len(workers) {
		end = len(workers)
	}
	return workers[offset:end], summary, nil
}
