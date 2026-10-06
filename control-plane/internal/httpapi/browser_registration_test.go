// TG-6 slice D: control-plane browser-resource registration tests.
//
// Registration-path choice: a browser resource is an `mcp`-typed
// registration whose endpoint_config declares `driver: "browser"` —
// NOT a new stored resource kind. The gateway dispatches on
// resource_type=="mcp" + config.driver=="browser"
// (tool-gateway/internal/httpapi/server.go grantRequestsBrowserDriver),
// so a new DB kind would add migration + grant-type surface for zero
// gain. There is deliberately no `POST /registry/browser` alias (it
// would return rows typed "mcp" and confuse the type model).
//
// Covers: happy path (enumerate via the TG-5 MCP path with the
// placeholder bearer + private egress), bad driver rejected, ceiling
// defaults materialized, ceiling validation, grant ⊆ ceiling narrowing,
// and the round-trip field-shape of the grant payload the gateway folds
// (GET /internal/v1/policy).
package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/stretchr/testify/require"
)

const browserSvcURL = "http://skquad-browser-service.skquad-browser.svc.cluster.local:8090/mcp"

// browserToolsForStub mirrors the six pinned browser-service tools
// (docs/tg6-browser-protocol.md §3).
func browserToolsForStub() []MCPToolInfo {
	names := []string{"browser.navigate", "browser.click", "browser.type", "browser.screenshot", "browser.extract", "browser.close_session"}
	out := make([]MCPToolInfo, 0, len(names))
	for _, n := range names {
		out = append(out, MCPToolInfo{Name: n, Description: "browser tool " + n, InputSchema: json.RawMessage(`{"type":"object"}`)})
	}
	return out
}

func browserRegHandler() (http.Handler, *stubMCPEnumerate) {
	stub := &stubMCPEnumerate{tools: browserToolsForStub(), hash: fakeMCPHash}
	return mcpRegHandler(stub), stub
}

func browserBody(name string, ceiling map[string]any) map[string]any {
	return map[string]any{
		"name":            name,
		"endpoint_config": map[string]any{"url": browserSvcURL, "driver": "browser"},
		"policy_ceiling":  ceiling,
		"egress_class":    "internal",
	}
}

// (1) Happy path: browser resource registers as mcp-typed, enumerates
// through the TG-5 MCP path with allow_private=true and the placeholder
// bearer (no BYO token needed), and persists the tool snapshot.
func TestBrowserRegistrationHappyPath(t *testing.T) {
	handler, stub := browserRegHandler()

	res := createTypedResource(t, handler, "mcp", browserBody("lab-browser", map[string]any{
		"deny_hosts": []string{"*.internal", "metadata.google.internal"},
		"max_pages":  20,
	}))
	require.Equal(t, "mcp", string(res.Type), "browser resources are mcp-typed registrations")
	require.Equal(t, 1, stub.calls, "registration must enumerate exactly once")
	require.Equal(t, browserSvcURL, stub.lastURL)
	require.Equal(t, browserEnumeratePlaceholder, stub.lastToken, "no BYO token → placeholder bearer")
	require.True(t, stub.lastPriv, "in-cluster browser upstream requires private-egress enumeration")
	require.Equal(t, fakeMCPHash, res.ToolsHash)
	require.NotNil(t, res.ToolsEnumeratedAt)

	var tools []MCPToolInfo
	require.NoError(t, json.Unmarshal(res.ToolsSnapshot, &tools))
	require.Len(t, tools, 6)
	require.Equal(t, "browser.navigate", tools[0].Name)
}

// (1b) A BYO bearer is still accepted and custodied (optional, not forbidden).
func TestBrowserRegistrationWithBYOToken(t *testing.T) {
	handler, stub := browserRegHandler()
	body := browserBody("browser-with-token", map[string]any{"max_pages": 5})
	body["auth"] = map[string]any{"token": "BROWSER-BYO-TOKEN"}

	res := createTypedResource(t, handler, "mcp", body)
	require.Equal(t, "mcp", string(res.Type))
	require.Equal(t, "BROWSER-BYO-TOKEN", stub.lastToken)

	var got map[string]any
	doJSON(t, handler, http.MethodGet, registryBase+"mcp/"+res.ID, nil, http.StatusOK, &got)
	require.NotContains(t, mustJSONString(t, got), "BROWSER-BYO-TOKEN", "responses never echo the secret")
}

// (2) Bad driver rejected; the /registry/browser path alias does NOT
// exist (documented choice — see file header).
func TestBrowserRegistrationBadDriverAndAlias(t *testing.T) {
	handler, stub := browserRegHandler()

	rec := doRawJSON(t, handler, http.MethodPost, registryBase+"mcp", mustJSONString(t, map[string]any{
		"name":            "bad-driver",
		"endpoint_config": map[string]any{"url": "https://x.example/mcp", "auth_kind": "bearer", "driver": "firefox"},
		"policy_ceiling":  map[string]any{},
		"auth":            map[string]any{"token": "t"},
	}))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "endpoint_config.driver")
	require.Contains(t, rec.Body.String(), "invalid_value")
	rec = doRawJSON(t, handler, http.MethodPost, registryBase+"browser", mustJSONString(t, browserBody("alias-nope", map[string]any{})))
	require.Equal(t, http.StatusNotFound, rec.Code, "no browser URL-kind alias; register under /mcp with config.driver")
	require.Equal(t, 0, stub.calls, "rejected registrations must never reach the enumerate path")
}

// (3) Ceiling defaults are MATERIALIZED into the stored ceiling.
func TestBrowserCeilingDefaultsApplied(t *testing.T) {
	handler, _ := browserRegHandler()

	res := createTypedResource(t, handler, "mcp", browserBody("defaults-browser", map[string]any{"max_pages": 7}))

	var ce map[string]any
	require.NoError(t, json.Unmarshal(res.PolicyCeiling, &ce))
	require.Equal(t, float64(7), ce["max_pages"], "explicit value kept")
	require.Equal(t, float64(2097152), ce["max_screenshot_bytes"])
	require.Equal(t, float64(600), ce["idle_timeout_s"])
	require.Equal(t, float64(30), ce["max_session_minutes"])
	require.Equal(t, float64(1), ce["max_sessions_per_agent"])
	require.Equal(t, []any{}, ce["deny_hosts"])
	require.Len(t, ce, 6, "normalized ceiling carries exactly the six browser fields")
}

// (4) Ceiling validation: bounds, types, unknown keys, wrong egress class.
func TestBrowserCeilingValidation(t *testing.T) {
	handler, _ := browserRegHandler()

	cases := []struct {
		name    string
		ceiling map[string]any
		egress  string
		field   string
	}{
		{"max_pages_over_bound", map[string]any{"max_pages": 501}, "internal", "max_pages"},
		{"session_minutes_over_bound", map[string]any{"max_session_minutes": 241}, "internal", "max_session_minutes"},
		{"sessions_zero", map[string]any{"max_sessions_per_agent": 0}, "internal", "max_sessions_per_agent"},
		{"unknown_key", map[string]any{"max_tabs": 4}, "internal", "max_tabs"},
		{"mcp_ceiling_key", map[string]any{"tools_allow": []string{"browser.navigate"}}, "internal", "tools_allow"},
		{"bad_type", map[string]any{"max_pages": "many"}, "internal", "max_pages"},
		{"public_class_rejected", map[string]any{}, "public", "egress_class"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := browserBody("bad-"+tc.name, tc.ceiling)
			body["egress_class"] = tc.egress
			rec := doRawJSON(t, handler, http.MethodPost, registryBase+"mcp", mustJSONString(t, body))
			require.Equal(t, http.StatusBadRequest, rec.Code)
			require.Contains(t, rec.Body.String(), tc.field)
		})
	}
}

// (5) Grant ⊆ ceiling: narrowing allowed, widening rejected.
func TestBrowserGrantNarrowsNotWidens(t *testing.T) {
	handler, _ := browserRegHandler()

	res := createTypedResource(t, handler, "mcp", browserBody("narrow-browser", map[string]any{
		"deny_hosts":             []string{"*.internal"},
		"max_pages":              100,
		"max_sessions_per_agent": 4,
	}))

	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, pathSquads, map[string]any{"name": "browser-squad"}, http.StatusCreated, &squad)
	var agent domain.Agent
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathAgents, map[string]any{"name": "browser-agent"}, http.StatusCreated, &agent)

	grant := func(constraints map[string]any, want int) string {
		body, _ := json.Marshal([]map[string]any{{"resource_type": "mcp", "resource_id": res.ID, "constraints": constraints}})
		rec := doRawJSON(t, handler, http.MethodPut, pathAgentsPrefix+agent.ID+pathPermissions, string(body))
		require.Equal(t, want, rec.Code, rec.Body.String())
		return rec.Body.String()
	}

	// Narrowing: pages 100→10, sessions 4→1, deny list grows.
	grant(map[string]any{"max_pages": 10, "max_sessions_per_agent": 1, "deny_hosts": []string{"*.internal", "evil.example"}}, http.StatusOK)

	// Widening rejected: pages above ceiling.
	out := grant(map[string]any{"max_pages": 200}, http.StatusBadRequest)
	require.Contains(t, out, "exceeds_ceiling")

	// Widening rejected: sessions above ceiling.
	out = grant(map[string]any{"max_sessions_per_agent": 5}, http.StatusBadRequest)
	require.Contains(t, out, "constraints.max_sessions_per_agent")

	// Shrinking the deny list is escalation.
	out = grant(map[string]any{"deny_hosts": []string{"other.example"}}, http.StatusBadRequest)
	require.Contains(t, out, "denylist_shrunk")

	// Unknown constraint key rejected.
	out = grant(map[string]any{"max_windows": 2}, http.StatusBadRequest)
	require.Contains(t, out, "unknown_key")
}

// (6) Round-trip: the grant payload the gateway folds carries EXACTLY
// the field names browser policy.go expects (config.driver, config.url,
// flat ceiling with all six fields, constraints as granted).
func TestBrowserGrantRoundTripShape(t *testing.T) {
	handler, _ := browserRegHandler()

	res := createTypedResource(t, handler, "mcp", browserBody("rt-browser", map[string]any{
		"deny_hosts": []string{"*.internal"},
		"max_pages":  40,
	}))

	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, pathSquads, map[string]any{"name": "rt-squad"}, http.StatusCreated, &squad)
	var agent domain.Agent
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathAgents, map[string]any{"name": "rt-agent"}, http.StatusCreated, &agent)
	body, _ := json.Marshal([]map[string]any{{"resource_type": "mcp", "resource_id": res.ID, "constraints": map[string]any{"max_pages": 5}}})
	rec := doRawJSON(t, handler, http.MethodPut, pathAgentsPrefix+agent.ID+pathPermissions, string(body))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = doRawNoAuth(t, handler, http.MethodGet, "/internal/v1/policy?agent="+agent.ID, "")
	require.Equal(t, http.StatusOK, rec.Code)

	var snap struct {
		AgentID string `json:"agent_id"`
		Grants  []struct {
			ResourceID   string         `json:"resource_id"`
			ResourceType string         `json:"resource_type"`
			Config       map[string]any `json:"config"`
			Ceiling      map[string]any `json:"ceiling"`
			Constraints  map[string]any `json:"constraints"`
		} `json:"grants"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &snap))
	require.Equal(t, agent.ID, snap.AgentID)
	require.Len(t, snap.Grants, 1)
	g := snap.Grants[0]
	require.Equal(t, res.ID, g.ResourceID)
	require.Equal(t, "mcp", g.ResourceType)

	// Config: exactly the shape browserConfigShape reads.
	require.Equal(t, "browser", g.Config["driver"])
	require.Equal(t, browserSvcURL, g.Config["url"])

	// Ceiling: fully materialized flat browser shape — every field
	// policy.go folds is present with the expected (defaulted) value.
	require.Equal(t, map[string]any{
		"deny_hosts":             []any{"*.internal"},
		"max_pages":              float64(40),
		"max_screenshot_bytes":   float64(2097152),
		"idle_timeout_s":         float64(600),
		"max_session_minutes":    float64(30),
		"max_sessions_per_agent": float64(1),
	}, g.Ceiling)

	// Constraints: the grant narrowing, same flat field names.
	require.Equal(t, map[string]any{"max_pages": float64(5)}, g.Constraints)
}
