package storage

// S-190: per-day metering aggregation for the dashboard histograms.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

func seedDailyMeteringFixture(t *testing.T, store Store) (*domain.Squad, *domain.Squad, *domain.Agent, *domain.Agent, *domain.LLMProvider, *domain.LLMProvider) {
	t.Helper()
	ctx := context.Background()
	user, err := store.UpsertUser(ctx, &domain.User{
		OIDCSubject: "daily-meter-user", Email: "daily-meter@example.test", Name: "Daily Meter", Role: domain.RoleUser,
	})
	require.NoError(t, err)

	squadA, err := store.CreateSquad(ctx, &domain.Squad{Name: "alpha-daily", OwnerID: user.ID, Namespace: "alpha-daily-ns"})
	require.NoError(t, err)
	squadB, err := store.CreateSquad(ctx, &domain.Squad{Name: "beta-daily", OwnerID: user.ID, Namespace: "beta-daily-ns"})
	require.NoError(t, err)

	agentA, err := store.CreateAgent(ctx, &domain.Agent{SquadID: squadA.ID, Name: "agent-alpha", Role: "coder", Permissions: []byte("[]"), IdleTimeoutSec: 300})
	require.NoError(t, err)
	agentB, err := store.CreateAgent(ctx, &domain.Agent{SquadID: squadB.ID, Name: "agent-beta", Role: "coder", Permissions: []byte("[]"), IdleTimeoutSec: 300})
	require.NoError(t, err)

	provX, err := store.CreateLLMProvider(ctx, &domain.LLMProvider{Name: "prov-x", Kind: "openai", BaseURL: "http://x.test"})
	require.NoError(t, err)
	provY, err := store.CreateLLMProvider(ctx, &domain.LLMProvider{Name: "prov-y", Kind: "anthropic", BaseURL: "http://y.test"})
	require.NoError(t, err)

	return squadA, squadB, agentA, agentB, provX, provY
}

func recordDaily(t *testing.T, store Store, agent *domain.Agent, squad *domain.Squad, provider *domain.LLMProvider, model string, day time.Time, in, out int, cost float64) {
	t.Helper()
	require.NoError(t, store.RecordMetering(context.Background(), &domain.MeteringEvent{
		AgentID: agent.ID, SquadID: squad.ID, ProviderID: provider.ID, Model: model,
		InputTokens: in, OutputTokens: out, Cost: cost, Currency: "USD", Timestamp: day,
	}))
}

func TestMemoryStoreSumMeteringDailyGroupsAndTotals(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	ctx := context.Background()
	squadA, squadB, agentA, agentB, provX, provY := seedDailyMeteringFixture(t, store)

	today := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	yesterday := today.AddDate(0, 0, -1)
	lastWeek := today.AddDate(0, 0, -7)

	// Same (day, squad, agent, provider, model) twice → one row, summed.
	recordDaily(t, store, agentA, squadA, provX, "gpt-x", today, 100, 20, 0.01)
	recordDaily(t, store, agentA, squadA, provX, "gpt-x", today, 50, 10, 0.005)
	// Different model, same day/provider → separate row.
	recordDaily(t, store, agentA, squadA, provX, "gpt-mini", today, 10, 5, 0.001)
	// Different provider.
	recordDaily(t, store, agentB, squadB, provY, "claude-y", today, 200, 40, 0.02)
	// Yesterday.
	recordDaily(t, store, agentA, squadA, provX, "gpt-x", yesterday, 7, 3, 0.0002)
	// Last week — excluded by `since`.
	recordDaily(t, store, agentA, squadA, provX, "gpt-x", lastWeek, 999, 999, 9.99)

	rows, err := store.SumMeteringDaily(ctx, yesterday, nil)
	require.NoError(t, err)
	require.Len(t, rows, 4, "last-week event must be excluded by since")

	byKey := map[string]domain.MeteringDailyRow{}
	for _, r := range rows {
		byKey[r.Day+"|"+r.SquadName+"|"+r.AgentName+"|"+r.ProviderName+"|"+r.Model] = r
	}

	got := byKey["2026-10-01|alpha-daily|agent-alpha|prov-x|gpt-x"]
	require.Equal(t, 150, got.InputTokens)
	require.Equal(t, 30, got.OutputTokens)
	require.InDelta(t, 0.015, got.Cost, 1e-9)
	require.Equal(t, squadA.ID, got.SquadID)
	require.Equal(t, agentA.ID, got.AgentID)
	require.Equal(t, provX.ID, got.ProviderID)
	require.Equal(t, "USD", got.Currency)

	require.Contains(t, byKey, "2026-10-01|alpha-daily|agent-alpha|prov-x|gpt-mini")
	require.Contains(t, byKey, "2026-10-01|beta-daily|agent-beta|prov-y|claude-y")
	require.Contains(t, byKey, "2026-09-30|alpha-daily|agent-alpha|prov-x|gpt-x")
}

func TestMemoryStoreSumMeteringDailySquadFilter(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	ctx := context.Background()
	squadA, squadB, agentA, agentB, provX, provY := seedDailyMeteringFixture(t, store)

	today := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	recordDaily(t, store, agentA, squadA, provX, "gpt-x", today, 100, 20, 0.01)
	recordDaily(t, store, agentB, squadB, provY, "claude-y", today, 200, 40, 0.02)

	rows, err := store.SumMeteringDaily(ctx, time.Time{}, []string{squadA.ID})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, squadA.ID, rows[0].SquadID)

	// Empty allowlist = all squads (documented semantics).
	all, err := store.SumMeteringDaily(ctx, time.Time{}, []string{})
	require.NoError(t, err)
	require.Len(t, all, 2)
}

func TestMemoryStoreSumMeteringDailySortedByDay(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	ctx := context.Background()
	squadA, _, agentA, _, provX, _ := seedDailyMeteringFixture(t, store)

	base := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
	for i := 3; i >= 0; i-- {
		recordDaily(t, store, agentA, squadA, provX, "gpt-x", base.AddDate(0, 0, -i), 1, 1, 0.001)
	}
	rows, err := store.SumMeteringDaily(ctx, time.Time{}, nil)
	require.NoError(t, err)
	require.Len(t, rows, 4)
	for i := 1; i < len(rows); i++ {
		require.GreaterOrEqual(t, rows[i].Day, rows[i-1].Day)
	}
}
