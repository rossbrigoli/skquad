// S-309: MemoryStore bulk-import semantics — idempotency, dedupe,
// validation and atomicity.

package storage

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/stretchr/testify/require"
)

func importMeta(batchID, sha string) json.RawMessage {
	m, err := json.Marshal(map[string]string{
		"kind":           "memory_import",
		"batch_id":       batchID,
		"content_sha256": sha,
	})
	if err != nil {
		panic(err)
	}
	return m
}

func importableMemory(agentID, content, sha string) *domain.AgentMemory {
	return &domain.AgentMemory{
		AgentID:      agentID,
		Content:      content,
		TrustLevel:   "trusted_user",
		Provenance:   "migrated",
		ReviewStatus: "approved",
		Metadata:     importMeta("b1", sha),
	}
}

func seedImportAgent(t *testing.T, store *MemoryStore) string {
	t.Helper()
	ctx := context.Background()
	squad, err := store.CreateSquad(ctx, &domain.Squad{Name: "imp-squad-" + uuid.NewString()[:8], Status: domain.SquadActive})
	require.NoError(t, err)
	agent, err := store.CreateAgent(ctx, &domain.Agent{
		SquadID: squad.ID, Name: "imp-agent-" + uuid.NewString()[:8], Status: domain.AgentIdle,
	})
	require.NoError(t, err)
	return agent.ID
}

func TestImportAgentMemoryIdempotent(t *testing.T) {
	store := NewMemoryStore()
	agentID := seedImportAgent(t, store)
	ctx := context.Background()

	items := []*domain.AgentMemory{
		importableMemory(agentID, "first memory", "sha-a"),
		importableMemory(agentID, "second memory", "sha-b"),
	}
	imported, err := store.ImportAgentMemory(ctx, items)
	require.NoError(t, err)
	require.Equal(t, 2, imported)

	// Exact resend: all skipped.
	imported, err = store.ImportAgentMemory(ctx, items)
	require.NoError(t, err)
	require.Equal(t, 0, imported)

	// Same batch, one new sha: only the new row lands.
	imported, err = store.ImportAgentMemory(ctx, []*domain.AgentMemory{
		importableMemory(agentID, "first memory", "sha-a"),
		importableMemory(agentID, "third memory", "sha-c"),
	})
	require.NoError(t, err)
	require.Equal(t, 1, imported)

	mem, err := store.ListAgentMemory(ctx, agentID, "", nil, 50)
	require.NoError(t, err)
	require.Len(t, mem, 3)
	for _, m := range mem {
		require.Equal(t, "trusted_user", m.TrustLevel)
		require.Equal(t, "migrated", m.Provenance)
		require.Equal(t, "approved", m.ReviewStatus)
	}
}

func TestImportAgentMemorySameBatchDifferentAgents(t *testing.T) {
	store := NewMemoryStore()
	agentA := seedImportAgent(t, store)
	agentB := seedImportAgent(t, store)
	ctx := context.Background()

	// Identical content + batch but different agents: both import —
	// the dedupe key includes agent_id.
	for _, agentID := range []string{agentA, agentB} {
		imported, err := store.ImportAgentMemory(ctx, []*domain.AgentMemory{
			importableMemory(agentID, "shared text", "sha-s"),
		})
		require.NoError(t, err)
		require.Equal(t, 1, imported)
	}
}

func TestImportAgentMemoryValidation(t *testing.T) {
	store := NewMemoryStore()
	agentID := seedImportAgent(t, store)
	ctx := context.Background()

	// Unknown agent.
	_, err := store.ImportAgentMemory(ctx, []*domain.AgentMemory{
		importableMemory("missing-agent", "x", "sha-x"),
	})
	require.ErrorIs(t, err, ErrNotFound)

	// Missing idempotency keys.
	_, err = store.ImportAgentMemory(ctx, []*domain.AgentMemory{{
		AgentID: agentID, Content: "no metadata",
	}})
	require.ErrorIs(t, err, ErrInvalidInput)

	_, err = store.ImportAgentMemory(ctx, []*domain.AgentMemory{{
		AgentID:  agentID,
		Content:  "partial metadata",
		Metadata: json.RawMessage(`{"batch_id": "b1"}`),
	}})
	require.ErrorIs(t, err, ErrInvalidInput)
}

func TestImportAgentMemoryAtomicOnInvalidItem(t *testing.T) {
	store := NewMemoryStore()
	agentID := seedImportAgent(t, store)
	ctx := context.Background()

	// A valid item followed by an invalid one: nothing is imported.
	_, err := store.ImportAgentMemory(ctx, []*domain.AgentMemory{
		importableMemory(agentID, "valid", "sha-v"),
		{AgentID: agentID, Content: "no keys"},
	})
	require.ErrorIs(t, err, ErrInvalidInput)

	mem, err := store.ListAgentMemory(ctx, agentID, "", nil, 50)
	require.NoError(t, err)
	require.Empty(t, mem)
}

func TestImportAgentMemoryEmptyBatch(t *testing.T) {
	store := NewMemoryStore()
	imported, err := store.ImportAgentMemory(context.Background(), nil)
	require.NoError(t, err)
	require.Equal(t, 0, imported)
}
