package sqs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/openjobspec/ojs-backend-sqs/internal/core"
	"github.com/openjobspec/ojs-backend-sqs/internal/state"
)

// CreateWorkflow creates and starts a workflow.
func (b *SQSBackend) CreateWorkflow(ctx context.Context, req *core.WorkflowRequest) (*core.Workflow, error) {
	if err := validateWorkflowRequest(req); err != nil {
		return nil, err
	}
	if store, ok := b.store.(state.WorkflowReliabilityStore); ok {
		return b.createWorkflowReliable(ctx, store, req)
	}

	now := time.Now()
	wfID := core.NewUUIDv7()

	// Determine job list (chain uses Steps, group/batch uses Jobs)
	jobs := req.Jobs
	if req.Type == "chain" {
		jobs = req.Steps
	}

	total := len(jobs)

	// Build the workflow response
	wf := &core.Workflow{
		ID:        wfID,
		Name:      req.Name,
		Type:      req.Type,
		State:     "running",
		CreatedAt: core.FormatTime(now),
	}

	if req.Type == "chain" {
		wf.StepsTotal = &total
		zero := 0
		wf.StepsCompleted = &zero
	} else {
		wf.JobsTotal = &total
		zero := 0
		wf.JobsCompleted = &zero
	}

	// Store workflow metadata
	var callbacksJSON string
	if req.Callbacks != nil {
		data, err := json.Marshal(req.Callbacks)
		if err != nil {
			slog.Warn("create workflow: failed to marshal callbacks", "error", err)
		} else {
			callbacksJSON = string(data)
		}
	}

	jobDefsJSON, err := json.Marshal(jobs)
	if err != nil {
		slog.Warn("create workflow: failed to marshal job definitions", "error", err)
	}

	record := &state.WorkflowRecord{
		ID:        wfID,
		SK:        "WORKFLOW",
		Type:      req.Type,
		Name:      req.Name,
		State:     "running",
		Total:     total,
		Completed: 0,
		Failed:    0,
		CreatedAt: core.FormatTime(now),
		Callbacks: callbacksJSON,
		JobDefs:   string(jobDefsJSON),
	}

	if err := b.store.PutWorkflow(ctx, record); err != nil {
		return nil, err
	}

	if req.Type == "chain" {
		// Chain: only enqueue the first step.
		created, err := b.Push(ctx, buildWorkflowJob(jobs[0], wfID, 0))
		if err != nil {
			return nil, err
		}
		b.trackWorkflowJob(ctx, wfID, created.ID)
	} else {
		// Group/Batch: enqueue all jobs immediately.
		for i, step := range jobs {
			created, err := b.Push(ctx, buildWorkflowJob(step, wfID, i))
			if err != nil {
				return nil, err
			}
			b.trackWorkflowJob(ctx, wfID, created.ID)
		}
	}

	return wf, nil
}

func validateWorkflowRequest(req *core.WorkflowRequest) error {
	if req == nil {
		return core.NewInvalidRequestError("Workflow request is required.", nil)
	}
	var jobs []core.WorkflowJobRequest
	switch req.Type {
	case "chain":
		jobs = req.Steps
	case "group", "batch":
		jobs = req.Jobs
	default:
		return core.NewInvalidRequestError("Workflow type must be chain, group, or batch.", map[string]any{"type": req.Type})
	}
	if len(jobs) == 0 {
		return core.NewInvalidRequestError("Workflow must contain at least one job.", nil)
	}
	if req.Type != "chain" && len(jobs) > 99 {
		return core.NewInvalidRequestError("Workflow contains too many initial jobs.", map[string]any{"maximum": 99})
	}
	for i, job := range jobs {
		if job.Type == "" {
			return core.NewInvalidRequestError("Workflow job type is required.", map[string]any{"index": i})
		}
	}
	return nil
}

func (b *SQSBackend) createWorkflowReliable(ctx context.Context, store state.WorkflowReliabilityStore, req *core.WorkflowRequest) (*core.Workflow, error) {
	now := time.Now()
	wfID := core.NewUUIDv7()
	jobs := req.Jobs
	if req.Type == "chain" {
		jobs = req.Steps
	}

	callbacksJSON, err := json.Marshal(req.Callbacks)
	if err != nil {
		return nil, fmt.Errorf("marshal workflow callbacks: %w", err)
	}
	jobDefsJSON, err := json.Marshal(jobs)
	if err != nil {
		return nil, fmt.Errorf("marshal workflow jobs: %w", err)
	}
	record := &state.WorkflowRecord{
		ID:          wfID,
		SK:          "WORKFLOW",
		Type:        req.Type,
		Name:        req.Name,
		State:       "running",
		Total:       len(jobs),
		CreatedAt:   core.FormatTime(now),
		Callbacks:   string(callbacksJSON),
		JobDefs:     string(jobDefsJSON),
		CurrentStep: 0,
		Version:     1,
	}

	initial := jobs
	if req.Type == "chain" {
		initial = jobs[:1]
	}
	effects := make([]*state.JobEffectRecord, 0, len(initial))
	for i, step := range initial {
		job := buildWorkflowJob(step, wfID, i)
		job.ID = core.NewUUIDv7()
		effect, err := newJobEffect("workflow_step", wfID, fmt.Sprintf("step:%d", i), job)
		if err != nil {
			return nil, err
		}
		effects = append(effects, effect)
	}
	createPlan := &state.WorkflowCreatePlan{
		Workflow: record,
		Effects:  effects,
	}
	if err := store.CreateWorkflowAtomic(ctx, createPlan); err != nil {
		existing, getErr := b.store.GetWorkflow(ctx, wfID)
		if getErr != nil || existing.CreatedAt != record.CreatedAt {
			return nil, err
		}
	}

	b.drainJobEffectsBestEffort(ctx)
	total := len(jobs)
	zero := 0
	wf := &core.Workflow{
		ID:        wfID,
		Name:      req.Name,
		Type:      req.Type,
		State:     "running",
		CreatedAt: record.CreatedAt,
	}
	if req.Type == "chain" {
		wf.StepsTotal = &total
		wf.StepsCompleted = &zero
	} else {
		wf.JobsTotal = &total
		wf.JobsCompleted = &zero
	}
	return wf, nil
}

func newJobEffect(kind, ownerID, suffix string, job *core.Job) (*state.JobEffectRecord, error) {
	data, err := json.Marshal(job)
	if err != nil {
		return nil, fmt.Errorf("marshal job effect: %w", err)
	}
	effectID := ownerID + ":" + suffix
	return &state.JobEffectRecord{
		PK:           "EFFECT#" + effectID,
		SK:           "JOB_EFFECT",
		ID:           effectID,
		Kind:         kind,
		OwnerID:      ownerID,
		JobID:        job.ID,
		JobJSON:      string(data),
		WorkflowID:   job.WorkflowID,
		WorkflowStep: job.WorkflowStep,
		CreatedAt:    core.NowFormatted(),
	}, nil
}

func newWorkflowAdvanceIntent(record *state.JobRecord, result []byte, failed bool) *state.WorkflowAdvanceIntent {
	if record == nil || record.WorkflowID == "" {
		return nil
	}
	return &state.WorkflowAdvanceIntent{
		PK:         "WORKFLOW_ADVANCE#" + record.WorkflowID + "#" + record.ID,
		SK:         "WORKFLOW_ADVANCE",
		WorkflowID: record.WorkflowID,
		JobID:      record.ID,
		Result:     string(result),
		Failed:     failed,
		CreatedAt:  core.NowFormatted(),
	}
}

// buildWorkflowJob constructs a job for a workflow step, resolving the queue and
// retry policy from the step options.
func buildWorkflowJob(step core.WorkflowJobRequest, workflowID string, stepIdx int) *core.Job {
	queue := "default"
	if step.Options != nil && step.Options.Queue != "" {
		queue = step.Options.Queue
	}

	job := &core.Job{
		Type:         step.Type,
		Args:         step.Args,
		Queue:        queue,
		WorkflowID:   workflowID,
		WorkflowStep: stepIdx,
	}
	if step.Options != nil {
		job.Priority = step.Options.Priority
		job.TimeoutMs = step.Options.TimeoutMs
		job.ScheduledAt = step.Options.ScheduledAt
		if job.ScheduledAt == "" {
			job.ScheduledAt = step.Options.DelayUntil
		}
		job.ExpiresAt = step.Options.ExpiresAt
		job.Unique = step.Options.Unique
		job.Tags = append([]string(nil), step.Options.Tags...)
		job.VisibilityTimeoutMs = step.Options.VisibilityTimeoutMs
		job.Meta = step.Options.Metadata
		if step.Options.RetryPolicy != nil {
			job.Retry = step.Options.RetryPolicy
			job.MaxAttempts = &step.Options.RetryPolicy.MaxAttempts
		} else if step.Options.Retry != nil {
			job.Retry = step.Options.Retry
			job.MaxAttempts = &step.Options.Retry.MaxAttempts
		}
	}
	return job
}

// trackWorkflowJob records a job in a workflow's job list (best-effort; the job
// is already enqueued).
func (b *SQSBackend) trackWorkflowJob(ctx context.Context, workflowID, jobID string) {
	if err := b.store.AddWorkflowJob(ctx, workflowID, jobID); err != nil {
		slog.Warn("workflow: failed to track job", "workflow_id", workflowID, "job_id", jobID, "error", err)
	}
}

// GetWorkflow retrieves a workflow by ID.
func (b *SQSBackend) GetWorkflow(ctx context.Context, id string) (*core.Workflow, error) {
	record, err := b.store.GetWorkflow(ctx, id)
	if err != nil {
		return nil, core.NewNotFoundError("Workflow", id)
	}

	wf := &core.Workflow{
		ID:        id,
		Name:      record.Name,
		Type:      record.Type,
		State:     record.State,
		CreatedAt: record.CreatedAt,
	}
	if record.CompletedAt != "" {
		wf.CompletedAt = record.CompletedAt
	}

	if wf.Type == "chain" {
		wf.StepsTotal = &record.Total
		wf.StepsCompleted = &record.Completed
	} else {
		wf.JobsTotal = &record.Total
		wf.JobsCompleted = &record.Completed
	}

	return wf, nil
}

// CancelWorkflow cancels a workflow and its active/pending jobs.
func (b *SQSBackend) CancelWorkflow(ctx context.Context, id string) (*core.Workflow, error) {
	wf, err := b.GetWorkflow(ctx, id)
	if err != nil {
		return nil, err
	}

	if wf.State == "completed" || wf.State == "failed" || wf.State == "cancelled" {
		return nil, core.NewConflictError(
			"Cannot cancel workflow in state '"+wf.State+"'.",
			nil,
		)
	}

	if store, ok := b.store.(state.WorkflowReliabilityStore); ok {
		return b.cancelWorkflowReliable(ctx, store, wf)
	}

	// Cancel all jobs belonging to this workflow (best-effort).
	if err := b.cancelWorkflowJobs(ctx, id); err != nil {
		slog.Warn("cancel workflow: failed to list jobs", "workflow_id", id, "error", err)
	}

	wf.State = "cancelled"
	wf.CompletedAt = core.NowFormatted()

	updates := map[string]any{
		"state":        "cancelled",
		"completed_at": wf.CompletedAt,
	}
	if err := b.store.UpdateWorkflow(ctx, id, updates); err != nil {
		return nil, fmt.Errorf("cancel workflow: %w", err)
	}

	return wf, nil
}

func (b *SQSBackend) cancelWorkflowReliable(ctx context.Context, store state.WorkflowReliabilityStore, wf *core.Workflow) (*core.Workflow, error) {
	completedAt := core.NowFormatted()
	if err := store.CancelWorkflowAtomic(ctx, wf.ID, completedAt); err != nil {
		if errors.Is(err, state.ErrConditionFailed) {
			return nil, core.NewConflictError("Workflow was already finalized by another caller.", nil)
		}
		current, getErr := b.store.GetWorkflow(ctx, wf.ID)
		if getErr != nil || current.State != "cancelled" {
			return nil, err
		}
	}
	wf.State = "cancelled"
	wf.CompletedAt = completedAt
	if err := b.cancelWorkflowJobs(ctx, wf.ID); err != nil {
		return nil, err
	}
	b.drainJobEffectsBestEffort(ctx)
	return wf, nil
}

func (b *SQSBackend) cancelWorkflowJobs(ctx context.Context, workflowID string) error {
	jobIDs, err := b.store.GetWorkflowJobs(ctx, workflowID)
	if err != nil {
		return err
	}
	for _, jobID := range jobIDs {
		record, getErr := b.store.GetJob(ctx, jobID)
		if getErr != nil || core.IsTerminalState(record.State) {
			continue
		}
		if _, cancelErr := b.Cancel(ctx, jobID); cancelErr != nil {
			slog.Warn("cancel workflow: failed to cancel job", "workflow_id", workflowID, "job_id", jobID, "error", cancelErr)
		}
	}
	return nil
}

// AdvanceWorkflow is called after ACK or NACK to update workflow state.
func (b *SQSBackend) AdvanceWorkflow(ctx context.Context, workflowID string, jobID string, result json.RawMessage, failed bool) error {
	if store, ok := b.store.(state.WorkflowReliabilityStore); ok {
		return b.advanceWorkflowReliable(ctx, store, workflowID, jobID, result, failed)
	}

	record, err := b.store.GetWorkflow(ctx, workflowID)
	if err != nil {
		return nil
	}

	if record.State != "running" {
		return nil
	}

	// Get the job's workflow step index
	jobRecord, _ := b.store.GetJob(ctx, jobID)
	if jobRecord == nil {
		return nil
	}
	stepIdx := jobRecord.WorkflowStep

	// Store the result for chain result passing (best-effort).
	if len(result) > 0 {
		if err := b.store.SetWorkflowResult(ctx, workflowID, stepIdx, string(result)); err != nil {
			slog.Warn("workflow: failed to store step result", "workflow_id", workflowID, "step", stepIdx, "error", err)
		}
	}

	completed := record.Completed
	failedCount := record.Failed

	if failed {
		failedCount++
	} else {
		completed++
	}

	// Track total finished (completed + failed) for determining when all jobs are done.
	totalFinished := completed + failedCount

	updates := map[string]any{
		"completed": completed,
		"failed":    failedCount,
	}

	if record.Type == "chain" {
		if failed {
			// Chain stops on failure.
			updates["state"] = "failed"
			updates["completed_at"] = core.NowFormatted()
			b.updateWorkflow(ctx, workflowID, updates)
			return nil
		}

		if totalFinished >= record.Total {
			// Chain complete.
			updates["state"] = "completed"
			updates["completed_at"] = core.NowFormatted()
			b.updateWorkflow(ctx, workflowID, updates)
			return nil
		}

		// Enqueue next step.
		b.updateWorkflow(ctx, workflowID, updates)
		return b.enqueueChainStep(ctx, workflowID, record, stepIdx+1)
	}

	// Group/Batch: check if all jobs are done.
	b.updateWorkflow(ctx, workflowID, updates)

	if totalFinished >= record.Total {
		finalState := "completed"
		if failedCount > 0 {
			finalState = "failed"
		}
		b.updateWorkflow(ctx, workflowID, map[string]any{
			"state":        finalState,
			"completed_at": core.NowFormatted(),
		})

		// Fire batch callbacks.
		if record.Type == "batch" {
			b.fireBatchCallbacks(ctx, record, failedCount > 0)
		}
	}

	return nil
}

func (b *SQSBackend) advanceWorkflowReliable(ctx context.Context, store state.WorkflowReliabilityStore, workflowID, jobID string, result json.RawMessage, failed bool) error {
	const retryLimit = 256
	for attempt := 0; attempt < retryLimit; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		record, jobRecord, done, err := b.loadWorkflowAdvance(ctx, store, workflowID, jobID)
		if err != nil || done {
			return err
		}
		plan, err := b.buildWorkflowAdvancePlan(ctx, record, jobRecord, result, failed)
		if err != nil {
			return err
		}
		retry, err := b.commitWorkflowAdvance(ctx, store, plan)
		if err != nil {
			return err
		}
		if !retry {
			return nil
		}
	}
	return fmt.Errorf("workflow %s remained contended after %d retries", workflowID, retryLimit)
}

func (b *SQSBackend) loadWorkflowAdvance(ctx context.Context, store state.WorkflowReliabilityStore, workflowID, jobID string) (*state.WorkflowRecord, *state.JobRecord, bool, error) {
	record, err := b.store.GetWorkflow(ctx, workflowID)
	if err != nil {
		return nil, nil, false, err
	}
	if record.State != "running" {
		return nil, nil, true, nil
	}
	jobRecord, err := b.store.GetJob(ctx, jobID)
	if err != nil {
		return nil, nil, false, err
	}
	if jobRecord.WorkflowID != workflowID {
		return nil, nil, true, nil
	}
	if record.Type != "chain" || jobRecord.WorkflowStep == record.CurrentStep {
		return record, jobRecord, false, nil
	}
	applied, err := store.IsWorkflowCompletionApplied(ctx, workflowID, jobID)
	if err != nil || applied {
		return nil, nil, applied, err
	}
	return nil, nil, false, fmt.Errorf("workflow chain step %d is not current step %d", jobRecord.WorkflowStep, record.CurrentStep)
}

func (b *SQSBackend) buildWorkflowAdvancePlan(ctx context.Context, record *state.WorkflowRecord, jobRecord *state.JobRecord, result json.RawMessage, failed bool) (*state.WorkflowAdvancePlan, error) {
	plan := &state.WorkflowAdvancePlan{
		WorkflowID:          record.ID,
		JobID:               jobRecord.ID,
		StepIndex:           jobRecord.WorkflowStep,
		ExpectedCompleted:   record.Completed,
		ExpectedFailed:      record.Failed,
		ExpectedCurrentStep: record.CurrentStep,
		MatchCurrentStep:    record.Type == "chain",
		Completed:           record.Completed,
		Failed:              record.Failed,
		CurrentStep:         record.CurrentStep,
		State:               "running",
		Result:              string(result),
	}
	if failed {
		plan.Failed++
	} else {
		plan.Completed++
	}
	switch record.Type {
	case "chain":
		return b.buildChainAdvancePlan(ctx, record, plan, result, failed)
	case "group", "batch":
		return buildParallelAdvancePlan(record, plan)
	default:
		return nil, fmt.Errorf("unsupported stored workflow type %q", record.Type)
	}
}

func (b *SQSBackend) buildChainAdvancePlan(ctx context.Context, record *state.WorkflowRecord, plan *state.WorkflowAdvancePlan, result json.RawMessage, failed bool) (*state.WorkflowAdvancePlan, error) {
	if failed {
		plan.State = "failed"
		plan.CompletedAt = core.NowFormatted()
		return plan, nil
	}
	if plan.Completed+plan.Failed >= record.Total {
		plan.State = "completed"
		plan.CompletedAt = core.NowFormatted()
		return plan, nil
	}
	plan.CurrentStep = record.CurrentStep + 1
	effect, err := b.nextChainEffect(ctx, record, record.ID, plan.CurrentStep, result)
	if err != nil {
		return nil, err
	}
	plan.Effects = []*state.JobEffectRecord{effect}
	return plan, nil
}

func buildParallelAdvancePlan(record *state.WorkflowRecord, plan *state.WorkflowAdvancePlan) (*state.WorkflowAdvancePlan, error) {
	if plan.Completed+plan.Failed < record.Total {
		return plan, nil
	}
	plan.State = "completed"
	if plan.Failed > 0 {
		plan.State = "failed"
	}
	plan.CompletedAt = core.NowFormatted()
	if record.Type != "batch" {
		return plan, nil
	}
	effects, err := batchCallbackEffects(record, record.ID, plan.Failed > 0)
	if err != nil {
		return nil, err
	}
	plan.Effects = effects
	return plan, nil
}

func (b *SQSBackend) commitWorkflowAdvance(ctx context.Context, store state.WorkflowReliabilityStore, plan *state.WorkflowAdvancePlan) (bool, error) {
	err := store.AdvanceWorkflowAtomic(ctx, plan)
	if err == nil {
		b.drainJobEffectsBestEffort(ctx)
		return false, nil
	}
	if errors.Is(err, state.ErrAlreadyApplied) {
		return false, nil
	}
	applied, checkErr := store.IsWorkflowCompletionApplied(ctx, plan.WorkflowID, plan.JobID)
	if checkErr == nil && applied {
		b.drainJobEffectsBestEffort(ctx)
		return false, nil
	}
	if errors.Is(err, state.ErrConditionFailed) {
		return true, nil
	}
	return false, err
}

func (b *SQSBackend) nextChainEffect(ctx context.Context, record *state.WorkflowRecord, workflowID string, stepIdx int, currentResult json.RawMessage) (*state.JobEffectRecord, error) {
	var definitions []core.WorkflowJobRequest
	if err := json.Unmarshal([]byte(record.JobDefs), &definitions); err != nil {
		return nil, fmt.Errorf("decode workflow definitions: %w", err)
	}
	if stepIdx >= len(definitions) {
		return nil, fmt.Errorf("workflow step %d is outside %d definitions", stepIdx, len(definitions))
	}

	results, err := b.store.GetWorkflowResults(ctx, workflowID)
	if err != nil {
		return nil, err
	}
	if len(currentResult) > 0 {
		results[stepIdx-1] = string(currentResult)
	}
	parentResults := make([]json.RawMessage, 0, stepIdx)
	for i := 0; i < stepIdx; i++ {
		if value, ok := results[i]; ok {
			parentResults = append(parentResults, json.RawMessage(value))
		}
	}

	job := buildWorkflowJob(definitions[stepIdx], workflowID, stepIdx)
	job.ID = core.NewUUIDv7()
	job.ParentResults = parentResults
	return newJobEffect("workflow_step", workflowID, fmt.Sprintf("step:%d", stepIdx), job)
}

func batchCallbackEffects(record *state.WorkflowRecord, workflowID string, hasFailure bool) ([]*state.JobEffectRecord, error) {
	if record.Callbacks == "" || record.Callbacks == "null" {
		return nil, nil
	}
	var callbacks core.WorkflowCallbacks
	if err := json.Unmarshal([]byte(record.Callbacks), &callbacks); err != nil {
		return nil, fmt.Errorf("decode workflow callbacks: %w", err)
	}

	var selected []struct {
		name string
		cb   *core.WorkflowCallback
	}
	if callbacks.OnComplete != nil {
		selected = append(selected, struct {
			name string
			cb   *core.WorkflowCallback
		}{"on_complete", callbacks.OnComplete})
	}
	if hasFailure && callbacks.OnFailure != nil {
		selected = append(selected, struct {
			name string
			cb   *core.WorkflowCallback
		}{"on_failure", callbacks.OnFailure})
	}
	if !hasFailure && callbacks.OnSuccess != nil {
		selected = append(selected, struct {
			name string
			cb   *core.WorkflowCallback
		}{"on_success", callbacks.OnSuccess})
	}

	effects := make([]*state.JobEffectRecord, 0, len(selected))
	for _, selectedCallback := range selected {
		job := callbackJob(selectedCallback.cb)
		job.ID = core.NewUUIDv7()
		effect, err := newJobEffect("workflow_callback", workflowID, "callback:"+selectedCallback.name, job)
		if err != nil {
			return nil, err
		}
		effects = append(effects, effect)
	}
	return effects, nil
}

func callbackJob(cb *core.WorkflowCallback) *core.Job {
	queue := "default"
	if cb.Options != nil && cb.Options.Queue != "" {
		queue = cb.Options.Queue
	}
	job := &core.Job{Type: cb.Type, Args: cb.Args, Queue: queue}
	if cb.Options != nil {
		job.Priority = cb.Options.Priority
		job.TimeoutMs = cb.Options.TimeoutMs
		job.ScheduledAt = cb.Options.ScheduledAt
		if job.ScheduledAt == "" {
			job.ScheduledAt = cb.Options.DelayUntil
		}
		job.ExpiresAt = cb.Options.ExpiresAt
		job.Unique = cb.Options.Unique
		job.Tags = append([]string(nil), cb.Options.Tags...)
		job.VisibilityTimeoutMs = cb.Options.VisibilityTimeoutMs
		job.Meta = cb.Options.Metadata
		if cb.Options.RetryPolicy != nil {
			job.Retry = cb.Options.RetryPolicy
			job.MaxAttempts = &cb.Options.RetryPolicy.MaxAttempts
		} else if cb.Options.Retry != nil {
			job.Retry = cb.Options.Retry
			job.MaxAttempts = &cb.Options.Retry.MaxAttempts
		}
	}
	return job
}

// DrainJobEffects materializes durable workflow/cron effects as jobs. The
// effect is retired only after the job and its delivery marker/intent commit.
func (b *SQSBackend) DrainJobEffects(ctx context.Context) error {
	effectStore, ok := b.store.(state.WorkflowReliabilityStore)
	if !ok {
		return nil
	}
	jobStore, ok := b.reliabilityStore()
	if !ok {
		return nil
	}

	effects, err := effectStore.ListJobEffects(ctx, outboxDrainLimit)
	if err != nil {
		return err
	}
	var firstErr error
	for _, effect := range effects {
		if err := b.materializeJobEffect(ctx, effectStore, jobStore, effect); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if err := b.DrainDeliveryOutbox(ctx); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// DrainWorkflowAdvancements applies durable terminal-job workflow accounting.
// Completion deduplication makes retries safe across crashes and replicas.
func (b *SQSBackend) DrainWorkflowAdvancements(ctx context.Context) error {
	store, ok := b.store.(state.WorkflowReliabilityStore)
	if !ok {
		return nil
	}
	intents, err := store.ListWorkflowAdvances(ctx, outboxDrainLimit)
	if err != nil {
		return err
	}
	var firstErr error
	for _, intent := range intents {
		if err := b.AdvanceWorkflow(ctx, intent.WorkflowID, intent.JobID, json.RawMessage(intent.Result), intent.Failed); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if err := store.CompleteWorkflowAdvance(ctx, intent); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (b *SQSBackend) materializeJobEffect(ctx context.Context, effectStore state.WorkflowReliabilityStore, jobStore state.JobReliabilityStore, effect *state.JobEffectRecord) error {
	var job core.Job
	if err := json.Unmarshal([]byte(effect.JobJSON), &job); err != nil {
		return fmt.Errorf("decode job effect %s: %w", effect.ID, err)
	}
	job.ID = effect.JobID
	job.WorkflowID = effect.WorkflowID
	job.WorkflowStep = effect.WorkflowStep

	fenced, err := b.workflowEffectIsFenced(ctx, job.WorkflowID)
	if err != nil {
		return err
	}
	if fenced {
		return effectStore.CompleteJobEffect(ctx, effect)
	}

	now := time.Now()
	job.CreatedAt = core.FormatTime(now)
	job.Attempt = 0
	plan, existing, err := b.prepareReliableJobPlan(ctx, jobStore, &job, now)
	if err != nil {
		return err
	}
	if existing != nil {
		return fmt.Errorf("job effect %s resolved to unrelated existing job %s", effect.ID, existing.ID)
	}
	if err := b.createJobReliable(ctx, jobStore, plan); err != nil {
		if _, getErr := b.store.GetJob(ctx, effect.JobID); getErr == nil {
			return effectStore.CompleteJobEffect(ctx, effect)
		}
		fenced, fenceErr := b.workflowEffectIsFenced(ctx, job.WorkflowID)
		if fenceErr != nil {
			return fenceErr
		}
		if fenced {
			return effectStore.CompleteJobEffect(ctx, effect)
		}
		return err
	}
	return effectStore.CompleteJobEffect(ctx, effect)
}

func (b *SQSBackend) workflowEffectIsFenced(ctx context.Context, workflowID string) (bool, error) {
	if workflowID == "" {
		return false, nil
	}
	workflow, err := b.store.GetWorkflow(ctx, workflowID)
	if err != nil {
		return false, err
	}
	return workflow.State != "running", nil
}

func (b *SQSBackend) drainJobEffectsBestEffort(ctx context.Context) {
	if err := b.DrainJobEffects(ctx); err != nil {
		b.logger.Warn("job effect drain deferred", "error", err)
	}
}

// updateWorkflow applies a best-effort workflow record update, logging failures.
func (b *SQSBackend) updateWorkflow(ctx context.Context, workflowID string, updates map[string]any) {
	if err := b.store.UpdateWorkflow(ctx, workflowID, updates); err != nil {
		slog.Warn("workflow: failed to update record", "workflow_id", workflowID, "error", err)
	}
}

// enqueueChainStep enqueues the next step in a chain workflow.
func (b *SQSBackend) enqueueChainStep(ctx context.Context, workflowID string, wfRecord *state.WorkflowRecord, stepIdx int) error {
	// Load job definitions
	var jobDefs []core.WorkflowJobRequest
	if err := json.Unmarshal([]byte(wfRecord.JobDefs), &jobDefs); err != nil {
		return err
	}

	if stepIdx >= len(jobDefs) {
		return nil
	}

	step := jobDefs[stepIdx]

	// Collect parent results from previous steps.
	var parentResults []json.RawMessage
	resultsMap, _ := b.store.GetWorkflowResults(ctx, workflowID)
	for i := 0; i < stepIdx; i++ {
		if r, ok := resultsMap[i]; ok {
			parentResults = append(parentResults, json.RawMessage(r))
		}
	}

	job := buildWorkflowJob(step, workflowID, stepIdx)
	job.ParentResults = parentResults

	created, err := b.Push(ctx, job)
	if err != nil {
		return err
	}

	b.trackWorkflowJob(ctx, workflowID, created.ID)
	return nil
}

// fireBatchCallbacks fires callback jobs based on batch outcome.
func (b *SQSBackend) fireBatchCallbacks(ctx context.Context, wfRecord *state.WorkflowRecord, hasFailure bool) {
	if wfRecord.Callbacks == "" {
		return
	}

	var callbacks core.WorkflowCallbacks
	if err := json.Unmarshal([]byte(wfRecord.Callbacks), &callbacks); err != nil {
		return
	}

	// on_complete always fires
	if callbacks.OnComplete != nil {
		b.fireCallback(ctx, callbacks.OnComplete)
	}

	// on_success fires only when all jobs succeeded
	if !hasFailure && callbacks.OnSuccess != nil {
		b.fireCallback(ctx, callbacks.OnSuccess)
	}

	// on_failure fires when any job failed
	if hasFailure && callbacks.OnFailure != nil {
		b.fireCallback(ctx, callbacks.OnFailure)
	}
}

// fireCallback creates a job from a workflow callback definition.
func (b *SQSBackend) fireCallback(ctx context.Context, cb *core.WorkflowCallback) {
	queue := "default"
	if cb.Options != nil && cb.Options.Queue != "" {
		queue = cb.Options.Queue
	}
	if _, err := b.Push(ctx, &core.Job{
		Type:  cb.Type,
		Args:  cb.Args,
		Queue: queue,
	}); err != nil {
		slog.Error("workflow: error firing callback", "type", cb.Type, "error", err)
	}
}

// advanceWorkflow advances a workflow when a job completes or fails.
func (b *SQSBackend) advanceWorkflow(ctx context.Context, jobID, jobState string, result []byte) {
	// Check if this job belongs to a workflow
	record, _ := b.store.GetJob(ctx, jobID)
	if record == nil || record.WorkflowID == "" {
		return
	}

	failed := jobState == core.StateDiscarded || jobState == core.StateCancelled
	if err := b.AdvanceWorkflow(ctx, record.WorkflowID, jobID, json.RawMessage(result), failed); err != nil {
		slog.Warn("workflow: failed to advance", "workflow_id", record.WorkflowID, "job_id", jobID, "error", err)
		return
	}
	if store, ok := b.store.(state.WorkflowReliabilityStore); ok {
		if err := store.CompleteWorkflowAdvance(ctx, newWorkflowAdvanceIntent(record, result, failed)); err != nil {
			slog.Warn("workflow: failed to retire advancement intent", "workflow_id", record.WorkflowID, "job_id", jobID, "error", err)
		}
	}
}
