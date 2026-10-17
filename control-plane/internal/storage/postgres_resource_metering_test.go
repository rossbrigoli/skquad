package storage

// S-269 regression: SumMeteringByResource must run against the real
// Postgres schema (the memory-store tests cannot catch SQL-shape bugs —
// see the S-195 lesson in postgres_metering_daily_test.go). Exercises
// the resource join, the metered flag, the explicit-zero rule and the
// filters via SKQUAD_TEST_DATABASE_URL.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

func TestPostgresSumMeteringByResourceRealSchema(t *testing.T) {
	store := postgresTestStore(t)
	f := newPGFixture(t, store)
	ctx := context.Background()

	price := 0.05
	paid, err := store.CreateResource(ctx, &domain.RegistryResource{
		Type: domain.ResRest, Name: "pg-paid-rest", Manifest: []byte("{}"), Status: domain.ResourceActive,
		RegisteredBy: f.user.ID, Metered: true, PricePerCall: &price, Currency: "USD",
	})
	require.NoError(t, err)
	free, err := store.CreateResource(ctx, &domain.RegistryResource{
		Type: domain.ResSkill, Name: "pg-free-skill", Manifest: []byte("{}"), Status: domain.ResourceActive,
		RegisteredBy: f.user.ID,
	})
	require.NoError(t, err)

	now := time.Now().UTC()
	// LLM event without resource attribution — excluded.
	require.NoError(t, store.RecordMetering(ctx, &domain.MeteringEvent{
		AgentID: f.agent.ID, SquadID: f.squad.ID, Model: "pg-model",
		InputTokens: 10, OutputTokens: 2, Cost: 0.5, Currency: "USD", Timestamp: now,
	}))
	recordResourceUsage(t, store, f.squad.ID, f.agent.ID, paid.ID, "rest", 0.05, now.Add(-time.Hour))
	recordResourceUsage(t, store, f.squad.ID, f.agent.ID, paid.ID, "rest", 0.05, now.Add(-time.Minute))
	recordResourceUsage(t, store, f.squad.ID, f.agent.ID, free.ID, "skill", 0, now.Add(-2*time.Hour))

	rows, err := store.SumMeteringByResource(ctx, time.Time{}, "", "")
	require.NoError(t, err)
	require.Len(t, rows, 2)

	// calls desc → paid first.
	require.Equal(t, paid.ID, rows[0].ResourceID)
	require.Equal(t, "rest", rows[0].ResourceType)
	require.Equal(t, "pg-paid-rest", rows[0].Name)
	require.True(t, rows[0].Metered)
	require.EqualValues(t, 2, rows[0].Calls)
	require.InDelta(t, 0.10, rows[0].Cost, 1e-9)
	require.True(t, rows[0].LastSeen.After(now.Add(-2*time.Minute)))

	require.Equal(t, free.ID, rows[1].ResourceID)
	require.False(t, rows[1].Metered)
	require.EqualValues(t, 1, rows[1].Calls)
	require.Equal(t, 0.0, rows[1].Cost)

	// Filters.
	rows, err = store.SumMeteringByResource(ctx, time.Time{}, free.ID, "")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, free.ID, rows[0].ResourceID)

	rows, err = store.SumMeteringByResource(ctx, time.Time{}, "", "api") // legacy alias → rest
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, paid.ID, rows[0].ResourceID)

	rows, err = store.SumMeteringByResource(ctx, now.Add(-30*time.Minute), "", "")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, paid.ID, rows[0].ResourceID)
	require.EqualValues(t, 1, rows[0].Calls)
}
