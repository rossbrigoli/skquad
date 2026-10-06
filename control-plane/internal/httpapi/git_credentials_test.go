// TG-4b (S-244 series, card 518fc777) tests: git resource custody,
// per-agent credentials, discovery endpoint derivation and ceiling
// validation. Fake token values only ("FAKE-GIT-TOKEN…").
package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/config"
	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/kube"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

const (
	gitFakePAT       = "FAKE-GIT-TOKEN-resource-DO-NOT-USE"
	gitFakePATAgentA = "FAKE-GIT-TOKEN-agentA-DO-NOT-USE"
	gitFakePATAgentB = "FAKE-GIT-TOKEN-agentB-DO-NOT-USE"
)

func gitResourceBody(name string) map[string]any {
	return map[string]any{
		"name":            name,
		"endpoint_config": map[string]any{"base_url": "https://github.com"},
		"policy_ceiling":  map[string]any{"repos_allow": []string{"acme/*", "ross/app"}, "allow_push": true},
		"risk_tier":       "medium",
		"egress_class":    "public",
		"auth":            map[string]string{"token": gitFakePAT},
	}
}

// ── Registration + custody ───────────────────────────────────────────────

func TestGitResourceRegistrationCustody(t *testing.T) {
	secrets := newFakeResourceSecretStore()
	handler := newServer(testConfig(), storage.NewMemoryStore(), serverDeps{resourceSecrets: secrets})

	res := createTypedResource(t, handler, "git", gitResourceBody("gh-main"))
	require.Equal(t, domain.ResGit, res.Type)
	require.NotEmpty(t, res.AuthRef, "BYO git auth must set auth_ref")
	require.Contains(t, res.AuthRef, kube.GitSecretName(res.ID), "git custody uses the skquad-git- prefix")

	stored, ok := secrets.get(kube.GitSecretName(res.ID))
	require.True(t, ok)
	require.Equal(t, gitFakePAT, stored["token"])

	// The registration response must never carry the token.
	require.NotContains(t, mustMarshalString(t, res), gitFakePAT)
}

func TestGitRegistrationRequiresReposAllow(t *testing.T) {
	handler := newServer(testConfig(), storage.NewMemoryStore(), serverDeps{resourceSecrets: newFakeResourceSecretStore()})
	body := gitResourceBody("no-ceiling")
	delete(body, "policy_ceiling")
	rec := doRawJSON(t, handler, http.MethodPost, registryBase+"git", mustJSONString(t, body))
	require.Equal(t, http.StatusBadRequest, rec.Code, "git ceiling is required")

	body["policy_ceiling"] = map[string]any{"allow_push": true} // repos_allow missing
	rec = doRawJSON(t, handler, http.MethodPost, registryBase+"git", mustJSONString(t, body))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "repos_allow")

	body["policy_ceiling"] = map[string]any{"repos_allow": []string{}}
	rec = doRawJSON(t, handler, http.MethodPost, registryBase+"git", mustJSONString(t, body))
	require.Equal(t, http.StatusBadRequest, rec.Code, "empty repos_allow violates default-deny")
}

func TestGitAuthPayloadValidation(t *testing.T) {
	handler := newServer(testConfig(), storage.NewMemoryStore(), serverDeps{resourceSecrets: newFakeResourceSecretStore()})
	body := gitResourceBody("bad-auth")
	body["auth"] = map[string]string{"token": "", "extra": "x"}
	rec := doRawJSON(t, handler, http.MethodPost, registryBase+"git", mustJSONString(t, body))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "extra")
}

func mustJSONString(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}

// ── Internal credentials resolution + isolation ──────────────────────────

func TestGitInternalCredentialsResourceDefault(t *testing.T) {
	secrets := newFakeResourceSecretStore()
	handler := newServer(testConfig(), storage.NewMemoryStore(), serverDeps{resourceSecrets: secrets})
	res := createTypedResource(t, handler, "git", gitResourceBody("gh-default"))

	code, got := resolveCreds(t, handler, res.ID, "agent-1")
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, "bearer", got.Kind, "git custody kind is always bearer")
	require.Equal(t, "resource", got.Scope)
	require.Equal(t, gitFakePAT, got.Fields["token"])
}

func TestGitPerAgentCredentialIsolation(t *testing.T) {
	cfg := testConfig()
	cfg.AuthMode = config.AuthOIDC
	cfg.OIDCAdminGroups = []string{platformAdminGroup}
	secrets := newFakeResourceSecretStore()
	handler := newServer(cfg, storage.NewMemoryStore(), serverDeps{
		oidcAuth: headerOIDC{
			authOwner: {Email: "git-owner@example.com", Name: "Owner"},
			authAdmin: {Email: "git-admin@example.com", Name: "Admin", Groups: []string{platformAdminGroup}},
		},
		resourceSecrets: secrets,
	})

	// Squad + two agents owned by authOwner.
	var squad domain.Squad
	doJSONAuth(t, handler, authOwner, http.MethodPost, pathSquads, map[string]any{"name": "git-squad"}, http.StatusCreated, &squad)
	var agentA, agentB domain.Agent
	doJSONAuth(t, handler, authOwner, http.MethodPost, pathSquadsPrefix+squad.ID+pathAgents, map[string]any{"name": "agent-a"}, http.StatusCreated, &agentA)
	doJSONAuth(t, handler, authOwner, http.MethodPost, pathSquadsPrefix+squad.ID+pathAgents, map[string]any{"name": "agent-b"}, http.StatusCreated, &agentB)

	res := createTypedResourceAuthed(t, handler, "git-iso")

	credPath := func(agentID string) string {
		return registryBase + "git/" + res.ID + "/agent-credentials/" + agentID
	}

	// Owner sets agent A's own credential (write-only).
	var out map[string]any
	doJSONAuth(t, handler, authOwner, http.MethodPut, credPath(agentA.ID),
		map[string]any{"auth": map[string]string{"token": gitFakePATAgentA}}, http.StatusOK, &out)
	require.Equal(t, "agent", out["source"])
	require.NotContains(t, mustMarshalString(t, out), gitFakePATAgentA)

	// Secret lands under the git per-agent name.
	secretA := kube.GitAgentSecretName(res.ID, agentA.ID)
	require.Contains(t, secretA, "skquad-git-")
	stored, ok := secrets.get(secretA)
	require.True(t, ok)
	require.Equal(t, gitFakePATAgentA, stored["token"])

	// Resolution: agent A gets its OWN token; agent B gets the
	// resource default — never agent A's material.
	code, gotA := resolveCreds(t, handler, res.ID, agentA.ID)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, "agent", gotA.Scope)
	require.Equal(t, gitFakePATAgentA, gotA.Fields["token"])

	code, gotB := resolveCreds(t, handler, res.ID, agentB.ID)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, "resource", gotB.Scope)
	require.Equal(t, gitFakePAT, gotB.Fields["token"])
	require.NotEqual(t, gitFakePATAgentA, gotB.Fields["token"], "agent B must never resolve agent A's secret")

	// Probe for B: no own credential.
	var probe map[string]any
	doJSONAuth(t, handler, authOwner, http.MethodGet, credPath(agentB.ID), nil, http.StatusOK, &probe)
	require.Equal(t, false, probe["has_own_credential"])
	require.Equal(t, "bearer", probe["auth_kind"])

	// Delete A's credential → A falls back to the resource default.
	rec := rawCall(t, handler, http.MethodDelete, credPath(agentA.ID), "", authOwner, "")
	require.Equal(t, http.StatusNoContent, rec.Code)
	code, gotA2 := resolveCreds(t, handler, res.ID, agentA.ID)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, "resource", gotA2.Scope)
	require.Equal(t, gitFakePAT, gotA2.Fields["token"])
}

// ── Discovery endpoint derivation ────────────────────────────────────────

func TestGitDiscoveryEndpointDerivation(t *testing.T) {
	cfg := testConfig()
	cfg.ToolGatewayURL = "http://skquad-tool-gateway.skquad-system.svc.cluster.local:8080"
	crWriter := &fakeCRWriter{}
	handler := newServer(cfg, storage.NewMemoryStore(), serverDeps{crWriter: crWriter, resourceSecrets: newFakeResourceSecretStore()})

	res := createTypedResource(t, handler, "git", gitResourceBody("gh-discovery"))

	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, pathSquads, map[string]any{"name": "git-disc-squad"}, http.StatusCreated, &squad)
	var agent domain.Agent
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathAgents, map[string]any{"name": "git-disc-agent"}, http.StatusCreated, &agent)
	var identity domain.AgentIdentity
	doJSON(t, handler, http.MethodPost, pathAgentsPrefix+agent.ID+pathIdentity, nil, http.StatusCreated, &identity)
	token := crWriter.credentialTokens[identity.CredentialRef]

	doJSON(t, handler, http.MethodPut, pathAgentsPrefix+agent.ID+pathPermissions,
		[]map[string]any{{"resource_type": "git", "resource_id": res.ID}}, http.StatusOK, &[]map[string]any{})

	var resources []map[string]any
	doAgentJSON(t, handler, agent.ID, token, http.MethodGet, "/api/v1/agents/me/resources", nil, http.StatusOK, &resources)
	require.Len(t, resources, 1)
	got := resources[0]
	require.Equal(t, "git", got["resource_type"])
	require.Equal(t, cfg.ToolGatewayURL+"/git/"+res.ID+"/", got["endpoint"],
		"git discovery publishes the gateway git base, not the upstream")
	// No synthetic tool for git: the agent uses the git CLI.
	tools, hasTools := got["tools"]
	require.True(t, !hasTools || tools == nil, "git resources publish no synthetic tool")
}

func TestGitDiscoveryEndpointWithoutGatewayURL(t *testing.T) {
	cfg := testConfig() // ToolGatewayURL empty
	handler := newServer(cfg, storage.NewMemoryStore(), serverDeps{resourceSecrets: newFakeResourceSecretStore()})
	res := createTypedResource(t, handler, "git", gitResourceBody("gh-nogw"))
	require.NotEmpty(t, res.ID)
	// Registration still works; endpoint derivation is skipped (no
	// gateway configured). The gateway-side would fail closed anyway.
}

// createTypedResourceAuthed registers via the admin principal.
func createTypedResourceAuthed(t *testing.T, handler http.Handler, name string) domain.RegistryResource {
	t.Helper()
	var res domain.RegistryResource
	doJSONAuth(t, handler, authAdmin, http.MethodPost, registryBase+"git", gitResourceBody(name), http.StatusCreated, &res)
	require.NotEmpty(t, res.ID)
	return res
}
