package storage

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

// PlatformSettingsStore is the platform-admin key/value settings store
// (S-183). Values are stored as strings so new settings can be added
// without a schema change; validation of shape/range lives at the API
// boundary. Seeded defaults ship in migration 0026.
type PlatformSettingsStore interface {
	// GetPlatformSetting returns the raw value and whether the key
	// exists. Missing keys are not an error.
	GetPlatformSetting(ctx context.Context, key string) (string, bool, error)
	// SetPlatformSetting upserts one setting, recording who changed it.
	SetPlatformSetting(ctx context.Context, key, value, updatedBy string) error
}

// AgentMirrorQueue re-emits an upsert_agent Kubernetes outbox event for
// every agent. S-183 uses it when the platform idle timeout changes so
// every Agent CR picks the new effective idleTimeout up without waiting
// for the agent's next status transition. Mirrors are idempotent
// server-side apply patches, so duplicate events are harmless.
type AgentMirrorQueue interface {
	EnqueueAllAgentUpserts(ctx context.Context) (int, error)
}

// --- Memory -------------------------------------------------------------

func (m *MemoryStore) GetPlatformSetting(_ context.Context, key string) (string, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	value, ok := m.platformSettings[key]
	return value, ok, nil
}

func (m *MemoryStore) SetPlatformSetting(_ context.Context, key, value, updatedBy string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.platformSettings == nil {
		m.platformSettings = map[string]string{}
	}
	m.platformSettings[key] = value
	_ = updatedBy // memory store keeps no audit row; the API layer audits.
	return nil
}

func (m *MemoryStore) EnqueueAllAgentUpserts(_ context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	count := 0
	for _, agent := range m.agents {
		m.enqueueAgentOutboxLocked(domain.KubernetesOpUpsertAgent, agent)
		count++
	}
	return count, nil
}

// --- Postgres -----------------------------------------------------------

func (p *PostgresStore) GetPlatformSetting(ctx context.Context, key string) (string, bool, error) {
	var value string
	err := p.pool.QueryRow(ctx, `SELECT value FROM platform_settings WHERE key = $1`, key).Scan(&value)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}

func (p *PostgresStore) SetPlatformSetting(ctx context.Context, key, value, updatedBy string) error {
	_, err := p.pool.Exec(ctx, `
		INSERT INTO platform_settings (key, value, updated_at, updated_by)
		VALUES ($1, $2, now(), $3)
		ON CONFLICT (key) DO UPDATE
		SET value = EXCLUDED.value, updated_at = now(), updated_by = EXCLUDED.updated_by`,
		key, value, updatedBy)
	return err
}

func (p *PostgresStore) EnqueueAllAgentUpserts(ctx context.Context) (int, error) {
	agents, err := p.ListAllAgents(ctx)
	if err != nil {
		return 0, err
	}
	if len(agents) == 0 {
		return 0, nil
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	for _, agent := range agents {
		if err := p.enqueueAgentOutboxTx(ctx, tx, domain.KubernetesOpUpsertAgent, agent); err != nil {
			_ = tx.Rollback(ctx)
			return 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return len(agents), nil
}
