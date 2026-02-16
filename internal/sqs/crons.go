package sqs

import (
	"context"
	"encoding/json"
	"fmt"
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

	// Validate cron expression
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

	var schedule cron.Schedule
	var err error

	if cronJob.Timezone != "" {
		// Parse with timezone
		loc, locErr := time.LoadLocation(cronJob.Timezone)
		if locErr != nil {
			return nil, core.NewInvalidRequestError(
				fmt.Sprintf("Invalid timezone: %s", cronJob.Timezone),
				map[string]any{"timezone": cronJob.Timezone},
			)
		}
		// Wrap expression with timezone prefix
		schedule, err = parser.Parse("CRON_TZ=" + loc.String() + " " + expr)
		if err != nil {
			// Try without CRON_TZ prefix for special expressions like @daily
			schedule, err = parser.Parse(expr)
		}
	} else {
		schedule, err = parser.Parse(expr)
	}

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
		data, _ := json.Marshal(cronJob.JobTemplate)
		jobTemplateJSON = string(data)
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

// FireCronJobs fires any cron jobs that are due.
func (b *SQSBackend) FireCronJobs(ctx context.Context) error {
	crons, err := b.store.ListCrons(ctx)
	if err != nil {
		return err
	}

	now := time.Now()
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)
	var firstErr error

	for _, cronRecord := range crons {
		if !cronRecord.Enabled {
			continue
		}

		if cronRecord.NextRunAt == "" {
			continue
		}

		nextRun, err := time.Parse(time.RFC3339, cronRecord.NextRunAt)
		if err != nil {
			nextRun, err = time.Parse(core.TimeFormat, cronRecord.NextRunAt)
			if err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("parse cron next_run_at for %s: %w", cronRecord.Name, err)
				}
				b.logger.Warn("invalid cron next_run_at", "cron", cronRecord.Name, "next_run_at", cronRecord.NextRunAt, "error", err)
				continue
			}
		}

		if now.Before(nextRun) {
			continue
		}

		// Acquire lock to prevent double-firing
		lockTimestamp := nextRun.Unix()
		acquired, err := b.store.AcquireCronLock(ctx, cronRecord.Name, lockTimestamp)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("acquire cron lock %s: %w", cronRecord.Name, err)
			}
			b.logger.Error("failed to acquire cron lock", "cron", cronRecord.Name, "error", err)
			continue
		}
		if !acquired {
			continue
		}

		// Handle overlap policy
		if cronRecord.OverlapPolicy == "skip" {
			instanceJobID, err := b.store.GetCronInstance(ctx, cronRecord.Name)
			if err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("get cron instance %s: %w", cronRecord.Name, err)
				}
				b.logger.Error("failed to get cron instance", "cron", cronRecord.Name, "error", err)
				continue
			}
			if instanceJobID != "" {
				record, err := b.store.GetJob(ctx, instanceJobID)
				if err != nil {
					if firstErr == nil {
						firstErr = fmt.Errorf("get cron instance job %s: %w", instanceJobID, err)
					}
					b.logger.Error("failed to load cron instance job", "cron", cronRecord.Name, "job_id", instanceJobID, "error", err)
					continue
				}
				if record != nil && !core.IsTerminalState(record.State) {
					// Previous instance still running, skip but update next_run_at
					schedule, parseErr := parser.Parse(cronRecord.Expression)
					if parseErr == nil {
						updatedCron := *cronRecord
						updatedCron.NextRunAt = core.FormatTime(schedule.Next(now))
						updatedCron.LastRunAt = core.FormatTime(now)
						if err := b.store.PutCron(ctx, &updatedCron); err != nil {
							if firstErr == nil {
								firstErr = fmt.Errorf("update skipped cron schedule %s: %w", cronRecord.Name, err)
							}
							b.logger.Error("failed to persist skipped cron schedule", "cron", cronRecord.Name, "error", err)
						}
					} else {
						if firstErr == nil {
							firstErr = fmt.Errorf("parse cron expression for skip %s: %w", cronRecord.Name, parseErr)
						}
						b.logger.Error("failed to parse cron expression while skipping overlap", "cron", cronRecord.Name, "expression", cronRecord.Expression, "error", parseErr)
					}
					continue
				}
			}
		}

		// Build and enqueue the cron job
		queue := cronRecord.Queue
		if queue == "" {
			queue = "default"
		}

		var jobType string
		var args json.RawMessage
		if cronRecord.JobTemplate != "" {
			var template core.CronJobTemplate
			if err := json.Unmarshal([]byte(cronRecord.JobTemplate), &template); err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("unmarshal cron job template %s: %w", cronRecord.Name, err)
				}
				b.logger.Error("failed to decode cron job template", "cron", cronRecord.Name, "error", err)
				continue
			}
			jobType = template.Type
			args = template.Args
			if template.Options != nil && template.Options.Queue != "" {
				queue = template.Options.Queue
			}
		}

		if jobType == "" {
			continue
		}

		created, err := b.Push(ctx, &core.Job{
			Type:  jobType,
			Args:  args,
			Queue: queue,
		})
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("enqueue cron job for %s: %w", cronRecord.Name, err)
			}
			b.logger.Error("failed to enqueue cron job", "cron", cronRecord.Name, "error", err)
			continue
		}

		// Store instance for overlap tracking
		if err := b.store.SetCronInstance(ctx, cronRecord.Name, created.ID); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("set cron instance %s: %w", cronRecord.Name, err)
			}
			b.logger.Error("failed to set cron instance", "cron", cronRecord.Name, "job_id", created.ID, "error", err)
			continue
		}

		// Calculate next run time and update cron record
		schedule, parseErr := parser.Parse(cronRecord.Expression)
		updatedCron := *cronRecord
		if parseErr == nil {
			updatedCron.NextRunAt = core.FormatTime(schedule.Next(now))
		} else {
			if firstErr == nil {
				firstErr = fmt.Errorf("parse cron expression for next run %s: %w", cronRecord.Name, parseErr)
			}
			b.logger.Error("failed to parse cron expression for next run", "cron", cronRecord.Name, "expression", cronRecord.Expression, "error", parseErr)
		}
		updatedCron.LastRunAt = core.FormatTime(now)
		if err := b.store.PutCron(ctx, &updatedCron); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("persist cron update %s: %w", cronRecord.Name, err)
			}
			b.logger.Error("failed to persist cron run metadata", "cron", cronRecord.Name, "error", err)
		}
	}

	return firstErr
}
