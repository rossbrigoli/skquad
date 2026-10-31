package mcpserver

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/k8s-mcp/internal/config"
	"github.com/rossbrigoli/skquad/k8s-mcp/internal/tools"
)

const testToken = "s3cr3t-bearer-token"

func newTestServer(t *testing.T) http.Handler {
	t.Helper()
	cfg := &config.Config{AllowedNamespaces: []string{"skquad-system"}, ArgoNamespace: "argocd", ArgoAllowedApps: []string{"skquad"}, MaxLogLines: 100}
	// The registry tolerates a nil client for wire-level tests that never
	// reach the cluster (auth/list/unknown-tool paths) — but tools/call
	// tests here only exercise denial/unknown paths, so no client is used.
	reg := tools.NewRegistry(cfg, nil)
	return New(testToken, reg).Handler()
}

func post(t *testing.T, h http.Handler, body string, auth string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHealthzNoAuth(t *testing.T) {
	h := newTestServer(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestUnauthenticatedRejected(t *testing.T) {
	h := newTestServer(t)
	for _, auth := range []string{"", "Bearer wrong", "Basic dXNlcjpwYXNz"} {
		rec := post(t, h, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, auth)
		assert.Equal(t, http.StatusUnauthorized, rec.Code, "auth=%q", auth)
	}
}

func TestInitializeHandshake(t *testing.T) {
	h := newTestServer(t)
	rec := post(t, h, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`, "Bearer "+testToken)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp struct {
		Result struct {
			ProtocolVersion string            `json:"protocolVersion"`
			ServerInfo      map[string]string `json:"serverInfo"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, ProtocolVersion, resp.Result.ProtocolVersion)
	assert.Equal(t, "skquad-k8s-mcp", resp.Result.ServerInfo["name"])
}

func TestToolsListReturnsCatalog(t *testing.T) {
	h := newTestServer(t)
	rec := post(t, h, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`, "Bearer "+testToken)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp struct {
		Result struct {
			Tools []struct {
				Name        string          `json:"name"`
				InputSchema json.RawMessage `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Result.Tools, 8)
	for _, tl := range resp.Result.Tools {
		require.NotEmpty(t, tl.InputSchema, "tool %s missing schema", tl.Name)
	}
}

func TestNotificationAccepted(t *testing.T) {
	h := newTestServer(t)
	rec := post(t, h, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, "Bearer "+testToken)
	assert.Equal(t, http.StatusAccepted, rec.Code)
	assert.Empty(t, bytes.TrimSpace(rec.Body.Bytes()))
}

func TestUnknownMethod(t *testing.T) {
	h := newTestServer(t)
	rec := post(t, h, `{"jsonrpc":"2.0","id":3,"method":"prompts/list"}`, "Bearer "+testToken)
	var resp struct {
		Error *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Error)
	assert.Equal(t, jsonRPCMethodNotFound, resp.Error.Code)
}

func TestToolCallUnknownToolIsRPCError(t *testing.T) {
	h := newTestServer(t)
	rec := post(t, h, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"kubectl","arguments":{}}}`, "Bearer "+testToken)
	var resp struct {
		Error *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Error, "unknown tool must be an RPC error, not a result")
	assert.Equal(t, jsonRPCInvalidParams, resp.Error.Code)
}

func TestToolCallDeniedNamespaceRendersIsError(t *testing.T) {
	h := newTestServer(t)
	rec := post(t, h, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"k8s_pods_list","arguments":{"namespace":"kube-system"}}}`, "Bearer "+testToken)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp struct {
		Result struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.True(t, resp.Result.IsError)
	assert.Contains(t, resp.Result.Content[0].Text, "denied")
}

func TestGetOnMcpNotAllowed(t *testing.T) {
	h := newTestServer(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/mcp", nil))
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

func TestInvalidJSONRPCRejected(t *testing.T) {
	h := newTestServer(t)
	rec := post(t, h, `{"jsonrpc":"1.0","id":1,"method":"tools/list"}`, "Bearer "+testToken)
	var resp struct {
		Error *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Error)
	assert.Equal(t, jsonRPCInvalidRequest, resp.Error.Code)
}
