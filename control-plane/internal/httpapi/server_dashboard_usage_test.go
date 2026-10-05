package httpapi

// S-190: GET /dashboard/usage — daily series, MTD totals, provider/model
// rollup, and admin-vs-personal scoping.

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/auth"
	"github.com/rossbrigoli/skquad/control-plane/internal/config"
	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

const pathDashboardUsage = "/api/v1/dashboard/usage"

// meterNow records a metering event for the agent/squad pair with the
// given age. The relative `ago` is anchored to midday UTC today rather
// than the wall clock so that "fresh" events (ago < ~12h) always land on
// the current UTC calendar day.
//
// Without the midday anchor, running the suite just after 00:00 UTC (e.g.
// 00:00:57) pushes the 1h/2h/3h-old events into YESTERDAY's bucket, so
// the "today" day-axis assertions (input/output/token/cost totals) see
// zeros and the test fails every morning in the 00:00–03:00 UTC window.
// The metering query has no upper bound, so a midday-today timestamp is
// always included; the 40-day-old event still falls outside both the
// 30-day axis and the current month regardless of the anchor.
func meterNow(t *testing.T, store *storage.MemoryStore, squad domain.Squad, agent domain.Agent, providerID, model string, ago time.Duration, in, out int, cost float64) {
	t.Helper()
	now := time.Now().UTC()
	anchor := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.UTC)
	require.NoError(t, store.RecordMetering(context.Background(), &domain.MeteringEvent{
		AgentID: agent.ID, SquadID: squad.ID, ProviderID: providerID, Model: model,
		InputTokens: in, OutputTokens: out, Cost: cost, Currency: "USD",
		Timestamp: anchor.Add(-ago),
	}))
}

func registerProvider(t *testing.T, handler http.Handler, bearer, name, url string) string {
	t.Helper()
	var provider domain.AIProvider
	doJSONAuth(t, handler, bearer, http.MethodPost, "/api/v1/registry/ai-providers", map[string]any{
		"name": name, "kind": "openai", "base_url": url,
	}, http.StatusCreated, &provider)
	return provider.ID
}

func TestDashboardUsageAdminSeesSeriesMTDAndPlatform(t *testing.T) {
	t.Parallel()

	store := storage.NewMemoryStore()
	handler := New(testConfig(), store)

	squadA, agentA := dashboardFixture(t, handler, "", "Usage Alpha", []string{"todo"}, 0, 0)
	squadB, agentB := dashboardFixture(t, handler, "", "Usage Beta", []string{"todo"}, 0, 0)
	provX := registerProvider(t, handler, "", "Prov X", "http://x.invalid")
	provY := registerProvider(t, handler, "", "Prov Y", "http://y.invalid")

	// Fresh events (always inside the current month) + one old event that
	// must show up in series/totals but never in MTD numbers.
	meterNow(t, store, squadA, agentA, provX, "gpt-x", time.Hour, 100, 20, 0.5)
	meterNow(t, store, squadA, agentA, provX, "gpt-mini", 2*time.Hour, 10, 5, 0.25)
	meterNow(t, store, squadB, agentB, provY, "claude-y", 3*time.Hour, 200, 40, 1.0)
	meterNow(t, store, squadA, agentA, provX, "gpt-x", 40*24*time.Hour, 999, 999, 100.0)

	var payload DashboardUsagePayload
	doJSON(t, handler, http.MethodGet, pathDashboardUsage, nil, http.StatusOK, &payload)

	require.Equal(t, "all", payload.Scope)
	require.Len(t, payload.Days, defaultUsageDays)
	require.Equal(t, time.Now().UTC().Format(usageDayLayout), payload.Days[len(payload.Days)-1])
	require.Equal(t, "USD", payload.Currency)

	// MTD = fresh events only (0.5 + 0.25 + 1.0).
	require.InDelta(t, 1.75, payload.SquadMTDCost, 1e-9)

	require.Len(t, payload.BySquad, 2)
	require.Len(t, payload.ByAgent, 2)
	today := payload.Days[len(payload.Days)-1]
	oldDay := time.Now().UTC().Add(-40 * 24 * time.Hour).Format(usageDayLayout)

	alpha := payload.BySquad[0]
	require.Equal(t, "Usage Alpha", alpha.Name)
	require.Len(t, alpha.Points, defaultUsageDays)
	// Points align with the axis; the 40-day-old event is outside the
	// 30-day window, so only today carries usage for alpha.
	require.Equal(t, today, alpha.Points[len(alpha.Points)-1].Day)
	require.Equal(t, 110, alpha.Points[len(alpha.Points)-1].InputTokens)
	require.Equal(t, 25, alpha.Points[len(alpha.Points)-1].OutputTokens)
	require.Equal(t, 135, alpha.Points[len(alpha.Points)-1].Tokens)
	require.InDelta(t, 0.75, alpha.Points[len(alpha.Points)-1].Cost, 1e-9)
	for _, p := range alpha.Points[:len(alpha.Points)-1] {
		require.NotEqual(t, oldDay, p.Day, "old event must fall outside the day axis")
		require.Zero(t, p.Tokens)
	}

	// Provider/model rolling-30-day rollup: X has two models, Y has one.
	require.Len(t, payload.Providers, 2)
	provByName := map[string]ProviderUsage{}
	for _, p := range payload.Providers {
		provByName[p.ProviderName] = p
	}
	x := provByName["Prov X"]
	require.Equal(t, provX, x.ProviderID)
	require.Len(t, x.Models, 2)
	require.Equal(t, "gpt-mini", x.Models[0].Model) // sorted by model name
	require.Equal(t, 15, x.Models[0].Tokens)
	require.InDelta(t, 0.25, x.Models[0].Cost, 1e-9)
	require.Equal(t, "gpt-x", x.Models[1].Model)
	require.Equal(t, 120, x.Models[1].Tokens)
	require.InDelta(t, 0.75, x.Cost, 1e-9) // 0.5 (gpt-x) + 0.25 (gpt-mini), 30-day window
	require.Len(t, provByName["Prov Y"].Models, 1)

	// Admin-only platform block.
	require.NotNil(t, payload.Platform)
	require.InDelta(t, 101.75, payload.Platform.TotalCost, 1e-9)
	require.InDelta(t, 1.75, payload.Platform.MTDCost, 1e-9)
	require.GreaterOrEqual(t, payload.Platform.Users, 1)
	require.Equal(t, 2, payload.Platform.Agents)
}

func TestDashboardUsageProviderRollupLast30Days(t *testing.T) {
	t.Parallel()

	// S-230 regression: the gateway callback often carries NO provider_id.
	// The rollup must resolve the provider through the registered AI model
	// (model_name → ai_models.provider_id) and aggregate over the rolling
	// 30-day window — not the calendar month — so provider tiles show
	// real cost instead of "no usage".
	store := storage.NewMemoryStore()
	handler := New(testConfig(), store)

	squad, agent := dashboardFixture(t, handler, "", "Rollup Squad", []string{"todo"}, 0, 0)
	prov := registerProvider(t, handler, "", "Rollup Provider", "http://rollup.invalid")

	var model struct{ ID string }
	doJSONAuth(t, handler, "", http.MethodPost, "/api/v1/ai-models", map[string]any{
		"provider_id": prov, "model_name": "gpt-registered", "context_window": 128000,
		"pricing": validPricing(),
	}, http.StatusCreated, &model)
	require.NotEmpty(t, model.ID)

	// Empty provider_id on the events: only the registry join can attribute them.
	meterNow(t, store, squad, agent, "", "gpt-registered", 10*24*time.Hour, 1000, 500, 0.4)
	meterNow(t, store, squad, agent, "", "gpt-registered", time.Hour, 2000, 1000, 0.6)
	// Outside the rolling 30-day window: must NOT be counted.
	meterNow(t, store, squad, agent, "", "gpt-registered", 40*24*time.Hour, 9999, 9999, 99.0)

	var payload DashboardUsagePayload
	doJSON(t, handler, http.MethodGet, pathDashboardUsage, nil, http.StatusOK, &payload)

	require.Len(t, payload.Providers, 1)
	provUsage := payload.Providers[0]
	require.Equal(t, prov, provUsage.ProviderID)
	require.Equal(t, "Rollup Provider", provUsage.ProviderName)
	// Rolling 30 days = 0.4 + 0.6, never the 99.0 outside the window.
	require.InDelta(t, 1.0, provUsage.Cost, 1e-9)
	require.Equal(t, 4500, provUsage.Tokens)
	require.Len(t, provUsage.Models, 1)
	require.Equal(t, "gpt-registered", provUsage.Models[0].Model)
	require.InDelta(t, 1.0, provUsage.Models[0].Cost, 1e-9)
}

func TestDashboardUsageDaysParam(t *testing.T) {
	t.Parallel()

	store := storage.NewMemoryStore()
	handler := New(testConfig(), store)
	dashboardFixture(t, handler, "", "Param Squad", []string{"todo"}, 0, 0)

	var payload DashboardUsagePayload
	doJSON(t, handler, http.MethodGet, pathDashboardUsage+"?days=7", nil, http.StatusOK, &payload)
	require.Len(t, payload.Days, 7)

	doJSON(t, handler, http.MethodGet, pathDashboardUsage+"?days=999", nil, http.StatusOK, &payload)
	require.Len(t, payload.Days, maxUsageDays)

	doJSON(t, handler, http.MethodGet, pathDashboardUsage+"?days=nonsense", nil, http.StatusOK, &payload)
	require.Len(t, payload.Days, defaultUsageDays)

	doJSON(t, handler, http.MethodGet, pathDashboardUsage+"?days=0", nil, http.StatusOK, &payload)
	require.Len(t, payload.Days, defaultUsageDays)
}

func TestDashboardUsagePersonalScopeNoPlatformBlock(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.AuthMode = config.AuthOIDC
	store := storage.NewMemoryStore()
	handler := NewWithOIDCAuthenticator(cfg, store, headerOIDC{
		authOwner:  {Issuer: testIssuer, Subject: "own-1", Email: "owner@example.com", EmailVerified: true, Name: "Owner"},
		authViewer: {Issuer: testIssuer, Subject: "view-1", Email: "viewer@example.com", EmailVerified: true, Name: "Viewer"},
	})

	ownedSquad, ownedAgent := dashboardFixture(t, handler, authOwner, "Owned Usage", []string{"todo"}, 0, 0)
	// Provider registered out-of-band: metering carries the raw ID and the
	// memory store has no matching row, exercising the "unknown provider"
	// label path as well.
	const ownerProviderID = "11111111-1111-4111-8111-111111111111"
	meterNow(t, store, ownedSquad, ownedAgent, ownerProviderID, "gpt-x", time.Hour, 50, 10, 0.125)

	// Viewer without a grant: empty series, no platform block, zero MTD.
	var viewerPayload DashboardUsagePayload
	doJSONAuth(t, handler, authViewer, http.MethodGet, pathDashboardUsage, nil, http.StatusOK, &viewerPayload)
	require.Equal(t, "personal", viewerPayload.Scope)
	require.Empty(t, viewerPayload.BySquad)
	require.Empty(t, viewerPayload.ByAgent)
	require.Empty(t, viewerPayload.Providers)
	require.Zero(t, viewerPayload.SquadMTDCost)
	require.Nil(t, viewerPayload.Platform)

	// Owner sees their squad's series but still no platform block.
	var ownerPayload DashboardUsagePayload
	doJSONAuth(t, handler, authOwner, http.MethodGet, pathDashboardUsage, nil, http.StatusOK, &ownerPayload)
	require.Equal(t, "personal", ownerPayload.Scope)
	require.Len(t, ownerPayload.BySquad, 1)
	require.Equal(t, ownedSquad.ID, ownerPayload.BySquad[0].ID)
	require.InDelta(t, 0.125, ownerPayload.SquadMTDCost, 1e-9)
	require.Nil(t, ownerPayload.Platform)
	require.Len(t, ownerPayload.Providers, 1)
	require.Equal(t, ownerProviderID, ownerPayload.Providers[0].ProviderID)
	require.Equal(t, "unknown provider", ownerPayload.Providers[0].ProviderName)
}

func TestDashboardUsageUnauthenticated(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.AuthMode = config.AuthOIDC
	handler := NewWithOIDCAuthenticator(cfg, storage.NewMemoryStore(), fakeOIDC{err: auth.ErrUnauthorized})

	var body map[string]map[string]string
	doJSON(t, handler, http.MethodGet, pathDashboardUsage, nil, http.StatusUnauthorized, &body)
	require.Equal(t, "unauthorized", body["error"]["code"])
}
