// TG-4c (S-259) tests: per-agent BYO REST credential write APIs —
// set / probe / delete, ownership posture, kind validation, stale-kind
// fallback and redaction. Fake token values only.
package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/config"
	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/kube"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

const (
	paAgentToken   = "fake-pat-agentA-DO-NOT-USE"
	paRotatedToken = "fake-pat-agentA-rotated-DO-NOT-USE"
)

// paFixture wires an OIDC-mode handler with three principals:
// squad owner, platform admin, and an outsider with nothing.
type paFixture struct {
	handler  http.Handler
	store    *storage.MemoryStore
	secrets  *fakeResourceSecretStore
	resource domain.RegistryResource
	agent    domain.Agent
	squadID  string
}

func paSetup(t *testing.T) paFixture {
	t.Helper()
	cfg := testConfig()
	cfg.AuthMode = config.AuthOIDC
	cfg.OIDCAdminGroups = []string{platformAdminGroup}
	store := storage.NewMemoryStore()
	secrets := newFakeResourceSecretStore()
	handler := newServer(cfg, store, serverDeps{
		oidcAuth: headerOIDC{
			authOwner:  {Email: "pa-owner@example.com", Name: "Owner"},
			authAdmin:  {Email: "pa-admin@example.com", Name: "Admin", Groups: []string{platformAdminGroup}},
			authViewer: {Email: "pa-outsider@example.com", Name: "Outsider"},
		},
		resourceSecrets: secrets,
	})
	f := paFixture{handler: handler, store: store, secrets: secrets}

	// Squad + agent owned by authOwner.
	var squad domain.Squad
	doJSONAuth(t, handler, authOwner, http.MethodPost, pathSquads, map[string]any{"name": "PA Squad"}, http.StatusCreated, &squad)
	f.squadID = squad.ID
	var agent domain.Agent
	doJSONAuth(t, handler, authOwner, http.MethodPost, pathSquadsPrefix+squad.ID+pathAgents, map[string]any{"name": "PA Agent"}, http.StatusCreated, &agent)
	f.agent = agent

	// BYO bearer resource registered by the platform admin.
	f.resource = createBYORestResourceAuthed(t, handler, "pa-api")
	return f
}

func createBYORestResourceAuthed(t *testing.T, handler http.Handler, name string) domain.RegistryResource {
	t.Helper()
	var res domain.RegistryResource
	doJSONAuth(t, handler, authAdmin, http.MethodPost, registryBase+"rest", byoBearerBody(name), http.StatusCreated, &res)
	require.NotEmpty(t, res.ID)
	return res
}

func paPath(resID, agentID string) string {
	return registryBase + "rest/" + resID + "/agent-credentials/" + agentID
}

func TestPerAgentCredentialSetProbeDelete(t *testing.T) {
	f := paSetup(t)
	secretName := kube.ResourceAgentSecretName(f.resource.ID, f.agent.ID)

	// Owner sets the agent's own credential (write-only).
	var out map[string]any
	doJSONAuth(t, f.handler, authOwner, http.MethodPut, paPath(f.resource.ID, f.agent.ID),
		map[string]any{"auth": map[string]string{"token": paAgentToken}}, http.StatusOK, &out)
	require.Equal(t, "agent", out["source"])
	require.NotContains(t, mustMarshalString(t, out), paAgentToken, "set response must not echo the value")

	stored, ok := f.secrets.get(secretName)
	require.True(t, ok, "per-agent secret must land under the derived name")
	require.Equal(t, paAgentToken, stored["token"])

	// Probe reports existence without values.
	var probe map[string]any
	doJSONAuth(t, f.handler, authOwner, http.MethodGet, paPath(f.resource.ID, f.agent.ID), nil, http.StatusOK, &probe)
	require.Equal(t, true, probe["has_own_credential"])
	require.Equal(t, "agent", probe["source"])
	require.NotContains(t, mustMarshalString(t, probe), paAgentToken)

	// Gateway resolution serves the agent's own credential.
	code, got := resolveCreds(t, f.handler, f.resource.ID, f.agent.ID)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, "agent", got.Scope)
	require.Equal(t, paAgentToken, got.Fields["token"])

	// Rotation replaces the value; old one is gone everywhere.
	doJSONAuth(t, f.handler, authOwner, http.MethodPut, paPath(f.resource.ID, f.agent.ID),
		map[string]any{"auth": map[string]string{"token": paRotatedToken}}, http.StatusOK, &out)
	stored, _ = f.secrets.get(secretName)
	require.Equal(t, paRotatedToken, stored["token"])
	_, got = resolveCreds(t, f.handler, f.resource.ID, f.agent.ID)
	require.Equal(t, paRotatedToken, got.Fields["token"])
	require.NotContains(t, mustMarshalString(t, got.Fields), paAgentToken)

	// Delete → falls back to the resource default.
	req := httptest.NewRequest(http.MethodDelete, paPath(f.resource.ID, f.agent.ID), nil)
	req.Header.Set("Authorization", authOwner)
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	require.Contains(t, f.secrets.deletedNames(), secretName)

	doJSONAuth(t, f.handler, authOwner, http.MethodGet, paPath(f.resource.ID, f.agent.ID), nil, http.StatusOK, &probe)
	require.Equal(t, false, probe["has_own_credential"])
	require.Equal(t, "resource_default", probe["source"])

	code, got = resolveCreds(t, f.handler, f.resource.ID, f.agent.ID)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, "resource", got.Scope, "after delete the agent uses the resource default")
	require.Equal(t, byoFakeToken, got.Fields["token"])
}

func TestPerAgentCredentialAuthorization(t *testing.T) {
	f := paSetup(t)
	body := map[string]any{"auth": map[string]string{"token": paAgentToken}}

	// Outsider (neither admin nor squad owner) is denied.
	var errBody map[string]map[string]string
	doJSONAuth(t, f.handler, authViewer, http.MethodPut, paPath(f.resource.ID, f.agent.ID), body, http.StatusForbidden, &errBody)
	doJSONAuth(t, f.handler, authViewer, http.MethodGet, paPath(f.resource.ID, f.agent.ID), nil, http.StatusForbidden, &errBody)
	doJSONAuth(t, f.handler, authViewer, http.MethodDelete, paPath(f.resource.ID, f.agent.ID), nil, http.StatusForbidden, &errBody)

	// Platform admin can set for ANY agent (not just owned squads).
	doJSONAuth(t, f.handler, authAdmin, http.MethodPut, paPath(f.resource.ID, f.agent.ID), body, http.StatusOK, &map[string]any{})
}

func TestPerAgentCredentialValidation(t *testing.T) {
	f := paSetup(t)

	t.Run("wrong field for kind", func(t *testing.T) {
		doJSONAuth(t, f.handler, authOwner, http.MethodPut, paPath(f.resource.ID, f.agent.ID),
			map[string]any{"auth": map[string]string{"password": "x"}}, http.StatusBadRequest, &map[string]any{})
	})
	t.Run("empty auth", func(t *testing.T) {
		doJSONAuth(t, f.handler, authOwner, http.MethodPut, paPath(f.resource.ID, f.agent.ID),
			map[string]any{"auth": map[string]string{}}, http.StatusBadRequest, &map[string]any{})
	})
	t.Run("unknown agent", func(t *testing.T) {
		doJSONAuth(t, f.handler, authOwner, http.MethodPut, paPath(f.resource.ID, "00000000-0000-0000-0000-000000000099"),
			map[string]any{"auth": map[string]string{"token": paAgentToken}}, http.StatusNotFound, &map[string]any{})
	})
	t.Run("auth_kind none resource", func(t *testing.T) {
		var open domain.RegistryResource
		doJSONAuth(t, f.handler, authAdmin, http.MethodPost, registryBase+"rest",
			map[string]any{
				"name":            "pa-open",
				"endpoint_config": map[string]any{"base_url": "https://api.example.com/v2", "auth_kind": "none"},
			}, http.StatusCreated, &open)
		doJSONAuth(t, f.handler, authOwner, http.MethodPut, paPath(open.ID, f.agent.ID),
			map[string]any{"auth": map[string]string{"token": paAgentToken}}, http.StatusBadRequest, &map[string]any{})
	})
	t.Run("non-rest resource type", func(t *testing.T) {
		var web domain.RegistryResource
		doJSONAuth(t, f.handler, authAdmin, http.MethodPost, registryBase+"web",
			map[string]any{"name": "pa-web"}, http.StatusCreated, &web)
		doJSONAuth(t, f.handler, authOwner, http.MethodPut, registryBase+"web/"+web.ID+"/agent-credentials/"+f.agent.ID,
			map[string]any{"auth": map[string]string{"token": paAgentToken}}, http.StatusBadRequest, &map[string]any{})
	})
}

func TestPerAgentStaleKindFallsBackToDefault(t *testing.T) {
	f := paSetup(t)
	// Agent owns a bearer credential on a bearer resource.
	doJSONAuth(t, f.handler, authOwner, http.MethodPut, paPath(f.resource.ID, f.agent.ID),
		map[string]any{"auth": map[string]string{"token": paAgentToken}}, http.StatusOK, &map[string]any{})

	// Resource rotates to basic (with its own basic default).
	doJSONAuth(t, f.handler, authAdmin, http.MethodPatch, registryBase+"rest/"+f.resource.ID, map[string]any{
		"endpoint_config": map[string]any{"base_url": "https://api.example.com/v2", "auth_kind": "basic"},
		"auth":            map[string]string{"username": "u", "password": "***"},
	}, http.StatusOK, &domain.RegistryResource{})

	// The stale bearer payload must NOT be served for the basic kind;
	// resolution falls back to the resource default.
	code, got := resolveCreds(t, f.handler, f.resource.ID, f.agent.ID)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, "resource", got.Scope, "stale per-agent kind must fall back, never inject the wrong shape")
	require.Equal(t, "u", got.Fields["username"])

	// Probe agrees: no usable own credential.
	var probe map[string]any
	doJSONAuth(t, f.handler, authOwner, http.MethodGet, paPath(f.resource.ID, f.agent.ID), nil, http.StatusOK, &probe)
	require.Equal(t, false, probe["has_own_credential"])
}

func TestPerAgentCredentialRedactionEveryRead(t *testing.T) {
	f := paSetup(t)
	doJSONAuth(t, f.handler, authOwner, http.MethodPut, paPath(f.resource.ID, f.agent.ID),
		map[string]any{"auth": map[string]string{"token": paAgentToken}}, http.StatusOK, &map[string]any{})

	// Admin reads of the resource: no value.
	var got domain.RegistryResource
	doJSONAuth(t, f.handler, authAdmin, http.MethodGet, registryBase+"rest/"+f.resource.ID, nil, http.StatusOK, &got)
	require.NotContains(t, mustMarshalString(t, got), paAgentToken)

	// Probe: no value.
	var probe map[string]any
	doJSONAuth(t, f.handler, authOwner, http.MethodGet, paPath(f.resource.ID, f.agent.ID), nil, http.StatusOK, &probe)
	require.NotContains(t, mustMarshalString(t, probe), paAgentToken)

	// Audit: set/delete actions carry ids and kind only.
	entries, err := f.store.ListAudit(context.Background(), "", 200)
	require.NoError(t, err)
	var setSeen bool
	for _, e := range entries {
		raw, _ := json.Marshal(e)
		require.NotContains(t, string(raw), paAgentToken, "audit %s leaked the per-agent secret", e.Action)
		if e.Action == "rest.credential.agent_set" {
			setSeen = true
			require.Equal(t, f.resource.ID, e.ResourceID)
			require.Equal(t, f.squadID, e.SquadID)
		}
	}
	require.True(t, setSeen, "agent_set audit must be recorded")
}

func mustMarshalString(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}
