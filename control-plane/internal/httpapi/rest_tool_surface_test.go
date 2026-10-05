// TG-4 (S-250): fetch-at-wake tool-surface tests — rest_call appears in
// GET /agents/me/resources iff the agent holds an active rest grant,
// with the effective (ceiling ∧ grant) bounds; it never appears in the
// builtin tools universe (GET /agents/me/tools).
package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

// restDiscoverySetup creates a rest resource (given ceiling), an agent in
// a fresh squad with a working credential, and optionally grants the
// resource with the given constraints. Returns the resource, agent, token
// and the agent's discovery payload.
func restDiscoverySetup(t *testing.T, ceiling map[string]any, constraints map[string]any, grant bool) (domain.RegistryResource, domain.Agent, string, []map[string]any) {
	t.Helper()
	crWriter := &fakeCRWriter{}
	handler := NewWithCRWriter(testConfig(), storage.NewMemoryStore(), crWriter)

	res := createTypedResource(t, handler, "rest", map[string]any{
		"name":            "byo-api",
		"endpoint_config": map[string]any{"base_url": "https://api.example.com/v2", "auth_kind": "bearer"},
		"policy_ceiling":  ceiling,
		"risk_tier":       "medium",
		"egress_class":    "public",
	})
	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, pathSquads, map[string]any{"name": "rest-surface-squad"}, http.StatusCreated, &squad)
	var agent domain.Agent
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathAgents, map[string]any{"name": "rest-surface-agent"}, http.StatusCreated, &agent)
	var identity domain.AgentIdentity
	doJSON(t, handler, http.MethodPost, pathAgentsPrefix+agent.ID+pathIdentity, nil, http.StatusCreated, &identity)
	token := crWriter.credentialTokens[identity.CredentialRef]

	if grant {
		payload := []map[string]any{{"resource_type": "rest", "resource_id": res.ID}}
		if constraints != nil {
			payload[0]["constraints"] = constraints
		}
		doJSON(t, handler, http.MethodPut, pathAgentsPrefix+agent.ID+pathPermissions, payload, http.StatusOK, &[]map[string]any{})
	}

	var resources []map[string]any
	doAgentJSON(t, handler, agent.ID, token, http.MethodGet, "/api/v1/agents/me/resources", nil, http.StatusOK, &resources)
	return res, agent, token, resources
}

func restCallTool(t *testing.T, resource map[string]any) map[string]any {
	t.Helper()
	raw, ok := resource["tools"]
	require.True(t, ok, "rest resource must carry a tools field")
	b, err := json.Marshal(raw)
	require.NoError(t, err)
	var tools []map[string]any
	require.NoError(t, json.Unmarshal(b, &tools))
	require.Len(t, tools, 1)
	return tools[0]
}

func TestRestCallToolSurfaceGranted(t *testing.T) {
	ceiling := map[string]any{
		"methods":            []string{"GET", "POST", "PUT"},
		"path_allow":         []string{"/issues/**", "/search"},
		"path_deny":          []string{"/admin/**"},
		"max_request_bytes":  8192,
		"max_response_bytes": 32768,
		"rate_per_min":       60,
	}
	constraints := map[string]any{
		"methods":           []string{"GET", "POST"},
		"path_allow":        []string{"/issues/**"},
		"max_request_bytes": 4096,
		"rate_per_min":      30,
	}
	res, _, _, resources := restDiscoverySetup(t, ceiling, constraints, true)
	require.Len(t, resources, 1)
	tool := restCallTool(t, resources[0])

	require.Equal(t, "rest_call", tool["name"])
	params := tool["parameters"].(map[string]any)
	require.Equal(t, "object", params["type"])
	require.Equal(t, []any{"resource_id", "method", "path"}, params["required"])
	props := params["properties"].(map[string]any)

	// resource_id is bound to the granted resource via const.
	require.Equal(t, res.ID, props["resource_id"].(map[string]any)["const"])

	// method enum = ceiling ∩ grant.
	enum := props["method"].(map[string]any)["enum"].([]any)
	require.Equal(t, []any{"GET", "POST"}, enum)

	// headers/body exist and are optional.
	require.Contains(t, props, "headers")
	require.Contains(t, props, "body")

	// Effective constraints mirror gateway folding: grant path_allow wins,
	// deny unions from the ceiling, numerics take the tightest bound.
	cons := tool["constraints"].(map[string]any)
	require.Equal(t, []any{"/issues/**"}, cons["path_allow"])
	require.Equal(t, []any{"/admin/**"}, cons["path_deny"])
	require.EqualValues(t, 4096, cons["max_request_bytes"])
	require.EqualValues(t, 32768, cons["max_response_bytes"])
	require.EqualValues(t, 30, cons["rate_per_min"])
	require.ElementsMatch(t, []any{"content-type", "accept"}, cons["allowed_headers"])
}

func TestRestCallToolSurfaceCeilingOnly(t *testing.T) {
	ceiling := map[string]any{
		"methods":           []string{"get", "post"},
		"path_allow":        []string{"/things/*"},
		"rate_per_min":      90,
		"max_request_bytes": 2048,
	}
	_, _, _, resources := restDiscoverySetup(t, ceiling, nil, true)
	require.Len(t, resources, 1)
	tool := restCallTool(t, resources[0])
	params := tool["parameters"].(map[string]any)
	enum := params["properties"].(map[string]any)["method"].(map[string]any)["enum"].([]any)
	require.Equal(t, []any{"GET", "POST"}, enum, "ceiling methods upper-cased")
	cons := tool["constraints"].(map[string]any)
	require.Equal(t, []any{"/things/*"}, cons["path_allow"])
	require.EqualValues(t, 2048, cons["max_request_bytes"])
	require.EqualValues(t, 262144, cons["max_response_bytes"], "default response cap when unset")
	require.EqualValues(t, 90, cons["rate_per_min"])
}

func TestRestCallToolDefaultDenyWithoutCeilingMethods(t *testing.T) {
	_, _, _, resources := restDiscoverySetup(t, map[string]any{"path_allow": []string{"/x"}}, nil, true)
	require.Len(t, resources, 1)
	tool := restCallTool(t, resources[0])
	enum := tool["parameters"].(map[string]any)["properties"].(map[string]any)["method"].(map[string]any)["enum"].([]any)
	require.Empty(t, enum, "unset ceiling methods ⇒ empty enum (default-deny)")
}

func TestRestCallToolAbsentWithoutGrant(t *testing.T) {
	_, _, _, resources := restDiscoverySetup(t,
		map[string]any{"methods": []string{"GET"}, "path_allow": []string{"/issues/**"}},
		nil, false)
	require.Empty(t, resources, "ungranted agent sees no resources — and thus no rest_call")
}

func TestRestCallToolAbsentForNonRestResources(t *testing.T) {
	crWriter := &fakeCRWriter{}
	handler := NewWithCRWriter(testConfig(), storage.NewMemoryStore(), crWriter)
	web := createTypedResource(t, handler, "web", map[string]any{
		"name":           "sys-web",
		"policy_ceiling": map[string]any{"max_bytes": 1024},
	})
	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, pathSquads, map[string]any{"name": "web-only-squad"}, http.StatusCreated, &squad)
	var agent domain.Agent
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathAgents, map[string]any{"name": "web-only-agent"}, http.StatusCreated, &agent)
	var identity domain.AgentIdentity
	doJSON(t, handler, http.MethodPost, pathAgentsPrefix+agent.ID+pathIdentity, nil, http.StatusCreated, &identity)
	token := crWriter.credentialTokens[identity.CredentialRef]
	doJSON(t, handler, http.MethodPut, pathAgentsPrefix+agent.ID+pathPermissions,
		[]map[string]any{{"resource_type": "web", "resource_id": web.ID}}, http.StatusOK, &[]map[string]any{})

	var resources []map[string]any
	doAgentJSON(t, handler, agent.ID, token, http.MethodGet, "/api/v1/agents/me/resources", nil, http.StatusOK, &resources)
	require.Len(t, resources, 1)
	require.NotContains(t, resources[0], "tools", "web grants publish no tool surface in v1")
}

func TestRestCallNotInBuiltinUniverse(t *testing.T) {
	crWriter := &fakeCRWriter{}
	handler := NewWithCRWriter(testConfig(), storage.NewMemoryStore(), crWriter)
	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, pathSquads, map[string]any{"name": "univ-squad"}, http.StatusCreated, &squad)
	var agent domain.Agent
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathAgents, map[string]any{"name": "univ-agent"}, http.StatusCreated, &agent)
	var identity domain.AgentIdentity
	doJSON(t, handler, http.MethodPost, pathAgentsPrefix+agent.ID+pathIdentity, nil, http.StatusCreated, &identity)
	token := crWriter.credentialTokens[identity.CredentialRef]

	var payload map[string]any
	doAgentJSON(t, handler, agent.ID, token, http.MethodGet, pathMyTools, nil, http.StatusOK, &payload)
	tools, _ := payload["tools"].([]any)
	for _, item := range tools {
		name, _ := item.(map[string]any)["name"].(string)
		require.NotEqual(t, "rest_call", name, "rest_call must never enter the builtin tools universe")
	}
}
