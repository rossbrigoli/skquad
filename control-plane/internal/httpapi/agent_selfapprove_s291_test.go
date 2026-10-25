package httpapi

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/config"
	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

// S-291 (ADR-0016 D2): PATCH /agents/{id} accepts the tri-state
// self_approve_allowed override — true/false set it, null clears it
// (inherit the squad default), absent leaves it unchanged. Owner (or
// platform admin) only, audited like every other agent mutation.

func createSquadAndAgent(t *testing.T, handler http.Handler, authorization, squadName, agentName string) (domain.Squad, domain.Agent) {
	t.Helper()
	var squad domain.Squad
	doJSONAuth(t, handler, authorization, http.MethodPost, pathSquads, map[string]any{"name": squadName}, http.StatusCreated, &squad)
	var agent domain.Agent
	doJSONAuth(t, handler, authorization, http.MethodPost, pathSquadsPrefix+squad.ID+pathAgents, map[string]any{
		"name": agentName,
	}, http.StatusCreated, &agent)
	return squad, agent
}

func TestAgentPatchSelfApproveAllowed(t *testing.T) {
	t.Parallel()

	handler := New(testConfig(), storage.NewMemoryStore())
	_, agent := createSquadAndAgent(t, handler, "", "S291 Squad", "s291-agent")
	path := pathAgentsPrefix + agent.ID

	// Set true.
	var patched domain.Agent
	doJSON(t, handler, http.MethodPatch, path, map[string]any{"self_approve_allowed": true}, http.StatusOK, &patched)
	require.NotNil(t, patched.SelfApproveAllowed)
	require.True(t, *patched.SelfApproveAllowed)

	// Set false.
	doJSON(t, handler, http.MethodPatch, path, map[string]any{"self_approve_allowed": false}, http.StatusOK, &patched)
	require.NotNil(t, patched.SelfApproveAllowed)
	require.False(t, *patched.SelfApproveAllowed)

	// Persisted (GET reflects the override).
	var fetched domain.Agent
	doJSON(t, handler, http.MethodGet, path, nil, http.StatusOK, &fetched)
	require.NotNil(t, fetched.SelfApproveAllowed)
	require.False(t, *fetched.SelfApproveAllowed)

	// Null clears the override → inherit squad default. NOTE: fresh
	// decode targets — the response omits the field when nil, so a
	// reused struct would keep the stale value.
	var cleared domain.Agent
	doJSON(t, handler, http.MethodPatch, path, map[string]any{"self_approve_allowed": nil}, http.StatusOK, &cleared)
	require.Nil(t, cleared.SelfApproveAllowed)
	var fetchedAfterClear domain.Agent
	doJSON(t, handler, http.MethodGet, path, nil, http.StatusOK, &fetchedAfterClear)
	require.Nil(t, fetchedAfterClear.SelfApproveAllowed)
}

func TestAgentPatchSelfApproveInvalidType(t *testing.T) {
	t.Parallel()

	handler := New(testConfig(), storage.NewMemoryStore())
	_, agent := createSquadAndAgent(t, handler, "", "S291 Squad B", "s291-agent-b")

	var errBody map[string]map[string]string
	doJSONAuth(t, handler, "", http.MethodPatch, pathAgentsPrefix+agent.ID,
		map[string]any{"self_approve_allowed": "definitely"}, http.StatusBadRequest, &errBody)
	require.Equal(t, "bad_request", errBody["error"]["code"])
	require.Contains(t, errBody["error"]["message"], "self_approve_allowed")
}

func TestAgentPatchSelfApproveOwnerOnly(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.AuthMode = config.AuthOIDC
	store := storage.NewMemoryStore()
	handler := NewWithOIDCAuthenticator(cfg, store, headerOIDC{
		authOwner:  {Email: "owner@example.com", Name: "Owner"},
		authViewer: {Email: "viewer@example.com", Name: "Viewer"},
	})

	_, agent := createSquadAndAgent(t, handler, authOwner, "S291 Owned", "s291-owned-agent")

	// Non-owner is rejected (403) and nothing changes.
	var denied map[string]map[string]string
	doJSONAuth(t, handler, authViewer, http.MethodPatch, pathAgentsPrefix+agent.ID,
		map[string]any{"self_approve_allowed": false}, http.StatusForbidden, &denied)
	fresh, err := store.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	require.Nil(t, fresh.SelfApproveAllowed)

	// The owner can set it.
	var patched domain.Agent
	doJSONAuth(t, handler, authOwner, http.MethodPatch, pathAgentsPrefix+agent.ID,
		map[string]any{"self_approve_allowed": true}, http.StatusOK, &patched)
	require.NotNil(t, patched.SelfApproveAllowed)
	require.True(t, *patched.SelfApproveAllowed)
}

func TestAgentPatchSelfApproveAudited(t *testing.T) {
	t.Parallel()

	store := storage.NewMemoryStore()
	handler := New(testConfig(), store)
	squad, agent := createSquadAndAgent(t, handler, "", "S291 Audit", "s291-audit-agent")

	doJSONNoBody(t, handler, http.MethodPatch, pathAgentsPrefix+agent.ID,
		map[string]any{"self_approve_allowed": false}, http.StatusOK)

	entries, err := store.ListAudit(context.Background(), squad.ID, 100)
	require.NoError(t, err)
	var found bool
	for _, e := range entries {
		if e.Action == "agent.update" && e.ActorType == "user" && e.ResourceID == agent.ID {
			found = true
		}
	}
	require.True(t, found, "expected agent.update audit entry for self_approve_allowed change")
}
