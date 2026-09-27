package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/promptcompo"
)

// S-PROMPT WP2: every prompt-tier save must append its revision in the
// same transaction as the entity update, and revisions are retained
// forever (append-only, never pruned).

func expectedSHA(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func TestMemoryOrgPromptRevisionAppendAndRetention(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	ctx := context.Background()

	settings, err := store.GetInstanceSettings(ctx)
	require.NoError(t, err)
	require.Empty(t, settings.OrgPrompt)

	// First save: revision appended with derived token + sha.
	settings.OrgPrompt = "org policy v1"
	ctx1 := WithPromptRevision(ctx, PromptRevisionIntent{Scope: domain.PromptScopeOrganization, SavedBy: "admin-1"})
	saved, err := store.UpdateInstanceSettings(ctx1, settings)
	require.NoError(t, err)
	require.Equal(t, "org policy v1", saved.OrgPrompt)

	// No-op save (same content): NO new revision.
	ctx2 := WithPromptRevision(ctx, PromptRevisionIntent{Scope: domain.PromptScopeOrganization, SavedBy: "admin-2"})
	_, err = store.UpdateInstanceSettings(ctx2, settings)
	require.NoError(t, err)

	revs, err := store.ListPromptRevisions(ctx, domain.PromptScopeOrganization, "", 10)
	require.NoError(t, err)
	require.Len(t, revs, 1, "no-op save must not create a revision")
	require.Equal(t, "org policy v1", revs[0].Content)
	require.Equal(t, expectedSHA("org policy v1"), revs[0].SHA256)
	require.Equal(t, promptcompo.EstimateTokens("org policy v1"), revs[0].Tokens)
	require.Equal(t, "admin-1", revs[0].SavedBy)
	require.False(t, revs[0].SavedAt.IsZero())

	// Second distinct save: history grows, newest first, nothing pruned.
	settings.OrgPrompt = "org policy v2 — longer content for tokens"
	ctx3 := WithPromptRevision(ctx, PromptRevisionIntent{Scope: domain.PromptScopeOrganization, SavedBy: "admin-3"})
	_, err = store.UpdateInstanceSettings(ctx3, settings)
	require.NoError(t, err)

	revs, err = store.ListPromptRevisions(ctx, domain.PromptScopeOrganization, "", 10)
	require.NoError(t, err)
	require.Len(t, revs, 2, "append-only history keeps every revision (retention: forever)")
	require.Equal(t, "org policy v2 — longer content for tokens", revs[0].Content)
	require.Equal(t, "org policy v1", revs[1].Content)
}

func TestMemorySquadAndAgentPromptRevisions(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	ctx := context.Background()

	squad, err := store.CreateSquad(ctx, &domain.Squad{Name: "prompt-squad", OwnerID: "u1"})
	require.NoError(t, err)
	agent, err := store.CreateAgent(ctx, &domain.Agent{SquadID: squad.ID, Name: "worker", Role: "builder"})
	require.NoError(t, err)

	// Squad prompt change with intent → revision.
	squad.Prompt = "squad layer text"
	squadCtx := WithPromptRevision(ctx, PromptRevisionIntent{Scope: domain.PromptScopeSquad, ScopeID: squad.ID, SavedBy: "owner-1"})
	updatedSquad, err := store.UpdateSquad(squadCtx, squad)
	require.NoError(t, err)
	require.Equal(t, "squad layer text", updatedSquad.Prompt)

	revs, err := store.ListPromptRevisions(ctx, domain.PromptScopeSquad, squad.ID, 10)
	require.NoError(t, err)
	require.Len(t, revs, 1)
	require.Equal(t, expectedSHA("squad layer text"), revs[0].SHA256)

	// Unrelated squad update WITHOUT intent (name only) → no new revision.
	updatedSquad.Name = "renamed-squad"
	_, err = store.UpdateSquad(ctx, updatedSquad)
	require.NoError(t, err)
	revs, err = store.ListPromptRevisions(ctx, domain.PromptScopeSquad, squad.ID, 10)
	require.NoError(t, err)
	require.Len(t, revs, 1, "non-prompt updates must not create prompt revisions")

	// Agent system_prompt change with intent → agent-scoped revision.
	agent.SystemPrompt = "agent identity text"
	agentCtx := WithPromptRevision(ctx, PromptRevisionIntent{Scope: domain.PromptScopeAgent, ScopeID: agent.ID, SavedBy: "owner-2"})
	updatedAgent, err := store.UpdateAgent(agentCtx, agent)
	require.NoError(t, err)
	require.Equal(t, "agent identity text", updatedAgent.SystemPrompt)

	revs, err = store.ListPromptRevisions(ctx, domain.PromptScopeAgent, agent.ID, 10)
	require.NoError(t, err)
	require.Len(t, revs, 1)
	require.Equal(t, "agent identity text", revs[0].Content)
	require.Equal(t, "owner-2", revs[0].SavedBy)

	// Scopes are isolated: squad revisions never show agent rows.
	squadRevs, err := store.ListPromptRevisions(ctx, domain.PromptScopeSquad, squad.ID, 10)
	require.NoError(t, err)
	require.Len(t, squadRevs, 1)
}

func TestMemoryPromptRevisionDrainedOnce(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	ctx := context.Background()

	squad, err := store.CreateSquad(ctx, &domain.Squad{Name: "drain-squad", OwnerID: "u1"})
	require.NoError(t, err)

	drafted := WithPromptRevision(ctx, PromptRevisionIntent{Scope: domain.PromptScopeSquad, ScopeID: squad.ID, SavedBy: "u1"})

	squad.Prompt = "first"
	_, err = store.UpdateSquad(drafted, squad)
	require.NoError(t, err)

	// A second mutation on the SAME context must NOT re-record the
	// drained intent (same semantics as PendingAudits).
	squad.Prompt = "second"
	_, err = store.UpdateSquad(drafted, squad)
	require.NoError(t, err)

	revs, err := store.ListPromptRevisions(ctx, domain.PromptScopeSquad, squad.ID, 10)
	require.NoError(t, err)
	require.Len(t, revs, 1, "revision intent must be drained once per context")
	require.Equal(t, "first", revs[0].Content)
}

func TestMemoryAgentCreateWithPromptRecordsRevision(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	ctx := context.Background()

	squad, err := store.CreateSquad(ctx, &domain.Squad{Name: "born-with-prompt", OwnerID: "u1"})
	require.NoError(t, err)

	createCtx := WithPromptRevision(ctx, PromptRevisionIntent{Scope: domain.PromptScopeAgent, SavedBy: "u1"})
	agent, err := store.CreateAgent(createCtx, &domain.Agent{
		SquadID:      squad.ID,
		Name:         "prompted",
		SystemPrompt: "born with identity",
	})
	require.NoError(t, err)

	revs, err := store.ListPromptRevisions(ctx, domain.PromptScopeAgent, agent.ID, 10)
	require.NoError(t, err)
	require.Len(t, revs, 1, "create-with-prompt records the first revision keyed to the new agent id")
	require.Equal(t, "born with identity", revs[0].Content)
}

// --- Postgres (skipped without SKQUAD_TEST_DATABASE_URL) -------------------

func TestPostgresPromptRevisionAppendAndRetention(t *testing.T) {
	store := postgresTestStore(t)
	ctx := context.Background()
	f := newPGFixture(t, store)

	// Org tier: two saves → two revisions, newest first, nothing pruned.
	settings, err := store.GetInstanceSettings(ctx)
	require.NoError(t, err)
	settings.OrgPrompt = "pg org v1"
	_, err = store.UpdateInstanceSettings(WithPromptRevision(ctx, PromptRevisionIntent{
		Scope: domain.PromptScopeOrganization, SavedBy: f.user.ID,
	}), settings)
	require.NoError(t, err)

	settings.OrgPrompt = "pg org v2"
	_, err = store.UpdateInstanceSettings(WithPromptRevision(ctx, PromptRevisionIntent{
		Scope: domain.PromptScopeOrganization, SavedBy: f.user.ID,
	}), settings)
	require.NoError(t, err)

	revs, err := store.ListPromptRevisions(ctx, domain.PromptScopeOrganization, "", 50)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(revs), 2)
	require.Equal(t, "pg org v2", revs[0].Content)
	require.Equal(t, expectedSHA("pg org v2"), revs[0].SHA256)
	require.Equal(t, "pg org v1", revs[1].Content)

	// Squad tier through the normal UpdateSquad transaction.
	squad := f.squad
	squad.Prompt = "pg squad prompt"
	_, err = store.UpdateSquad(WithPromptRevision(ctx, PromptRevisionIntent{
		Scope: domain.PromptScopeSquad, ScopeID: squad.ID, SavedBy: f.user.ID,
	}), squad)
	require.NoError(t, err)

	revs, err = store.ListPromptRevisions(ctx, domain.PromptScopeSquad, squad.ID, 50)
	require.NoError(t, err)
	require.Len(t, revs, 1)
	require.Equal(t, "pg squad prompt", revs[0].Content)
	require.Equal(t, promptcompo.EstimateTokens("pg squad prompt"), revs[0].Tokens)
}

// TestPostgresPromptRevisionRollbackWithFailedAudit injects an audit-write
// failure (non-uuid resource id) alongside a prompt revision intent and
// asserts BOTH the squad prompt change and the revision row roll back —
// the revision is transactional with the mutation, not best-effort.
func TestPostgresPromptRevisionRollbackWithFailedAudit(t *testing.T) {
	store := postgresTestStore(t)
	ctx := context.Background()
	f := newPGFixture(t, store)

	before, err := store.GetSquad(ctx, f.squad.ID)
	require.NoError(t, err)
	require.Empty(t, before.Prompt)

	badEntry := NewAuditEntry("user", f.user.ID, "squad.update", "squad", "not-a-uuid", f.squad.ID, nil)
	mutated := f.squad
	mutated.Prompt = "must not survive a failed audit write"
	ctxBad := WithPromptRevision(WithPendingAudit(ctx, badEntry), PromptRevisionIntent{
		Scope: domain.PromptScopeSquad, ScopeID: f.squad.ID, SavedBy: f.user.ID,
	})
	_, err = store.UpdateSquad(ctxBad, mutated)
	require.Error(t, err, "audit insert with invalid uuid must fail")

	after, err := store.GetSquad(ctx, f.squad.ID)
	require.NoError(t, err)
	require.Equal(t, before.Prompt, after.Prompt, "squad prompt must have rolled back with its audit")

	revs, err := store.ListPromptRevisions(ctx, domain.PromptScopeSquad, f.squad.ID, 50)
	require.NoError(t, err)
	require.Empty(t, revs, "revision row must have rolled back with its mutation")
}
