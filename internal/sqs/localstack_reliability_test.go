package sqs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/openjobspec/ojs-backend-sqs/internal/core"
	"github.com/openjobspec/ojs-backend-sqs/internal/state"
)

type localStackFetchFaultSQS struct {
	sqsAPI
	mu            sync.Mutex
	receiveCalls  int
	receiveFn     func(context.Context, int, *awssqs.ReceiveMessageInput) (*awssqs.ReceiveMessageOutput, error)
	visibilityErr error
}

func (f *localStackFetchFaultSQS) ReceiveMessage(ctx context.Context, input *awssqs.ReceiveMessageInput, _ ...func(*awssqs.Options)) (*awssqs.ReceiveMessageOutput, error) {
	f.mu.Lock()
	f.receiveCalls++
	call := f.receiveCalls
	fn := f.receiveFn
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, call, input)
	}
	return f.sqsAPI.ReceiveMessage(ctx, input)
}

func (f *localStackFetchFaultSQS) ChangeMessageVisibility(ctx context.Context, input *awssqs.ChangeMessageVisibilityInput, options ...func(*awssqs.Options)) (*awssqs.ChangeMessageVisibilityOutput, error) {
	if f.visibilityErr != nil {
		return nil, f.visibilityErr
	}
	return f.sqsAPI.ChangeMessageVisibility(ctx, input, options...)
}

type localStackClaimFaultStore struct {
	*state.DynamoDBStore
	failJobID string
	failed    atomic.Bool
}

func (s *localStackClaimFaultStore) ClaimJob(ctx context.Context, claim state.JobClaim) (*state.JobRecord, error) {
	if claim.JobID == s.failJobID && s.failed.CompareAndSwap(false, true) {
		return nil, errors.New("injected conditional-store failure")
	}
	return s.DynamoDBStore.ClaimJob(ctx, claim)
}

func collectLocalStackMessages(ctx context.Context, client sqsAPI, input *awssqs.ReceiveMessageInput, count int) (*awssqs.ReceiveMessageOutput, error) {
	messages := make([]sqstypes.Message, 0, count)
	deadline := time.Now().Add(3 * time.Second)
	for len(messages) < count && time.Now().Before(deadline) {
		remaining := count - len(messages)
		if remaining > 10 {
			remaining = 10
		}
		request := *input
		request.MaxNumberOfMessages = int32(remaining)
		response, err := client.ReceiveMessage(ctx, &request)
		if err != nil {
			return nil, err
		}
		messages = append(messages, response.Messages...)
		if len(messages) < count {
			time.Sleep(25 * time.Millisecond)
		}
	}
	if len(messages) != count {
		return nil, fmt.Errorf("collected %d messages, want %d", len(messages), count)
	}
	return &awssqs.ReceiveMessageOutput{Messages: messages}, nil
}

func orderLocalStackMessages(t *testing.T, messages []sqstypes.Message, firstJobID string) {
	t.Helper()
	sort.SliceStable(messages, func(i, _ int) bool {
		var job core.Job
		if messages[i].Body == nil || json.Unmarshal([]byte(*messages[i].Body), &job) != nil {
			return false
		}
		return job.ID == firstJobID
	})
}

func corruptLocalStackGeneration(t *testing.T, messages []sqstypes.Message, jobID string) {
	t.Helper()
	for i := range messages {
		var job core.Job
		if messages[i].Body == nil || json.Unmarshal([]byte(*messages[i].Body), &job) != nil || job.ID != jobID {
			continue
		}
		value := messages[i].MessageAttributes[AttrOJSDeliveryGeneration]
		value.StringValue = aws.String("corrupt")
		messages[i].MessageAttributes[AttrOJSDeliveryGeneration] = value
		return
	}
	t.Fatalf("message for %s not found", jobID)
}

func fetchLocalStackJobEventually(t *testing.T, ctx context.Context, backend *SQSBackend, queue, jobID string) *core.Job {
	t.Helper()
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		jobs, err := backend.Fetch(ctx, []string{queue}, 2, "retry-worker", 1_000)
		if err != nil {
			t.Fatalf("retry fetch: %v", err)
		}
		for _, job := range jobs {
			if job.ID != jobID {
				t.Fatalf("retry returned unexpected job %s, want %s", job.ID, jobID)
			}
			return job
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("job %s was not redelivered", jobID)
	return nil
}

func TestLocalStack_ReliabilityAndConcurrency(t *testing.T) { //nolint:gocyclo // Shared fixture keeps real-service stress cases isolated in one table.
	endpoint := os.Getenv("OJS_LOCALSTACK_ENDPOINT")
	if endpoint == "" {
		t.Skip("set OJS_LOCALSTACK_ENDPOINT to run LocalStack reliability tests")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cfg, err := config.LoadDefaultConfig(
		ctx,
		config.WithRegion("us-east-1"),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test", "test", "test")),
		config.WithBaseEndpoint(endpoint),
	)
	if err != nil {
		t.Fatalf("load LocalStack config: %v", err)
	}
	ddb := dynamodb.NewFromConfig(cfg)
	sqsClient := awssqs.NewFromConfig(cfg)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	tableName := "ojs-reliability-" + suffix
	queuePrefix := "ojs-it-" + suffix
	store := state.NewDynamoDBStore(ddb, tableName)
	if err := store.EnsureTable(ctx); err != nil {
		t.Fatalf("ensure LocalStack table: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cleanupCancel()
		_, _ = ddb.DeleteTable(cleanupCtx, &dynamodb.DeleteTableInput{TableName: aws.String(tableName)})
		queues, _ := sqsClient.ListQueues(cleanupCtx, &awssqs.ListQueuesInput{QueueNamePrefix: aws.String(queuePrefix)})
		for _, queueURL := range queues.QueueUrls {
			_, _ = sqsClient.DeleteQueue(cleanupCtx, &awssqs.DeleteQueueInput{QueueUrl: aws.String(queueURL)})
		}
	})

	backend := New(sqsClient, store, queuePrefix, false)
	backend.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	useShortPolling := func(queue string) {
		queueURL, getErr := backend.getQueueURL(ctx, queue)
		if getErr != nil {
			return
		}
		_, _ = sqsClient.SetQueueAttributes(ctx, &awssqs.SetQueueAttributesInput{
			QueueUrl: aws.String(queueURL),
			Attributes: map[string]string{
				"ReceiveMessageWaitTimeSeconds": "0",
			},
		})
	}

	t.Run("unique replace is atomic and rejects active", func(t *testing.T) {
		policy := &core.UniquePolicy{
			Keys:       []string{"type", "args"},
			Period:     "PT1H",
			OnConflict: "replace",
		}
		original, err := backend.Push(ctx, &core.Job{
			Type:   "unique.concurrent",
			Args:   json.RawMessage(`["same"]`),
			Queue:  "unique",
			Unique: policy,
		})
		if err != nil {
			t.Fatalf("push original: %v", err)
		}
		useShortPolling("unique")

		const contenders = 16
		start := make(chan struct{})
		var successes atomic.Int32
		var winnerMu sync.Mutex
		var winnerID string
		var wg sync.WaitGroup
		for i := 0; i < contenders; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				replacement, pushErr := backend.Push(ctx, &core.Job{
					Type:   "unique.concurrent",
					Args:   json.RawMessage(`["same"]`),
					Queue:  "unique",
					Unique: policy,
				})
				if pushErr == nil {
					successes.Add(1)
					winnerMu.Lock()
					winnerID = replacement.ID
					winnerMu.Unlock()
				}
			}()
		}
		close(start)
		wg.Wait()
		if successes.Load() != 1 {
			t.Fatalf("successful concurrent replacements = %d, want 1", successes.Load())
		}
		originalInfo, err := backend.Info(ctx, original.ID)
		if err != nil || originalInfo.State != core.StateCancelled {
			t.Fatalf("original after replace = %#v, err=%v", originalInfo, err)
		}
		winnerMu.Lock()
		replacementID := winnerID
		winnerMu.Unlock()
		if replacementID == "" {
			t.Fatal("missing replacement ID")
		}

		active, err := backend.Fetch(ctx, []string{"unique"}, 1, "unique-worker", 30_000)
		if err != nil || len(active) != 1 {
			t.Fatalf("fetch replacement: jobs=%d err=%v", len(active), err)
		}
		if active[0].ID != replacementID {
			t.Fatalf("fetched %s, want replacement %s", active[0].ID, replacementID)
		}
		_, err = backend.Push(ctx, &core.Job{
			Type:   "unique.concurrent",
			Args:   json.RawMessage(`["same"]`),
			Queue:  "unique",
			Unique: policy,
		})
		if err == nil {
			t.Fatal("active predecessor replacement unexpectedly succeeded")
		}
	})

	t.Run("conditional claim and terminal fencing", func(t *testing.T) {
		job, err := backend.Push(ctx, &core.Job{Type: "claim.once", Args: json.RawMessage(`[]`), Queue: "claims"})
		if err != nil {
			t.Fatalf("push: %v", err)
		}
		useShortPolling("claims")
		var claimed atomic.Int32
		var claimedJob *core.Job
		var claimedMu sync.Mutex
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(worker int) {
				defer wg.Done()
				jobs, fetchErr := backend.Fetch(ctx, []string{"claims"}, 1, fmt.Sprintf("worker-%d", worker), 30_000)
				if fetchErr == nil && len(jobs) == 1 {
					claimed.Add(1)
					claimedMu.Lock()
					claimedJob = jobs[0]
					claimedMu.Unlock()
				}
			}(i)
		}
		wg.Wait()
		if claimed.Load() != 1 {
			t.Fatalf("claim winners = %d, want 1", claimed.Load())
		}
		claimedMu.Lock()
		got := claimedJob
		claimedMu.Unlock()
		if got == nil || got.Attempt != 1 {
			t.Fatalf("claimed job = %#v, want attempt 1", got)
		}
		if _, ackErr := backend.Ack(ctx, job.ID, json.RawMessage(`{"ok":true}`)); ackErr != nil {
			t.Fatalf("ack: %v", ackErr)
		}

		record, err := store.GetJob(ctx, job.ID)
		if err != nil {
			t.Fatalf("get completed record: %v", err)
		}
		queueURL, err := backend.getQueueURL(ctx, record.Queue)
		if err != nil {
			t.Fatalf("get queue URL: %v", err)
		}
		body, _ := EncodeJob(state.RecordToJob(record))
		_, err = sqsClient.SendMessage(ctx, &awssqs.SendMessageInput{
			QueueUrl:    aws.String(queueURL),
			MessageBody: aws.String(body),
			MessageAttributes: map[string]sqstypes.MessageAttributeValue{
				AttrOJSDeliveryGeneration: {
					DataType:    aws.String("Number"),
					StringValue: aws.String(fmt.Sprint(record.DeliveryGeneration)),
				},
			},
		})
		if err != nil {
			t.Fatalf("inject stale message: %v", err)
		}
		stale, err := backend.Fetch(ctx, []string{"claims"}, 1, "late-worker", 1000)
		if err != nil || len(stale) != 0 {
			t.Fatalf("terminal stale fetch = %v, err=%v", stale, err)
		}
		info, _ := backend.Info(ctx, job.ID)
		if info.State != core.StateCompleted {
			t.Fatalf("stale delivery reactivated terminal job: %s", info.State)
		}
	})

	t.Run("ack and cancel race has one transition winner", func(t *testing.T) {
		job, err := backend.Push(ctx, &core.Job{Type: "transition.race", Args: json.RawMessage(`[]`), Queue: "transition-race"})
		if err != nil {
			t.Fatalf("push: %v", err)
		}
		useShortPolling("transition-race")
		jobs, err := backend.Fetch(ctx, []string{"transition-race"}, 1, "race-worker", 30_000)
		if err != nil || len(jobs) != 1 {
			t.Fatalf("fetch jobs=%d err=%v", len(jobs), err)
		}

		start := make(chan struct{})
		var winners atomic.Int32
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			if _, ackErr := backend.Ack(ctx, job.ID, nil); ackErr == nil {
				winners.Add(1)
			}
		}()
		go func() {
			defer wg.Done()
			<-start
			if _, cancelErr := backend.Cancel(ctx, job.ID); cancelErr == nil {
				winners.Add(1)
			}
		}()
		close(start)
		wg.Wait()
		if winners.Load() != 1 {
			t.Fatalf("terminal transition winners = %d, want 1", winners.Load())
		}
		info, err := backend.Info(ctx, job.ID)
		if err != nil || (info.State != core.StateCompleted && info.State != core.StateCancelled) {
			t.Fatalf("race result = %#v err=%v", info, err)
		}
	})

	t.Run("scheduled promotion and requeue use new delivery generations", func(t *testing.T) {
		job, err := backend.Push(ctx, &core.Job{
			Type:        "scheduled.requeue",
			Args:        json.RawMessage(`[]`),
			Queue:       "scheduled-requeue",
			ScheduledAt: core.FormatTime(time.Now().Add(500 * time.Millisecond)),
		})
		if err != nil || job.State != core.StateScheduled {
			t.Fatalf("scheduled push = %#v err=%v", job, err)
		}
		useShortPolling("scheduled-requeue")
		time.Sleep(600 * time.Millisecond)
		deadline := time.Now().Add(5 * time.Second)
		for {
			if promoteErr := backend.PromoteScheduled(ctx); promoteErr != nil {
				t.Fatalf("promote scheduled: %v", promoteErr)
			}
			info, infoErr := backend.Info(ctx, job.ID)
			if infoErr == nil && info.State == core.StateAvailable {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("scheduled marker was not promoted: info=%v err=%v", info, infoErr)
			}
			time.Sleep(100 * time.Millisecond)
		}
		marker, err := ddb.GetItem(ctx, &dynamodb.GetItemInput{
			TableName: aws.String(tableName),
			Key: map[string]ddbtypes.AttributeValue{
				"PK": &ddbtypes.AttributeValueMemberS{Value: "SCHEDULED#" + job.ID},
				"SK": &ddbtypes.AttributeValueMemberS{Value: "SCHEDULED"},
			},
			ConsistentRead: aws.Bool(true),
		})
		if err != nil || len(marker.Item) != 0 {
			t.Fatalf("scheduled source marker remained after confirmed send: item=%v err=%v", marker.Item, err)
		}
		first, err := backend.Fetch(ctx, []string{"scheduled-requeue"}, 1, "scheduled-worker", 30_000)
		if err != nil || len(first) != 1 || first[0].Attempt != 1 {
			t.Fatalf("first delivery=%v err=%v", first, err)
		}
		if _, nackErr := backend.Nack(ctx, job.ID, &core.JobError{Message: "requeue"}, true); nackErr != nil {
			t.Fatalf("nack requeue: %v", nackErr)
		}
		second, err := backend.Fetch(ctx, []string{"scheduled-requeue"}, 1, "scheduled-worker", 30_000)
		if err != nil || len(second) != 1 || second[0].Attempt != 2 {
			t.Fatalf("second delivery=%v err=%v", second, err)
		}
		if _, err := backend.Ack(ctx, job.ID, nil); err != nil {
			t.Fatalf("ack requeued job: %v", err)
		}
	})

	t.Run("crashed worker exhausts tracked attempts into OJS DLQ", func(t *testing.T) {
		maxAttempts := 2
		job, err := backend.Push(ctx, &core.Job{
			Type:        "crash.retry",
			Args:        json.RawMessage(`[]`),
			Queue:       "crashes",
			MaxAttempts: &maxAttempts,
			Retry: &core.RetryPolicy{
				MaxAttempts:        maxAttempts,
				InitialInterval:    "PT0S",
				BackoffCoefficient: 1,
				OnExhaustion:       "dead_letter",
			},
		})
		if err != nil {
			t.Fatalf("push: %v", err)
		}
		useShortPolling("crashes")
		for attempt := 1; attempt <= maxAttempts; attempt++ {
			jobs, fetchErr := backend.Fetch(ctx, []string{"crashes"}, 1, "crash-worker", 1000)
			if fetchErr != nil || len(jobs) != 1 {
				t.Fatalf("attempt %d fetch jobs=%d err=%v", attempt, len(jobs), fetchErr)
			}
			if jobs[0].Attempt != attempt {
				t.Fatalf("attempt field = %d, want %d", jobs[0].Attempt, attempt)
			}
			_, err = ddb.UpdateItem(ctx, &dynamodb.UpdateItemInput{
				TableName: aws.String(tableName),
				Key: map[string]ddbtypes.AttributeValue{
					"PK": &ddbtypes.AttributeValueMemberS{Value: job.ID},
					"SK": &ddbtypes.AttributeValueMemberS{Value: "JOB"},
				},
				UpdateExpression: aws.String("SET delivery_deadline_ms = :zero"),
				ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
					":zero": &ddbtypes.AttributeValueMemberN{Value: "0"},
				},
			})
			if err != nil {
				t.Fatalf("expire claim: %v", err)
			}
			if reapErr := backend.ReapExpiredClaims(ctx); reapErr != nil {
				t.Fatalf("reap attempt %d: %v", attempt, reapErr)
			}
			if attempt < maxAttempts {
				deadline := time.Now().Add(5 * time.Second)
				for {
					if promoteErr := backend.PromoteRetries(ctx); promoteErr != nil {
						t.Fatalf("promote retry: %v", promoteErr)
					}
					info, infoErr := backend.Info(ctx, job.ID)
					if infoErr == nil && info.State == core.StateAvailable {
						break
					}
					if time.Now().After(deadline) {
						t.Fatalf("retry marker was not promoted; state=%v err=%v", info, infoErr)
					}
					time.Sleep(100 * time.Millisecond)
				}
			}
		}
		info, err := backend.Info(ctx, job.ID)
		if err != nil || info.State != core.StateDiscarded {
			t.Fatalf("exhausted job = %#v, err=%v", info, err)
		}
		inDLQ, err := store.IsInDeadLetter(ctx, job.ID)
		if err != nil || !inDLQ {
			t.Fatalf("OJS DLQ marker = %v, err=%v", inDLQ, err)
		}
		if _, retryErr := backend.RetryDeadLetter(ctx, job.ID); retryErr != nil {
			t.Fatalf("replay dead letter: %v", retryErr)
		}
		replayed, err := backend.Fetch(ctx, []string{"crashes"}, 1, "replay-worker", 30_000)
		if err != nil || len(replayed) != 1 || replayed[0].Attempt != 1 {
			t.Fatalf("replayed fetch=%v err=%v", replayed, err)
		}
		if _, err := backend.Ack(ctx, job.ID, nil); err != nil {
			t.Fatalf("ack replay: %v", err)
		}
	})

	t.Run("workflow completions count once under concurrency", func(t *testing.T) {
		const total = 12
		request := &core.WorkflowRequest{Type: "group", Name: "concurrent-group"}
		for i := 0; i < total; i++ {
			request.Jobs = append(request.Jobs, core.WorkflowJobRequest{
				Name: fmt.Sprintf("step-%d", i),
				Type: "workflow.member",
				Args: json.RawMessage(`[]`),
			})
		}
		workflow, err := backend.CreateWorkflow(ctx, request)
		if err != nil {
			t.Fatalf("create workflow: %v", err)
		}
		useShortPolling("default")
		jobs, err := backend.Fetch(ctx, []string{"default"}, total, "workflow-worker", 30_000)
		if err != nil || len(jobs) != total {
			t.Fatalf("workflow fetch jobs=%d err=%v", len(jobs), err)
		}
		var wg sync.WaitGroup
		var failures atomic.Int32
		for _, job := range jobs {
			job := job
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, ackErr := backend.Ack(ctx, job.ID, json.RawMessage(`{"done":true}`)); ackErr != nil {
					failures.Add(1)
				}
			}()
		}
		wg.Wait()
		if failures.Load() != 0 {
			t.Fatalf("workflow ACK failures = %d", failures.Load())
		}
		if advanceErr := backend.AdvanceWorkflow(ctx, workflow.ID, jobs[0].ID, json.RawMessage(`{"done":true}`), false); advanceErr != nil {
			t.Fatalf("duplicate workflow advancement: %v", advanceErr)
		}
		current, err := backend.GetWorkflow(ctx, workflow.ID)
		if err != nil {
			t.Fatalf("get workflow: %v", err)
		}
		if current.State != "completed" || current.JobsCompleted == nil || *current.JobsCompleted != total {
			t.Fatalf("workflow = %#v, want completed count %d", current, total)
		}
	})

	t.Run("workflow advancement survives restart after terminal transition", func(t *testing.T) {
		workflow, err := backend.CreateWorkflow(ctx, &core.WorkflowRequest{
			Type: "group",
			Name: "restart-safe",
			Jobs: []core.WorkflowJobRequest{{
				Name:    "only",
				Type:    "workflow.restart-member",
				Args:    json.RawMessage(`[]`),
				Options: &core.EnqueueOptions{Queue: "workflow-restart"},
			}},
		})
		if err != nil {
			t.Fatalf("create workflow: %v", err)
		}
		useShortPolling("workflow-restart")
		jobs, err := backend.Fetch(ctx, []string{"workflow-restart"}, 1, "restart-worker", 30_000)
		if err != nil || len(jobs) != 1 {
			t.Fatalf("fetch jobs=%d err=%v", len(jobs), err)
		}
		record, err := store.GetJob(ctx, jobs[0].ID)
		if err != nil {
			t.Fatalf("get active job: %v", err)
		}
		_, err = store.TransitionJob(ctx, &state.JobTransitionPlan{
			JobID:                      record.ID,
			Queue:                      record.Queue,
			CreatedAt:                  record.CreatedAt,
			FromState:                  core.StateActive,
			ToState:                    core.StateCompleted,
			ExpectedVersion:            record.Version,
			MatchVersion:               true,
			ExpectedDeliveryGeneration: record.DeliveryGeneration,
			MatchDeliveryGeneration:    true,
			ExpectedReceiptHandle:      record.SQSReceiptHandle,
			Updates: map[string]any{
				"completed_at":         core.NowFormatted(),
				"sqs_receipt_handle":   "",
				"worker_id":            "",
				"delivery_deadline_ms": int64(0),
			},
			WorkflowAdvance: newWorkflowAdvanceIntent(record, nil, false),
		})
		if err != nil {
			t.Fatalf("terminal transition: %v", err)
		}
		before, err := backend.GetWorkflow(ctx, workflow.ID)
		if err != nil || before.State != "running" {
			t.Fatalf("workflow advanced before durable intent drain: %#v err=%v", before, err)
		}

		restarted := New(sqsClient, store, queuePrefix, false)
		restarted.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
		if drainErr := restarted.DrainWorkflowAdvancements(ctx); drainErr != nil {
			t.Fatalf("restart advancement drain: %v", drainErr)
		}
		after, err := restarted.GetWorkflow(ctx, workflow.ID)
		if err != nil || after.State != "completed" || after.JobsCompleted == nil || *after.JobsCompleted != 1 {
			t.Fatalf("workflow after restart drain: %#v err=%v", after, err)
		}
	})

	t.Run("workflow finalization callback and cancellation are fenced", func(t *testing.T) {
		const members = 4
		request := &core.WorkflowRequest{
			Type: "batch",
			Name: "callback-batch",
			Callbacks: &core.WorkflowCallbacks{
				OnComplete: &core.WorkflowCallback{
					Type:    "workflow.callback",
					Args:    json.RawMessage(`[]`),
					Options: &core.EnqueueOptions{Queue: "callbacks"},
				},
			},
		}
		for i := 0; i < members; i++ {
			request.Jobs = append(request.Jobs, core.WorkflowJobRequest{
				Name:    fmt.Sprintf("member-%d", i),
				Type:    "workflow.batch-member",
				Args:    json.RawMessage(`[]`),
				Options: &core.EnqueueOptions{Queue: "batch-members"},
			})
		}
		_, err := backend.CreateWorkflow(ctx, request)
		if err != nil {
			t.Fatalf("create batch: %v", err)
		}
		useShortPolling("batch-members")
		jobs, err := backend.Fetch(ctx, []string{"batch-members"}, members, "batch-worker", 30_000)
		if err != nil || len(jobs) != members {
			t.Fatalf("batch fetch jobs=%d err=%v", len(jobs), err)
		}
		var wg sync.WaitGroup
		for _, job := range jobs {
			job := job
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _ = backend.Ack(ctx, job.ID, nil)
			}()
		}
		wg.Wait()
		if drainErr := backend.DrainJobEffects(ctx); drainErr != nil {
			t.Fatalf("drain callback effect: %v", drainErr)
		}
		callbacks, count, err := backend.ListJobs(ctx, core.JobListFilters{Type: "workflow.callback"}, 10, 0)
		if err != nil || count != 1 || len(callbacks) != 1 {
			t.Fatalf("callbacks count=%d page=%d err=%v", count, len(callbacks), err)
		}

		chain, err := backend.CreateWorkflow(ctx, &core.WorkflowRequest{
			Type: "chain",
			Name: "cancel-chain",
			Steps: []core.WorkflowJobRequest{
				{Name: "first", Type: "workflow.cancel-first", Args: json.RawMessage(`[]`), Options: &core.EnqueueOptions{Queue: "cancel-chain"}},
				{Name: "second", Type: "workflow.cancel-second", Args: json.RawMessage(`[]`), Options: &core.EnqueueOptions{Queue: "cancel-chain"}},
			},
		})
		if err != nil {
			t.Fatalf("create chain: %v", err)
		}
		useShortPolling("cancel-chain")
		first, err := backend.Fetch(ctx, []string{"cancel-chain"}, 1, "chain-worker", 30_000)
		if err != nil || len(first) != 1 {
			t.Fatalf("chain fetch jobs=%d err=%v", len(first), err)
		}
		if _, cancelErr := backend.Cancel(ctx, first[0].ID); cancelErr != nil {
			t.Fatalf("cancel chain step: %v", cancelErr)
		}
		current, err := backend.GetWorkflow(ctx, chain.ID)
		if err != nil || current.State != "failed" {
			t.Fatalf("cancelled-step workflow = %#v err=%v", current, err)
		}
		if drainErr := backend.DrainJobEffects(ctx); drainErr != nil {
			t.Fatalf("drain cancelled chain effects: %v", drainErr)
		}
		second, _, err := backend.ListJobs(ctx, core.JobListFilters{Type: "workflow.cancel-second"}, 10, 0)
		if err != nil || len(second) != 0 {
			t.Fatalf("cancelled chain emitted second step: jobs=%d err=%v", len(second), err)
		}
	})

	t.Run("cron occurrence has one multi-replica owner", func(t *testing.T) {
		name := "cron-" + suffix
		_, err := backend.RegisterCron(ctx, &core.CronJob{
			Name:       name,
			Expression: "* * * * *",
			Timezone:   "America/New_York",
			JobTemplate: &core.CronJobTemplate{
				Type: "cron.once",
				Args: json.RawMessage(`[]`),
			},
		})
		if err != nil {
			t.Fatalf("register cron: %v", err)
		}
		due := core.FormatTime(time.Now().Add(-time.Minute))
		_, err = ddb.UpdateItem(ctx, &dynamodb.UpdateItemInput{
			TableName: aws.String(tableName),
			Key: map[string]ddbtypes.AttributeValue{
				"PK": &ddbtypes.AttributeValueMemberS{Value: "CRON#" + name},
				"SK": &ddbtypes.AttributeValueMemberS{Value: "CRON"},
			},
			UpdateExpression: aws.String("SET next_run_at = :due"),
			ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
				":due": &ddbtypes.AttributeValueMemberS{Value: due},
			},
		})
		if err != nil {
			t.Fatalf("force cron due: %v", err)
		}

		var wg sync.WaitGroup
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_ = backend.FireCronJobs(ctx)
			}()
		}
		wg.Wait()
		if drainErr := backend.DrainJobEffects(ctx); drainErr != nil {
			t.Fatalf("drain cron effect: %v", drainErr)
		}
		jobs, count, err := backend.ListJobs(ctx, core.JobListFilters{Type: "cron.once"}, 20, 0)
		if err != nil {
			t.Fatalf("list cron jobs: %v", err)
		}
		if count != 1 || len(jobs) != 1 {
			t.Fatalf("cron jobs count=%d page=%d, want one", count, len(jobs))
		}
	})

	t.Run("worker directives survive heartbeat and are consumed once", func(t *testing.T) {
		if err := backend.SetWorkerState(ctx, "directed-worker", "quiet"); err != nil {
			t.Fatalf("set directive: %v", err)
		}
		first, err := backend.Heartbeat(ctx, "directed-worker", nil, 30_000)
		if err != nil || first.Directive != "quiet" {
			t.Fatalf("first heartbeat directive=%q err=%v", first.Directive, err)
		}
		second, err := backend.Heartbeat(ctx, "directed-worker", nil, 30_000)
		if err != nil || second.Directive != "continue" {
			t.Fatalf("second heartbeat directive=%q err=%v", second.Directive, err)
		}
	})

	t.Run("heartbeat extends only the matching delivery owner", func(t *testing.T) {
		job, err := backend.Push(ctx, &core.Job{Type: "heartbeat.owner", Args: json.RawMessage(`[]`), Queue: "heartbeats"})
		if err != nil {
			t.Fatalf("push: %v", err)
		}
		useShortPolling("heartbeats")
		jobs, err := backend.Fetch(ctx, []string{"heartbeats"}, 1, "owner", 30_000)
		if err != nil || len(jobs) != 1 {
			t.Fatalf("fetch jobs=%d err=%v", len(jobs), err)
		}
		wrong, err := backend.Heartbeat(ctx, "other-worker", []string{job.ID}, 30_000)
		if err != nil || len(wrong.JobsExtended) != 0 {
			t.Fatalf("wrong-owner heartbeat extended=%v err=%v", wrong.JobsExtended, err)
		}
		right, err := backend.Heartbeat(ctx, "owner", []string{job.ID}, 1500)
		if err != nil || len(right.JobsExtended) != 1 || right.JobsExtended[0] != job.ID {
			t.Fatalf("owner heartbeat extended=%v err=%v", right.JobsExtended, err)
		}
		if _, err := backend.Cancel(ctx, job.ID); err != nil {
			t.Fatalf("cleanup cancel: %v", err)
		}
	})

	t.Run("stored visibility overrides fetch default", func(t *testing.T) {
		visibilityMs := 1200
		job, err := backend.Push(ctx, &core.Job{
			Type:                "visibility.override",
			Args:                json.RawMessage(`[]`),
			Queue:               "visibility-override",
			VisibilityTimeoutMs: &visibilityMs,
		})
		if err != nil {
			t.Fatalf("push: %v", err)
		}
		useShortPolling("visibility-override")
		jobs, err := backend.Fetch(ctx, []string{"visibility-override"}, 1, "visibility-worker", 30_000)
		if err != nil || len(jobs) != 1 {
			t.Fatalf("fetch jobs=%d err=%v", len(jobs), err)
		}
		record, err := store.GetJob(ctx, job.ID)
		if err != nil {
			t.Fatalf("get active job: %v", err)
		}
		remaining := time.Until(time.UnixMilli(record.DeliveryDeadlineMs))
		if remaining > 3*time.Second {
			t.Fatalf("tracked deadline used fetch default instead of job override: %s", remaining)
		}
		time.Sleep(2100 * time.Millisecond)
		if reapErr := backend.ReapExpiredClaims(ctx); reapErr != nil {
			t.Fatalf("reap overridden visibility: %v", reapErr)
		}
		info, err := backend.Info(ctx, job.ID)
		if err != nil || info.State != core.StateAvailable {
			t.Fatalf("job was not requeued after stored visibility: %#v err=%v", info, err)
		}
	})

	t.Run("partial fetch preserves success before corrupt generation", func(t *testing.T) {
		const queue = "partial-corrupt"
		good, err := backend.Push(ctx, &core.Job{Type: "partial.corrupt-good", Args: json.RawMessage(`[]`), Queue: queue})
		if err != nil {
			t.Fatalf("push good: %v", err)
		}
		corrupt, err := backend.Push(ctx, &core.Job{Type: "partial.corrupt-bad", Args: json.RawMessage(`[]`), Queue: queue})
		if err != nil {
			t.Fatalf("push corrupt: %v", err)
		}
		useShortPolling(queue)

		faultClient := &localStackFetchFaultSQS{sqsAPI: sqsClient}
		faultClient.receiveFn = func(callCtx context.Context, call int, input *awssqs.ReceiveMessageInput) (*awssqs.ReceiveMessageOutput, error) {
			if call != 1 {
				return sqsClient.ReceiveMessage(callCtx, input)
			}
			response, collectErr := collectLocalStackMessages(callCtx, sqsClient, input, 2)
			if collectErr != nil {
				return nil, collectErr
			}
			orderLocalStackMessages(t, response.Messages, good.ID)
			corruptLocalStackGeneration(t, response.Messages, corrupt.ID)
			return response, nil
		}
		faultBackend := newWithSQSClient(faultClient, store, queuePrefix, false)
		faultBackend.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
		faultBackend.queueURLs[queue], err = backend.getQueueURL(ctx, queue)
		if err != nil {
			t.Fatalf("get queue URL: %v", err)
		}

		jobs, err := faultBackend.Fetch(ctx, []string{queue}, 2, "partial-worker", 1_000)
		if err != nil || len(jobs) != 1 || jobs[0].ID != good.ID {
			t.Fatalf("partial corrupt fetch=%v err=%v", jobs, err)
		}
		if _, err := backend.Ack(ctx, good.ID, nil); err != nil {
			t.Fatalf("ack good: %v", err)
		}
		retried := fetchLocalStackJobEventually(t, ctx, backend, queue, corrupt.ID)
		if retried.Attempt != 1 {
			t.Fatalf("corrupt retry attempt=%d, want 1", retried.Attempt)
		}
		if _, err := backend.Ack(ctx, corrupt.ID, nil); err != nil {
			t.Fatalf("ack corrupt retry: %v", err)
		}
	})

	t.Run("partial fetch recovers visibility-change throttle", func(t *testing.T) {
		const queue = "partial-visibility"
		good, err := backend.Push(ctx, &core.Job{Type: "partial.visibility-good", Args: json.RawMessage(`[]`), Queue: queue})
		if err != nil {
			t.Fatalf("push good: %v", err)
		}
		visibilityMs := 5_000
		throttled, err := backend.Push(ctx, &core.Job{
			Type:                "partial.visibility-bad",
			Args:                json.RawMessage(`[]`),
			Queue:               queue,
			VisibilityTimeoutMs: &visibilityMs,
		})
		if err != nil {
			t.Fatalf("push throttled: %v", err)
		}
		useShortPolling(queue)

		faultClient := &localStackFetchFaultSQS{
			sqsAPI:        sqsClient,
			visibilityErr: errors.New("injected visibility throttle"),
		}
		faultClient.receiveFn = func(callCtx context.Context, call int, input *awssqs.ReceiveMessageInput) (*awssqs.ReceiveMessageOutput, error) {
			if call != 1 {
				return sqsClient.ReceiveMessage(callCtx, input)
			}
			response, collectErr := collectLocalStackMessages(callCtx, sqsClient, input, 2)
			if collectErr != nil {
				return nil, collectErr
			}
			orderLocalStackMessages(t, response.Messages, good.ID)
			return response, nil
		}
		faultBackend := newWithSQSClient(faultClient, store, queuePrefix, false)
		faultBackend.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
		faultBackend.queueURLs[queue], err = backend.getQueueURL(ctx, queue)
		if err != nil {
			t.Fatalf("get queue URL: %v", err)
		}

		jobs, err := faultBackend.Fetch(ctx, []string{queue}, 2, "partial-worker", 1_000)
		if err != nil || len(jobs) != 1 || jobs[0].ID != good.ID {
			t.Fatalf("partial visibility fetch=%v err=%v", jobs, err)
		}
		if _, err := backend.Ack(ctx, good.ID, nil); err != nil {
			t.Fatalf("ack good: %v", err)
		}

		deadline := time.Now().Add(6 * time.Second)
		for {
			if err := backend.ReapExpiredClaims(ctx); err != nil {
				t.Fatalf("reap visibility-throttled claim: %v", err)
			}
			info, infoErr := backend.Info(ctx, throttled.ID)
			if infoErr == nil && info.State == core.StateAvailable {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("visibility-throttled job was not requeued: info=%#v err=%v", info, infoErr)
			}
			time.Sleep(100 * time.Millisecond)
		}
		retried := fetchLocalStackJobEventually(t, ctx, backend, queue, throttled.ID)
		if retried.Attempt != 2 {
			t.Fatalf("visibility retry attempt=%d, want 2", retried.Attempt)
		}
		if _, err := backend.Ack(ctx, throttled.ID, nil); err != nil {
			t.Fatalf("ack visibility retry: %v", err)
		}
	})

	t.Run("partial fetch retries conditional-store error", func(t *testing.T) {
		const queue = "partial-store"
		good, err := backend.Push(ctx, &core.Job{Type: "partial.store-good", Args: json.RawMessage(`[]`), Queue: queue})
		if err != nil {
			t.Fatalf("push good: %v", err)
		}
		retry, err := backend.Push(ctx, &core.Job{Type: "partial.store-bad", Args: json.RawMessage(`[]`), Queue: queue})
		if err != nil {
			t.Fatalf("push retry: %v", err)
		}
		useShortPolling(queue)

		faultClient := &localStackFetchFaultSQS{sqsAPI: sqsClient}
		faultClient.receiveFn = func(callCtx context.Context, call int, input *awssqs.ReceiveMessageInput) (*awssqs.ReceiveMessageOutput, error) {
			if call != 1 {
				return sqsClient.ReceiveMessage(callCtx, input)
			}
			response, collectErr := collectLocalStackMessages(callCtx, sqsClient, input, 2)
			if collectErr != nil {
				return nil, collectErr
			}
			orderLocalStackMessages(t, response.Messages, good.ID)
			return response, nil
		}
		faultStore := &localStackClaimFaultStore{DynamoDBStore: store, failJobID: retry.ID}
		faultBackend := newWithSQSClient(faultClient, faultStore, queuePrefix, false)
		faultBackend.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
		faultBackend.queueURLs[queue], err = backend.getQueueURL(ctx, queue)
		if err != nil {
			t.Fatalf("get queue URL: %v", err)
		}

		jobs, err := faultBackend.Fetch(ctx, []string{queue}, 2, "partial-worker", 1_000)
		if err != nil || len(jobs) != 1 || jobs[0].ID != good.ID {
			t.Fatalf("partial store fetch=%v err=%v", jobs, err)
		}
		if !faultStore.failed.Load() {
			t.Fatal("conditional-store fault was not exercised")
		}
		if _, err := backend.Ack(ctx, good.ID, nil); err != nil {
			t.Fatalf("ack good: %v", err)
		}
		retried := fetchLocalStackJobEventually(t, ctx, backend, queue, retry.ID)
		if retried.Attempt != 1 {
			t.Fatalf("store retry attempt=%d, want 1", retried.Attempt)
		}
		if _, err := backend.Ack(ctx, retry.ID, nil); err != nil {
			t.Fatalf("ack store retry: %v", err)
		}
	})

	t.Run("partial fetch returns claims on second receive failure", func(t *testing.T) {
		const queue = "partial-receive"
		job, err := backend.Push(ctx, &core.Job{Type: "partial.receive", Args: json.RawMessage(`[]`), Queue: queue})
		if err != nil {
			t.Fatalf("push: %v", err)
		}
		useShortPolling(queue)

		faultClient := &localStackFetchFaultSQS{sqsAPI: sqsClient}
		faultClient.receiveFn = func(callCtx context.Context, call int, input *awssqs.ReceiveMessageInput) (*awssqs.ReceiveMessageOutput, error) {
			if call == 1 {
				return collectLocalStackMessages(callCtx, sqsClient, input, 1)
			}
			return nil, errors.New("injected second receive failure")
		}
		faultBackend := newWithSQSClient(faultClient, store, queuePrefix, false)
		faultBackend.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
		faultBackend.queueURLs[queue], err = backend.getQueueURL(ctx, queue)
		if err != nil {
			t.Fatalf("get queue URL: %v", err)
		}

		jobs, err := faultBackend.Fetch(ctx, []string{queue}, 2, "partial-worker", 1_000)
		if err != nil || len(jobs) != 1 || jobs[0].ID != job.ID {
			t.Fatalf("partial receive fetch=%v err=%v", jobs, err)
		}
		if _, ackErr := backend.Ack(ctx, job.ID, nil); ackErr != nil {
			t.Fatalf("ack returned job: %v", ackErr)
		}
		time.Sleep(1100 * time.Millisecond)
		duplicates, err := backend.Fetch(ctx, []string{queue}, 1, "duplicate-worker", 1_000)
		if err != nil || len(duplicates) != 0 {
			t.Fatalf("returned job was delivered twice: jobs=%v err=%v", duplicates, err)
		}
	})

	t.Run("native redrive policy is absent", func(t *testing.T) {
		queueURL, err := backend.getQueueURL(ctx, "claims")
		if err != nil {
			t.Fatalf("get queue: %v", err)
		}
		attributes, err := sqsClient.GetQueueAttributes(ctx, &awssqs.GetQueueAttributesInput{
			QueueUrl:       aws.String(queueURL),
			AttributeNames: []sqstypes.QueueAttributeName{sqstypes.QueueAttributeNameRedrivePolicy},
		})
		if err != nil {
			t.Fatalf("get queue attributes: %v", err)
		}
		if policy := attributes.Attributes["RedrivePolicy"]; policy != "" {
			t.Fatalf("unexpected native redrive policy: %s", policy)
		}
	})
}
