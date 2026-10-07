package storage

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

// TG-8 slice B: grant-request workflow (docs/tg8-grant-approvals-spec.md §B).
// Kept out of the already-gigantic postgres.go, following the inbox split.

const grantRequestColumns = `id::text, resource_id::text, coalesce(agent_id::text, ''),
	requester_user_id::text, tier, state, requested_scope, findings_json,
	approved_by_owner_at, approved_by_admin_at, denied_reason, expiry, created_at, updated_at`

func scanGrantRequest(row pgx.Row) (*domain.GrantRequest, error) {
	var r domain.GrantRequest
	var scope, findings []byte
	err := row.Scan(&r.ID, &r.ResourceID, &r.AgentID, &r.RequesterUserID, &r.Tier, &r.State,
		&scope, &findings, &r.ApprovedByOwnerAt, &r.ApprovedByAdminAt,
		&r.DeniedReason, &r.Expiry, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return nil, mapPgErr(err)
	}
	if len(scope) > 0 {
		r.RequestedScope = json.RawMessage(scope)
	}
	if len(findings) > 0 {
		if err := json.Unmarshal(findings, &r.Findings); err != nil {
			return nil, mapPgErr(err)
		}
	}
	if r.Findings == nil {
		r.Findings = []domain.GrantLintFinding{}
	}
	return &r, nil
}

func (p *PostgresStore) CreateGrantRequest(ctx context.Context, r *domain.GrantRequest) (*domain.GrantRequest, error) {
	state := r.State
	if state == "" {
		state = domain.GrantRequestPendingOwner
	}
	scope := r.RequestedScope
	if len(scope) == 0 {
		scope = json.RawMessage(`{}`)
	}
	findings := r.Findings
	if findings == nil {
		findings = []domain.GrantLintFinding{}
	}
	findingsJSON, err := json.Marshal(findings)
	if err != nil {
		return nil, mapPgErr(err)
	}
	var agentArg any
	if r.AgentID != "" {
		agentArg = r.AgentID
	}
	var expiryArg any
	if r.Expiry != nil {
		expiryArg = *r.Expiry
	}
	row := p.pool.QueryRow(ctx, `
		INSERT INTO grant_requests
			(resource_id, agent_id, requester_user_id, tier, state, requested_scope,
			 findings_json, denied_reason, expiry)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING `+grantRequestColumns,
		r.ResourceID, agentArg, r.RequesterUserID, r.Tier, string(state), scope, findingsJSON,
		r.DeniedReason, expiryArg)
	return scanGrantRequest(row)
}

func (p *PostgresStore) GetGrantRequest(ctx context.Context, id string) (*domain.GrantRequest, error) {
	row := p.pool.QueryRow(ctx, `SELECT `+grantRequestColumns+` FROM grant_requests WHERE id = $1`, id)
	return scanGrantRequest(row)
}

func (p *PostgresStore) ListGrantRequests(ctx context.Context, f GrantRequestFilter) ([]*domain.GrantRequest, error) {
	sql := `SELECT ` + grantRequestColumns + ` FROM grant_requests gr`
	where := " WHERE true"
	args := []any{}
	n := 0
	if f.State != "" {
		n++
		where += ` AND gr.state = $` + strconv.Itoa(n)
		args = append(args, f.State)
	}
	if f.RequesterUserID != "" {
		n++
		where += ` AND gr.requester_user_id = $` + strconv.Itoa(n)
		args = append(args, f.RequesterUserID)
	}
	if f.OwnerUserID != "" {
		n++
		where += ` AND EXISTS (SELECT 1 FROM registry_resources rr
			                 WHERE rr.id = gr.resource_id AND rr.owner_user_id = $` + strconv.Itoa(n) + `)`
		args = append(args, f.OwnerUserID)
	}
	sql += where + " ORDER BY gr.created_at DESC, gr.id DESC"
	rows, err := p.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer rows.Close()
	out := []*domain.GrantRequest{}
	for rows.Next() {
		req, err := scanGrantRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, req)
	}
	return out, mapPgErr(rows.Err())
}

func (p *PostgresStore) UpdateGrantRequestState(ctx context.Context, id string, expectedFrom domain.GrantRequestState, up GrantRequestUpdate) (*domain.GrantRequest, error) {
	tag, err := p.pool.Exec(ctx, `
		UPDATE grant_requests SET
			state = $3,
			approved_by_owner_at = COALESCE($4, approved_by_owner_at),
			approved_by_admin_at = COALESCE($5, approved_by_admin_at),
			denied_reason = CASE WHEN $6::text = '' THEN denied_reason ELSE $6::text END,
			updated_at = now()
		WHERE id = $1 AND state = $2`,
		id, string(expectedFrom), string(up.State),
		timeArg(up.ApprovedByOwnerAt), timeArg(up.ApprovedByAdminAt), up.DeniedReason)
	if err != nil {
		return nil, mapPgErr(err)
	}
	if tag.RowsAffected() == 0 {
		// Distinguish "no such row" from "row moved under us".
		if _, getErr := p.GetGrantRequest(ctx, id); getErr != nil {
			return nil, getErr
		}
		return nil, ErrConflict
	}
	return p.GetGrantRequest(ctx, id)
}

func timeArg(t *time.Time) any {
	if t == nil {
		return nil
	}
	return *t
}
