package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/promptcompo"
)

// Prompt revision intent (S-PROMPT WP2): a handler that changes a prompt
// tier declares the revision it is making by attaching the intent to the
// context. The mutating store method drains the intent inside its
// transaction and appends the prompt_revisions row together with the
// entity update — the same drain-in-tx pattern as the pending-audit queue
// (S-86), so a committed prompt change always carries its revision and a
// failed revision write rolls the change back.

type promptRevisionKey struct{}

// PromptRevisionIntent declares "this mutation changes prompt tier
// <Scope>/<ScopeID> to the entity's new value, on behalf of SavedBy".
// The store compares old vs new and only appends a revision when the
// content actually changed, so no-op saves never spam the history.
type PromptRevisionIntent struct {
	Scope   string // domain.PromptScope{Organization,Squad,Agent}
	ScopeID string // squad/agent id; "" for the organization tier
	SavedBy string
}

// WithPromptRevision returns a derived context whose next drained
// prompt-tier mutation appends a revision row in the same transaction.
func WithPromptRevision(ctx context.Context, intent PromptRevisionIntent) context.Context {
	return context.WithValue(ctx, promptRevisionKey{}, &intent)
}

// DrainPromptRevision removes and returns the pending revision intent.
// Called by mutating store methods inside their transaction/lock. A
// second drain on the same context returns nil (drained), matching the
// PendingAudits semantics.
func DrainPromptRevision(ctx context.Context) *PromptRevisionIntent {
	intent, ok := ctx.Value(promptRevisionKey{}).(*PromptRevisionIntent)
	if !ok || intent == nil || intent.Scope == "" {
		return nil
	}
	drained := *intent
	*intent = PromptRevisionIntent{}
	return &drained
}

// buildPromptRevision computes the derived fields (token estimate +
// content hash) shared by the Postgres and memory implementations. Only
// derived metadata is kept alongside the content — never anything that
// outlives the content itself.
func buildPromptRevision(intent *PromptRevisionIntent, content string) *domain.PromptRevision {
	sum := sha256.Sum256([]byte(content))
	return &domain.PromptRevision{
		Scope:   intent.Scope,
		ScopeID: intent.ScopeID,
		Content: content,
		Tokens:  promptcompo.EstimateTokens(content),
		SHA256:  hex.EncodeToString(sum[:]),
		SavedBy: intent.SavedBy,
	}
}

// appendPromptRevisionTx appends a revision row inside the caller's
// transaction when the tier content actually changed. oldQuery fetches
// the previous value for the change check in the same tx, so the compare
// and the append see one consistent snapshot.
func (p *PostgresStore) appendPromptRevisionTx(ctx context.Context, tx pgx.Tx, intent *PromptRevisionIntent, oldQuery, newContent string, oldArgs ...any) error {
	var old string
	if err := tx.QueryRow(ctx, oldQuery, oldArgs...).Scan(&old); err != nil {
		return mapPgErr(err)
	}
	if old == newContent {
		return nil
	}
	rev := buildPromptRevision(intent, newContent)
	return insertPromptRevisionTx(ctx, tx, rev)
}

// insertPromptRevisionTx writes one prepared revision row inside the
// caller's transaction.
func insertPromptRevisionTx(ctx context.Context, tx pgx.Tx, rev *domain.PromptRevision) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO prompt_revisions (scope, scope_id, content, tokens, sha256, saved_by)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, rev.Scope, rev.ScopeID, rev.Content, rev.Tokens, rev.SHA256, rev.SavedBy)
	return mapPgErr(err)
}

func (p *PostgresStore) GetInstanceSettings(ctx context.Context) (*domain.InstanceSettings, error) {
	var s domain.InstanceSettings
	err := p.pool.QueryRow(ctx, `
		SELECT org_name, org_prompt, updated_at, updated_by
		FROM instance_settings
		WHERE id = 1
	`).Scan(&s.OrgName, &s.OrgPrompt, &s.UpdatedAt, &s.UpdatedBy)
	if err != nil {
		return nil, mapPgErr(err)
	}
	return &s, nil
}

func (p *PostgresStore) UpdateInstanceSettings(ctx context.Context, settings *domain.InstanceSettings) (*domain.InstanceSettings, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer tx.Rollback(ctx)

	if intent := DrainPromptRevision(ctx); intent != nil {
		if err := p.appendPromptRevisionTx(ctx, tx, intent, `SELECT org_prompt FROM instance_settings WHERE id = 1`, settings.OrgPrompt); err != nil {
			return nil, err
		}
	}

	var updated domain.InstanceSettings
	err = tx.QueryRow(ctx, `
		UPDATE instance_settings
		SET org_name = $1, org_prompt = $2, updated_by = $3, updated_at = now()
		WHERE id = 1
		RETURNING org_name, org_prompt, updated_at, updated_by
	`, settings.OrgName, settings.OrgPrompt, settings.UpdatedBy).
		Scan(&updated.OrgName, &updated.OrgPrompt, &updated.UpdatedAt, &updated.UpdatedBy)
	if err != nil {
		return nil, mapPgErr(err)
	}
	if err := p.writePendingAuditsTx(ctx, tx, ""); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, mapPgErr(err)
	}
	return &updated, nil
}

func (p *PostgresStore) ListPromptRevisions(ctx context.Context, scope, scopeID string, limit int) ([]*domain.PromptRevision, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := p.pool.Query(ctx, `
		SELECT id::text, scope, scope_id, content, tokens, sha256, saved_by, saved_at
		FROM prompt_revisions
		WHERE scope = $1 AND scope_id = $2
		ORDER BY saved_at DESC, id DESC
		LIMIT $3
	`, scope, scopeID, limit)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer rows.Close()

	revisions := make([]*domain.PromptRevision, 0)
	for rows.Next() {
		var r domain.PromptRevision
		if err := rows.Scan(&r.ID, &r.Scope, &r.ScopeID, &r.Content, &r.Tokens, &r.SHA256, &r.SavedBy, &r.SavedAt); err != nil {
			return nil, mapPgErr(err)
		}
		revisions = append(revisions, &r)
	}
	return revisions, mapPgErr(rows.Err())
}

// --- MemoryStore implementation -------------------------------------------

// appendPromptRevisionLocked appends one revision row. Callers must hold
// m.mu (write lock). The row is never mutated or removed afterwards.
func (m *MemoryStore) appendPromptRevisionLocked(intent *PromptRevisionIntent, content string) {
	rev := buildPromptRevision(intent, content)
	rev.ID = uuid.NewString()
	rev.SavedAt = time.Now().UTC()
	m.promptRevisions = append(m.promptRevisions, rev)
}

func (m *MemoryStore) GetInstanceSettings(_ context.Context) (*domain.InstanceSettings, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.instanceSettings == nil {
		return nil, ErrNotFound
	}
	s := *m.instanceSettings
	return &s, nil
}

func (m *MemoryStore) UpdateInstanceSettings(ctx context.Context, settings *domain.InstanceSettings) (*domain.InstanceSettings, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.instanceSettings == nil {
		return nil, ErrNotFound
	}
	if intent := DrainPromptRevision(ctx); intent != nil && m.instanceSettings.OrgPrompt != settings.OrgPrompt {
		m.appendPromptRevisionLocked(intent, settings.OrgPrompt)
	}
	updated := *settings
	updated.UpdatedAt = time.Now().UTC()
	m.instanceSettings = &updated
	m.drainPendingAuditsLocked(ctx, "")
	return &updated, nil
}

func (m *MemoryStore) ListPromptRevisions(_ context.Context, scope, scopeID string, limit int) ([]*domain.PromptRevision, error) {
	if limit <= 0 {
		limit = 50
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*domain.PromptRevision, 0)
	// Newest-first, matching the Postgres ORDER BY saved_at DESC, id DESC.
	for i := len(m.promptRevisions) - 1; i >= 0 && len(out) < limit; i-- {
		r := m.promptRevisions[i]
		if r.Scope == scope && r.ScopeID == scopeID {
			rev := *r
			out = append(out, &rev)
		}
	}
	return out, nil
}
