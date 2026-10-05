package storage

// S-190: per-day metering aggregation for the dashboard histograms.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

func seedDailyMeteringFixture(t *testing.T, store Store) (*domain.Squad, *domain.Squad, *domain.Agent, *domain.Agent, *domain.AIProvider, *domain.AIProvider) {
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

	provX, err := store.CreateAIProvider(ctx, &domain.AIProvider{Name: "prov-x", Kind: "openai", BaseURL: "http://x.test"})
	require.NoError(t, err)
	provY, err := store.CreateAIProvider(ctx, &domain.AIProvider{Name: "prov-y", Kind: "anthropic", BaseURL: "http://y.test"})
	require.NoError(t, err)

	return squadA, squadB, agentA, agentB, provX, provY
}

func recordDaily(t *testing.T, store Store, agent *domain.Agent, squad *domain.Squad, provider *domain.AIProvider, model string, day time.Time, in, out int, cost float64) {
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

// S-238: provider resolution for the daily rollup must key on the
// SERVED model (model_used, falling back to the requested model), not
// the requested model alone. Covers: served-model resolution when the
// requested alias is unregistered, unknown fallback, and no row
// multiplication when a model_name exists under two providers.
func TestMemoryStoreSumMeteringDailyResolvesProviderFromServedModel(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	ctx := context.Background()
	squadA, _, agentA, _, provX, provY := seedDailyMeteringFixture(t, store)

	_, err := store.CreateAIModel(ctx, &domain.AIModel{
		ProviderID: provX.ID, DisplayName: "X Served", ModelName: "served-x",
		Status: domain.ResourceActive, RegisteredBy: "tester",
	})
	require.NoError(t, err)
	// "shared-model" is registered under BOTH providers: the deterministic
	// lowest-provider-id pick must yield exactly one bucket, never two.
	_, err = store.CreateAIModel(ctx, &domain.AIModel{
		ProviderID: provY.ID, DisplayName: "Y Shared", ModelName: "shared-model",
		Status: domain.ResourceActive, RegisteredBy: "tester",
	})
	require.NoError(t, err)
	_, err = store.CreateAIModel(ctx, &domain.AIModel{
		ProviderID: provX.ID, DisplayName: "X Shared", ModelName: "shared-model",
		Status: domain.ResourceActive, RegisteredBy: "tester",
	})
	require.NoError(t, err)

	today := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	record := func(model, modelUsed string, cost float64) {
		require.NoError(t, store.RecordMetering(ctx, &domain.MeteringEvent{
			AgentID: agentA.ID, SquadID: squadA.ID, ProviderID: "",
			Model: model, ModelUsed: modelUsed,
			InputTokens: 10, OutputTokens: 1, Cost: cost, Currency: "USD", Timestamp: today,
		}))
	}
	// Requested alias is unregistered; served model resolves to provX.
	record("alias-fast", "served-x", 0.10)
	// No model_used: falls back to requested model; shared-model exists
	// under two providers → deterministic single pick (min provider id).
	record("shared-model", "", 0.20)
	// Nothing resolves → unknown provider path (empty provider id).
	record("mystery-model", "still-mystery", 0.30)

	rows, err := store.SumMeteringDaily(ctx, time.Time{}, nil)
	require.NoError(t, err)
	require.Len(t, rows, 3, "one bucket per event; shared-model must NOT split by provider")

	byModel := map[string]domain.MeteringDailyRow{}
	for _, r := range rows {
		byModel[r.Model] = r
	}

	served := byModel["alias-fast"]
	require.Equal(t, provX.ID, served.ProviderID, "model_used must resolve the provider")
	require.Equal(t, "prov-x", served.ProviderName)

	shared := byModel["shared-model"]
	wantShared := provX.ID
	if provY.ID < provX.ID {
		wantShared = provY.ID
	}
	require.Equal(t, wantShared, shared.ProviderID, "deterministic min-provider pick on ambiguous model_name")

	unknown := byModel["mystery-model"]
	require.Empty(t, unknown.ProviderID, "unresolvable model stays unlinked (unknown provider label)")
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
