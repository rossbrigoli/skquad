package storage

// S-203 WP3: budget enforcement persistence.
//
// budget_blocks holds the per-user blocked state (period-scoped so the
// calendar-month reset needs no cron) and budget_notify_markers holds
// the once-per-month notification claims. Both stores implement the
// same contract:
//
//   - ClaimBudgetNotification is an atomic claim: exactly one caller
//     gets `true` per (user, period, marker), which is the
//     exactly-once-per-threshold-per-month guard under concurrent
//     turns for the same user.
//   - SetBudgetBlock upserts with period rollover: writing the current
//     period resets flags left over from a previous month, and the two
//     sources (own budget vs platform limit) never clobber each other
//     within a period.
//   - IsAgentBudgetBlocked resolves agent → squad → owner and answers
//     the scheduling check in one query.

import (
	"context"
	"fmt"
	"time"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

// BudgetEnforcementStore persists blocked state and notification
// markers for budget enforcement (S-203 WP3).
type BudgetEnforcementStore interface {
	// GetBudgetBlock returns the user's block row, or ErrNotFound when
	// the user has never been blocked.
	GetBudgetBlock(ctx context.Context, userID string) (*domain.BudgetBlock, error)
	// SetBudgetBlock sets or clears one source's block flag
	// (source: "user" or "platform") for the given period. Writing a
	// new period resets the other source's stale flag. Returns true
	// when the effective flags changed.
	SetBudgetBlock(ctx context.Context, userID, period, source string, blocked bool) (bool, error)
	// ClaimBudgetNotification atomically claims a notification marker
	// for (user, period). Returns true only for the first claim;
	// subsequent claims return false.
	ClaimBudgetNotification(ctx context.Context, userID, period, marker string) (bool, error)
	// ClearBudgetBlocksBySource clears one source's block flag for
	// every user and returns the affected user IDs (platform-limit
	// resume fan-out).
	ClearBudgetBlocksBySource(ctx context.Context, source string) ([]string, error)
	// IsAgentBudgetBlocked reports whether the agent's squad owner is
	// effectively blocked in the given period.
	IsAgentBudgetBlocked(ctx context.Context, agentID, period string) (bool, error)
}

// budgetBlockColumn maps an enforcement source to its block column.
// Whitelisted so the column name can never come from user input.
func budgetBlockColumn(source string) (string, error) {
	switch source {
	case domain.BudgetSourceUser:
		return "blocked_by_user", nil
	case domain.BudgetSourcePlatform:
		return "blocked_by_platform", nil
	default:
		return "", fmt.Errorf("unknown budget enforcement source %q", source)
	}
}

// ---------------------------------------------------------------------------
// PostgresStore
// ---------------------------------------------------------------------------

func (p *PostgresStore) GetBudgetBlock(ctx context.Context, userID string) (*domain.BudgetBlock, error) {
	row := p.pool.QueryRow(ctx, `
		SELECT user_id::text, period, blocked_by_user, blocked_by_platform, updated_at
		FROM budget_blocks
		WHERE user_id = $1::uuid
	`, userID)
	var b domain.BudgetBlock
	if err := row.Scan(&b.UserID, &b.Period, &b.BlockedByUser, &b.BlockedByPlatform, &b.UpdatedAt); err != nil {
		return nil, mapPgErr(err)
	}
	return &b, nil
}

func (p *PostgresStore) SetBudgetBlock(ctx context.Context, userID, period, source string, blocked bool) (bool, error) {
	if _, err := budgetBlockColumn(source); err != nil {
		return false, err
	}
	// Read-modify-write inside one transaction so concurrent evaluations
	// of the two sources serialize on the row and neither loses the
	// other's flag. Period rollover: a row from a previous period is
	// replaced wholesale (both flags restart from this write).
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return false, mapPgErr(err)
	}
	defer tx.Rollback(ctx)

	var oldUser, oldPlatform bool
	var oldPeriod string
	scanErr := tx.QueryRow(ctx, `
		SELECT period, blocked_by_user, blocked_by_platform
		FROM budget_blocks
		WHERE user_id = $1::uuid
		FOR UPDATE
	`, userID).Scan(&oldPeriod, &oldUser, &oldPlatform)
	fresh := false
	if scanErr != nil {
		if mapPgErr(scanErr) != ErrNotFound {
			return false, mapPgErr(scanErr)
		}
		fresh = true
	}
	samePeriod := !fresh && oldPeriod == period
	newUser, newPlatform := oldUser, oldPlatform
	if !samePeriod {
		newUser, newPlatform = false, false
	}
	switch source {
	case domain.BudgetSourceUser:
		newUser = blocked
	case domain.BudgetSourcePlatform:
		newPlatform = blocked
	}
	changed := fresh || !samePeriod || newUser != oldUser || newPlatform != oldPlatform
	_, err = tx.Exec(ctx, `
		INSERT INTO budget_blocks (user_id, period, blocked_by_user, blocked_by_platform, updated_at)
		VALUES ($1::uuid, $2, $3, $4, now())
		ON CONFLICT (user_id)
		DO UPDATE SET period = EXCLUDED.period,
		            blocked_by_user = EXCLUDED.blocked_by_user,
		            blocked_by_platform = EXCLUDED.blocked_by_platform,
		            updated_at = now()
	`, userID, period, newUser, newPlatform)
	if err != nil {
		return false, mapPgErr(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, mapPgErr(err)
	}
	return changed, nil
}

func (p *PostgresStore) ClaimBudgetNotification(ctx context.Context, userID, period, marker string) (bool, error) {
	tag, err := p.pool.Exec(ctx, `
		INSERT INTO budget_notify_markers (user_id, period, marker)
		VALUES ($1::uuid, $2, $3)
		ON CONFLICT (user_id, period, marker) DO NOTHING
	`, userID, period, marker)
	if err != nil {
		return false, mapPgErr(err)
	}
	return tag.RowsAffected() > 0, nil
}

func (p *PostgresStore) ClearBudgetBlocksBySource(ctx context.Context, source string) ([]string, error) {
	col, err := budgetBlockColumn(source)
	if err != nil {
		return nil, err
	}
	// Only rows currently blocked by this source are touched; the other
	// source's flag survives. Rows left with both flags false are
	// removed so the table stays small.
	query := fmt.Sprintf(`
		UPDATE budget_blocks
		SET %s = false, updated_at = now()
		WHERE %s = true
		RETURNING user_id::text
	`, col, col)
	rows, err := p.pool.Query(ctx, query)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, mapPgErr(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, mapPgErr(err)
	}
	if len(ids) == 0 {
		return ids, nil
	}
	_, err = p.pool.Exec(ctx, `
		DELETE FROM budget_blocks
		WHERE user_id = ANY($1::uuid[]) AND NOT blocked_by_user AND NOT blocked_by_platform
	`, ids)
	if err != nil {
		return nil, mapPgErr(err)
	}
	return ids, nil
}

func (p *PostgresStore) IsAgentBudgetBlocked(ctx context.Context, agentID, period string) (bool, error) {
	row := p.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM agents a
			JOIN squads s ON s.id = a.squad_id
			JOIN budget_blocks b ON b.user_id = s.owner_id
			WHERE a.id = $1::uuid AND b.period = $2
			  AND (b.blocked_by_user OR b.blocked_by_platform)
		)
	`, agentID, period)
	var blocked bool
	if err := row.Scan(&blocked); err != nil {
		return false, mapPgErr(err)
	}
	return blocked, nil
}

// ---------------------------------------------------------------------------
// MemoryStore
// ---------------------------------------------------------------------------

func (m *MemoryStore) GetBudgetBlock(_ context.Context, userID string) (*domain.BudgetBlock, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	b, ok := m.budgetBlocks[userID]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *b
	return &cp, nil
}

func (m *MemoryStore) SetBudgetBlock(_ context.Context, userID, period, source string, blocked bool) (bool, error) {
	col, err := budgetBlockColumn(source)
	if err != nil {
		return false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.budgetBlocks[userID]
	if !ok {
		b = &domain.BudgetBlock{UserID: userID}
		m.budgetBlocks[userID] = b
	}
	oldUser, oldPlatform := b.BlockedByUser, b.BlockedByPlatform
	if b.Period != period {
		b.BlockedByUser, b.BlockedByPlatform = false, false
		b.Period = period
	}
	switch col {
	case "blocked_by_user":
		b.BlockedByUser = blocked
	case "blocked_by_platform":
		b.BlockedByPlatform = blocked
	}
	b.UpdatedAt = time.Now().UTC()
	return b.BlockedByUser != oldUser || b.BlockedByPlatform != oldPlatform, nil
}

func (m *MemoryStore) ClaimBudgetNotification(_ context.Context, userID, period, marker string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := userID + "|" + period + "|" + marker
	if m.budgetNotifyMarkers[key] {
		return false, nil
	}
	m.budgetNotifyMarkers[key] = true
	return true, nil
}

func (m *MemoryStore) ClearBudgetBlocksBySource(_ context.Context, source string) ([]string, error) {
	col, err := budgetBlockColumn(source)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := []string{}
	for id, b := range m.budgetBlocks {
		isSet := (col == "blocked_by_user" && b.BlockedByUser) || (col == "blocked_by_platform" && b.BlockedByPlatform)
		if !isSet {
			continue
		}
		if col == "blocked_by_user" {
			b.BlockedByUser = false
		} else {
			b.BlockedByPlatform = false
		}
		b.UpdatedAt = time.Now().UTC()
		ids = append(ids, id)
	}
	for _, id := range ids {
		b := m.budgetBlocks[id]
		if !b.BlockedByUser && !b.BlockedByPlatform {
			delete(m.budgetBlocks, id)
		}
	}
	return ids, nil
}

func (m *MemoryStore) IsAgentBudgetBlocked(_ context.Context, agentID, period string) (bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	agent, ok := m.agents[agentID]
	if !ok {
		return false, ErrNotFound
	}
	squad, ok := m.squads[agent.SquadID]
	if !ok {
		return false, nil
	}
	return m.budgetBlocks[squad.OwnerID].EffectiveBlocked(period), nil
}
