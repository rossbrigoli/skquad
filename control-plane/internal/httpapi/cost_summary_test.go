package httpapi

// S-203 WP1: scoped cost aggregation tests. A non-admin must only see
// costs for squads they own/are granted; the platform admin sees
// everything. Groupings: squad, agent, model, provider.

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

func newCostFixture(t *testing.T) (http.Handler, *storage.MemoryStore) {
	t.Helper()
	return newBudgetFixture(t)
}

// createSquadWithAgent creates a squad owned by `auth` with one agent and
// returns both ids.
func createSquadWithAgent(t *testing.T, handler http.Handler, auth, squadName, agentName string) (domain.Squad, domain.Agent) {
	t.Helper()
	var squad domain.Squad
	doJSONAuth(t, handler, auth, http.MethodPost, pathSquads, map[string]any{"name": squadName}, http.StatusCreated, &squad)
	var agent domain.Agent
	doJSONAuth(t, handler, auth, http.MethodPost, pathSquadsPrefix+squad.ID+pathAgents, map[string]any{"name": agentName}, http.StatusCreated, &agent)
	return squad, agent
}

func recordCost(t *testing.T, store *storage.MemoryStore, squadID, agentID, providerID, model string, cost float64, ts time.Time) {
	t.Helper()
	require.NoError(t, store.RecordMetering(context.Background(), &domain.MeteringEvent{
		AgentID: agentID, SquadID: squadID, ProviderID: providerID, Model: model,
		InputTokens: 1000, OutputTokens: 200, Cost: cost, Currency: "USD", Timestamp: ts,
	}))
}

func seriesNames(series []UsageSeries) []string {
	out := make([]string, 0, len(series))
	for _, s := range series {
		out = append(out, s.Name)
	}
	return out
}

func seriesCost(series []UsageSeries, id string) float64 {
	for _, s := range series {
		if s.ID == id {
			total := 0.0
			for _, p := range s.Points {
				total += p.Cost
			}
			return total
		}
	}
	return -1
}

// seedBudgetProvider seeds an LLM provider directly in the store and
// returns its id so metering rows resolve a real provider name.
func seedBudgetProvider(t *testing.T, store *storage.MemoryStore, name string) string {
	t.Helper()
	p, err := store.CreateLLMProvider(context.Background(), &domain.LLMProvider{
		Name: name, Kind: "openai", BaseURL: "https://" + name + ".skquad.test",
		APIKeyRef: "secret/" + name, Status: domain.ResourceActive, RegisteredBy: "test",
	})
	require.NoError(t, err)
	return p.ID
}

func TestCostSummaryScoping(t *testing.T) {
	handler, store := newCostFixture(t)
	now := time.Now().UTC()

	aliceSquad, aliceAgent := createSquadWithAgent(t, handler, authAlice, "Alice Squad", "AA")
	ownerSquad, ownerAgent := createSquadWithAgent(t, handler, authOwner, "Owner Squad", "OA")

	provX := seedBudgetProvider(t, store, "prov-x")
	provY := seedBudgetProvider(t, store, "prov-y")

	recordCost(t, store, aliceSquad.ID, aliceAgent.ID, provX, "model-alpha", 3, now)
	recordCost(t, store, ownerSquad.ID, ownerAgent.ID, provY, "model-beta", 7, now)

	// Alice: only her squad/agent/model/provider appear.
	var aliceView CostSummaryPayload
	doJSONAuth(t, handler, authAlice, http.MethodGet, "/api/v1/costs/summary", nil, http.StatusOK, &aliceView)
	require.Equal(t, "personal", aliceView.Scope)
	require.InDelta(t, 3.0, aliceView.TotalCost, 1e-9)
	require.InDelta(t, 3.0, aliceView.MTDCost, 1e-9)
	require.InDelta(t, 3.0, aliceView.RunningCost, 1e-9)
	require.Equal(t, []string{"Alice Squad"}, seriesNames(aliceView.BySquad))
	require.Equal(t, []string{"AA"}, seriesNames(aliceView.ByAgent))
	require.Equal(t, []string{"model-alpha"}, seriesNames(aliceView.ByModel))
	require.Equal(t, []string{"prov-x"}, seriesNames(aliceView.ByProvider))
	require.Nil(t, aliceView.Budget, "no budget set ⇒ no budget block")

	// Admin: everything.
	var adminView CostSummaryPayload
	doJSONAuth(t, handler, authAdmin, http.MethodGet, "/api/v1/costs/summary", nil, http.StatusOK, &adminView)
	require.Equal(t, "all", adminView.Scope)
	require.InDelta(t, 10.0, adminView.TotalCost, 1e-9)
	require.ElementsMatch(t, []string{"Alice Squad", "Owner Squad"}, seriesNames(adminView.BySquad))
	require.ElementsMatch(t, []string{"AA", "OA"}, seriesNames(adminView.ByAgent))
	require.ElementsMatch(t, []string{"model-alpha", "model-beta"}, seriesNames(adminView.ByModel))
	require.ElementsMatch(t, []string{"prov-x", "prov-y"}, seriesNames(adminView.ByProvider))
}

func TestCostSummaryTotalVsMTD(t *testing.T) {
	handler, store := newCostFixture(t)
	now := time.Now().UTC()
	squad, agent := createSquadWithAgent(t, handler, authOwner, "Time Squad", "TA")

	recordCost(t, store, squad.ID, agent.ID, "p", "m", 4, now)
	lastMonth := mtdStartUTC(now).AddDate(0, 0, -1)
	recordCost(t, store, squad.ID, agent.ID, "p", "m", 6, lastMonth)

	var view CostSummaryPayload
	doJSONAuth(t, handler, authOwner, http.MethodGet, "/api/v1/costs/summary?days=30", nil, http.StatusOK, &view)
	require.InDelta(t, 10.0, view.TotalCost, 1e-9, "total is all-time")
	require.InDelta(t, 4.0, view.MTDCost, 1e-9, "MTD excludes last month")
	require.InDelta(t, 4.0, view.RunningCost, 1e-9)
	// The 30-day window includes last month's day (it is within 30
	// days), so the series total is 10 while MTD is 4.
	require.InDelta(t, 10.0, seriesCost(view.BySquad, squad.ID), 1e-9)
	require.Len(t, view.Days, 30)
}

func TestCostSummaryIncludesCallerBudget(t *testing.T) {
	handler, _ := newCostFixture(t)
	ownerID := userIDFor(t, handler, authOwner)
	doJSONAuth(t, handler, authAdmin, http.MethodPut, "/api/v1/admin/budgets/users/"+ownerID, map[string]any{"monthly_budget_usd": 100}, http.StatusOK, &map[string]any{})

	var view CostSummaryPayload
	doJSONAuth(t, handler, authOwner, http.MethodGet, "/api/v1/costs/summary", nil, http.StatusOK, &view)
	require.NotNil(t, view.Budget)
	require.Equal(t, ownerID, view.Budget.UserID)
	require.NotNil(t, view.Budget.MonthlyBudgetUSD)
	require.Equal(t, 100.0, *view.Budget.MonthlyBudgetUSD)
	require.False(t, view.Budget.OverBudget)
}

func TestCostSummaryEmptyForUserWithoutSquads(t *testing.T) {
	handler, store := newCostFixture(t)
	// Cost exists in a squad alice cannot see.
	squad, agent := createSquadWithAgent(t, handler, authOwner, "Hidden Squad", "HA")
	recordCost(t, store, squad.ID, agent.ID, "p", "m", 5, time.Now().UTC())

	var view CostSummaryPayload
	doJSONAuth(t, handler, authAlice, http.MethodGet, "/api/v1/costs/summary", nil, http.StatusOK, &view)
	require.InDelta(t, 0, view.TotalCost, 1e-9)
	require.Empty(t, view.BySquad)
	require.Empty(t, view.ByModel)
}

func TestCostSummaryGrantedSquadVisible(t *testing.T) {
	handler, store := newCostFixture(t)
	squad, agent := createSquadWithAgent(t, handler, authOwner, "Granted Squad", "GA")
	recordCost(t, store, squad.ID, agent.ID, "p", "m", 2.5, time.Now().UTC())

	// Grant alice read access to owner's squad.
	doJSONAuth(t, handler, authOwner, http.MethodPost, pathSquadsPrefix+squad.ID+"/access-grants",
		map[string]any{"grantee_type": "user", "grantee_id": mustUserID(t, handler, authAlice), "permissions": "read"}, http.StatusCreated, &map[string]any{})

	var view CostSummaryPayload
	doJSONAuth(t, handler, authAlice, http.MethodGet, "/api/v1/costs/summary", nil, http.StatusOK, &view)
	require.InDelta(t, 2.5, view.TotalCost, 1e-9, "granted squad costs are visible")
}
