package sqs

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/openjobspec/ojs-backend-sqs/internal/core"
	"github.com/openjobspec/ojs-backend-sqs/internal/state"
)

// CreateWorkflow creates and starts a workflow.
func (b *SQSBackend) CreateWorkflow(ctx context.Context, req *core.WorkflowRequest) (*core.Workflow, error) {
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
		data, _ := json.Marshal(req.Callbacks)
		callbacksJSON = string(data)
	}

	jobDefsJSON, _ := json.Marshal(jobs)

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
		// Chain: only enqueue the first step
		step := jobs[0]
		queue := "default"
		if step.Options != nil && step.Options.Queue != "" {
			queue = step.Options.Queue
		}

		job := &core.Job{
			Type:         step.Type,
			Args:         step.Args,
			Queue:        queue,
			WorkflowID:   wfID,
			WorkflowStep: 0,
		}
		if step.Options != nil && step.Options.RetryPolicy != nil {
			job.Retry = step.Options.RetryPolicy
			job.MaxAttempts = &step.Options.RetryPolicy.MaxAttempts
		} else if step.Options != nil && step.Options.Retry != nil {
			job.Retry = step.Options.Retry
			job.MaxAttempts = &step.Options.Retry.MaxAttempts
		}

		created, err := b.Push(ctx, job)
		if err != nil {
			return nil, err
		}

		// Store job ID in workflow's job list
		b.store.AddWorkflowJob(ctx, wfID, created.ID)
	} else {
		// Group/Batch: enqueue all jobs immediately
		for i, step := range jobs {
			queue := "default"
			if step.Options != nil && step.Options.Queue != "" {
				queue = step.Options.Queue
			}

			job := &core.Job{
				Type:         step.Type,
				Args:         step.Args,
				Queue:        queue,
				WorkflowID:   wfID,
				WorkflowStep: i,
			}
			if step.Options != nil && step.Options.RetryPolicy != nil {
				job.Retry = step.Options.RetryPolicy
				job.MaxAttempts = &step.Options.RetryPolicy.MaxAttempts
			} else if step.Options != nil && step.Options.Retry != nil {
				job.Retry = step.Options.Retry
				job.MaxAttempts = &step.Options.Retry.MaxAttempts
			}

			created, err := b.Push(ctx, job)
			if err != nil {
				return nil, err
			}

			b.store.AddWorkflowJob(ctx, wfID, created.ID)
		}
	}

	return wf, nil
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

	// Cancel all jobs belonging to this workflow
	jobIDs, _ := b.store.GetWorkflowJobs(ctx, id)
	for _, jobID := range jobIDs {
		record, _ := b.store.GetJob(ctx, jobID)
		if record != nil && !core.IsTerminalState(record.State) {
			b.Cancel(ctx, jobID)
		}
	}

	wf.State = "cancelled"
	wf.CompletedAt = core.NowFormatted()

	updates := map[string]any{
		"state":        "cancelled",
		"completed_at": wf.CompletedAt,
	}
	b.store.UpdateWorkflow(ctx, id, updates)

	return wf, nil
}

// AdvanceWorkflow is called after ACK or NACK to update workflow state.
func (b *SQSBackend) AdvanceWorkflow(ctx context.Context, workflowID string, jobID string, result json.RawMessage, failed bool) error {
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

	// Store the result for chain result passing
	if result != nil && len(result) > 0 {
		b.store.SetWorkflowResult(ctx, workflowID, stepIdx, string(result))
	}

	completed := record.Completed
	failedCount := record.Failed

	if failed {
		failedCount++
	} else {
		completed++
	}

	// Track total finished (completed + failed) for determining when all jobs are done
	totalFinished := completed + failedCount

	updates := map[string]any{
		"completed": completed,
		"failed":    failedCount,
	}

	if record.Type == "chain" {
		if failed {
			// Chain stops on failure
			updates["state"] = "failed"
			updates["completed_at"] = core.NowFormatted()
			b.store.UpdateWorkflow(ctx, workflowID, updates)
			return nil
		}

		if totalFinished >= record.Total {
			// Chain complete
			updates["state"] = "completed"
			updates["completed_at"] = core.NowFormatted()
			b.store.UpdateWorkflow(ctx, workflowID, updates)
			return nil
		}

		// Enqueue next step
		b.store.UpdateWorkflow(ctx, workflowID, updates)
		return b.enqueueChainStep(ctx, workflowID, record, stepIdx+1)
	}

	// Group/Batch: check if all jobs are done
	b.store.UpdateWorkflow(ctx, workflowID, updates)

	if totalFinished >= record.Total {
		finalState := "completed"
		if failedCount > 0 {
			finalState = "failed"
		}
		finalUpdates := map[string]any{
			"state":        finalState,
			"completed_at": core.NowFormatted(),
		}
		b.store.UpdateWorkflow(ctx, workflowID, finalUpdates)

		// Fire batch callbacks
		if record.Type == "batch" {
			b.fireBatchCallbacks(ctx, workflowID, record, failedCount > 0)
		}
	}

	return nil
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
	queue := "default"
	if step.Options != nil && step.Options.Queue != "" {
		queue = step.Options.Queue
	}

	// Collect parent results from previous steps
	var parentResults []json.RawMessage
	resultsMap, _ := b.store.GetWorkflowResults(ctx, workflowID)
	for i := 0; i < stepIdx; i++ {
		if r, ok := resultsMap[i]; ok {
			parentResults = append(parentResults, json.RawMessage(r))
		}
	}

	job := &core.Job{
		Type:          step.Type,
		Args:          step.Args,
		Queue:         queue,
		WorkflowID:    workflowID,
		WorkflowStep:  stepIdx,
		ParentResults: parentResults,
	}
	if step.Options != nil && step.Options.RetryPolicy != nil {
		job.Retry = step.Options.RetryPolicy
		job.MaxAttempts = &step.Options.RetryPolicy.MaxAttempts
	} else if step.Options != nil && step.Options.Retry != nil {
		job.Retry = step.Options.Retry
		job.MaxAttempts = &step.Options.Retry.MaxAttempts
	}

	created, err := b.Push(ctx, job)
	if err != nil {
		return err
	}

	b.store.AddWorkflowJob(ctx, workflowID, created.ID)
	return nil
}

// fireBatchCallbacks fires callback jobs based on batch outcome.
func (b *SQSBackend) fireBatchCallbacks(ctx context.Context, workflowID string, wfRecord *state.WorkflowRecord, hasFailure bool) {
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
	b.AdvanceWorkflow(ctx, record.WorkflowID, jobID, json.RawMessage(result), failed)
}
