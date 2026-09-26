// S-133: platform admins can see, edit and delete any squad/agent even when
// they do not own them (orphan cleanup). S-130: GET /api/v1/versions exposes
// component versions to every authenticated user for the About page.

package httpapi

import (
	"net/http"
	"testing"

	"github.com/rossbrigoli/skquad/control-plane/internal/config"
	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
	"github.com/stretchr/testify/require"
)

func TestPlatformAdminCanEditAndDeleteForeignSquadAndAgent(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.AuthMode = config.AuthOIDC
	store := storage.NewMemoryStore()
	handler := NewWithOIDCAuthenticator(cfg, store, headerOIDC{
		authOwner: {Email: "owner@example.com", Name: "Owner"},
		authAdmin: {Email: adminEmail, Name: "Admin"},
	})
	promoteAdmin(t, store, handler, authAdmin)

	var squad domain.Squad
	doJSONAuth(t, handler, authOwner, http.MethodPost, pathSquads, map[string]any{
		"name": "Orphaned Squad",
	}, http.StatusCreated, &squad)
	var agent domain.Agent
	doJSONAuth(t, handler, authOwner, http.MethodPost, pathSquadsPrefix+squad.ID+pathAgents, map[string]any{
		"name": "test agent",
	}, http.StatusCreated, &agent)

	// Admin edits a squad they do not own (S-133).
	var editedSquad domain.Squad
	doJSONAuth(t, handler, authAdmin, http.MethodPatch, pathSquadsPrefix+squad.ID, map[string]any{
		"mission": "renamed by platform admin",
	}, http.StatusOK, &editedSquad)
	require.Equal(t, "renamed by platform admin", editedSquad.Mission)

	// Admin edits an agent they do not own (S-133).
	var editedAgent domain.Agent
	doJSONAuth(t, handler, authAdmin, http.MethodPatch, pathAgentsPrefix+agent.ID, map[string]any{
		"name": "renamed agent",
	}, http.StatusOK, &editedAgent)
	require.Equal(t, "renamed agent", editedAgent.Name)

	// Admin deletes the foreign agent, then the foreign squad (S-133).
	rec := doRaw(t, handler, http.MethodDelete, pathAgentsPrefix+agent.ID, "", authAdmin)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	rec = doRaw(t, handler, http.MethodDelete, pathSquadsPrefix+squad.ID, "", authAdmin)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	var gone map[string]map[string]string
	doJSONAuth(t, handler, authAdmin, http.MethodGet, pathSquadsPrefix+squad.ID, nil, http.StatusNotFound, &gone)
}

func TestNonOwnerNonAdminStillCannotEditOrDeleteForeignSquadAndAgent(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.AuthMode = config.AuthOIDC
	store := storage.NewMemoryStore()
	handler := NewWithOIDCAuthenticator(cfg, store, headerOIDC{
		authOwner:  {Email: "owner@example.com", Name: "Owner"},
		authViewer: {Email: "viewer@example.com", Name: "Viewer"},
	})

	var squad domain.Squad
	doJSONAuth(t, handler, authOwner, http.MethodPost, pathSquads, map[string]any{
		"name": "Private Squad",
	}, http.StatusCreated, &squad)
	var agent domain.Agent
	doJSONAuth(t, handler, authOwner, http.MethodPost, pathSquadsPrefix+squad.ID+pathAgents, map[string]any{
		"name": "private agent",
	}, http.StatusCreated, &agent)

	var forbidden map[string]map[string]string
	doJSONAuth(t, handler, authViewer, http.MethodPatch, pathSquadsPrefix+squad.ID, map[string]any{
		"mission": "nope",
	}, http.StatusForbidden, &forbidden)
	doJSONAuth(t, handler, authViewer, http.MethodDelete, pathSquadsPrefix+squad.ID, nil, http.StatusForbidden, &forbidden)
	doJSONAuth(t, handler, authViewer, http.MethodPatch, pathAgentsPrefix+agent.ID, map[string]any{
		"name": "nope",
	}, http.StatusForbidden, &forbidden)
	doJSONAuth(t, handler, authViewer, http.MethodDelete, pathAgentsPrefix+agent.ID, nil, http.StatusForbidden, &forbidden)
}

func TestVersionsEndpointVisibleToAllRoles(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.AuthMode = config.AuthOIDC
	cfg.APIServerVersion = "1.2.3"
	cfg.OperatorVersion = "1.2.3"
	cfg.AgentRuntimeVersion = "0.9.9"
	cfg.LLMGatewayVersion = "unknown"
	cfg.WebUIVersion = "1.2.3"
	cfg.GitCommit = "4ae923c5226229165b919d422f8ca056b39496b0"
	store := storage.NewMemoryStore()
	handler := NewWithOIDCAuthenticator(cfg, store, headerOIDC{
		authAlice: {Email: aliceEmail, Name: "Alice"},
		authAdmin: {Email: adminEmail, Name: "Admin"},
	})

	for _, token := range []string{authAlice, authAdmin} {
		var versions map[string]string
		doJSONAuth(t, handler, token, http.MethodGet, "/api/v1/versions", nil, http.StatusOK, &versions)
		require.Equal(t, "1.2.3", versions["api_server"])
		require.Equal(t, "1.2.3", versions["operator"])
		require.Equal(t, "0.9.9", versions["agent_runtime"])
		require.Equal(t, "unknown", versions["llm_gateway"])
		require.Equal(t, "1.2.3", versions["web_ui"])
		// S-141: the commit SHA travels with the versions so the About page can
		// show exactly what this release was built from.
		require.Equal(t, "4ae923c5226229165b919d422f8ca056b39496b0", versions["commit"])
	}

	// Unauthenticated callers must not learn versions.
	rec := doRaw(t, handler, http.MethodGet, "/api/v1/versions", "", "")
	require.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
}
