// TG-5 slice B1: HTTP-layer tests for POST /internal/mcp/enumerate —
// internal-token auth, structured failures, and the caplog guarantee
// (the upstream credential never reaches audit logs or responses).
package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/tool-gateway/internal/audit"
)

const (
	enumerateInternalToken = "internal-test-token-XYZ"
	enumerateUpstreamToken = "FAKE-MCP-TOKEN"
)

// fakeUpstreamMCP is a minimal streamable-HTTP MCP server:
// initialize + tools/list (3 tools). Records the last Authorization
// header and hit count.
type fakeUpstreamMCP struct {
	mu       sync.Mutex
	lastAuth string
	hits     atomic.Int64
}

func newFakeUpstream(t *testing.T) (*fakeUpstreamMCP, *httptest.Server) {
	t.Helper()
	fu := &fakeUpstreamMCP{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fu.hits.Add(1)
		fu.mu.Lock()
		fu.lastAuth = r.Header.Get("Authorization")
		fu.mu.Unlock()

		body, _ := io.ReadAll(r.Body)
		var rpc struct {
			ID     json.Number `json:"id"`
			Method string      `json:"method"`
		}
		if err := json.Unmarshal(body, &rpc); err != nil {
			http.Error(w, "parse", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch rpc.Method {
		case "initialize":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-03-26","serverInfo":{"name":"fake","version":"0.1"}}}`, rpc.ID)
		case "tools/list":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"tools":[`+
				`{"name":"tool_a","description":"A","inputSchema":{"type":"object","properties":{"q":{"type":"string"}}}},`+
				`{"name":"tool_b","description":"B","inputSchema":{"type":"object"}},`+
				`{"name":"tool_c","description":"C","inputSchema":{"type":"object","required":["id"]}}`+
				`]}}`, rpc.ID)
		default:
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"method not found"}}`, rpc.ID)
		}
	}))
	t.Cleanup(srv.Close)
	return fu, srv
}

func enumerateBody(t *testing.T, url string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"resource_id":   "mcp-ref",
		"url":           url,
		"auth":          map[string]string{"kind": "bearer", "token": enumerateUpstreamToken},
		"allow_private": true, // httptest upstream lives on loopback
	})
	require.NoError(t, err)
	return b
}

func newEnumerateServer(t *testing.T, internalToken string) (*Server, *bytes.Buffer) {
	t.Helper()
	var enabled atomic.Bool
	enabled.Store(true)
	buf := &bytes.Buffer{}
	s := New(Deps{
		InternalToken: internalToken,
		Enabled:       &enabled,
		Audit:         audit.NewStdoutEmitter(buf), // caplog
	})
	return s, buf
}

func doEnumerate(t *testing.T, s *Server, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/internal/mcp/enumerate", bytes.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

// ── happy path: enumerate through the HTTP endpoint ──────────────────────

func TestEnumerateEndpointHappy(t *testing.T) {
	fu, upstream := newFakeUpstream(t)
	s, _ := newEnumerateServer(t, enumerateInternalToken)

	rec := doEnumerate(t, s, enumerateBody(t, upstream.URL), map[string]string{
		InternalTokenHeader: enumerateInternalToken,
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var out struct {
		ResourceID string `json:"resource_id"`
		Tools      []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"inputSchema"`
		} `json:"tools"`
		Hash string `json:"hash"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Equal(t, "mcp-ref", out.ResourceID)
	require.Len(t, out.Tools, 3)
	require.Equal(t, "tool_a", out.Tools[0].Name)
	require.NotEmpty(t, out.Tools[0].InputSchema)
	require.Len(t, out.Hash, 64)

	// Upstream received the CP-provided bearer credential.
	fu.mu.Lock()
	defer fu.mu.Unlock()
	require.Equal(t, "Bearer "+enumerateUpstreamToken, fu.lastAuth)
}

// Authorization: Bearer fallback also works (mirrors CP callback style).
func TestEnumerateBearerFallback(t *testing.T) {
	_, upstream := newFakeUpstream(t)
	s, _ := newEnumerateServer(t, enumerateInternalToken)
	rec := doEnumerate(t, s, enumerateBody(t, upstream.URL), map[string]string{
		"Authorization": "Bearer " + enumerateInternalToken,
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// ── (4) auth: missing / wrong internal token → 401; unconfigured → 503 ──

func TestEnumerateRequiresInternalToken(t *testing.T) {
	fu, upstream := newFakeUpstream(t)
	s, _ := newEnumerateServer(t, enumerateInternalToken)
	body := enumerateBody(t, upstream.URL)

	// No token at all.
	rec := doEnumerate(t, s, body, nil)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.Contains(t, rec.Body.String(), "unauthorized")

	// Wrong token.
	rec = doEnumerate(t, s, body, map[string]string{InternalTokenHeader: "wrong-token"})
	require.Equal(t, http.StatusUnauthorized, rec.Code)

	// Empty header value.
	rec = doEnumerate(t, s, body, map[string]string{InternalTokenHeader: "   "})
	require.Equal(t, http.StatusUnauthorized, rec.Code)

	// The upstream must never have been contacted for unauthenticated calls.
	require.Zero(t, fu.hits.Load(), "unauthenticated requests must not touch the upstream")
}

func TestEnumerateUnconfiguredIsFailClosed(t *testing.T) {
	s, _ := newEnumerateServer(t, "") // no internal token configured
	rec := doEnumerate(t, s, []byte(`{}`), map[string]string{InternalTokenHeader: "anything"})
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Contains(t, rec.Body.String(), "internal_unconfigured")
}

// ── (3) unreachable upstream → structured 502 (no panic) ─────────────────

func TestEnumerateUnreachableUpstream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := srv.URL
	srv.Close()

	s, caplog := newEnumerateServer(t, enumerateInternalToken)
	rec := doEnumerate(t, s, enumerateBody(t, deadURL), map[string]string{
		InternalTokenHeader: enumerateInternalToken,
	})
	require.Equal(t, http.StatusBadGateway, rec.Code)
	var out map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Equal(t, "mcp_enumerate_failed", out["error"])
	require.NotEmpty(t, out["message"])
	// Structured error must not leak the credential.
	require.NotContains(t, rec.Body.String(), enumerateUpstreamToken)
	_ = caplog
}

// SSRF posture: loopback upstream refused unless allow_private is set.
func TestEnumerateSSRFGuard(t *testing.T) {
	_, upstream := newFakeUpstream(t)
	s, _ := newEnumerateServer(t, enumerateInternalToken)
	body, err := json.Marshal(map[string]any{
		"resource_id": "mcp-ssrf",
		"url":         upstream.URL,
		"auth":        map[string]string{"kind": "bearer", "token": enumerateUpstreamToken},
		// allow_private omitted → default false → loopback refused
	})
	require.NoError(t, err)
	rec := doEnumerate(t, s, body, map[string]string{InternalTokenHeader: enumerateInternalToken})
	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.Contains(t, rec.Body.String(), "egress_denied")
}

// ── validation errors ─────────────────────────────────────────────────────

func TestEnumerateValidation(t *testing.T) {
	s, _ := newEnumerateServer(t, enumerateInternalToken)
	h := map[string]string{InternalTokenHeader: enumerateInternalToken}

	cases := []struct {
		name string
		body string
	}{
		{"bad json", `not-json`},
		{"missing resource_id", `{"url":"https://x/mcp","auth":{"kind":"bearer","token":"t"}}`},
		{"path traversal resource_id", `{"resource_id":"../etc","url":"https://x/mcp","auth":{"kind":"bearer","token":"t"}}`},
		{"wrong auth kind", `{"resource_id":"r","url":"https://x/mcp","auth":{"kind":"api_key","token":"t"}}`},
		{"empty token", `{"resource_id":"r","url":"https://x/mcp","auth":{"kind":"bearer","token":""}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doEnumerate(t, s, []byte(tc.body), h)
			require.Equal(t, http.StatusBadRequest, rec.Code, tc.name+": "+rec.Body.String())
			require.Contains(t, rec.Body.String(), "bad_request")
		})
	}
}

// Kill switch blocks enumeration too.
func TestEnumerateKillSwitch(t *testing.T) {
	s, _ := newEnumerateServer(t, enumerateInternalToken)
	s.deps.Enabled.Store(false)
	rec := doEnumerate(t, s, []byte(`{}`), map[string]string{InternalTokenHeader: enumerateInternalToken})
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Contains(t, rec.Body.String(), "gateway_disabled")
}

// ── (6) credential never appears in logs (caplog) ────────────────────────

func TestEnumerateTokenNeverLogged(t *testing.T) {
	_, upstream := newFakeUpstream(t)
	s, caplog := newEnumerateServer(t, enumerateInternalToken)

	// Happy path + failure path, both audited.
	rec := doEnumerate(t, s, enumerateBody(t, upstream.URL), map[string]string{InternalTokenHeader: enumerateInternalToken})
	require.Equal(t, http.StatusOK, rec.Code)

	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	rec = doEnumerate(t, s, enumerateBody(t, deadURL), map[string]string{InternalTokenHeader: enumerateInternalToken})
	require.Equal(t, http.StatusBadGateway, rec.Code)

	logged := caplog.String()
	require.Contains(t, logged, "mcp_enumerate", "audit events should have been emitted")
	require.NotContains(t, logged, enumerateUpstreamToken, "upstream credential must never appear in audit logs")
	require.NotContains(t, rec.Body.String(), enumerateUpstreamToken)

	// Audit detail carries tool count + hash only (no schemas, no token).
	var ev audit.Event
	lines := strings.Split(strings.TrimSpace(logged), "\n")
	require.NotEmpty(t, lines)
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &ev))
	require.Equal(t, "mcp_enumerate", ev.Operation)
	require.Contains(t, ev.Detail, "hash=")
	require.Contains(t, ev.Detail, "tools=3")
}
