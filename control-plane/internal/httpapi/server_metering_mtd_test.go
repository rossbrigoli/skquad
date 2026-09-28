package httpapi

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

type meteringBody struct {
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	Cost         float64 `json:"cost"`
}

// S-169: GET /agents/{id}/metering?since=<RFC3339> aggregates only the
// events at/after `since` (the month-to-date spend chip). Without the
// param the lifetime aggregate is unchanged, and a malformed since is a
// loud 400 — never a silent fallback to all-time.
func TestAgentMeteringSinceFilter(t *testing.T) {
	t.Parallel()

	store := storage.NewMemoryStore()
	handler := New(testConfig(), store)

	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, "/api/v1/squads", map[string]any{"name": "MTD Squad"}, http.StatusCreated, &squad)
	var agent domain.Agent
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+"/agents", map[string]any{
		"name": "mtd-agent", "role": "worker",
	}, http.StatusCreated, &agent)

	fresh := time.Now().UTC()
	old := fresh.AddDate(0, 0, -40) // well before the MTD window

	require.NoError(t, store.RecordMetering(context.Background(), &domain.MeteringEvent{
		AgentID: agent.ID, SquadID: squad.ID, InputTokens: 900, OutputTokens: 100, Cost: 0.09, Currency: "USD", Timestamp: old,
	}))
	require.NoError(t, store.RecordMetering(context.Background(), &domain.MeteringEvent{
		AgentID: agent.ID, SquadID: squad.ID, InputTokens: 100, OutputTokens: 50, Cost: 0.02, Currency: "USD", Timestamp: fresh,
	}))

	var lifetime meteringBody
	doJSON(t, handler, http.MethodGet, "/api/v1/agents/"+agent.ID+"/metering", nil, http.StatusOK, &lifetime)
	require.Equal(t, 1000, lifetime.InputTokens)
	require.Equal(t, 150, lifetime.OutputTokens)
	require.InDelta(t, 0.11, lifetime.Cost, 1e-9)

	since := fresh.Add(-time.Hour)
	var mtd meteringBody
	doJSON(t, handler, http.MethodGet, "/api/v1/agents/"+agent.ID+"/metering?since="+since.Format(time.RFC3339), nil, http.StatusOK, &mtd)
	require.Equal(t, 100, mtd.InputTokens)
	require.Equal(t, 50, mtd.OutputTokens)
	require.InDelta(t, 0.02, mtd.Cost, 1e-9)

	// Malformed since must fail loudly, not widen the window.
	var errBody map[string]any
	doJSON(t, handler, http.MethodGet, "/api/v1/agents/"+agent.ID+"/metering?since=yesterday", nil, http.StatusBadRequest, &errBody)
}
