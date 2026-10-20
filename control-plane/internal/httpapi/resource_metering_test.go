package httpapi

// S-269: per-resource metering API tests. Coverage:
//   - registration validation (metered ⇒ price >= 0)
//   - PATCH metering updates (effective-value invariant, un-metering)
//   - GET /api/v1/resources/{id}/metering (zero row vs real rollup, since filter)
//   - GET /api/v1/metering/resources (list + type filter, admin-only)
//   - POST /internal/v1/resource-usage ingest math: metered price ×
//     count, non-metered costs EXPLICITLY ZERO, agent→squad resolution.

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

func createMeteredResource(t *testing.T, handler http.Handler, name string, metered bool, price *float64) domain.RegistryResource {
	t.Helper()
	body := map[string]any{"name": name}
	if metered {
		body["metered"] = true
	}
	if price != nil {
		body["price_per_call"] = *price
	}
	var res domain.RegistryResource
	doJSONAuth(t, handler, authAdmin, http.MethodPost, "/api/v1/registry/skills", body, http.StatusCreated, &res)
	return res
}

func f64(v float64) *float64 { return &v }

func TestResourceMeteringCreateValidation(t *testing.T) {
	handler, _ := newCostFixture(t)

	// Metered without a price → 400.
	doJSONAuth(t, handler, authAdmin, http.MethodPost, "/api/v1/registry/skills",
		map[string]any{"name": "bad-metered", "metered": true},
		http.StatusBadRequest, &map[string]any{})

	// Metered with a negative price → 400.
	doJSONAuth(t, handler, authAdmin, http.MethodPost, "/api/v1/registry/skills",
		map[string]any{"name": "neg-metered", "metered": true, "price_per_call": -0.01},
		http.StatusBadRequest, &map[string]any{})

	// Metered with a zero price is legal (free-but-metered).
	res := createMeteredResource(t, handler, "zero-metered", true, f64(0))
	require.True(t, res.Metered)
	require.NotNil(t, res.PricePerCall)
	require.Equal(t, 0.0, *res.PricePerCall)

	// Non-metered default: metered=false, no price required.
	plain := createMeteredResource(t, handler, "plain", false, nil)
	require.False(t, plain.Metered)
	require.Nil(t, plain.PricePerCall)
}

func TestResourceMeteringUpdateValidation(t *testing.T) {
	handler, _ := newCostFixture(t)
	res := createMeteredResource(t, handler, "upd-metered", false, nil)

	// Flipping to metered without a price → 400, nothing applied.
	doJSONAuth(t, handler, authAdmin, http.MethodPatch, "/api/v1/registry/skills/"+res.ID,
		map[string]any{"metered": true}, http.StatusBadRequest, &map[string]any{})
	var after domain.RegistryResource
	doJSONAuth(t, handler, authAdmin, http.MethodGet, "/api/v1/registry/skills/"+res.ID, nil, http.StatusOK, &after)
	require.False(t, after.Metered)

	// Metered + price → applied.
	var updated domain.RegistryResource
	doJSONAuth(t, handler, authAdmin, http.MethodPatch, "/api/v1/registry/skills/"+res.ID,
		map[string]any{"metered": true, "price_per_call": 0.02},
		http.StatusOK, &updated)
	require.True(t, updated.Metered)
	require.Equal(t, 0.02, *updated.PricePerCall)

	// Un-metering keeps the stored price (re-enable friendly) and is legal.
	var off domain.RegistryResource
	doJSONAuth(t, handler, authAdmin, http.MethodPatch, "/api/v1/registry/skills/"+res.ID,
		map[string]any{"metered": false}, http.StatusOK, &off)
	require.False(t, off.Metered)
	require.NotNil(t, off.PricePerCall)

	// Removing the price while still metered → 400 is NOT expressible
	// via null in this API shape; setting a negative price is rejected.
	doJSONAuth(t, handler, authAdmin, http.MethodPatch, "/api/v1/registry/skills/"+res.ID,
		map[string]any{"price_per_call": -1}, http.StatusBadRequest, &map[string]any{})
}

func seedResourceUsage(t *testing.T, store *storage.MemoryStore, agentID, squadID, resourceID, resourceType string, count int, cost float64, ts time.Time) {
	t.Helper()
	for i := 0; i < count; i++ {
		require.NoError(t, store.RecordMetering(context.Background(), &domain.MeteringEvent{
			AgentID: agentID, SquadID: squadID, Cost: cost, Currency: "USD",
			Timestamp:  ts.Add(time.Duration(i) * time.Second),
			ResourceID: resourceID, ResourceType: resourceType,
		}))
	}
}

func TestResourceMeteringGetSingleAndList(t *testing.T) {
	handler, store := newCostFixture(t)
	now := time.Now().UTC()

	// Squad + agent for usage attribution.
	squad, agent := createSquadWithAgent(t, handler, authAlice, "Meter Squad", "meter-agent")

	metered := createMeteredResource(t, handler, "paid-api", true, f64(0.05))
	free := createMeteredResource(t, handler, "free-api", false, nil)

	seedResourceUsage(t, store, agent.ID, squad.ID, metered.ID, "skill", 3, 0.05, now.Add(-time.Hour))
	seedResourceUsage(t, store, agent.ID, squad.ID, free.ID, "skill", 2, 0, now.Add(-2*time.Hour))

	// Single-resource rollup: metered math.
	var row domain.ResourceMeteringRow
	doJSONAuth(t, handler, authAdmin, http.MethodGet, "/api/v1/resources/"+metered.ID+"/metering", nil, http.StatusOK, &row)
	require.Equal(t, metered.ID, row.ResourceID)
	require.Equal(t, "skill", row.ResourceType)
	require.Equal(t, "paid-api", row.Name)
	require.True(t, row.Metered)
	require.EqualValues(t, 3, row.Calls)
	require.InDelta(t, 0.15, row.Cost, 1e-9)
	require.Equal(t, "USD", row.Currency)
	require.False(t, row.LastSeen.IsZero())

	// Non-metered: explicit zero cost, calls still recorded.
	var freeRow domain.ResourceMeteringRow
	doJSONAuth(t, handler, authAdmin, http.MethodGet, "/api/v1/resources/"+free.ID+"/metering", nil, http.StatusOK, &freeRow)
	require.False(t, freeRow.Metered)
	require.EqualValues(t, 2, freeRow.Calls)
	require.Equal(t, 0.0, freeRow.Cost)

	// since filter excludes the older free-resource traffic.
	var sinceRow domain.ResourceMeteringRow
	doJSONAuth(t, handler, authAdmin,
		http.MethodGet, "/api/v1/resources/"+free.ID+"/metering?since="+now.Add(-30*time.Minute).Format(time.RFC3339),
		nil, http.StatusOK, &sinceRow)
	require.EqualValues(t, 0, sinceRow.Calls)
	require.Equal(t, free.Name, sinceRow.Name) // zero row still carries identity

	// Unknown resource → 404.
	doJSONAuth(t, handler, authAdmin, http.MethodGet, "/api/v1/resources/00000000-0000-4000-8000-000000000000/metering",
		nil, http.StatusNotFound, &map[string]any{})

	// List endpoint: both resources, ordered by calls desc.
	var listResp struct {
		Rows []domain.ResourceMeteringRow `json:"rows"`
	}
	doJSONAuth(t, handler, authAdmin, http.MethodGet, "/api/v1/metering/resources", nil, http.StatusOK, &listResp)
	require.Len(t, listResp.Rows, 2)
	require.Equal(t, metered.ID, listResp.Rows[0].ResourceID) // 3 calls first
	require.Equal(t, free.ID, listResp.Rows[1].ResourceID)

	// Type filter excludes everything (no rest usage).
	var emptyResp struct {
		Rows []domain.ResourceMeteringRow `json:"rows"`
	}
	doJSONAuth(t, handler, authAdmin, http.MethodGet, "/api/v1/metering/resources?type=rest", nil, http.StatusOK, &emptyResp)
	require.Empty(t, emptyResp.Rows)

	// Non-admins cannot read platform per-resource metering.
	doJSONAuth(t, handler, authAlice, http.MethodGet, "/api/v1/metering/resources", nil, http.StatusForbidden, &map[string]any{})
	doJSONAuth(t, handler, authAlice, http.MethodGet, "/api/v1/resources/"+metered.ID+"/metering", nil, http.StatusForbidden, &map[string]any{})
}

func TestResourceUsageIngestMath(t *testing.T) {
	handler, store := newCostFixture(t)
	squad, agent := createSquadWithAgent(t, handler, authAlice, "Ingest Squad", "ingest-agent")
	metered := createMeteredResource(t, handler, "ingest-paid", true, f64(0.03))
	free := createMeteredResource(t, handler, "ingest-free", false, nil)

	// Metered: 4 calls × 0.03 = 0.12, squad resolved from the agent.
	var ingested struct {
		Recorded bool    `json:"recorded"`
		Metered  bool    `json:"metered"`
		Cost     float64 `json:"cost"`
		Currency string  `json:"currency"`
	}
	doJSONAuth(t, handler, "", http.MethodPost, "/internal/v1/resource-usage",
		map[string]any{"resource_id": metered.ID, "resource_type": "skill", "agent_id": agent.ID, "count": 4},
		http.StatusCreated, &ingested)
	require.True(t, ingested.Recorded)
	require.True(t, ingested.Metered)
	require.InDelta(t, 0.12, ingested.Cost, 1e-9)
	require.Equal(t, "USD", ingested.Currency)

	// Non-metered: explicit zero cost.
	var freeIngest struct {
		Metered bool    `json:"metered"`
		Cost    float64 `json:"cost"`
	}
	doJSONAuth(t, handler, "", http.MethodPost, "/internal/v1/resource-usage",
		map[string]any{"resource_id": free.ID, "agent_id": agent.ID}, http.StatusCreated, &freeIngest)
	require.False(t, freeIngest.Metered)
	require.Equal(t, 0.0, freeIngest.Cost)

	// The events landed with the resource dimension and the agent's squad.
	rows, err := store.SumMeteringByResource(context.Background(), time.Time{}, metered.ID, "")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.EqualValues(t, 4, rows[0].Calls)
	require.InDelta(t, 0.12, rows[0].Cost, 1e-9)

	// Squad rollups see the same spend (per-squad metering unchanged).
	squadSum, err := store.SumMetering(context.Background(), squad.ID, "", time.Time{})
	require.NoError(t, err)
	require.InDelta(t, 0.12, squadSum.Cost, 1e-9)

	// Validation: missing fields, unknown resource, bad count.
	doJSONAuth(t, handler, "", http.MethodPost, "/internal/v1/resource-usage",
		map[string]any{"agent_id": agent.ID}, http.StatusBadRequest, &map[string]any{})
	doJSONAuth(t, handler, "", http.MethodPost, "/internal/v1/resource-usage",
		map[string]any{"resource_id": metered.ID}, http.StatusBadRequest, &map[string]any{})
	doJSONAuth(t, handler, "", http.MethodPost, "/internal/v1/resource-usage",
		map[string]any{"resource_id": "00000000-0000-4000-8000-000000000000", "agent_id": agent.ID},
		http.StatusNotFound, &map[string]any{})
	doJSONAuth(t, handler, "", http.MethodPost, "/internal/v1/resource-usage",
		map[string]any{"resource_id": metered.ID, "agent_id": agent.ID, "count": -2},
		http.StatusBadRequest, &map[string]any{})
}
