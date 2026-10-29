// S-309: Postgres bulk-import semantics. Gated on
// SKQUAD_TEST_DATABASE_URL (pgvector) like the other live-store tests.

package storage

import (
	"context"
	"testing"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/stretchr/testify/require"
)

func TestPostgresImportAgentMemoryIdempotent(t *testing.T) {
	store := postgresTestStore(t)
	f := newPGFixture(t, store)
	ctx := context.Background()

	items := []*domain.AgentMemory{
		importableMemory(f.agent.ID, "pg first memory", "pg-sha-a"),
		importableMemory(f.agent.ID, "pg second memory", "pg-sha-b"),
	}
	imported, err := store.ImportAgentMemory(ctx, items)
	require.NoError(t, err)
	require.Equal(t, 2, imported)

	// Exact resend: the unique index turns every insert into a skip.
	imported, err = store.ImportAgentMemory(ctx, items)
	require.NoError(t, err)
	require.Equal(t, 0, imported)

	// Same batch, new content: only the new row lands.
	imported, err = store.ImportAgentMemory(ctx, []*domain.AgentMemory{
		importableMemory(f.agent.ID, "pg first memory", "pg-sha-a"),
		importableMemory(f.agent.ID, "pg third memory", "pg-sha-c"),
	})
	require.NoError(t, err)
	require.Equal(t, 1, imported)

	mem, err := store.ListAgentMemory(ctx, f.agent.ID, f.squad.ID, nil, 50)
	require.NoError(t, err)
	require.Len(t, mem, 3)
	for _, m := range mem {
		require.Equal(t, "trusted_user", m.TrustLevel)
		require.Equal(t, "migrated", m.Provenance)
		require.Equal(t, "approved", m.ReviewStatus)
	}
}

func TestPostgresImportAgentMemoryValidation(t *testing.T) {
	store := postgresTestStore(t)
	f := newPGFixture(t, store)
	ctx := context.Background()

	// Missing idempotency keys → invalid input, nothing written.
	_, err := store.ImportAgentMemory(ctx, []*domain.AgentMemory{{
		AgentID: f.agent.ID, Content: "no keys",
	}})
	require.ErrorIs(t, err, ErrInvalidInput)

	// Unknown agent → FK violation surfaces as a conflict, and the
	// transaction rolls back the valid item that preceded it.
	_, err = store.ImportAgentMemory(ctx, []*domain.AgentMemory{
		importableMemory(f.agent.ID, "rolled back", "pg-sha-rb"),
		importableMemory("00000000-dead-beef-0000-000000000000", "ghost", "pg-sha-ghost"),
	})
	require.Error(t, err)

	mem, err := store.ListAgentMemory(ctx, f.agent.ID, f.squad.ID, nil, 50)
	require.NoError(t, err)
	require.Empty(t, mem)
}

func TestPostgresImportAgentMemoryEmptyBatch(t *testing.T) {
	store := postgresTestStore(t)
	imported, err := store.ImportAgentMemory(context.Background(), nil)
	require.NoError(t, err)
	require.Equal(t, 0, imported)
}
