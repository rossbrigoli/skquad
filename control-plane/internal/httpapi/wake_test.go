package httpapi

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

// S-154: POST /agents/{id}/wake pre-warms a scaled-to-zero agent when the
// chat UI sees the user start typing. Idle agents flip to busy (which drives
// the CR's desiredActive); every other status is reported back untouched.
func TestWakeAgent(t *testing.T) {
	t.Parallel()

	store := storage.NewMemoryStore()
	handler := New(testConfig(), store)

	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, pathSquads, map[string]any{
		"name": "Wake Squad",
	}, http.StatusCreated, &squad)

	var agent domain.Agent
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathAgents, map[string]any{
		"name":          "Sleeper",
		"role":          "worker",
		"system_prompt": "sleep then work",
	}, http.StatusCreated, &agent)
	require.Equal(t, domain.AgentIdle, agent.Status)

	var woken map[string]any
	doJSON(t, handler, http.MethodPost, pathAgentsPrefix+agent.ID+"/wake", map[string]any{}, http.StatusOK, &woken)
	require.Equal(t, true, woken["waking"])
	require.Equal(t, string(domain.AgentBusy), woken["status"])

	var refreshed domain.Agent
	doJSON(t, handler, http.MethodGet, pathAgentsPrefix+agent.ID, nil, http.StatusOK, &refreshed)
	require.Equal(t, domain.AgentBusy, refreshed.Status)

	// Second wake on an already-busy agent is a no-op, not an error.
	var again map[string]any
	doJSON(t, handler, http.MethodPost, pathAgentsPrefix+agent.ID+"/wake", map[string]any{}, http.StatusOK, &again)
	require.Equal(t, false, again["waking"])
	require.Equal(t, string(domain.AgentBusy), again["status"])

	// Error state must never be clobbered by a wake ping.
	require.NoError(t, store.SetAgentStatus(context.Background(), agent.ID, domain.AgentError))
	var errored map[string]any
	doJSON(t, handler, http.MethodPost, pathAgentsPrefix+agent.ID+"/wake", map[string]any{}, http.StatusOK, &errored)
	require.Equal(t, false, errored["waking"])
	require.Equal(t, string(domain.AgentError), errored["status"])

	// Unknown agent surfaces the storage error, not a 200.
	var missing map[string]map[string]string
	doJSON(t, handler, http.MethodPost, pathAgentsPrefix+"00000000-0000-0000-0000-000000000000/wake", map[string]any{}, http.StatusNotFound, &missing)
	require.Equal(t, "not_found", missing["error"]["code"])
}
