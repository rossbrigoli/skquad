// TG-5 slice B2a (S-244-series): MCP resource registration tests.
//
// Covers the CP-side registration contract: the CP delegates tool
// enumeration to the tool gateway (stubbed here via stubMCPEnumerate),
// stores the snapshot (tools + gateway hash + enumerated_at), fails
// registration when the gateway reports the upstream unreachable, and
// validates that ceiling.tools_allow only names REAL enumerated tools.
// Also covers per-agent bearer custody (skquad-mcp- prefix), redaction,
// and grant-narrows-not-widens.
//
// The gateway's canonical hash is NEVER recomputed here — the stub
// returns a fixed 64-hex value and we assert it is persisted verbatim.
package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/kube"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
	"github.com/stretchr/testify/require"
)

// stubMCPEnumerate is a test MCPEnumerateClient. It returns a fixed tool
// set (or the error the test configured) and records the last request so
// tests can assert what the CP sent to the gateway.
type stubMCPEnumerate struct {
	tools     []MCPToolInfo
	hash      string
	err       error
	calls     int
	lastURL   string
	lastToken string
	lastPriv  bool
	lastResID string
}

func (s *stubMCPEnumerate) Enumerate(_ context.Context, resourceID, upstreamURL, bearerToken string, allowPrivate bool) (*MCPEnumerateResult, error) {
	s.calls++
	s.lastResID = resourceID
	s.lastURL = upstreamURL
	s.lastToken = bearerToken
	s.lastPriv = allowPrivate
	if s.err != nil {
		return nil, s.err
	}
	tools := s.tools
	if tools == nil {
		tools = []MCPToolInfo{}
	}
	return &MCPEnumerateResult{Tools: tools, Hash: s.hash}, nil
}

// defaultMCPTools is a realistic GitHub-MCP-shaped tool set used by most
// tests. Includes the names the shared egress tests allowlist (a, b,
// list_pulls, get_issue) so a single stub satisfies them.
func defaultMCPTools() []MCPToolInfo {
	return []MCPToolInfo{
		{Name: "a", Description: "tool a", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "b", Description: "tool b", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "list_pulls", Description: "list pulls", InputSchema: json.RawMessage(`{"type":"object","properties":{"repo":{"type":"string"}}}`)},
		{Name: "get_issue", Description: "get issue", InputSchema: json.RawMessage(`{"type":"object","properties":{"number":{"type":"integer"}}}`)},
		{Name: "merge_pull", Description: "merge a pull", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}
}

const (
	fakeMCPHash = "1111111111111111111111111111111111111111111111111111111111111111"
	fakeMCPTok  = "FAKE-MCP-TOKEN"
)

// mcpRegHandler builds a handler with a fake secret store + the given
// enumerate stub, using the default (non-OIDC) test config so
// registration is exercised without the admin-auth layer.
func mcpRegHandler(stub MCPEnumerateClient) http.Handler {
	return newServer(testConfig(), storage.NewMemoryStore(), serverDeps{
		resourceSecrets: newFakeResourceSecretStore(),
		mcpEnumerate:    stub,
	})
}

func mcpBody(name string, ceiling map[string]any) map[string]any {
	return map[string]any{
		"name":            name,
		"endpoint_config": map[string]any{"url": "https://api.github.com/mcp", "auth_kind": "bearer"},
		"policy_ceiling":  ceiling,
		"auth":            map[string]any{"token": fakeMCPTok},
	}
}

// (1) registration calls the gateway enumerate and persists snapshot tools + hash + enumerated_at.
func TestMCPRegistrationPersistsSnapshot(t *testing.T) {
	stub := &stubMCPEnumerate{tools: defaultMCPTools(), hash: fakeMCPHash}
	handler := mcpRegHandler(stub)

	res := createTypedResource(t, handler, "mcp", mcpBody("gh-mcp", map[string]any{"tools_allow": []string{"list_pulls", "get_issue"}}))
	require.Equal(t, "mcp", string(res.Type))
	require.Equal(t, 1, stub.calls, "registration must call the gateway enumerate exactly once")
	require.Equal(t, "https://api.github.com/mcp", stub.lastURL)
	require.Equal(t, fakeMCPTok, stub.lastToken)

	require.NotEmpty(t, res.ToolsSnapshot)
	require.Equal(t, fakeMCPHash, res.ToolsHash)
	require.NotNil(t, res.ToolsEnumeratedAt)

	var snapTools []MCPToolInfo
	require.NoError(t, json.Unmarshal(res.ToolsSnapshot, &snapTools))
	require.Len(t, snapTools, 5)
	require.Equal(t, fakeMCPHash, res.ToolsHash, "hash stored verbatim, never recomputed")
}

// (2) registration FAILS when the gateway reports unreachable — no resource created.
func TestMCPRegistrationFailsWhenGatewayUnreachable(t *testing.T) {
	stub := &stubMCPEnumerate{err: &mcpEnumerateError{Status: 0, Code: "gateway_unreachable"}}
	handler := mcpRegHandler(stub)

	body, _ := json.Marshal(mcpBody("dead-mcp", map[string]any{"tools_allow": []string{"a"}}))
	rec := doRawJSON(t, handler, http.MethodPost, registryBase+"mcp", string(body))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "gateway_unreachable")

	var list []map[string]any
	doJSON(t, handler, http.MethodGet, registryBase+"mcp", nil, http.StatusOK, &list)
	for _, r := range list {
		require.NotEqual(t, "dead-mcp", r["name"], "unreachable upstream must not create a resource")
	}
}

// (3) tools_allow entry not present in the snapshot → 400.
func TestMCPRegistrationRejectsUnknownToolAllow(t *testing.T) {
	stub := &stubMCPEnumerate{tools: defaultMCPTools(), hash: fakeMCPHash}
	handler := mcpRegHandler(stub)

	body, _ := json.Marshal(mcpBody("bad-allow", map[string]any{"tools_allow": []string{"list_pulls", "drop_table"}}))
	rec := doRawJSON(t, handler, http.MethodPost, registryBase+"mcp", string(body))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "drop_table")
	require.Contains(t, rec.Body.String(), "unknown_tool")
}

// (4) wildcard allow accepted when it matches ≥1 snapshot tool; rejected when it matches none.
func TestMCPRegistrationWildcard(t *testing.T) {
	stub := &stubMCPEnumerate{tools: defaultMCPTools(), hash: fakeMCPHash}
	handler := mcpRegHandler(stub)

	res := createTypedResource(t, handler, "mcp", mcpBody("wild-ok", map[string]any{"tools_allow": []string{"*_pull", "get_*"}}))
	require.Equal(t, "mcp", string(res.Type))

	body, _ := json.Marshal(mcpBody("wild-bad", map[string]any{"tools_allow": []string{"*_nothing"}}))
	rec := doRawJSON(t, handler, http.MethodPost, registryBase+"mcp", string(body))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "unmatched_wildcard")
}

// (5) per-agent credential set + isolation (agent A can't read agent B's) + clear.
func TestMCPPerAgentCredentialIsolation(t *testing.T) {
	stub := &stubMCPEnumerate{tools: defaultMCPTools(), hash: fakeMCPHash}
	secrets := newFakeResourceSecretStore()
	handler := newServer(testConfig(), storage.NewMemoryStore(), serverDeps{resourceSecrets: secrets, mcpEnumerate: stub})

	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, pathSquads, map[string]any{"name": "mcp-squad"}, http.StatusCreated, &squad)
	var agentA, agentB domain.Agent
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathAgents, map[string]any{"name": "agent-a"}, http.StatusCreated, &agentA)
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathAgents, map[string]any{"name": "agent-b"}, http.StatusCreated, &agentB)

	res := createTypedResource(t, handler, "mcp", mcpBody("iso-mcp", map[string]any{"tools_allow": []string{"a"}}))
	credPath := func(agentID string) string {
		return registryBase + "mcp/" + res.ID + "/agent-credentials/" + agentID
	}

	var out map[string]any
	doJSON(t, handler, http.MethodPut, credPath(agentA.ID), map[string]any{"auth": map[string]string{"token": "AGENT-A-MCP-TOKEN"}}, http.StatusOK, &out)
	require.Equal(t, "bearer", out["auth_kind"])
	require.Equal(t, "agent", out["source"])
	require.NotContains(t, mustJSONString(t, out), "AGENT-A-MCP-TOKEN", "PUT response must never echo the secret")

	// GET probe reports has_own_credential=true for agent A.
	var probe map[string]any
	doJSON(t, handler, http.MethodGet, credPath(agentA.ID), nil, http.StatusOK, &probe)
	require.Equal(t, true, probe["has_own_credential"])
	require.NotContains(t, mustJSONString(t, probe), "AGENT-A-MCP-TOKEN")

	// Secret lands under the mcp per-agent name (skquad-mcp- prefix).
	secretA := kube.MCPAgentSecretName(res.ID, agentA.ID)
	require.Contains(t, secretA, "skquad-mcp-")
	stored, ok := secrets.get(secretA)
	require.True(t, ok)
	require.Equal(t, "AGENT-A-MCP-TOKEN", stored["token"])

	doJSON(t, handler, http.MethodPut, credPath(agentB.ID), map[string]any{"auth": map[string]string{"token": "AGENT-B-MCP-TOKEN"}}, http.StatusOK, &out)
	require.Equal(t, "agent", out["source"])

	// Isolation: agent A's secret and agent B's are distinct entries.
	storedB, okB := secrets.get(kube.MCPAgentSecretName(res.ID, agentB.ID))
	require.True(t, okB)
	require.Equal(t, "AGENT-B-MCP-TOKEN", storedB["token"])
	require.NotEqual(t, stored["token"], storedB["token"])

	// Clear agent A (empty-body DELETE → 204 No Content).
	rec := doRawJSON(t, handler, http.MethodDelete, credPath(agentA.ID), "")
	require.Equal(t, http.StatusNoContent, rec.Code)
	// After clear, the probe reports no own credential (falls back to resource default).
	var after map[string]any
	doJSON(t, handler, http.MethodGet, credPath(agentA.ID), nil, http.StatusOK, &after)
	require.Equal(t, false, after["has_own_credential"])
}

// (6) redaction: no secret material in any GET/list response.
func TestMCPRegistrationRedaction(t *testing.T) {
	stub := &stubMCPEnumerate{tools: defaultMCPTools(), hash: fakeMCPHash}
	handler := mcpRegHandler(stub)

	res := createTypedResource(t, handler, "mcp", mcpBody("redact-mcp", map[string]any{"tools_allow": []string{"a"}}))

	var got map[string]any
	doJSON(t, handler, http.MethodGet, registryBase+"mcp/"+res.ID, nil, http.StatusOK, &got)
	require.NotContains(t, mustJSONString(t, got), fakeMCPTok)

	var list []map[string]any
	doJSON(t, handler, http.MethodGet, registryBase+"mcp", nil, http.StatusOK, &list)
	require.NotContains(t, mustJSONString(t, list), fakeMCPTok)
}

// (7) grant-narrows-not-widens for tools_allow.
func TestMCPGrantNarrowsNotWidens(t *testing.T) {
	stub := &stubMCPEnumerate{tools: defaultMCPTools(), hash: fakeMCPHash}
	crWriter := &fakeCRWriter{}
	handler := newServer(testConfig(), storage.NewMemoryStore(), serverDeps{
		resourceSecrets: newFakeResourceSecretStore(),
		mcpEnumerate:    stub,
		crWriter:        crWriter,
	})

	res := createTypedResource(t, handler, "mcp", mcpBody("narrow-mcp", map[string]any{"tools_allow": []string{"a", "b"}}))

	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, pathSquads, map[string]any{"name": "narrow-squad"}, http.StatusCreated, &squad)
	var agent domain.Agent
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathAgents, map[string]any{"name": "narrow-agent"}, http.StatusCreated, &agent)

	// Narrowing to a subset is allowed.
	doJSON(t, handler, http.MethodPut, pathAgentsPrefix+agent.ID+pathPermissions,
		[]map[string]any{{"resource_type": "mcp", "resource_id": res.ID, "constraints": map[string]any{"tools_allow": []string{"a"}}}},
		http.StatusOK, &[]map[string]any{})

	// Widening beyond the ceiling (adds merge_pull, not in ceiling) is rejected.
	wide, _ := json.Marshal([]map[string]any{{"resource_type": "mcp", "resource_id": res.ID, "constraints": map[string]any{"tools_allow": []string{"a", "merge_pull"}}}})
	rec := doRawJSON(t, handler, http.MethodPut, pathAgentsPrefix+agent.ID+pathPermissions, string(wide))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, strings.ToLower(rec.Body.String()), "ceiling")
}
