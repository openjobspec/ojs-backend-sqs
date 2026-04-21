package sqs

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/openjobspec/ojs-backend-sqs/internal/core"
	"github.com/openjobspec/ojs-backend-sqs/internal/state"
)

// RegisterCron registers a cron job.
func (b *SQSBackend) RegisterCron(ctx context.Context, cronJob *core.CronJob) (*core.CronJob, error) {
	// Use Expression field (or fall back to Schedule)
	expr := cronJob.Expression
	if expr == "" {
		expr = cronJob.Schedule
	}

	schedule, err := parseCronSchedule(expr, cronJob.Timezone)

	if err != nil {
		return nil, core.NewInvalidRequestError(
			fmt.Sprintf("Invalid cron expression: %s", expr),
			map[string]any{"expression": expr, "error": err.Error()},
		)
	}

	now := time.Now()
	cronJob.CreatedAt = core.FormatTime(now)
	cronJob.NextRunAt = core.FormatTime(schedule.Next(now))
	cronJob.Schedule = expr
	cronJob.Expression = expr

	if cronJob.Queue == "" && cronJob.JobTemplate != nil && cronJob.JobTemplate.Options != nil {
		cronJob.Queue = cronJob.JobTemplate.Options.Queue
	}
	if cronJob.Queue == "" {
		cronJob.Queue = "default"
	}
	if cronJob.OverlapPolicy == "" {
		cronJob.OverlapPolicy = "allow"
	}
	cronJob.Enabled = true

	// Convert to storage record
	var jobTemplateJSON string
	if cronJob.JobTemplate != nil {
		data, err := json.Marshal(cronJob.JobTemplate)
		if err != nil {
			slog.Warn("register cron: failed to marshal job template", "name", cronJob.Name, "error", err)
		} else {
			jobTemplateJSON = string(data)
		}
	}

	record := &state.CronRecord{
		PK:            "CRON#" + cronJob.Name,
		SK:            "CRON",
		Name:          cronJob.Name,
		Expression:    cronJob.Expression,
		Timezone:      cronJob.Timezone,
		OverlapPolicy: cronJob.OverlapPolicy,
		Enabled:       cronJob.Enabled,
		JobTemplate:   jobTemplateJSON,
		CreatedAt:     cronJob.CreatedAt,
		NextRunAt:     cronJob.NextRunAt,
		LastRunAt:     cronJob.LastRunAt,
		Queue:         cronJob.Queue,
	}

	if err := b.store.PutCron(ctx, record); err != nil {
		return nil, err
	}

	return cronJob, nil
}

// ListCron lists all registered cron jobs.
func (b *SQSBackend) ListCron(ctx context.Context) ([]*core.CronJob, error) {
	records, err := b.store.ListCrons(ctx)
	if err != nil {
		return nil, err
	}

	var crons []*core.CronJob
	for _, record := range records {
		cj := &core.CronJob{
			Name:          record.Name,
			Expression:    record.Expression,
			Timezone:      record.Timezone,
			OverlapPolicy: record.OverlapPolicy,
			Enabled:       record.Enabled,
			CreatedAt:     record.CreatedAt,
			NextRunAt:     record.NextRunAt,
			LastRunAt:     record.LastRunAt,
			Queue:         record.Queue,
		}

		if record.JobTemplate != "" {
			var template core.CronJobTemplate
			if json.Unmarshal([]byte(record.JobTemplate), &template) == nil {
				cj.JobTemplate = &template
			}
		}

		crons = append(crons, cj)
	}

	sort.Slice(crons, func(i, j int) bool {
		return crons[i].Name < crons[j].Name
	})

	return crons, nil
}

// DeleteCron removes a cron job and returns it.
func (b *SQSBackend) DeleteCron(ctx context.Context, name string) (*core.CronJob, error) {
	record, err := b.store.GetCron(ctx, name)
	if err != nil {
		return nil, core.NewNotFoundError("Cron job", name)
	}

	cj := &core.CronJob{
		Name:          record.Name,
		Expression:    record.Expression,
		Timezone:      record.Timezone,
		OverlapPolicy: record.OverlapPolicy,
		Enabled:       record.Enabled,
		CreatedAt:     record.CreatedAt,
		NextRunAt:     record.NextRunAt,
		LastRunAt:     record.LastRunAt,
	}

	if record.JobTemplate != "" {
		var template core.CronJobTemplate
		if json.Unmarshal([]byte(record.JobTemplate), &template) == nil {
			cj.JobTemplate = &template
		}
	}

	if err := b.store.DeleteCron(ctx, name); err != nil {
		return nil, err
	}

	return cj, nil
}

// cronParser builds the cron expression parser used for scheduling.
func cronParser() cron.Parser {
	return cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)
}

// parseCronSchedule is the single timezone-aware parser used at registration
// and every subsequent advancement. An omitted timezone is explicitly UTC.
func parseCronSchedule(expression, timezone string) (cron.Schedule, error) {
	if timezone == "" {
		timezone = "UTC"
	}
	if _, err := time.LoadLocation(timezone); err != nil {
		return nil, fmt.Errorf("invalid timezone %q: %w", timezone, err)
	}
	return cronParser().Parse("CRON_TZ=" + timezone + " " + expression)
}

// parseCronTime parses a stored cron timestamp, accepting both RFC3339 and the
// OJS time format.
func parseCronTime(value string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t, nil
	}
	return time.Parse(core.TimeFormat, value)
}

// FireCronJobs fires any cron jobs that are due.
func (b *SQSBackend) FireCronJobs(ctx context.Context) error {
	crons, err := b.store.ListCrons(ctx)
	if err != nil {
		return err
	}

	now := time.Now()
	parser := cronParser()

	var firstErr error
	for _, cronRecord := range crons {
		if err := b.fireDueCron(ctx, cronRecord, now, parser); err != nil && firstErr == nil {
			firstErr = err
		}
	}

	return firstErr
}

// fireDueCron evaluates and (if due) fires a single cron job. It returns nil when
// the cron is not due, disabled, locked by another node, or intentionally
// skipped, and the first error encountered otherwise (logging along the way).
func (b *SQSBackend) fireDueCron(ctx context.Context, cronRecord *state.CronRecord, now time.Time, parser cron.Parser) error {
	if !cronRecord.Enabled || cronRecord.NextRunAt == "" {
		return nil
	}

	nextRun, err := parseCronTime(cronRecord.NextRunAt)
	if err != nil {
		b.logger.Warn("invalid cron next_run_at", "cron", cronRecord.Name, "next_run_at", cronRecord.NextRunAt, "error", err)
		return fmt.Errorf("parse cron next_run_at for %s: %w", cronRecord.Name, err)
	}
	if now.Before(nextRun) {
		return nil
	}

	if store, ok := b.store.(state.CronReliabilityStore); ok {
		return b.fireDueCronReliable(ctx, store, cronRecord, nextRun, now)
	}

	// Acquire lock to prevent double-firing across nodes.
	acquired, err := b.store.AcquireCronLock(ctx, cronRecord.Name, nextRun.Unix())
	if err != nil {
		b.logger.Error("failed to acquire cron lock", "cron", cronRecord.Name, "error", err)
		return fmt.Errorf("acquire cron lock %s: %w", cronRecord.Name, err)
	}
	if !acquired {
		return nil
	}

	// Honor the overlap policy before enqueueing a new instance.
	if cronRecord.OverlapPolicy == "skip" {
		if skip, skipErr := b.cronOverlapShouldSkip(ctx, cronRecord, now, parser); skip || skipErr != nil {
			return skipErr
		}
	}

	jobType, args, queue, err := resolveCronJob(cronRecord)
	if err != nil {
		b.logger.Error("failed to decode cron job template", "cron", cronRecord.Name, "error", err)
		return fmt.Errorf("unmarshal cron job template %s: %w", cronRecord.Name, err)
	}
	if jobType == "" {
		return nil
	}

	created, err := b.Push(ctx, &core.Job{Type: jobType, Args: args, Queue: queue})
	if err != nil {
		b.logger.Error("failed to enqueue cron job", "cron", cronRecord.Name, "error", err)
		return fmt.Errorf("enqueue cron job for %s: %w", cronRecord.Name, err)
	}

	// Store instance for overlap tracking.
	if err := b.store.SetCronInstance(ctx, cronRecord.Name, created.ID); err != nil {
		b.logger.Error("failed to set cron instance", "cron", cronRecord.Name, "job_id", created.ID, "error", err)
		return fmt.Errorf("set cron instance %s: %w", cronRecord.Name, err)
	}

	return b.advanceCronSchedule(ctx, cronRecord, now, parser)
}

func (b *SQSBackend) fireDueCronReliable(ctx context.Context, store state.CronReliabilityStore, cronRecord *state.CronRecord, nextRun, now time.Time) error {
	schedule, err := parseCronSchedule(cronRecord.Expression, cronRecord.Timezone)
	if err != nil {
		return fmt.Errorf("parse cron expression for %s: %w", cronRecord.Name, err)
	}

	skip := false
	if cronRecord.OverlapPolicy == "skip" {
		skip, err = b.cronInstanceIsActive(ctx, cronRecord.Name)
		if err != nil {
			return err
		}
	}

	var effect *state.JobEffectRecord
	var instanceJobID string
	if !skip {
		jobType, args, queue, resolveErr := resolveCronJob(cronRecord)
		if resolveErr != nil {
			return fmt.Errorf("unmarshal cron job template %s: %w", cronRecord.Name, resolveErr)
		}
		if jobType != "" {
			job := &core.Job{
				ID:    core.NewUUIDv7(),
				Type:  jobType,
				Args:  args,
				Queue: queue,
			}
			instanceJobID = job.ID
			effect, err = newJobEffect(
				"cron",
				cronRecord.Name,
				fmt.Sprintf("occurrence:%d", nextRun.Unix()),
				job,
			)
			if err != nil {
				return err
			}
		}
	}

	acquired, err := store.ClaimCronOccurrence(ctx, &state.CronOccurrencePlan{
		Name:              cronRecord.Name,
		ExpectedNextRunAt: cronRecord.NextRunAt,
		NextRunAt:         core.FormatTime(schedule.Next(now)),
		LastRunAt:         core.FormatTime(now),
		OccurrenceUnix:    nextRun.Unix(),
		InstanceJobID:     instanceJobID,
		Effect:            effect,
	})
	if err != nil || !acquired {
		return err
	}
	b.drainJobEffectsBestEffort(ctx)
	return nil
}

func (b *SQSBackend) cronInstanceIsActive(ctx context.Context, name string) (bool, error) {
	instanceJobID, err := b.store.GetCronInstance(ctx, name)
	if err != nil {
		return false, fmt.Errorf("get cron instance %s: %w", name, err)
	}
	if instanceJobID == "" {
		return false, nil
	}
	record, err := b.store.GetJob(ctx, instanceJobID)
	if err != nil {
		// The durable occurrence effect may not have materialized its job yet.
		// Treat it as active so overlap=skip cannot create a second occurrence.
		return true, nil
	}
	return !core.IsTerminalState(record.State), nil
}

// cronOverlapShouldSkip reports whether a due cron with overlap policy "skip"
// must be skipped because its previous instance is still running. When skipping,
// it advances the schedule so the cron does not fire immediately next tick.
func (b *SQSBackend) cronOverlapShouldSkip(ctx context.Context, cronRecord *state.CronRecord, now time.Time, parser cron.Parser) (bool, error) {
	instanceJobID, err := b.store.GetCronInstance(ctx, cronRecord.Name)
	if err != nil {
		b.logger.Error("failed to get cron instance", "cron", cronRecord.Name, "error", err)
		return true, fmt.Errorf("get cron instance %s: %w", cronRecord.Name, err)
	}
	if instanceJobID == "" {
		return false, nil
	}

	record, err := b.store.GetJob(ctx, instanceJobID)
	if err != nil {
		b.logger.Error("failed to load cron instance job", "cron", cronRecord.Name, "job_id", instanceJobID, "error", err)
		return true, fmt.Errorf("get cron instance job %s: %w", instanceJobID, err)
	}
	if record == nil || core.IsTerminalState(record.State) {
		// Previous instance finished; proceed with firing.
		return false, nil
	}

	// Previous instance still running: skip but advance next_run_at.
	return true, b.advanceSkippedCronSchedule(ctx, cronRecord, now, parser)
}

// resolveCronJob extracts the job type, args, and queue from a cron record's
// template. It returns an empty type when no template is configured.
func resolveCronJob(cronRecord *state.CronRecord) (string, json.RawMessage, string, error) {
	queue := cronRecord.Queue
	if queue == "" {
		queue = "default"
	}
	if cronRecord.JobTemplate == "" {
		return "", nil, queue, nil
	}

	var template core.CronJobTemplate
	if err := json.Unmarshal([]byte(cronRecord.JobTemplate), &template); err != nil {
		return "", nil, queue, err
	}
	if template.Options != nil && template.Options.Queue != "" {
		queue = template.Options.Queue
	}
	return template.Type, template.Args, queue, nil
}

// advanceCronSchedule records a fired cron's next/last run. It always persists
// the last run even if the next run cannot be recomputed.
func (b *SQSBackend) advanceCronSchedule(ctx context.Context, cronRecord *state.CronRecord, now time.Time, _ cron.Parser) error {
	updatedCron := *cronRecord
	var firstErr error

	if schedule, parseErr := parseCronSchedule(cronRecord.Expression, cronRecord.Timezone); parseErr == nil {
		updatedCron.NextRunAt = core.FormatTime(schedule.Next(now))
	} else {
		firstErr = fmt.Errorf("parse cron expression for next run %s: %w", cronRecord.Name, parseErr)
		b.logger.Error("failed to parse cron expression for next run", "cron", cronRecord.Name, "expression", cronRecord.Expression, "error", parseErr)
	}

	updatedCron.LastRunAt = core.FormatTime(now)
	if err := b.store.PutCron(ctx, &updatedCron); err != nil {
		if firstErr == nil {
			firstErr = fmt.Errorf("persist cron update %s: %w", cronRecord.Name, err)
		}
		b.logger.Error("failed to persist cron run metadata", "cron", cronRecord.Name, "error", err)
	}
	return firstErr
}

// advanceSkippedCronSchedule advances the schedule for a cron whose run was
// skipped due to overlap. Unlike advanceCronSchedule it only persists when the
// next run can be recomputed.
func (b *SQSBackend) advanceSkippedCronSchedule(ctx context.Context, cronRecord *state.CronRecord, now time.Time, _ cron.Parser) error {
	schedule, err := parseCronSchedule(cronRecord.Expression, cronRecord.Timezone)
	if err != nil {
		b.logger.Error("failed to parse cron expression while skipping overlap", "cron", cronRecord.Name, "expression", cronRecord.Expression, "error", err)
		return fmt.Errorf("parse cron expression for skip %s: %w", cronRecord.Name, err)
	}

	updatedCron := *cronRecord
	updatedCron.NextRunAt = core.FormatTime(schedule.Next(now))
	updatedCron.LastRunAt = core.FormatTime(now)
	if err := b.store.PutCron(ctx, &updatedCron); err != nil {
		b.logger.Error("failed to persist skipped cron schedule", "cron", cronRecord.Name, "error", err)
		return fmt.Errorf("update skipped cron schedule %s: %w", cronRecord.Name, err)
	}
	return nil
}
