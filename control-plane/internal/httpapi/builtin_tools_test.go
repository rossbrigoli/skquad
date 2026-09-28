package httpapi

// BT-2 handler tests (ADR-0012): admin RBAC, merge-patch semantics,
// server-side policy validation 400s, audit-on-PATCH, the agent tools
// ETag round, and the web_search proxy with stubbed providers.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/config"
	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/search"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

const (
	pathAdminTools = "/api/v1/admin/tools"
	pathMyTools    = "/api/v1/agents/me/tools"
	pathWebSearch  = "/api/v1/tools/web_search"
)

// --- test doubles -------------------------------------------------------

type stubSearchProvider struct {
	results   []search.Result
	err       error
	lastQuery string
	lastMax   int
}

func (p *stubSearchProvider) Search(_ context.Context, query string, maxResults int) ([]search.Result, error) {
	p.lastQuery = query
	p.lastMax = maxResults
	return p.results, p.err
}

// agentWithCredential boots a squad+agent and returns (agentID, token)
// usable against the agent-credential endpoints.
func agentWithCredential(t *testing.T, handler http.Handler, fw *fakeCRWriter, squadName, agentName string) (string, string) {
	t.Helper()
	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, pathSquads, map[string]any{"name": squadName}, http.StatusCreated, &squad)
	var agent domain.Agent
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathAgents, map[string]any{
		"name": agentName, "role": "worker",
	}, http.StatusCreated, &agent)
	var identity domain.AgentIdentity
	doJSON(t, handler, http.MethodPost, pathAgentsPrefix+agent.ID+pathIdentity, nil, http.StatusCreated, &identity)
	tok, ok := fw.credentialTokens[identity.CredentialRef]
	require.True(t, ok, "credential token missing for %s", identity.CredentialRef)
	return agent.ID, tok
}

// toolsHandler builds a dev-auth handler with a fake CR writer (so agent
// identities can be created and their credentials read back) and the
// given search providers.
func toolsHandler(t *testing.T, providers map[string]search.Provider) (http.Handler, *fakeCRWriter) {
	t.Helper()
	fw := &fakeCRWriter{}
	handler := newServer(testConfig(), storage.NewMemoryStore(), nil, fw, providers, nil, nil)
	return handler, fw
}

func newAgentRequest(agentID, token, method, path string, body any) *http.Request {
	var payload []byte
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			panic(err)
		}
		payload = raw
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(payload))
	if body != nil {
		req.Header.Set(headerContentType, jsonContentType)
	}
	if agentID != "" {
		req.Header.Set("X-Skquad-Agent-ID", agentID)
	}
	if token != "" {
		req.Header.Set("Authorization", bearerPrefix+token)
	}
	return req
}

func serve(handler http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// --- admin surface ------------------------------------------------------

func TestAdminToolsListShowsThreeDisabledTools(t *testing.T) {
	t.Parallel()
	handler, _ := toolsHandler(t, nil)

	var resp struct {
		Tools []builtinToolAdminView `json:"tools"`
	}
	doJSON(t, handler, http.MethodGet, pathAdminTools, nil, http.StatusOK, &resp)
	require.Len(t, resp.Tools, 3)
	names := make([]string, 0, 3)
	for _, tool := range resp.Tools {
		names = append(names, tool.Name)
		require.False(t, tool.Enabled)
		require.JSONEq(t, "{}", string(tool.Policy))
		require.NotEmpty(t, tool.UpdatedAt)
	}
	require.Equal(t, []string{"exec", "web_fetch", "web_search"}, names)
}

func TestAdminToolsRBAC(t *testing.T) {
	t.Parallel()
	cfg := testConfig()
	cfg.AuthMode = config.AuthOIDC
	cfg.OIDCAdminGroups = []string{platformAdminGroup}
	handler := NewWithDependencies(cfg, storage.NewMemoryStore(), headerOIDC{
		authAdmin: {Email: adminEmail, Name: "Admin", Groups: []string{platformAdminGroup}},
		authAlice: {Email: aliceEmail, Name: "Alice"},
	}, &fakeCRWriter{}, nil)

	var denied map[string]map[string]string
	doJSONAuth(t, handler, authAlice, http.MethodGet, pathAdminTools, nil, http.StatusForbidden, &denied)
	require.Equal(t, "forbidden", denied["error"]["code"])
	doJSONAuth(t, handler, authAlice, http.MethodPatch, pathAdminTools+"/exec", map[string]any{"enabled": true}, http.StatusForbidden, &denied)

	// Admin passes.
	var ok struct {
		Tools []builtinToolAdminView `json:"tools"`
	}
	doJSONAuth(t, handler, authAdmin, http.MethodGet, pathAdminTools, nil, http.StatusOK, &ok)
	require.Len(t, ok.Tools, 3)
}

func TestAdminPatchMergeSemantics(t *testing.T) {
	t.Parallel()
	handler, _ := toolsHandler(t, nil)

	// Enable exec with a policy.
	var execView builtinToolAdminView
	doJSON(t, handler, http.MethodPatch, pathAdminTools+"/exec", map[string]any{
		"enabled": true,
		"policy":  map[string]any{"timeoutSeconds": 45, "maxOutputBytes": 8192},
	}, http.StatusOK, &execView)
	require.True(t, execView.Enabled)
	require.JSONEq(t, `{"timeoutSeconds":45,"maxOutputBytes":8192}`, string(execView.Policy))
	require.NotEmpty(t, execView.UpdatedBy)

	// Merge: disable only; policy survives.
	var merged builtinToolAdminView
	doJSON(t, handler, http.MethodPatch, pathAdminTools+"/exec", map[string]any{"enabled": false}, http.StatusOK, &merged)
	require.False(t, merged.Enabled)
	require.JSONEq(t, `{"timeoutSeconds":45,"maxOutputBytes":8192}`, string(merged.Policy))

	// Merge: policy only; enabled survives.
	var policyMerged builtinToolAdminView
	doJSON(t, handler, http.MethodPatch, pathAdminTools+"/exec", map[string]any{
		"policy": map[string]any{"timeoutSeconds": 10, "deniedPatterns": []string{"rm\\s+-rf"}},
	}, http.StatusOK, &policyMerged)
	require.False(t, policyMerged.Enabled)
	require.JSONEq(t, `{"timeoutSeconds":10,"deniedPatterns":["rm\\s+-rf"]}`, string(policyMerged.Policy))
}

func TestAdminPatchPolicyValidation(t *testing.T) {
	t.Parallel()
	handler, _ := toolsHandler(t, nil)

	cases := []struct {
		tool string
		body map[string]any
		msg  string
	}{
		{"exec", map[string]any{"policy": map[string]any{"unknownKey": 1}}, "unknown policy key"},
		{"exec", map[string]any{"policy": map[string]any{"timeoutSeconds": 0}}, "positive integer"},
		{"exec", map[string]any{"policy": map[string]any{"timeoutSeconds": "soon"}}, "integer"},
		{"exec", map[string]any{"policy": map[string]any{"deniedPatterns": "rm.*"}}, "array of strings"},
		{"exec", map[string]any{"policy": map[string]any{"deniedPatterns": []string{"([unclosed"}}}, "invalid regex"},
		{"web_fetch", map[string]any{"policy": map[string]any{"allowPrivateNetwork": "yes"}}, "boolean"},
		{"web_fetch", map[string]any{"policy": map[string]any{"deniedPatterns": []string{}}}, "unknown policy key"},
		{"web_search", map[string]any{"policy": map[string]any{"provider": "google"}}, "must be one of"},
		{"web_search", map[string]any{"policy": []string{"not", "an", "object"}}, "JSON object"},
	}
	for _, tc := range cases {
		var body map[string]map[string]string
		doJSON(t, handler, http.MethodPatch, pathAdminTools+"/"+tc.tool, tc.body, http.StatusBadRequest, &body)
		require.Equal(t, "invalid_policy", body["error"]["code"], "tool %s body %v", tc.tool, tc.body)
		require.Contains(t, body["error"]["message"], tc.msg)
	}

	// Nothing was persisted by the rejected writes.
	var tools struct{ Tools []builtinToolAdminView }
	doJSON(t, handler, http.MethodGet, pathAdminTools, nil, http.StatusOK, &tools)
	for _, tool := range tools.Tools {
		require.False(t, tool.Enabled)
		require.JSONEq(t, "{}", string(tool.Policy))
	}
}

func TestAdminPatchUnknownToolName(t *testing.T) {
	t.Parallel()
	handler, _ := toolsHandler(t, nil)
	var body map[string]map[string]string
	doJSON(t, handler, http.MethodPatch, pathAdminTools+"/teleport", map[string]any{"enabled": true}, http.StatusNotFound, &body)
	require.Equal(t, "not_found", body["error"]["code"])
}

func TestAdminPatchEmptyBodyRejected(t *testing.T) {
	t.Parallel()
	handler, _ := toolsHandler(t, nil)
	var body map[string]map[string]string
	doJSON(t, handler, http.MethodPatch, pathAdminTools+"/exec", map[string]any{}, http.StatusBadRequest, &body)
	require.Equal(t, "bad_request", body["error"]["code"])
}

func TestAdminPatchIsAuditLoggedWithActor(t *testing.T) {
	t.Parallel()
	handler, _ := toolsHandler(t, nil)
	doJSON(t, handler, http.MethodPatch, pathAdminTools+"/web_search", map[string]any{"enabled": true}, http.StatusOK, &builtinToolAdminView{})

	var audit []domain.AuditEntry
	doJSON(t, handler, http.MethodGet, "/api/v1/audit", nil, http.StatusOK, &audit)
	require.Contains(t, auditActions(audit), "builtin_tools.update")
	var actor string
	for _, e := range audit {
		if e.Action == "builtin_tools.update" {
			actor = e.ActorID
			require.Equal(t, "builtin_tools", e.ResourceType)
			require.Equal(t, "web_search", e.ResourceID)
			require.Equal(t, "user", e.ActorType)
		}
	}
	require.NotEmpty(t, actor, "audit entry must carry the acting admin")
}

// --- agent-facing config -------------------------------------------------

func TestAgentToolsETagRound(t *testing.T) {
	t.Parallel()
	handler, fw := toolsHandler(t, nil)
	agentID, token := agentWithCredential(t, handler, fw, "Tools ETag Squad", "Tools Agent")

	// Fresh fetch: 200 + ETag, all tools disabled by default.
	rec := doAgentRequest(t, handler, agentID, token, http.MethodGet, pathMyTools, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	etag := rec.Header().Get("ETag")
	require.NotEmpty(t, etag)
	var resp struct {
		Tools []builtinToolAgentView `json:"tools"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Tools, 3)
	for _, tool := range resp.Tools {
		require.False(t, tool.Enabled)
	}

	// Same ETag → 304, empty body.
	req := newAgentRequest(agentID, token, http.MethodGet, pathMyTools, nil)
	req.Header.Set("If-None-Match", etag)
	rec2 := serve(handler, req)
	require.Equal(t, http.StatusNotModified, rec2.Code)
	require.Empty(t, rec2.Body.String())

	// Admin enables web_search → ETag changes, fresh body served.
	doJSON(t, handler, http.MethodPatch, pathAdminTools+"/web_search", map[string]any{"enabled": true}, http.StatusOK, &builtinToolAdminView{})
	req3 := newAgentRequest(agentID, token, http.MethodGet, pathMyTools, nil)
	req3.Header.Set("If-None-Match", etag)
	rec3 := serve(handler, req3)
	require.Equal(t, http.StatusOK, rec3.Code)
	require.NotEqual(t, etag, rec3.Header().Get("ETag"))
	var resp3 struct {
		Tools []builtinToolAgentView `json:"tools"`
	}
	require.NoError(t, json.Unmarshal(rec3.Body.Bytes(), &resp3))
	for _, tool := range resp3.Tools {
		require.Equal(t, tool.Name == domain.BuiltinToolWebSearch, tool.Enabled)
	}
}

func TestAgentToolsNeverContainSecrets(t *testing.T) {
	t.Parallel()
	handler, fw := toolsHandler(t, nil)
	agentID, token := agentWithCredential(t, handler, fw, "NoSecret Squad", "NoSecret Agent")
	doJSON(t, handler, http.MethodPatch, pathAdminTools+"/web_search", map[string]any{
		"enabled": true,
		"policy":  map[string]any{"provider": "brave", "maxResults": 5},
	}, http.StatusOK, &builtinToolAdminView{})

	rec := doAgentRequest(t, handler, agentID, token, http.MethodGet, pathMyTools, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	require.NotContains(t, body, "api_key")
	require.NotContains(t, body, "API_KEY")
	require.NotContains(t, body, "token")
	require.NotContains(t, body, "secret")
}

// --- search proxy ---------------------------------------------------------

func TestWebSearchDisabledReturns403(t *testing.T) {
	t.Parallel()
	handler, fw := toolsHandler(t, map[string]search.Provider{
		"duckduckgo": &stubSearchProvider{results: []search.Result{{Title: "x", URL: "https://x", Snippet: "s"}}},
	})
	agentID, token := agentWithCredential(t, handler, fw, "Search Off Squad", "Search Agent")

	var body map[string]map[string]string
	doAgentJSON(t, handler, agentID, token, http.MethodPost, pathWebSearch, map[string]any{"query": "anything"}, http.StatusForbidden, &body)
	require.Equal(t, "tool_disabled", body["error"]["code"])
}

func TestWebSearchProxyHappyPath(t *testing.T) {
	t.Parallel()
	stub := &stubSearchProvider{results: []search.Result{
		{Title: "Go", URL: "https://go.dev", Snippet: "An open source language"},
		{Title: "Rust", URL: "https://rust-lang.org", Snippet: "Fast and safe"},
	}}
	handler, fw := toolsHandler(t, map[string]search.Provider{"duckduckgo": stub})
	agentID, token := agentWithCredential(t, handler, fw, "Search On Squad", "Search Agent")
	doJSON(t, handler, http.MethodPatch, pathAdminTools+"/web_search", map[string]any{"enabled": true}, http.StatusOK, &builtinToolAdminView{})

	var resp struct {
		Results []search.Result `json:"results"`
	}
	doAgentJSON(t, handler, agentID, token, http.MethodPost, pathWebSearch, map[string]any{"query": "languages"}, http.StatusOK, &resp)
	require.Len(t, resp.Results, 2)
	require.Equal(t, "languages", stub.lastQuery)
	require.Equal(t, defaultSearchMaxResults, stub.lastMax)

	// maxResults override flows through.
	doAgentJSON(t, handler, agentID, token, http.MethodPost, pathWebSearch, map[string]any{"query": "languages", "maxResults": 1}, http.StatusOK, &resp)
	require.Equal(t, 1, stub.lastMax)
}

func TestWebSearchPolicyMaxResultsUsedWhenNoOverride(t *testing.T) {
	t.Parallel()
	stub := &stubSearchProvider{}
	handler, fw := toolsHandler(t, map[string]search.Provider{"duckduckgo": stub})
	agentID, token := agentWithCredential(t, handler, fw, "Search Policy Squad", "Search Policy Agent")
	doJSON(t, handler, http.MethodPatch, pathAdminTools+"/web_search", map[string]any{
		"enabled": true,
		"policy":  map[string]any{"maxResults": 3},
	}, http.StatusOK, &builtinToolAdminView{})

	doAgentJSON(t, handler, agentID, token, http.MethodPost, pathWebSearch, map[string]any{"query": "q"}, http.StatusOK, &struct {
		Results []search.Result `json:"results"`
	}{})
	require.Equal(t, 3, stub.lastMax)
}

func TestWebSearchSelectedProviderMissingKeyReturns502(t *testing.T) {
	t.Parallel()
	handler, fw := toolsHandler(t, map[string]search.Provider{
		"duckduckgo": &stubSearchProvider{},
	})
	agentID, token := agentWithCredential(t, handler, fw, "Brave Missing Squad", "Brave Agent")
	doJSON(t, handler, http.MethodPatch, pathAdminTools+"/web_search", map[string]any{
		"enabled": true,
		"policy":  map[string]any{"provider": "brave"},
	}, http.StatusOK, &builtinToolAdminView{})

	var body map[string]map[string]string
	doAgentJSON(t, handler, agentID, token, http.MethodPost, pathWebSearch, map[string]any{"query": "q"}, http.StatusBadGateway, &body)
	require.Equal(t, "search_provider_unavailable", body["error"]["code"])
	require.Contains(t, body["error"]["message"], "brave")
}

func TestWebSearchProviderErrorReturns502(t *testing.T) {
	t.Parallel()
	stub := &stubSearchProvider{err: errors.New("provider exploded")}
	handler, fw := toolsHandler(t, map[string]search.Provider{"duckduckgo": stub})
	agentID, token := agentWithCredential(t, handler, fw, "Search Err Squad", "Search Err Agent")
	doJSON(t, handler, http.MethodPatch, pathAdminTools+"/web_search", map[string]any{"enabled": true}, http.StatusOK, &builtinToolAdminView{})

	var body map[string]map[string]string
	doAgentJSON(t, handler, agentID, token, http.MethodPost, pathWebSearch, map[string]any{"query": "q"}, http.StatusBadGateway, &body)
	require.Equal(t, "search_provider_error", body["error"]["code"])
}

func TestWebSearchRequestValidation(t *testing.T) {
	t.Parallel()
	stub := &stubSearchProvider{}
	handler, fw := toolsHandler(t, map[string]search.Provider{"duckduckgo": stub})
	agentID, token := agentWithCredential(t, handler, fw, "Search Valid Squad", "Valid Agent")
	doJSON(t, handler, http.MethodPatch, pathAdminTools+"/web_search", map[string]any{"enabled": true}, http.StatusOK, &builtinToolAdminView{})

	var body map[string]map[string]string
	doAgentJSON(t, handler, agentID, token, http.MethodPost, pathWebSearch, map[string]any{"query": "   "}, http.StatusBadRequest, &body)
	require.Equal(t, "bad_request", body["error"]["code"])
	doAgentJSON(t, handler, agentID, token, http.MethodPost, pathWebSearch, map[string]any{"query": "q", "maxResults": 999}, http.StatusBadRequest, &body)
	require.Contains(t, body["error"]["message"], "maxResults")
}

func TestWebSearchRequiresAgentCredential(t *testing.T) {
	t.Parallel()
	handler, _ := toolsHandler(t, map[string]search.Provider{"duckduckgo": &stubSearchProvider{}})
	// No agent headers at all → 401 from authenticateAgent.
	rec := serve(handler, newAgentRequest("", "", http.MethodPost, pathWebSearch, map[string]any{"query": "q"}))
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}
