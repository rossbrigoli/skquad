package storage

import (
	"context"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

// TG-8 slice C: confirmation gates + standing grants
// (docs/tg8-grant-approvals-spec.md §C). Kept out of the
// already-gigantic postgres.go, following the inbox/grant-requests split.

const pendingConfirmationColumns = `id::text, resource_id::text, agent_id::text, tool, args_hash,
	state, coalesce(inbox_message_id, ''), requested_by::text, denied_reason,
	approved_at, consumed_at, created_at, updated_at`

func scanPendingConfirmation(row pgx.Row) (*domain.PendingConfirmation, error) {
	var c domain.PendingConfirmation
	err := row.Scan(&c.ID, &c.ResourceID, &c.AgentID, &c.Tool, &c.ArgsHash,
		&c.State, &c.InboxMessageID, &c.RequestedBy, &c.DeniedReason,
		&c.ApprovedAt, &c.ConsumedAt, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return nil, mapPgErr(err)
	}
	return &c, nil
}

func (p *PostgresStore) CreatePendingConfirmation(ctx context.Context, c *domain.PendingConfirmation) (*domain.PendingConfirmation, error) {
	state := c.State
	if state == "" {
		state = domain.ConfirmationPending
	}
	var inboxArg any
	if c.InboxMessageID != "" {
		inboxArg = c.InboxMessageID
	}
	row := p.pool.QueryRow(ctx, `
		INSERT INTO pending_confirmations
			(resource_id, agent_id, tool, args_hash, state, inbox_message_id, requested_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING `+pendingConfirmationColumns,
		c.ResourceID, c.AgentID, c.Tool, c.ArgsHash, string(state), inboxArg, c.RequestedBy)
	return scanPendingConfirmation(row)
}

func (p *PostgresStore) GetPendingConfirmation(ctx context.Context, id string) (*domain.PendingConfirmation, error) {
	row := p.pool.QueryRow(ctx, `SELECT `+pendingConfirmationColumns+` FROM pending_confirmations WHERE id = $1`, id)
	return scanPendingConfirmation(row)
}

func (p *PostgresStore) ListPendingConfirmations(ctx context.Context, f ConfirmationFilter) ([]*domain.PendingConfirmation, error) {
	sql := `SELECT ` + pendingConfirmationColumns + ` FROM pending_confirmations pc`
	where := " WHERE true"
	args := []any{}
	n := 0
	if f.State != "" {
		n++
		where += ` AND pc.state = $` + strconv.Itoa(n)
		args = append(args, f.State)
	}
	if f.RequestedBy != "" {
		n++
		where += ` AND pc.requested_by = $` + strconv.Itoa(n)
		args = append(args, f.RequestedBy)
	}
	if f.ResourceID != "" {
		n++
		where += ` AND pc.resource_id = $` + strconv.Itoa(n)
		args = append(args, f.ResourceID)
	}
	sql += where + " ORDER BY pc.created_at DESC, pc.id DESC"
	rows, err := p.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer rows.Close()
	out := []*domain.PendingConfirmation{}
	for rows.Next() {
		c, err := scanPendingConfirmation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, mapPgErr(rows.Err())
}

func (p *PostgresStore) UpdateConfirmationState(ctx context.Context, id string, expectedFrom domain.ConfirmationState, up ConfirmationUpdate) (*domain.PendingConfirmation, error) {
	tag, err := p.pool.Exec(ctx, `
		UPDATE pending_confirmations SET
			state = $3,
			inbox_message_id = COALESCE($4, inbox_message_id),
			approved_at = COALESCE($5, approved_at),
			consumed_at = COALESCE($6, consumed_at),
			denied_reason = CASE WHEN $7::text = '' THEN denied_reason ELSE $7::text END,
			updated_at = now()
		WHERE id = $1 AND state = $2`,
		id, string(expectedFrom), string(up.State),
		up.InboxMessageID, timeArg(up.ApprovedAt), timeArg(up.ConsumedAt), up.DeniedReason)
	if err != nil {
		return nil, mapPgErr(err)
	}
	if tag.RowsAffected() == 0 {
		// Distinguish "no such row" from "row moved under us".
		if _, getErr := p.GetPendingConfirmation(ctx, id); getErr != nil {
			return nil, getErr
		}
		return nil, ErrConflict
	}
	return p.GetPendingConfirmation(ctx, id)
}

const standingGrantColumns = `id::text, resource_id::text, agent_id, tool,
	expires_at, created_by, created_at, revoked_at`

func scanStandingGrant(row pgx.Row) (*domain.StandingGrant, error) {
	var g domain.StandingGrant
	err := row.Scan(&g.ID, &g.ResourceID, &g.AgentID, &g.Tool,
		&g.ExpiresAt, &g.CreatedBy, &g.CreatedAt, &g.RevokedAt)
	if err != nil {
		return nil, mapPgErr(err)
	}
	return &g, nil
}

func (p *PostgresStore) FindLiveStandingGrant(ctx context.Context, resourceID, agentID, tool string, now time.Time) (*domain.StandingGrant, error) {
	row := p.pool.QueryRow(ctx, `
		SELECT `+standingGrantColumns+` FROM standing_grants
		WHERE resource_id = $1 AND agent_id = $2 AND tool = $3
		  AND revoked_at IS NULL AND expires_at > $4`,
		resourceID, agentID, tool, now)
	return scanStandingGrant(row)
}

func (p *PostgresStore) UpsertStandingGrant(ctx context.Context, g *domain.StandingGrant) (*domain.StandingGrant, error) {
	row := p.pool.QueryRow(ctx, `
		INSERT INTO standing_grants (resource_id, agent_id, tool, expires_at, created_by)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (resource_id, agent_id, tool) WHERE revoked_at IS NULL
		DO UPDATE SET expires_at = EXCLUDED.expires_at, created_by = EXCLUDED.created_by
		RETURNING `+standingGrantColumns,
		g.ResourceID, g.AgentID, g.Tool, g.ExpiresAt, g.CreatedBy)
	return scanStandingGrant(row)
}

func (p *PostgresStore) GetStandingGrant(ctx context.Context, id string) (*domain.StandingGrant, error) {
	row := p.pool.QueryRow(ctx, `SELECT `+standingGrantColumns+` FROM standing_grants WHERE id = $1`, id)
	return scanStandingGrant(row)
}

func (p *PostgresStore) ListStandingGrants(ctx context.Context, f StandingGrantFilter) ([]*domain.StandingGrant, error) {
	sql := `SELECT ` + standingGrantColumns + ` FROM standing_grants sg`
	where := " WHERE true"
	args := []any{}
	n := 0
	if !f.IncludeRevoked {
		where += ` AND sg.revoked_at IS NULL`
	}
	if f.OwnerUserID != "" {
		n++
		where += ` AND EXISTS (SELECT 1 FROM registry_resources rr
			                 WHERE rr.id = sg.resource_id AND rr.owner_user_id = $` + strconv.Itoa(n) + `)`
		args = append(args, f.OwnerUserID)
	}
	sql += where + " ORDER BY sg.created_at DESC, sg.id DESC"
	rows, err := p.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer rows.Close()
	out := []*domain.StandingGrant{}
	for rows.Next() {
		g, err := scanStandingGrant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, mapPgErr(rows.Err())
}

func (p *PostgresStore) RevokeStandingGrant(ctx context.Context, id string, at time.Time) (*domain.StandingGrant, error) {
	row := p.pool.QueryRow(ctx, `
		UPDATE standing_grants SET revoked_at = $2
		WHERE id = $1 AND revoked_at IS NULL
		RETURNING `+standingGrantColumns, id, at)
	g, err := scanStandingGrant(row)
	if err != nil {
		// scanStandingGrant maps pgx.ErrNoRows (already revoked or missing) to ErrNotFound.
		return nil, err
	}
	return g, nil
}
