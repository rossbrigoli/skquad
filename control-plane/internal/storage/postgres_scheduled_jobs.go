package storage

// S-281: Postgres ScheduledJobStore (scheduled_jobs, migration 0057).
//
// The claim is the load-bearing query: a CTE picks the due rows under
// FOR UPDATE SKIP LOCKED and the outer UPDATE marks them, all in one
// statement. Concurrent ticks (multiple API replicas) therefore never
// see the same job twice — a locked row is simply skipped, not queued
// behind it. The claim also pushes next_run_at to now+claimSafetyNet so
// a worker that dies mid-fire gets retried (at-least-once) instead of
// silently losing the job; Finish/Fail pin the real outcome right after.

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

const scheduledJobColumns = `id, agent_id, squad_id, title, message, schedule_kind, schedule_spec,
	enabled, next_run_at, last_run_at, last_run_status, fail_count, disabled_reason,
	created_by_type, created_by_id, created_at`

func scanScheduledJob(scan func(dest ...any) error) (*domain.ScheduledJob, error) {
	var j domain.ScheduledJob
	var spec []byte
	var lastRunAt *time.Time
	if err := scan(
		&j.ID, &j.AgentID, &j.SquadID, &j.Title, &j.Message, &j.ScheduleKind, &spec,
		&j.Enabled, &j.NextRunAt, &lastRunAt, &j.LastRunStatus, &j.FailCount, &j.DisabledReason,
		&j.CreatedByType, &j.CreatedByID, &j.CreatedAt,
	); err != nil {
		return nil, mapPgErr(err)
	}
	j.ScheduleSpec = json.RawMessage(spec)
	if lastRunAt != nil {
		j.LastRunAt = *lastRunAt
	}
	return &j, nil
}

func (p *PostgresStore) CreateScheduledJob(ctx context.Context, job *domain.ScheduledJob) (*domain.ScheduledJob, error) {
	id := job.ID
	if id == "" {
		id = uuid.NewString()
	}
	createdByType := job.CreatedByType
	if createdByType == "" {
		createdByType = "agent"
	}
	var created domain.ScheduledJob
	var spec []byte
	var lastRunAt *time.Time
	err := p.pool.QueryRow(ctx, `
		INSERT INTO scheduled_jobs
			(id, agent_id, squad_id, title, message, schedule_kind, schedule_spec,
			 enabled, next_run_at, created_by_type, created_by_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8, $9, $10, $11)
		RETURNING `+scheduledJobColumns,
		id, job.AgentID, job.SquadID, job.Title, job.Message, string(job.ScheduleKind), string(job.ScheduleSpec),
		job.Enabled, job.NextRunAt, createdByType, job.CreatedByID,
	).Scan(
		&created.ID, &created.AgentID, &created.SquadID, &created.Title, &created.Message, &created.ScheduleKind, &spec,
		&created.Enabled, &created.NextRunAt, &lastRunAt, &created.LastRunStatus, &created.FailCount, &created.DisabledReason,
		&created.CreatedByType, &created.CreatedByID, &created.CreatedAt,
	)
	if err != nil {
		return nil, mapPgErr(err)
	}
	created.ScheduleSpec = json.RawMessage(spec)
	if lastRunAt != nil {
		created.LastRunAt = *lastRunAt
	}
	return &created, nil
}

func (p *PostgresStore) GetScheduledJob(ctx context.Context, id string) (*domain.ScheduledJob, error) {
	row := p.pool.QueryRow(ctx, `SELECT `+scheduledJobColumns+` FROM scheduled_jobs WHERE id = $1`, id)
	return scanScheduledJob(row.Scan)
}

func (p *PostgresStore) ListScheduledJobs(ctx context.Context, agentID string) ([]*domain.ScheduledJob, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT `+scheduledJobColumns+`
		FROM scheduled_jobs
		WHERE agent_id = $1
		ORDER BY created_at, id
	`, agentID)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer rows.Close()
	jobs := make([]*domain.ScheduledJob, 0, 4)
	for rows.Next() {
		job, err := scanScheduledJob(rows.Scan)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, mapPgErr(rows.Err())
}

func (p *PostgresStore) DeleteScheduledJob(ctx context.Context, id string) error {
	tag, err := p.pool.Exec(ctx, `DELETE FROM scheduled_jobs WHERE id = $1`, id)
	if err != nil {
		return mapPgErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *PostgresStore) ClaimDueScheduledJobs(ctx context.Context, now time.Time, limit int, claimSafetyNet time.Duration) ([]*domain.ScheduledJob, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := p.pool.Query(ctx, `
		WITH picked AS (
			SELECT id
			FROM scheduled_jobs
			WHERE enabled AND next_run_at <= $1
			ORDER BY next_run_at, id
			LIMIT $2
			FOR UPDATE SKIP LOCKED
		)
		UPDATE scheduled_jobs s
		SET last_run_at = $1,
		    last_run_status = 'running',
		    next_run_at = $1 + ($3 * interval '1 microsecond')
		FROM picked
		WHERE s.id = picked.id
		RETURNING `+scheduledJobColumns,
		now, limit, claimSafetyNet.Microseconds(),
	)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer rows.Close()
	claimed := make([]*domain.ScheduledJob, 0, limit)
	for rows.Next() {
		job, err := scanScheduledJob(rows.Scan)
		if err != nil {
			return nil, err
		}
		claimed = append(claimed, job)
	}
	return claimed, mapPgErr(rows.Err())
}

func (p *PostgresStore) FinishScheduledJobRun(ctx context.Context, id string, firedAt time.Time, nextRunAt time.Time, enabled bool) error {
	disabledReason := ""
	if !enabled {
		disabledReason = "one-shot job completed"
	}
	tag, err := p.pool.Exec(ctx, `
		UPDATE scheduled_jobs
		SET last_run_at = $2,
		    last_run_status = 'fired',
		    fail_count = 0,
		    next_run_at = $3,
		    enabled = $4,
		    disabled_reason = $5
		WHERE id = $1
	`, id, firedAt, nextRunAt, enabled, disabledReason)
	if err != nil {
		return mapPgErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *PostgresStore) FailScheduledJobRun(ctx context.Context, id string, firedAt time.Time, maxFailures int, reason string) error {
	// The disable decision is made inside the UPDATE (fail_count + 1 is
	// computed atomically) so concurrent failure writes cannot race past
	// the cap.
	var disableExpr, reasonExpr string
	if maxFailures > 0 {
		disableExpr = `AND fail_count + 1 < $4`
		reasonExpr = `CASE WHEN fail_count + 1 >= $4 THEN $5 ELSE disabled_reason END`
	} else {
		disableExpr = ``
		reasonExpr = `disabled_reason`
	}
	query := `
		UPDATE scheduled_jobs
		SET last_run_at = $2,
		    last_run_status = 'failed',
		    fail_count = fail_count + 1,
		    enabled = enabled ` + disableExpr + `,
		    disabled_reason = ` + reasonExpr + `
		WHERE id = $1`
	var args []any
	args = append(args, id, firedAt)
	if maxFailures > 0 {
		args = append(args, maxFailures, reason)
	}
	tag, err := p.pool.Exec(ctx, query, args...)
	if err != nil {
		return mapPgErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *PostgresStore) SetScheduledJobEnabled(ctx context.Context, id string, enabled bool, nextRunAt time.Time) (*domain.ScheduledJob, error) {
	// A zero nextRunAt means "keep the stored value": send NULL and let
	// COALESCE hold the column (a zero Go time would otherwise land as
	// year 1 in Postgres).
	var nextArg any
	if !nextRunAt.IsZero() {
		nextArg = nextRunAt.UTC()
	}
	var updated domain.ScheduledJob
	var spec []byte
	var lastRunAt *time.Time
	err := p.pool.QueryRow(ctx, `
		UPDATE scheduled_jobs
		SET enabled = $2,
		    next_run_at = COALESCE($3::timestamptz, next_run_at),
		    fail_count = CASE WHEN $2 THEN 0 ELSE fail_count END,
		    disabled_reason = CASE WHEN $2 THEN '' ELSE disabled_reason END
		WHERE id = $1
		RETURNING `+scheduledJobColumns,
		id, enabled, nextArg,
	).Scan(
		&updated.ID, &updated.AgentID, &updated.SquadID, &updated.Title, &updated.Message, &updated.ScheduleKind, &spec,
		&updated.Enabled, &updated.NextRunAt, &lastRunAt, &updated.LastRunStatus, &updated.FailCount, &updated.DisabledReason,
		&updated.CreatedByType, &updated.CreatedByID, &updated.CreatedAt,
	)
	if err != nil {
		return nil, mapPgErr(err)
	}
	updated.ScheduleSpec = json.RawMessage(spec)
	if lastRunAt != nil {
		updated.LastRunAt = *lastRunAt
	}
	return &updated, nil
}
