package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

// S-174: inbox observability + dead-letter operations, and S-173: the
// consult-timeout sweep. Kept out of the already-gigantic postgres.go.

const messageColumns = `id::text, from_type, from_id::text, to_agent_id::text, squad_id::text,
		       type, payload, status, coalesce(correlation_id::text, ''), attempts, max_attempts,
		       next_retry_at, expires_at, timeout_at, terminal_reason, created_at, delivered_at`

// ListAgentInbox returns the agent's queue snapshot. Pending is
// oldest-first (the waiting queue); retrying/delivered/dead are
// newest-first. Counts are totals, lists are capped at limit.
func (p *PostgresStore) ListAgentInbox(ctx context.Context, agentID string, limit int) (*domain.AgentInboxSnapshot, error) {
	limit = clampInboxLimit(limit)
	if err := p.expireMessagesForAgent(ctx, agentID); err != nil {
		return nil, err
	}
	snap := &domain.AgentInboxSnapshot{
		Pending:   []*domain.Message{},
		Retrying:  []*domain.Message{},
		Delivered: []*domain.Message{},
		Dead:      []*domain.Message{},
	}
	var oldestPending sql.NullTime
	var pendingCount, retryingCount, deliveredCount, deadCount int64
	err := p.pool.QueryRow(ctx, `
		SELECT
			count(*) FILTER (WHERE status = 'pending' AND attempts <= 0),
			min(created_at) FILTER (WHERE status = 'pending' AND attempts <= 0),
			count(*) FILTER (WHERE status = 'pending' AND attempts > 0),
			count(*) FILTER (WHERE status = 'delivered'),
			count(*) FILTER (WHERE status = 'dead')
		FROM messages
		WHERE to_agent_id = $1
	`, agentID).Scan(&pendingCount, &oldestPending, &retryingCount, &deliveredCount, &deadCount)
	if err != nil {
		return nil, mapPgErr(err)
	}
	snap.PendingCount = int(pendingCount)
	snap.RetryingCount = int(retryingCount)
	snap.DeliveredCount = int(deliveredCount)
	snap.DeadCount = int(deadCount)
	if oldestPending.Valid {
		t := oldestPending.Time
		snap.OldestPendingAt = &t
	}
	sections := []struct {
		dst   *[]*domain.Message
		query string
	}{
		{&snap.Pending, fmt.Sprintf(`SELECT %s FROM messages WHERE to_agent_id = $1 AND status = 'pending' AND attempts <= 0 ORDER BY created_at ASC LIMIT $2`, messageColumns)},
		{&snap.Retrying, fmt.Sprintf(`SELECT %s FROM messages WHERE to_agent_id = $1 AND status = 'pending' AND attempts > 0 ORDER BY next_retry_at ASC, created_at DESC LIMIT $2`, messageColumns)},
		{&snap.Delivered, fmt.Sprintf(`SELECT %s FROM messages WHERE to_agent_id = $1 AND status = 'delivered' ORDER BY coalesce(delivered_at, created_at) DESC LIMIT $2`, messageColumns)},
		{&snap.Dead, fmt.Sprintf(`SELECT %s FROM messages WHERE to_agent_id = $1 AND status = 'dead' ORDER BY created_at DESC LIMIT $2`, messageColumns)},
	}
	for _, section := range sections {
		rows, err := p.pool.Query(ctx, section.query, agentID, limit)
		if err != nil {
			return nil, mapPgErr(err)
		}
		msgs, err := scanMessages(rows)
		if err != nil {
			return nil, err
		}
		*section.dst = msgs
	}
	return snap, nil
}

// ReplayDeadMessage returns a dead message to the delivery queue:
// dead -> pending, attempts reset, expiry refreshed by the default TTL.
// ErrNotFound when absent, ErrConflict when the message is not dead.
// The correlation chain is untouched — replay is an operator action;
// the send-path chain budget only gates brand-new sends.
func (p *PostgresStore) ReplayDeadMessage(ctx context.Context, messageID string) (*domain.Message, error) {
	row := p.pool.QueryRow(ctx, `
		UPDATE messages
		SET status = 'pending',
		    attempts = 0,
		    next_retry_at = now(),
		    expires_at = now() + $2::interval,
		    terminal_reason = '',
		    delivered_at = NULL
		WHERE id = $1 AND status = 'dead'
		RETURNING `+messageColumns,
		messageID, fmt.Sprintf("%d seconds", int(defaultMessageTTL.Seconds())))
	updated, err := scanMessage(row)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			if _, exists := p.GetMessage(ctx, messageID); exists != nil {
				return nil, ErrNotFound
			}
			return nil, ErrConflict
		}
		return nil, err
	}
	return updated, nil
}

// ListDeadLetters lists dead messages across all squads for the admin
// screen, newest first. Empty filter fields are ignored.
func (p *PostgresStore) ListDeadLetters(ctx context.Context, filter domain.DeadLetterFilter) ([]*domain.Message, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	where := []string{"status = 'dead'"}
	args := []any{}
	add := func(clause string, value any) {
		args = append(args, value)
		where = append(where, fmt.Sprintf(clause, len(args)))
	}
	if filter.SquadID != "" {
		add("squad_id = $%d", filter.SquadID)
	}
	if filter.AgentID != "" {
		add("to_agent_id = $%d", filter.AgentID)
	}
	if filter.Type != "" {
		add("type = $%d", filter.Type)
	}
	if reason := strings.TrimSpace(filter.Reason); reason != "" {
		add("terminal_reason ILIKE $%d", "%"+reason+"%")
	}
	if filter.Since != nil {
		add("created_at >= $%d", *filter.Since)
	}
	if filter.Until != nil {
		add("created_at <= $%d", *filter.Until)
	}
	query := fmt.Sprintf(`SELECT %s FROM messages WHERE %s ORDER BY created_at DESC LIMIT $%d`,
		messageColumns, strings.Join(where, " AND "), len(args)+1)
	args = append(args, limit)
	rows, err := p.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, mapPgErr(err)
	}
	return scanMessages(rows)
}

// DeleteMessage hard-prunes one message row (admin-only; the handler
// enforces authorization and the dead-status precondition).
func (p *PostgresStore) DeleteMessage(ctx context.Context, messageID string) error {
	tag, err := p.pool.Exec(ctx, `DELETE FROM messages WHERE id = $1`, messageID)
	if err != nil {
		return mapPgErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SweepConsultTimeouts (S-173) posts a synthetic consult_timeout reply
// to the asker for every agent consult whose deadline passed with no
// correlated reply from the target. Rows are claimed with SKIP LOCKED
// and stamped with timeout_notified_at, so concurrent replicas each
// notify exactly once. Consults whose asker was deleted are stamped but
// post nothing (nothing is deliverable).
func (p *PostgresStore) SweepConsultTimeouts(ctx context.Context, now time.Time) (int, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return 0, mapPgErr(err)
	}
	defer tx.Rollback(ctx)

	rows, err := tx.Query(ctx, `
		SELECT m.id::text, coalesce(m.correlation_id::text, m.id::text), m.from_id::text, m.to_agent_id::text, m.created_at
		FROM messages m
		WHERE m.type = 'consult'
		  AND m.from_type = 'agent'
		  AND m.timeout_at IS NOT NULL
		  AND m.timeout_notified_at IS NULL
		  AND m.timeout_at <= $1
		  AND m.status IN ('pending', 'delivered')
		  AND NOT EXISTS (
		      SELECT 1 FROM messages r
		      WHERE r.correlation_id = coalesce(m.correlation_id, m.id)
		        AND r.from_type = 'agent'
		        AND r.from_id = m.to_agent_id
		        AND r.type = 'reply'
		  )
		LIMIT 100
		FOR UPDATE OF m SKIP LOCKED
	`, now)
	if err != nil {
		return 0, mapPgErr(err)
	}
	type dueConsult struct {
		id        string
		threadID  string
		askerID   string
		targetID  string
		createdAt time.Time
	}
	var due []dueConsult
	for rows.Next() {
		var c dueConsult
		if err := rows.Scan(&c.id, &c.threadID, &c.askerID, &c.targetID, &c.createdAt); err != nil {
			return 0, mapPgErr(err)
		}
		due = append(due, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, mapPgErr(err)
	}

	posted := 0
	for _, c := range due {
		if _, err := tx.Exec(ctx, `UPDATE messages SET timeout_notified_at = now() WHERE id = $1`, c.id); err != nil {
			return 0, mapPgErr(err)
		}
		var askerSquadID string
		err := tx.QueryRow(ctx, `SELECT squad_id::text FROM agents WHERE id = $1`, c.askerID).Scan(&askerSquadID)
		if errors.Is(err, pgx.ErrNoRows) {
			continue // asker deleted: marked notified, nothing to deliver
		}
		if err != nil {
			return 0, mapPgErr(err)
		}
		payload, err := json.Marshal(consultTimeoutPayloadMap(c.id, c.createdAt))
		if err != nil {
			return 0, mapPgErr(err)
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO messages (from_type, from_id, to_agent_id, squad_id, type, payload, status,
			                    correlation_id, max_attempts, next_retry_at, expires_at, terminal_reason)
			VALUES ('agent', $1, $2, $3, 'reply', $4, 'pending', $5::uuid, $6, now(),
			        now() + $7::interval, $8)
		`, c.targetID, c.askerID, askerSquadID, payload, c.threadID,
			defaultMessageMaxAttempts,
			fmt.Sprintf("%d seconds", int(defaultMessageTTL.Seconds())),
			consultTimeoutReason)
		if err != nil {
			return 0, mapPgErr(err)
		}
		posted++
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, mapPgErr(err)
	}
	return posted, nil
}
