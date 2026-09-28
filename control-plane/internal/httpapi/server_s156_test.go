package httpapi

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/config"
	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

// S-156: squad namespace is skquad-<owner>-<squad>, names are immutable,
// and squad/agent names are unique per user (reusable across users).

func newS156Handler(t *testing.T) http.Handler {
	t.Helper()
	cfg := testConfig()
	cfg.AuthMode = config.AuthOIDC
	store := storage.NewMemoryStore()
	return NewWithOIDCAuthenticator(cfg, store, headerOIDC{
		authOwner: {Email: "owner@example.com", Name: "Owner"},
		authAlice: {Email: "alice@example.com", Name: "Alice"},
	})
}

func TestS156SquadNamespaceNaming(t *testing.T) {
	t.Parallel()
	handler := newS156Handler(t)

	var squad domain.Squad
	doJSONAuth(t, handler, authOwner, http.MethodPost, pathSquads, map[string]any{
		"name": "My Cool Squad",
	}, http.StatusCreated, &squad)
	require.Equal(t, "skquad-owner-my-cool-squad", squad.Namespace)
}

func TestS156SquadNameUniquePerUser(t *testing.T) {
	t.Parallel()
	handler := newS156Handler(t)

	var squad domain.Squad
	doJSONAuth(t, handler, authOwner, http.MethodPost, pathSquads, map[string]any{
		"name": "Alpha",
	}, http.StatusCreated, &squad)

	rec := doRaw(t, handler, http.MethodPost, pathSquads, `{"name":"alpha"}`, authOwner)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "name_taken")

	// A different user may reuse the same squad name.
	var otherSquad domain.Squad
	doJSONAuth(t, handler, authAlice, http.MethodPost, pathSquads, map[string]any{
		"name": "Alpha",
	}, http.StatusCreated, &otherSquad)
	require.Equal(t, "skquad-alice-alpha", otherSquad.Namespace)
}

func TestS156NamespaceCollisionAcrossIdenticalDisplayNames(t *testing.T) {
	t.Parallel()
	cfg := testConfig()
	cfg.AuthMode = config.AuthOIDC
	store := storage.NewMemoryStore()
	handler := NewWithOIDCAuthenticator(cfg, store, headerOIDC{
		"Bearer r1": {Email: "ross1@example.com", Name: "Ross"},
		"Bearer r2": {Email: "ross2@example.com", Name: "Ross"},
	})

	var first domain.Squad
	doJSONAuth(t, handler, "Bearer r1", http.MethodPost, pathSquads, map[string]any{
		"name": "lab",
	}, http.StatusCreated, &first)
	require.Equal(t, "skquad-ross-lab", first.Namespace)

	var second domain.Squad
	doJSONAuth(t, handler, "Bearer r2", http.MethodPost, pathSquads, map[string]any{
		"name": "lab",
	}, http.StatusCreated, &second)
	require.Equal(t, "skquad-ross-lab-2", second.Namespace)
}

func TestS156SquadNameImmutable(t *testing.T) {
	t.Parallel()
	handler := newS156Handler(t)

	var squad domain.Squad
	doJSONAuth(t, handler, authOwner, http.MethodPost, pathSquads, map[string]any{
		"name": "Keep Me",
	}, http.StatusCreated, &squad)

	rec := doRaw(t, handler, http.MethodPatch, pathSquadsPrefix+squad.ID, `{"name":"Renamed"}`, authOwner)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "name_immutable")

	// Same name back (any case) is a no-op, not a rename.
	var unchanged domain.Squad
	doJSONAuth(t, handler, authOwner, http.MethodPatch, pathSquadsPrefix+squad.ID, map[string]any{
		"name": "keep me",
	}, http.StatusOK, &unchanged)
	require.Equal(t, "Keep Me", unchanged.Name)

	// Mission edits still work.
	var edited domain.Squad
	doJSONAuth(t, handler, authOwner, http.MethodPatch, pathSquadsPrefix+squad.ID, map[string]any{
		"mission": "still editable",
	}, http.StatusOK, &edited)
	require.Equal(t, "still editable", edited.Mission)
}

func TestS156AgentDeploymentNamingAndPerUserUniqueness(t *testing.T) {
	t.Parallel()
	handler := newS156Handler(t)

	var squad domain.Squad
	doJSONAuth(t, handler, authOwner, http.MethodPost, pathSquads, map[string]any{
		"name": "Builders",
	}, http.StatusCreated, &squad)
	var otherSquad domain.Squad
	doJSONAuth(t, handler, authAlice, http.MethodPost, pathSquads, map[string]any{
		"name": "Makers",
	}, http.StatusCreated, &otherSquad)

	var agent domain.Agent
	doJSONAuth(t, handler, authOwner, http.MethodPost, pathSquadsPrefix+squad.ID+pathAgents, map[string]any{
		"name": "Build Bot",
	}, http.StatusCreated, &agent)
	require.Equal(t, "skquad-owner-agent-build-bot", agent.DeploymentName)

	// Same owner, same agent name in ANOTHER squad → conflict (per-user rule).
	rec := doRaw(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathAgents, `{"name":"build bot"}`, authOwner)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "name_taken")

	// Different user may reuse the agent name.
	var otherAgent domain.Agent
	doJSONAuth(t, handler, authAlice, http.MethodPost, pathSquadsPrefix+otherSquad.ID+pathAgents, map[string]any{
		"name": "Build Bot",
	}, http.StatusCreated, &otherAgent)
	require.Equal(t, "skquad-alice-agent-build-bot", otherAgent.DeploymentName)
}

func TestS156AgentNameImmutable(t *testing.T) {
	t.Parallel()
	handler := newS156Handler(t)

	var squad domain.Squad
	doJSONAuth(t, handler, authOwner, http.MethodPost, pathSquads, map[string]any{
		"name": "Static",
	}, http.StatusCreated, &squad)
	var agent domain.Agent
	doJSONAuth(t, handler, authOwner, http.MethodPost, pathSquadsPrefix+squad.ID+pathAgents, map[string]any{
		"name": "Steady",
	}, http.StatusCreated, &agent)

	rec := doRaw(t, handler, http.MethodPatch, pathAgentsPrefix+agent.ID, `{"name":"Wobbly"}`, authOwner)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "name_immutable")

	var unchanged domain.Agent
	doJSONAuth(t, handler, authOwner, http.MethodPatch, pathAgentsPrefix+agent.ID, map[string]any{
		"name": "steady",
	}, http.StatusOK, &unchanged)
	require.Equal(t, "Steady", unchanged.Name)
	require.Equal(t, "skquad-owner-agent-steady", unchanged.DeploymentName)
}
