package storage

// S-269: per-resource metering rollup tests (memory store). The
// Postgres mirror lives in postgres_resource_metering_test.go.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

func seedResourceMeteringFixture(t *testing.T, store Store) (*domain.Squad, *domain.Agent, *domain.RegistryResource, *domain.RegistryResource) {
	t.Helper()
	ctx := context.Background()
	user, err := store.UpsertUser(ctx, &domain.User{
		OIDCSubject: "res-meter-user", Email: "res-meter@example.test", Name: "Res Meter", Role: domain.RoleUser,
	})
	require.NoError(t, err)
	squad, err := store.CreateSquad(ctx, &domain.Squad{Name: "res-meter-squad", OwnerID: user.ID, Namespace: "res-meter-ns"})
	require.NoError(t, err)
	agent, err := store.CreateAgent(ctx, &domain.Agent{SquadID: squad.ID, Name: "res-agent", Role: "coder", Permissions: []byte("[]"), IdleTimeoutSec: 300})
	require.NoError(t, err)

	price := 0.05
	paid, err := store.CreateResource(ctx, &domain.RegistryResource{
		Type: domain.ResRest, Name: "paid-rest", Manifest: []byte("{}"), Status: domain.ResourceActive,
		RegisteredBy: user.ID, Metered: true, PricePerCall: &price, Currency: "USD",
	})
	require.NoError(t, err)
	free, err := store.CreateResource(ctx, &domain.RegistryResource{
		Type: domain.ResSkill, Name: "free-skill", Manifest: []byte("{}"), Status: domain.ResourceActive,
		RegisteredBy: user.ID,
	})
	require.NoError(t, err)
	return squad, agent, paid, free
}

func recordResourceUsage(t *testing.T, store Store, squadID, agentID, resourceID, resourceType string, cost float64, ts time.Time) {
	t.Helper()
	require.NoError(t, store.RecordMetering(context.Background(), &domain.MeteringEvent{
		AgentID: agentID, SquadID: squadID, Cost: cost, Currency: "USD", Timestamp: ts,
		ResourceID: resourceID, ResourceType: resourceType,
	}))
}

func TestMemoryStoreSumMeteringByResource(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	ctx := context.Background()
	squad, agent, paid, free := seedResourceMeteringFixture(t, store)

	now := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)

	// LLM event without resource attribution — must be ignored.
	require.NoError(t, store.RecordMetering(ctx, &domain.MeteringEvent{
		AgentID: agent.ID, SquadID: squad.ID, Model: "gpt-x", InputTokens: 10, OutputTokens: 2,
		Cost: 0.5, Currency: "USD", Timestamp: now,
	}))

	recordResourceUsage(t, store, squad.ID, agent.ID, paid.ID, "rest", 0.05, now.Add(-time.Hour))
	recordResourceUsage(t, store, squad.ID, agent.ID, paid.ID, "rest", 0.05, now.Add(-30*time.Minute))
	recordResourceUsage(t, store, squad.ID, agent.ID, free.ID, "skill", 0, now.Add(-2*time.Hour))

	rows, err := store.SumMeteringByResource(ctx, time.Time{}, "", "")
	require.NoError(t, err)
	require.Len(t, rows, 2) // the LLM event is excluded

	// Ordered by calls desc.
	require.Equal(t, paid.ID, rows[0].ResourceID)
	require.Equal(t, "rest", rows[0].ResourceType)
	require.Equal(t, "paid-rest", rows[0].Name)
	require.True(t, rows[0].Metered)
	require.EqualValues(t, 2, rows[0].Calls)
	require.InDelta(t, 0.10, rows[0].Cost, 1e-9)
	require.Equal(t, "USD", rows[0].Currency)
	require.True(t, rows[0].LastSeen.Equal(now.Add(-30*time.Minute)))

	// Non-metered: usage recorded, cost explicitly zero.
	require.Equal(t, free.ID, rows[1].ResourceID)
	require.False(t, rows[1].Metered)
	require.EqualValues(t, 1, rows[1].Calls)
	require.Equal(t, 0.0, rows[1].Cost)

	// since filter.
	rows, err = store.SumMeteringByResource(ctx, now.Add(-45*time.Minute), "", "")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, paid.ID, rows[0].ResourceID)
	require.EqualValues(t, 1, rows[0].Calls)

	// resourceID filter.
	rows, err = store.SumMeteringByResource(ctx, time.Time{}, free.ID, "")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, free.ID, rows[0].ResourceID)

	// type filter (canonical: legacy 'api' matches 'rest' rows).
	rows, err = store.SumMeteringByResource(ctx, time.Time{}, "", "api")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, paid.ID, rows[0].ResourceID)

	rows, err = store.SumMeteringByResource(ctx, time.Time{}, "", "mcp")
	require.NoError(t, err)
	require.Empty(t, rows)
}

func TestMemoryStoreResourceMeteringRoundTripsMeteringConfig(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	ctx := context.Background()
	_, _, paid, free := seedResourceMeteringFixture(t, store)

	got, err := store.GetResourceByID(ctx, paid.ID)
	require.NoError(t, err)
	require.True(t, got.Metered)
	require.NotNil(t, got.PricePerCall)
	require.Equal(t, 0.05, *got.PricePerCall)
	require.Equal(t, "USD", got.Currency)

	gotFree, err := store.GetResourceByID(ctx, free.ID)
	require.NoError(t, err)
	require.False(t, gotFree.Metered)
	require.Nil(t, gotFree.PricePerCall)

	// Update persists the metering config too.
	newPrice := 0.25
	got.Metered = true
	got.PricePerCall = &newPrice
	got.Currency = "EUR"
	updated, err := store.UpdateResource(ctx, got)
	require.NoError(t, err)
	require.True(t, updated.Metered)
	require.Equal(t, 0.25, *updated.PricePerCall)
	require.Equal(t, "EUR", updated.Currency)
}
