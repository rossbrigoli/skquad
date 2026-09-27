package storage

// Builtin tool config persistence (BT-2, ADR-0012 §1). The table is
// seeded with all three tools disabled; only the admin PATCH path writes
// it, always with a pending-audit entry drained in the same transaction
// (S-86 pattern).

import (
	"context"
	"encoding/json"
	"time"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

// BuiltinToolStore persists the per-tool enablement + policy rows for the
// three platform built-ins.
type BuiltinToolStore interface {
	ListBuiltinTools(ctx context.Context) ([]*domain.BuiltinToolConfig, error)
	GetBuiltinTool(ctx context.Context, name string) (*domain.BuiltinToolConfig, error)
	// UpdateBuiltinTool applies merge semantics: a nil enabled or policy
	// leaves that column untouched. updatedBy records the acting admin.
	UpdateBuiltinTool(ctx context.Context, name string, enabled *bool, policy json.RawMessage, updatedBy string) (*domain.BuiltinToolConfig, error)
}

func (p *PostgresStore) ListBuiltinTools(ctx context.Context) ([]*domain.BuiltinToolConfig, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT name, enabled, policy, updated_at, updated_by
		FROM builtin_tools_config
		ORDER BY name
	`)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer rows.Close()

	tools := make([]*domain.BuiltinToolConfig, 0, 3)
	for rows.Next() {
		var t domain.BuiltinToolConfig
		var policy []byte
		if err := rows.Scan(&t.Name, &t.Enabled, &policy, &t.UpdatedAt, &t.UpdatedBy); err != nil {
			return nil, mapPgErr(err)
		}
		t.Policy = json.RawMessage(policy)
		tools = append(tools, &t)
	}
	return tools, mapPgErr(rows.Err())
}

func (p *PostgresStore) GetBuiltinTool(ctx context.Context, name string) (*domain.BuiltinToolConfig, error) {
	var t domain.BuiltinToolConfig
	var policy []byte
	err := p.pool.QueryRow(ctx, `
		SELECT name, enabled, policy, updated_at, updated_by
		FROM builtin_tools_config
		WHERE name = $1
	`, name).Scan(&t.Name, &t.Enabled, &policy, &t.UpdatedAt, &t.UpdatedBy)
	if err != nil {
		return nil, mapPgErr(err)
	}
	t.Policy = json.RawMessage(policy)
	return &t, nil
}

func (p *PostgresStore) UpdateBuiltinTool(ctx context.Context, name string, enabled *bool, policy json.RawMessage, updatedBy string) (*domain.BuiltinToolConfig, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer tx.Rollback(ctx)

	// COALESCE gives the merge semantics: absent ($-args that are NULL)
	// keep the stored value.
	var policyArg any
	if len(policy) > 0 {
		policyArg = string(policy)
	}
	var updated domain.BuiltinToolConfig
	var storedPolicy []byte
	err = tx.QueryRow(ctx, `
		UPDATE builtin_tools_config
		SET enabled    = COALESCE($2, enabled),
		    policy     = COALESCE($3::jsonb, policy),
		    updated_by = $4,
		    updated_at = now()
		WHERE name = $1
		RETURNING name, enabled, policy, updated_at, updated_by
	`, name, enabled, policyArg, updatedBy).
		Scan(&updated.Name, &updated.Enabled, &storedPolicy, &updated.UpdatedAt, &updated.UpdatedBy)
	if err != nil {
		return nil, mapPgErr(err)
	}
	updated.Policy = json.RawMessage(storedPolicy)

	// resourceID fallback must stay empty: audit_log.resource_id is a uuid
	// column and built-in tool names are not uuids (the fallback would
	// re-inject "exec" etc. and fail the cast — see fix/bt-audit-uuid).
	if err := p.writePendingAuditsTx(ctx, tx, ""); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, mapPgErr(err)
	}
	return &updated, nil
}

// --- MemoryStore implementation -------------------------------------------

// seedBuiltinToolsLocked inserts the three disabled built-in rows.
// Callers must hold m.mu (write lock).
func (m *MemoryStore) seedBuiltinToolsLocked() {
	now := time.Now().UTC()
	for _, name := range domain.BuiltinToolNames {
		m.builtinTools[name] = &domain.BuiltinToolConfig{
			Name:      name,
			Enabled:   false,
			Policy:    json.RawMessage(`{}`),
			UpdatedAt: now,
			UpdatedBy: "",
		}
	}
}

func (m *MemoryStore) ListBuiltinTools(_ context.Context) ([]*domain.BuiltinToolConfig, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	tools := make([]*domain.BuiltinToolConfig, 0, len(m.builtinTools))
	for _, name := range domain.BuiltinToolNames { // canonical order, mirrors ORDER BY name
		if t, ok := m.builtinTools[name]; ok {
			tools = append(tools, cloneBuiltinTool(t))
		}
	}
	return tools, nil
}

func (m *MemoryStore) GetBuiltinTool(_ context.Context, name string) (*domain.BuiltinToolConfig, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	t, ok := m.builtinTools[name]
	if !ok {
		return nil, ErrNotFound
	}
	return cloneBuiltinTool(t), nil
}

func (m *MemoryStore) UpdateBuiltinTool(ctx context.Context, name string, enabled *bool, policy json.RawMessage, updatedBy string) (*domain.BuiltinToolConfig, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.builtinTools[name]
	if !ok {
		return nil, ErrNotFound
	}
	if enabled != nil {
		t.Enabled = *enabled
	}
	if len(policy) > 0 {
		t.Policy = append(json.RawMessage(nil), policy...)
	}
	t.UpdatedAt = time.Now().UTC()
	t.UpdatedBy = updatedBy
	m.drainPendingAuditsLocked(ctx, t.Name)
	return cloneBuiltinTool(t), nil
}

func cloneBuiltinTool(t *domain.BuiltinToolConfig) *domain.BuiltinToolConfig {
	clone := *t
	clone.Policy = append(json.RawMessage(nil), t.Policy...)
	return &clone
}
