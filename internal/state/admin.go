package state

import (
	"context"
	"fmt"
	"log/slog"
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
	// If only a state filter is set, use GSI2 for an efficient query.
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

	// Otherwise, scan all jobs and filter in-memory.
	allRecords, err := s.scanAllJobRecords(ctx)
	if err != nil {
		return nil, 0, err
	}

	filtered := make([]*core.Job, 0, len(allRecords))
	for _, r := range allRecords {
		if jobMatchesFilters(r, filters) {
			filtered = append(filtered, RecordToJob(r))
		}
	}

	total := len(filtered)
	sort.Slice(filtered, func(i, j int) bool {
		return filtered[i].CreatedAt > filtered[j].CreatedAt
	})

	return paginateJobs(filtered, limit, offset), total, nil
}

// scanAllJobRecords returns every job record via a paginated DynamoDB scan.
func (s *DynamoDBStore) scanAllJobRecords(ctx context.Context) ([]*JobRecord, error) {
	scanInput := &dynamodb.ScanInput{
		TableName:        aws.String(s.tableName),
		FilterExpression: aws.String("SK = :sk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":sk": &types.AttributeValueMemberS{Value: "JOB"},
		},
	}
	if s.pageLimit > 0 {
		scanInput.Limit = aws.Int32(s.pageLimit)
	}

	var allRecords []*JobRecord
	var lastKey map[string]types.AttributeValue
	for {
		if lastKey != nil {
			scanInput.ExclusiveStartKey = lastKey
		}
		result, err := s.client.Scan(ctx, scanInput)
		if err != nil {
			return nil, fmt.Errorf("failed to scan jobs: %w", err)
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
	return allRecords, nil
}

// jobMatchesFilters reports whether a job record matches all provided filters.
func jobMatchesFilters(r *JobRecord, filters core.JobListFilters) bool {
	if filters.State != "" && r.State != filters.State {
		return false
	}
	if filters.Queue != "" && r.Queue != filters.Queue {
		return false
	}
	if filters.Type != "" && r.Type != filters.Type {
		return false
	}
	if filters.WorkerID != "" && r.WorkerID != filters.WorkerID {
		return false
	}
	return true
}

// paginateJobs returns the offset/limit window of a job slice.
func paginateJobs(jobs []*core.Job, limit, offset int) []*core.Job {
	if offset >= len(jobs) {
		return []*core.Job{}
	}
	end := offset + limit
	if end > len(jobs) {
		end = len(jobs)
	}
	return jobs[offset:end]
}

// ListAllWorkers scans workers from DynamoDB with pagination.
func (s *DynamoDBStore) ListAllWorkers(ctx context.Context, limit, offset int) ([]*core.WorkerInfo, core.WorkerSummary, error) {
	workers, err := s.scanAllWorkers(ctx)
	if err != nil {
		return nil, core.WorkerSummary{}, err
	}

	summary := workerSummary(workers)

	if offset >= len(workers) {
		return []*core.WorkerInfo{}, summary, nil
	}
	end := offset + limit
	if end > len(workers) {
		end = len(workers)
	}
	return workers[offset:end], summary, nil
}

// scanAllWorkers returns every worker record via a paginated DynamoDB scan.
func (s *DynamoDBStore) scanAllWorkers(ctx context.Context) ([]*core.WorkerInfo, error) {
	scanInput := &dynamodb.ScanInput{
		TableName:        aws.String(s.tableName),
		FilterExpression: aws.String("SK = :sk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":sk": &types.AttributeValueMemberS{Value: "WORKER"},
		},
	}
	if s.pageLimit > 0 {
		scanInput.Limit = aws.Int32(s.pageLimit)
	}

	var workers []*core.WorkerInfo
	var lastKey map[string]types.AttributeValue
	for {
		if lastKey != nil {
			scanInput.ExclusiveStartKey = lastKey
		}
		result, err := s.client.Scan(ctx, scanInput)
		if err != nil {
			return nil, fmt.Errorf("failed to scan workers: %w", err)
		}

		for _, item := range result.Items {
			workers = append(workers, parseWorkerItem(item))
		}

		if result.LastEvaluatedKey == nil {
			break
		}
		lastKey = result.LastEvaluatedKey
	}
	return workers, nil
}

// parseWorkerItem converts a DynamoDB worker item into a WorkerInfo, deriving the
// worker state from its directive.
func parseWorkerItem(item map[string]types.AttributeValue) *core.WorkerInfo {
	w := &core.WorkerInfo{}

	if pk, ok := item["PK"].(*types.AttributeValueMemberS); ok {
		w.ID = strings.TrimPrefix(pk.Value, "WORKER#")
	}
	if dv, ok := item["directive"].(*types.AttributeValueMemberS); ok {
		w.Directive = dv.Value
	}
	if lhv, ok := item["last_heartbeat"].(*types.AttributeValueMemberS); ok {
		w.LastHeartbeat = lhv.Value
	}
	if ajv, ok := item["active_jobs"].(*types.AttributeValueMemberS); ok {
		n, err := strconv.Atoi(ajv.Value)
		if err != nil {
			slog.Warn("admin: invalid active_jobs value", "value", ajv.Value, "error", err)
		}
		w.ActiveJobs = n
	}

	// Derive state from directive.
	if w.Directive == "quiet" {
		w.State = "quiet"
	} else {
		w.State = "running"
	}
	return w
}

// workerSummary aggregates worker counts by derived state.
func workerSummary(workers []*core.WorkerInfo) core.WorkerSummary {
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
	return summary
}
