// TG-2 (S-248): HTTP-level tests for the governed egress plane —
// typed resource registration, no-escalation grant validation, the
// internal policy endpoint contract (ETag/304/no-secrets), and typed
// agent discovery.
package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

const registryBase = "/api/v1/registry/"

// rawCall issues a request with an optional body and optional extra header,
// returning the recorder (no auth unless authorization is non-empty).
func rawCall(t *testing.T, handler http.Handler, method, path, body, authorization, ifNoneMatch string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func doRawNoAuth(t *testing.T, handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	// Default test auth is injected by middleware only when present; the
	// /internal path ignores auth entirely.
	return rawCall(t, handler, method, path, body, "", "")
}

func doRawWithHeader(t *testing.T, handler http.Handler, method, path, ifNoneMatch string) *httptest.ResponseRecorder {
	t.Helper()
	return rawCall(t, handler, method, path, "", "", ifNoneMatch)
}

func doRawJSON(t *testing.T, handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	return rawCall(t, handler, method, path, body, authAdmin, "")
}

func doAgentRaw(t *testing.T, handler http.Handler, agentID, token, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("X-Skquad-Agent-ID", agentID)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func ctxBackground() context.Context { return context.Background() }

func createTypedResource(t *testing.T, handler http.Handler, kind string, body map[string]any) domain.RegistryResource {
	t.Helper()
	var res domain.RegistryResource
	doJSON(t, handler, http.MethodPost, registryBase+kind, body, http.StatusCreated, &res)
	require.NotEmpty(t, res.ID)
	return res
}

func TestRegisterTypedResources(t *testing.T) {
	handler := newServer(testConfig(), storage.NewMemoryStore(), serverDeps{
		resourceSecrets: newFakeResourceSecretStore(),
		mcpEnumerate:    &stubMCPEnumerate{tools: defaultMCPTools(), hash: fakeMCPHash},
	})

	web := createTypedResource(t, handler, "web", map[string]any{
		"name":            "system-web",
		"endpoint_config": map[string]any{"deny_domains": []string{"metadata.example"}, "rate_per_min": 60, "max_bytes": 1048576},
		"policy_ceiling":  map[string]any{"deny_domains": []string{"metadata.example"}, "rate_per_min": 120, "max_bytes": 2097152, "allow_private_network": false},
		"risk_tier":       "low",
		"egress_class":    "public",
	})
	require.Equal(t, "web", string(web.Type))
	require.Equal(t, "low", web.RiskTier)
	require.Equal(t, "public", web.EgressClass)

	rest := createTypedResource(t, handler, "rest", map[string]any{
		"name":            "github-api",
		"endpoint_config": map[string]any{"base_url": "https://api.github.com", "auth_kind": "bearer"},
		"policy_ceiling":  map[string]any{"methods": []string{"GET", "POST"}, "path_allow": []string{"/repos/**"}, "rate_per_min": 60, "egress_class": "public"},
		"risk_tier":       "medium",
	})
	require.Equal(t, "rest", string(rest.Type))

	mcp := createTypedResource(t, handler, "mcp", map[string]any{
		"name":            "gh-mcp",
		"endpoint_config": map[string]any{"url": "https://api.github.com/mcp", "auth_kind": "bearer"},
		"policy_ceiling":  map[string]any{"tools_allow": []string{"list_pulls", "get_issue"}, "tools_deny": []string{"*_delete"}},
		"auth":            map[string]any{"token": fakeMCPTok},
	})
	require.Equal(t, "mcp", string(mcp.Type))

	git := createTypedResource(t, handler, "git", map[string]any{
		"name":           "skquad-git",
		"policy_ceiling": map[string]any{"repos_allow": []string{"rossbrigoli/*"}, "allow_push": false},
	})
	require.Equal(t, "git", string(git.Type))
}

func TestRegisterTypedResourceValidationRejects(t *testing.T) {
	handler := New(testConfig(), storage.NewMemoryStore())

	var body struct {
		Error      map[string]string `json:"error"`
		Violations []struct {
			Field string `json:"field"`
			Code  string `json:"code"`
		} `json:"violations"`
	}

	rec := doRawJSON(t, handler, http.MethodPost, registryBase+"rest", `{"name":"bad","endpoint_config":{"base_url":"https://x","evil_key":1}}`)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, "invalid_resource_shape", body.Error["code"])
	require.Len(t, body.Violations, 1)
	require.Equal(t, "endpoint_config.evil_key", body.Violations[0].Field)
	require.Equal(t, "unknown_key", body.Violations[0].Code)

	// Unknown ceiling key on mcp.
	rec = doRawJSON(t, handler, http.MethodPost, registryBase+"mcp", `{"name":"bad2","policy_ceiling":{"tools_allow":["x"],"unlimited":true}}`)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	// Bad risk tier.
	rec = doRawJSON(t, handler, http.MethodPost, registryBase+"web", `{"name":"bad3","risk_tier":"catastrophic"}`)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	// Untyped resource may not smuggle typed config.
	rec = doRawJSON(t, handler, http.MethodPost, registryBase+"skills", `{"name":"bad4","endpoint_config":{"base_url":"https://x"}}`)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, "not_typed", body.Violations[0].Code)
}

func TestLegacyAPITypeAliasMapsToRest(t *testing.T) {
	handler := New(testConfig(), storage.NewMemoryStore())

	// The legacy "apis" route now operates on canonical 'rest' resources.
	res := createTypedResource(t, handler, "apis", map[string]any{
		"name":            "legacy-api",
		"endpoint_config": map[string]any{"base_url": "https://api.example.com", "auth_kind": "bearer"},
		"policy_ceiling":  map[string]any{"methods": []string{"GET"}},
	})
	require.Equal(t, "rest", string(res.Type))

	// And the explicit "rest" route sees the same resource.
	var fetched domain.RegistryResource
	doJSON(t, handler, http.MethodGet, registryBase+"rest/"+res.ID, nil, http.StatusOK, &fetched)
	require.Equal(t, res.ID, fetched.ID)

	// Grants via the legacy type string canonicalize too.
	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, pathSquads, map[string]any{"name": "alias-squad"}, http.StatusCreated, &squad)
	var agent domain.Agent
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathAgents, map[string]any{"name": "alias-agent"}, http.StatusCreated, &agent)
	doJSON(t, handler, http.MethodPut, pathAgentsPrefix+agent.ID+pathPermissions, []map[string]any{
		{"resource_type": "api", "resource_id": res.ID},
	}, http.StatusOK, &[]map[string]any{})
	var perms []domain.AgentPermission
	doJSON(t, handler, http.MethodGet, pathAgentsPrefix+agent.ID+pathPermissions, nil, http.StatusOK, &perms)
	require.Len(t, perms, 1)
	require.Equal(t, domain.ResRest, perms[0].ResourceType)
}

func TestGrantNoEscalationEnforced(t *testing.T) {
	handler := New(testConfig(), storage.NewMemoryStore())
	res := createTypedResource(t, handler, "rest", map[string]any{
		"name":            "scoped-api",
		"endpoint_config": map[string]any{"base_url": "https://api.example.com"},
		"policy_ceiling":  map[string]any{"methods": []string{"GET"}, "path_allow": []string{"/read/**"}, "rate_per_min": 30, "egress_class": "public"},
	})
	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, pathSquads, map[string]any{"name": "esc-squad"}, http.StatusCreated, &squad)
	var agent domain.Agent
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathAgents, map[string]any{"name": "esc-agent"}, http.StatusCreated, &agent)

	// Escalating grant (POST not in ceiling) → 400 grant_escalation.
	rec := doRawJSON(t, handler, http.MethodPut, pathAgentsPrefix+agent.ID+pathPermissions,
		`[{"resource_type":"rest","resource_id":"`+res.ID+`","constraints":{"methods":["GET","POST"]}}]`)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	var body struct {
		Error      map[string]string `json:"error"`
		Violations []struct {
			Field string `json:"field"`
			Code  string `json:"code"`
		} `json:"violations"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, "grant_escalation", body.Error["code"])
	require.NotEmpty(t, body.Violations)
	require.Equal(t, "constraints.methods", body.Violations[0].Field)

	// Tightening grant → accepted.
	doJSON(t, handler, http.MethodPut, pathAgentsPrefix+agent.ID+pathPermissions, []map[string]any{
		{"resource_type": "rest", "resource_id": res.ID, "constraints": map[string]any{"methods": []string{"GET"}, "rate_per_min": 10}},
	}, http.StatusOK, &[]map[string]any{})

	// Nothing was written for the rejected attempt: only the tightened grant exists.
	var perms []domain.AgentPermission
	doJSON(t, handler, http.MethodGet, pathAgentsPrefix+agent.ID+pathPermissions, nil, http.StatusOK, &perms)
	require.Len(t, perms, 1)
	require.JSONEq(t, `{"methods":["GET"],"rate_per_min":10}`, string(perms[0].Constraints))
}

func TestPolicyEndpointContract(t *testing.T) {
	crWriter := &fakeCRWriter{}
	store := storage.NewMemoryStore()
	handler := NewWithCRWriter(testConfig(), store, crWriter)

	res := createTypedResource(t, handler, "rest", map[string]any{
		"name":            "policy-api",
		"endpoint_config": map[string]any{"base_url": "https://api.example.com", "auth_kind": "bearer"},
		"policy_ceiling":  map[string]any{"methods": []string{"GET", "POST"}, "path_allow": []string{"/repos/**"}, "rate_per_min": 60, "egress_class": "public"},
		"risk_tier":       "medium",
	})
	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, pathSquads, map[string]any{"name": "pol-squad"}, http.StatusCreated, &squad)
	var agent domain.Agent
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathAgents, map[string]any{"name": "pol-agent"}, http.StatusCreated, &agent)
	var identity domain.AgentIdentity
	doJSON(t, handler, http.MethodPost, pathAgentsPrefix+agent.ID+pathIdentity, nil, http.StatusCreated, &identity)
	token := crWriter.credentialTokens[identity.CredentialRef]
	require.NotEmpty(t, token)

	doJSON(t, handler, http.MethodPut, pathAgentsPrefix+agent.ID+pathPermissions, []map[string]any{
		{"resource_type": "rest", "resource_id": res.ID, "constraints": map[string]any{"methods": []string{"GET"}}},
	}, http.StatusOK, &[]map[string]any{})

	// 200 + shape + ETag
	rec := doRawNoAuth(t, handler, http.MethodGet, "/internal/v1/policy?agent="+agent.ID, "")
	require.Equal(t, http.StatusOK, rec.Code)
	etag := rec.Header().Get("ETag")
	require.NotEmpty(t, etag)

	var snap policySnapshot
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &snap))
	require.Equal(t, agent.ID, snap.AgentID)
	require.Equal(t, 1, snap.Generation)
	sum := sha256.Sum256([]byte(token))
	require.Equal(t, base64.RawStdEncoding.EncodeToString(sum[:]), snap.CredentialHash)
	require.Len(t, snap.Grants, 1)
	g := snap.Grants[0]
	require.Equal(t, res.ID, g.ResourceID)
	require.Equal(t, "rest", g.ResourceType)
	require.JSONEq(t, `{"base_url":"https://api.example.com","auth_kind":"bearer"}`, string(g.Config))
	require.JSONEq(t, `{"methods":["GET"]}`, string(g.Constraints))
	require.JSONEq(t, `{"methods":["GET","POST"],"path_allow":["/repos/**"],"rate_per_min":60,"egress_class":"public"}`, string(g.Ceiling))
	require.Equal(t, "medium", g.RiskTier)

	// 304 on If-None-Match
	rec = doRawWithHeader(t, handler, http.MethodGet, "/internal/v1/policy?agent="+agent.ID, etag)
	require.Equal(t, http.StatusNotModified, rec.Code)
	require.Empty(t, rec.Body.Bytes())

	// Policy change → new ETag
	doJSON(t, handler, http.MethodPut, pathAgentsPrefix+agent.ID+pathPermissions, []map[string]any{
		{"resource_type": "rest", "resource_id": res.ID, "constraints": map[string]any{"methods": []string{"GET"}, "rate_per_min": 5}},
	}, http.StatusOK, &[]map[string]any{})
	rec = doRawWithHeader(t, handler, http.MethodGet, "/internal/v1/policy?agent="+agent.ID, etag)
	require.Equal(t, http.StatusOK, rec.Code)
	require.NotEqual(t, etag, rec.Header().Get("ETag"))

	// Unknown agent → 404; missing param → 400.
	rec = doRawNoAuth(t, handler, http.MethodGet, "/internal/v1/policy?agent=00000000-0000-0000-0000-000000000000", "")
	require.Equal(t, http.StatusNotFound, rec.Code)
	rec = doRawNoAuth(t, handler, http.MethodGet, "/internal/v1/policy", "")
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestPolicyEndpointNeverLeaksSecrets(t *testing.T) {
	crWriter := &fakeCRWriter{}
	store := storage.NewMemoryStore()
	handler := NewWithCRWriter(testConfig(), store, crWriter)

	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, pathSquads, map[string]any{"name": "sec-squad"}, http.StatusCreated, &squad)
	var agent domain.Agent
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathAgents, map[string]any{"name": "sec-agent"}, http.StatusCreated, &agent)
	var identity domain.AgentIdentity
	doJSON(t, handler, http.MethodPost, pathAgentsPrefix+agent.ID+pathIdentity, nil, http.StatusCreated, &identity)
	token := crWriter.credentialTokens[identity.CredentialRef]

	// Seed a resource whose endpoint_config contains planted secret
	// material DIRECTLY in the store (bypassing HTTP schema validation).
	// The policy endpoint must strip it anyway — schema validation is the
	// first line, secret-stripping is the second, and neither alone is
	// trusted.
	evil, err := store.CreateResource(ctxBackground(), &domain.RegistryResource{
		Type:           domain.ResRest,
		Name:           "evil-api",
		AuthRef:        "k8s://skquad/evil-secret",
		Manifest:       json.RawMessage(`{}`),
		Status:         domain.ResourceActive,
		RegisteredBy:   "test",
		EndpointConfig: json.RawMessage(`{"base_url":"https://x","api_key":"HUNTER2-DO-NOT-LEAK","nested":{"password":"<REDACTED>","safe":"ok"},"auth":{"token":"<REDACTED>"}}`),
		PolicyCeiling:  json.RawMessage(`{"methods":["GET"]}`),
	})
	require.NoError(t, err)
	require.NoError(t, store.GrantAgentPermission(ctxBackground(), &domain.AgentPermission{
		AgentID: agent.ID, ResourceType: domain.ResRest, ResourceID: evil.ID, GrantedBy: "test",
	}))

	rec := doRawNoAuth(t, handler, http.MethodGet, "/internal/v1/policy?agent="+agent.ID, "")
	require.Equal(t, http.StatusOK, rec.Code)
	bodyStr := rec.Body.String()
	require.NotContains(t, bodyStr, "HUNTER2")
	require.NotContains(t, bodyStr, "s3cr3t")
	require.NotContains(t, bodyStr, "DO-NOT-LEAK")
	require.NotContains(t, bodyStr, "evil-secret")
	require.NotContains(t, bodyStr, "auth_ref")
	// The agent's own raw token must never appear (only its hash).
	require.NotContains(t, bodyStr, token)
	// Safe fields survive.
	require.Contains(t, bodyStr, `"safe":"ok"`)
}

func TestPolicyGenerationBound(t *testing.T) {
	crWriter := &fakeCRWriter{}
	handler := NewWithCRWriter(testConfig(), storage.NewMemoryStore(), crWriter)

	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, pathSquads, map[string]any{"name": "gen-squad"}, http.StatusCreated, &squad)
	var agent domain.Agent
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathAgents, map[string]any{"name": "gen-agent"}, http.StatusCreated, &agent)
	var identity domain.AgentIdentity
	doJSON(t, handler, http.MethodPost, pathAgentsPrefix+agent.ID+pathIdentity, nil, http.StatusCreated, &identity)
	require.Equal(t, 1, identity.Generation)

	rec := doRawNoAuth(t, handler, http.MethodGet, "/internal/v1/policy?agent="+agent.ID, "")
	var snap policySnapshot
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &snap))
	require.Equal(t, 1, snap.Generation)
	hashBefore := snap.CredentialHash
	require.NotEmpty(t, hashBefore)

	// Rotation bumps the generation and invalidates the old credential.
	var rotated domain.AgentIdentity
	doJSON(t, handler, http.MethodPost, pathAgentsPrefix+agent.ID+"/identity/rotate", nil, http.StatusOK, &rotated)
	require.Equal(t, 2, rotated.Generation)

	rec = doRawNoAuth(t, handler, http.MethodGet, "/internal/v1/policy?agent="+agent.ID, "")
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &snap))
	require.Equal(t, 2, snap.Generation)
	require.NotEqual(t, hashBefore, snap.CredentialHash)

	// Old token no longer authenticates at the CP agent middleware.
	oldToken := crWriter.credentialTokens[identity.CredentialRef]
	rec = doAgentRaw(t, handler, agent.ID, oldToken, http.MethodGet, "/api/v1/agents/me/resources")
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	// New token does.
	newToken := crWriter.credentialTokens[rotated.CredentialRef]
	rec = doAgentRaw(t, handler, agent.ID, newToken, http.MethodGet, "/api/v1/agents/me/resources")
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestDiscoveryIncludesTypedResources(t *testing.T) {
	crWriter := &fakeCRWriter{}
	handler := newServer(testConfig(), storage.NewMemoryStore(), serverDeps{
		crWriter:        crWriter,
		resourceSecrets: newFakeResourceSecretStore(),
		mcpEnumerate:    &stubMCPEnumerate{tools: defaultMCPTools(), hash: fakeMCPHash},
	})

	res := createTypedResource(t, handler, "mcp", map[string]any{
		"name":            "disc-mcp",
		"endpoint_config": map[string]any{"url": "https://mcp.example.com/mcp", "auth_kind": "bearer"},
		"policy_ceiling":  map[string]any{"tools_allow": []string{"a", "b"}, "max_args_bytes": 1024},
		"risk_tier":       "medium",
		"egress_class":    "public",
		"auth":            map[string]any{"token": fakeMCPTok},
	})
	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, pathSquads, map[string]any{"name": "disc-squad"}, http.StatusCreated, &squad)
	var agent domain.Agent
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathAgents, map[string]any{"name": "disc-agent"}, http.StatusCreated, &agent)
	var identity domain.AgentIdentity
	doJSON(t, handler, http.MethodPost, pathAgentsPrefix+agent.ID+pathIdentity, nil, http.StatusCreated, &identity)
	token := crWriter.credentialTokens[identity.CredentialRef]

	doJSON(t, handler, http.MethodPut, pathAgentsPrefix+agent.ID+pathPermissions, []map[string]any{
		{"resource_type": "mcp", "resource_id": res.ID, "constraints": map[string]any{"tools_allow": []string{"a"}}},
	}, http.StatusOK, &[]map[string]any{})

	var resources []map[string]any
	doAgentJSON(t, handler, agent.ID, token, http.MethodGet, "/api/v1/agents/me/resources", nil, http.StatusOK, &resources)
	require.Len(t, resources, 1)
	r := resources[0]
	require.Equal(t, "mcp", r["resource_type"])
	require.Equal(t, res.ID, r["resource_id"])
	require.Equal(t, "medium", r["risk_tier"])
	require.Equal(t, "public", r["egress_class"])
	cfg := r["endpoint_config"].(map[string]any)
	require.Equal(t, "https://mcp.example.com/mcp", cfg["url"])
	constraints := r["constraints"].(map[string]any)
	require.Equal(t, []any{"a"}, constraints["tools_allow"])
	// No auth_ref / secret material in discovery either.
	require.NotContains(t, r, "auth_ref")
	require.NotContains(t, r, "policy_ceiling")
}
