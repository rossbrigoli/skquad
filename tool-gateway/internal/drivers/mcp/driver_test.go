package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/tool-gateway/internal/auth"
	"github.com/rossbrigoli/skquad/tool-gateway/internal/credentials"
	"github.com/rossbrigoli/skquad/tool-gateway/internal/drivers"
	"github.com/rossbrigoli/skquad/tool-gateway/internal/policy"
)

// ── reference MCP server (hand-rolled JSON-RPC, no external deps) ─────────

// refServer is a minimal streamable-HTTP MCP server for tests:
// initialize / tools/list (5 tools) / tools/call (canned content;
// the tool "boom" answers with a JSON-RPC error). It records every
// hit and the last Authorization header observed.
type refServer struct {
	mu       sync.Mutex
	lastAuth string
	hits     atomic.Int64
	sse      bool // reply with a single-event text/event-stream body
}

func newRefServer(t *testing.T, sse bool) (*refServer, *httptest.Server) {
	t.Helper()
	ref := &refServer{sse: sse}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ref.hits.Add(1)
		ref.mu.Lock()
		ref.lastAuth = r.Header.Get("Authorization")
		ref.mu.Unlock()

		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, `{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"read error"}}`, 400)
			return
		}
		var rpc struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.Number     `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(body, &rpc); err != nil {
			http.Error(w, `{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"parse error"}}`, 400)
			return
		}

		var payload any
		switch rpc.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "sess-ref-1")
			payload = map[string]any{
				"protocolVersion": "2025-03-26",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]string{"name": "ref-mcp", "version": "0.1.0"},
			}
		case "tools/list":
			payload = map[string]any{"tools": []map[string]any{
				{"name": "echo_tool", "description": "echoes its args", "inputSchema": map[string]any{"type": "object"}},
				{"name": "confirm_tool", "description": "needs approval", "inputSchema": map[string]any{"type": "object"}},
				{"name": "dangerous", "description": "never allowed", "inputSchema": map[string]any{"type": "object"}},
				{"name": "boom", "description": "always errors", "inputSchema": map[string]any{"type": "object"}},
				{"name": "list_files", "description": "lists files", "inputSchema": map[string]any{"type": "object"}},
			}}
		case "tools/call":
			var p struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			_ = json.Unmarshal(rpc.Params, &p)
			if p.Name == "boom" {
				payload = map[string]any{
					"jsonrpc": "2.0", "id": rpc.ID,
					"error": map[string]any{"code": -32602, "message": "tool boom: invalid arguments"},
				}
				writeRef(w, payload, sse)
				return
			}
			payload = map[string]any{
				"jsonrpc": "2.0", "id": rpc.ID,
				"result": map[string]any{
					"content": []map[string]any{
						{"type": "text", "text": "canned result for " + p.Name + " args=" + string(p.Arguments)},
					},
				},
			}
		default:
			payload = map[string]any{
				"jsonrpc": "2.0", "id": rpc.ID,
				"error": map[string]any{"code": -32601, "message": "method not found"},
			}
		}
		// Wrap plain results in the envelope for non-error paths.
		if env, ok := payload.(map[string]any); ok && env["jsonrpc"] == nil {
			payload = map[string]any{"jsonrpc": "2.0", "id": rpc.ID, "result": env}
		}
		writeRef(w, payload, sse)
	}))
	t.Cleanup(srv.Close)
	return ref, srv
}

func writeRef(w http.ResponseWriter, payload any, sse bool) {
	b, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, "marshal", 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if sse {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: message\ndata: " + string(b) + "\n\n"))
		return
	}
	_, _ = w.Write(b)
}

// ── helpers ───────────────────────────────────────────────────────────────

func raw(s string) json.RawMessage { return json.RawMessage(s) }

func grantFor(config, ceiling, constraints string) *policy.Grant {
	g := &policy.Grant{ResourceID: "mcp-github-1", ResourceType: "mcp"}
	if config != "" {
		g.Config = raw(config)
	}
	if ceiling != "" {
		g.Ceiling = raw(ceiling)
	}
	if constraints != "" {
		g.Constraints = raw(constraints)
	}
	return g
}

func callReq(agent, payload string, g *policy.Grant) *drivers.Request {
	return &drivers.Request{
		Agent:     &auth.AgentPrincipal{AgentID: agent},
		Resource:  g.ResourceID,
		Operation: "mcp_call",
		Payload:   []byte(payload),
		Grant:     g,
	}
}

// fakeCreds is a stub credentials.Resolver serving a fake bearer token.
// It counts resolutions so ACL-before-secret assertions hold.
type fakeCreds struct {
	secret *credentials.Secret
	calls  atomic.Int64
}

func (f *fakeCreds) Resolve(_ context.Context, _, agentID string) (*credentials.Secret, error) {
	f.calls.Add(1)
	if f.secret == nil {
		return nil, credentials.ErrUnavailable
	}
	return f.secret, nil
}

func bearerCreds() *fakeCreds {
	return &fakeCreds{secret: &credentials.Secret{
		Kind:   "bearer",
		Fields: map[string]string{"token": "FAKE-MCP-TOKEN"},
	}}
}

// grantForServer builds a grant pointing at the ref server with the
// given allow/deny lists. egress_class=internal on both layers so
// httptest's loopback address passes the SSRF guard.
func grantForServer(baseURL, toolsAllow, toolsDeny string) *policy.Grant {
	cfg := fmt.Sprintf(`{"base_url":%q,"auth_kind":"bearer"}`, baseURL)
	ce := fmt.Sprintf(`{"tools_allow":%s,"tools_deny":%s,"egress_class":"internal"}`, toolsAllow, toolsDeny)
	con := `{"egress_class":"internal"}`
	return grantFor(cfg, ce, con)
}

func callResult(t *testing.T, resp *drivers.Response) *CallResult {
	t.Helper()
	out, ok := resp.Body.(*CallResult)
	require.True(t, ok, "driver body must be *mcp.CallResult")
	return out
}

// ── (1) initialize handshake parsed ──────────────────────────────────────

func TestInitializeHandshakeParsed(t *testing.T) {
	_, srv := newRefServer(t, false)
	c := newClient(context.Background(), srv.URL+"/mcp", "FAKE-MCP-TOKEN", srv.Client())
	res, err := c.initialize()
	require.NoError(t, err)
	require.Equal(t, "2025-03-26", res.ProtocolVersion)
	require.Equal(t, "ref-mcp", res.ServerInfo.Name)
	require.Equal(t, "0.1.0", res.ServerInfo.Version)
	require.Equal(t, "sess-ref-1", res.SessionID, "Mcp-Session-Id captured from handshake")
}

// ── (2) tools/list enumerated ────────────────────────────────────────────

func TestToolsListEnumerated(t *testing.T) {
	_, srv := newRefServer(t, false)
	c := newClient(context.Background(), srv.URL+"/mcp", "FAKE-MCP-TOKEN", srv.Client())
	tools, err := c.toolsList()
	require.NoError(t, err)
	require.Len(t, tools, 5)
	names := make([]string, 0, len(tools))
	for _, tl := range tools {
		require.NotEmpty(t, tl.Name)
		require.NotEmpty(t, tl.Description)
		require.NotEmpty(t, tl.InputSchema, "inputSchema must be carried")
		names = append(names, tl.Name)
	}
	require.Contains(t, names, "echo_tool")
	require.Contains(t, names, "list_files")
}

// ── (3) allowed tool → wrapped-untrusted content ─────────────────────────

func TestCallAllowedReturnsWrappedUntrusted(t *testing.T) {
	_, srv := newRefServer(t, false)
	g := grantForServer(srv.URL, `["echo_tool"]`, `[]`)
	d := New(bearerCreds())
	resp, err := d.Handle(context.Background(), callReq("agent-1", `{"tool":"echo_tool","arguments":{"msg":"hello"}}`, g))
	require.NoError(t, err)
	cr := callResult(t, resp)
	require.False(t, cr.IsError)
	require.Equal(t, "echo_tool", cr.Tool)
	require.True(t, strings.HasPrefix(cr.Content, `<skquad_untrusted source="mcp"`), "WP3 tag applied")
	require.Contains(t, cr.Content, `resource="mcp-github-1"`)
	require.Contains(t, cr.Content, `tool="echo_tool"`)
	require.Contains(t, cr.Content, "canned result for echo_tool")
	require.True(t, strings.HasSuffix(cr.Content, "</skquad_untrusted>"))
}

// ── (4) tool NOT in allowlist → denied, NO upstream call ─────────────────

func TestToolNotInAllowlistDeniedNoUpstreamCall(t *testing.T) {
	ref, srv := newRefServer(t, false)
	g := grantForServer(srv.URL, `["other_tool"]`, `[]`)
	creds := bearerCreds()
	d := New(creds)
	_, err := d.Handle(context.Background(), callReq("agent-1", `{"tool":"echo_tool","arguments":{}}`, g))
	require.Error(t, err)
	var denied *drivers.DeniedError
	require.True(t, errors.As(err, &denied))
	require.Equal(t, "tool_denied", denied.Reason)
	require.Equal(t, int64(0), ref.hits.Load(), "no upstream call may happen for a denied tool")
	require.Equal(t, int64(0), creds.calls.Load(), "no secret fetch for a denied tool")
}

// ── (5) tools_deny wins over tools_allow ─────────────────────────────────

func TestToolDenyWinsOverAllow(t *testing.T) {
	ref, srv := newRefServer(t, false)
	g := grantForServer(srv.URL, `["*"]`, `["dangerous"]`)
	d := New(bearerCreds())
	_, err := d.Handle(context.Background(), callReq("agent-1", `{"tool":"dangerous","arguments":{}}`, g))
	require.Error(t, err)
	var denied *drivers.DeniedError
	require.True(t, errors.As(err, &denied))
	require.Equal(t, "tool_denied", denied.Reason)
	require.Equal(t, int64(0), ref.hits.Load())

	// A non-denied sibling through the same "*" allow still works.
	resp, err := d.Handle(context.Background(), callReq("agent-1", `{"tool":"list_files","arguments":{}}`, g))
	require.NoError(t, err)
	require.False(t, callResult(t, resp).IsError)
}

// ── (6) args over max_args_bytes → args_too_large ────────────────────────

func TestArgsOverMaxArgsBytes(t *testing.T) {
	ref, srv := newRefServer(t, false)
	g := grantFor(`{"base_url":"`+srv.URL+`","auth_kind":"bearer"}`,
		`{"tools_allow":["echo_tool"],"max_args_bytes":16,"egress_class":"internal"}`,
		`{"egress_class":"internal"}`)
	d := New(bearerCreds())
	big := `{"tool":"echo_tool","arguments":{"blob":"` + strings.Repeat("x", 64) + `"}}`
	_, err := d.Handle(context.Background(), callReq("agent-1", big, g))
	require.Error(t, err)
	var denied *drivers.DeniedError
	require.True(t, errors.As(err, &denied))
	require.Equal(t, "args_too_large", denied.Reason)
	require.Equal(t, int64(0), ref.hits.Load(), "oversized args must never reach upstream")

	// Small args under the same policy pass.
	_, err = d.Handle(context.Background(), callReq("agent-1", `{"tool":"echo_tool","arguments":{"a":1}}`, g))
	require.NoError(t, err)
}

// ── (7) JSON-RPC error → tool error, not transport failure ────────────────

func TestJSONRPCErrorSurfacedAsToolError(t *testing.T) {
	_, srv := newRefServer(t, false)
	g := grantForServer(srv.URL, `["boom"]`, `[]`)
	d := New(bearerCreds())
	resp, err := d.Handle(context.Background(), callReq("agent-1", `{"tool":"boom","arguments":{}}`, g))
	require.NoError(t, err, "JSON-RPC error is a tool-layer answer, NOT a transport failure")
	cr := callResult(t, resp)
	require.True(t, cr.IsError)
	require.NotNil(t, cr.RPCError)
	require.Equal(t, -32602, cr.RPCError.Code)
	require.Contains(t, cr.RPCError.Message, "invalid arguments")
}

// ── (8) transport failure → error (ok=false), not a denial ───────────────

func TestTransportFailureIsNotToolError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing listening anymore

	g := grantForServer(url, `["echo_tool"]`, `[]`)
	d := New(bearerCreds())
	_, err := d.Handle(context.Background(), callReq("agent-1", `{"tool":"echo_tool","arguments":{}}`, g))
	require.Error(t, err, "dead upstream must fail")
	var denied *drivers.DeniedError
	require.False(t, errors.As(err, &denied), "transport failure must not be a policy denial")
	require.Contains(t, err.Error(), "mcp_failed")
}

// ── (9) SSE single-event response parsed ─────────────────────────────────

func TestSSESingleEventResponseParsed(t *testing.T) {
	_, srv := newRefServer(t, true) // every reply is one SSE event
	g := grantForServer(srv.URL, `["echo_tool"]`, `[]`)
	d := New(bearerCreds())
	resp, err := d.Handle(context.Background(), callReq("agent-1", `{"tool":"echo_tool","arguments":{"k":"v"}}`, g))
	require.NoError(t, err)
	cr := callResult(t, resp)
	require.False(t, cr.IsError)
	require.Contains(t, cr.Content, "canned result for echo_tool")
	require.Contains(t, cr.Content, `<skquad_untrusted source="mcp"`)
}

// ── (10) credential injected, never logged ───────────────────────────────

func TestCredentialInjectedNeverLogged(t *testing.T) {
	ref, srv := newRefServer(t, false)
	g := grantForServer(srv.URL, `["echo_tool"]`, `[]`)
	d := New(bearerCreds())

	var buf strings.Builder
	orig := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(orig)

	_, err := d.Handle(context.Background(), callReq("agent-1",
		`{"tool":"echo_tool","arguments":{"secret_note":"CANARY-ARG-VALUE"}}`, g))
	require.NoError(t, err)

	ref.mu.Lock()
	lastAuth := ref.lastAuth
	ref.mu.Unlock()
	require.Equal(t, "Bearer FAKE-MCP-TOKEN", lastAuth, "bearer credential injected upstream")

	require.NotContains(t, buf.String(), "FAKE-MCP-TOKEN", "credential must never be logged")
	require.NotContains(t, buf.String(), "CANARY-ARG-VALUE", "arg values must never be logged")
}

// ── per-tool confirmation fail-closed ────────────────────────────────────

func TestRequiresConfirmationDeniedFailClosed(t *testing.T) {
	ref, srv := newRefServer(t, false)
	g := grantFor(`{"base_url":"`+srv.URL+`","auth_kind":"bearer"}`,
		`{"tools_allow":["confirm_tool"],"per_tool":{"confirm_tool":{"requires_confirmation":true}},"egress_class":"internal"}`,
		`{"egress_class":"internal"}`)
	d := New(bearerCreds())
	_, err := d.Handle(context.Background(), callReq("agent-1", `{"tool":"confirm_tool","arguments":{}}`, g))
	require.Error(t, err)
	var denied *drivers.DeniedError
	require.True(t, errors.As(err, &denied))
	require.Equal(t, "confirmation_required", denied.Reason)
	require.Equal(t, int64(0), ref.hits.Load())
}

// ── policy folding ───────────────────────────────────────────────────────

func TestEffectivePolicyRequiresBaseURL(t *testing.T) {
	_, err := EffectivePolicy(raw(`{}`), raw(`{}`), raw(`{}`))
	require.Error(t, err)
}

func TestEffectivePolicyDefaults(t *testing.T) {
	p, err := EffectivePolicy(raw(`{"url":"https://mcp.example.com/"}`), nil, nil)
	require.NoError(t, err)
	require.Equal(t, "https://mcp.example.com", p.BaseURL, "trailing slash trimmed, url alias accepted")
	require.Equal(t, AuthBearer, p.AuthKind, "bearer is the default kind")
	require.Empty(t, p.ToolsAllow, "unset allow must default-deny")
	require.Equal(t, DefaultMaxArgsBytes, p.MaxArgsBytes)
	require.False(t, p.toolAllowed("anything"))
	require.False(t, p.AllowPrivate)
}

func TestEffectivePolicyFoldsTighter(t *testing.T) {
	ce := `{"tools_allow":["a","b","c"],"tools_deny":["b"],"max_args_bytes":1000,"rate_per_min":30}`
	con := `{"tools_allow":["a","c"],"tools_deny":["c"],"max_args_bytes":500,"rate_per_min":10,"egress_class":"internal"}`
	p, err := EffectivePolicy(raw(`{"base_url":"https://x.test/mcp"}`), raw(ce), raw(con))
	require.NoError(t, err)
	require.Equal(t, []string{"a", "c"}, p.ToolsAllow, "grant list wins")
	require.Contains(t, p.ToolsDeny, "b")
	require.Contains(t, p.ToolsDeny, "c", "deny is union: grant can add, never remove")
	require.True(t, p.toolAllowed("a"))
	require.False(t, p.toolAllowed("b"))
	require.False(t, p.toolAllowed("c"), "deny wins over allow")
	require.Equal(t, 500, p.MaxArgsBytes, "min cap wins")
	require.Equal(t, 10, p.RatePerMin, "min rate wins")
	require.False(t, p.AllowPrivate, "internal requires BOTH layers")

	p2, err := EffectivePolicy(raw(`{"base_url":"https://x.test/mcp"}`),
		raw(`{"egress_class":"internal"}`), raw(`{"egress_class":"internal"}`))
	require.NoError(t, err)
	require.True(t, p2.AllowPrivate)
}

func TestPerToolConfirmationSticky(t *testing.T) {
	p, err := EffectivePolicy(raw(`{"base_url":"https://x.test/mcp"}`),
		raw(`{"per_tool":{"t":{"requires_confirmation":true}}}`),
		raw(`{"per_tool":{"t":{"requires_confirmation":false}}}`))
	require.NoError(t, err)
	require.True(t, p.requiresConfirmation("t"), "confirmation is sticky-true across layers")
	require.False(t, p.requiresConfirmation("other"))
}

func TestWildcardMatch(t *testing.T) {
	cases := []struct {
		pattern, s string
		want       bool
	}{
		{"get_*", "get_issue", true},
		{"*_delete", "row_delete", true},
		{"*_delete", "delete_row", false},
		{"*", "anything", true},
		{"exact", "exact", true},
		{"exact", "other", false},
		{"a*b*c", "aXXbYYc", true},
		{"a*b*c", "aXXcYY", false},
	}
	for _, tc := range cases {
		require.Equal(t, tc.want, wildcardMatch(tc.pattern, tc.s), "%s vs %s", tc.pattern, tc.s)
	}
}

func TestUnsupportedAuthKindDenied(t *testing.T) {
	_, srv := newRefServer(t, false)
	g := grantFor(`{"base_url":"`+srv.URL+`","auth_kind":"basic"}`,
		`{"tools_allow":["echo_tool"],"egress_class":"internal"}`, `{"egress_class":"internal"}`)
	d := New(bearerCreds())
	_, err := d.Handle(context.Background(), callReq("agent-1", `{"tool":"echo_tool"}`, g))
	var denied *drivers.DeniedError
	require.True(t, errors.As(err, &denied))
	require.Equal(t, "unsupported_auth_kind", denied.Reason)
}

func TestCredentialKindMismatchDenied(t *testing.T) {
	ref, srv := newRefServer(t, false)
	g := grantForServer(srv.URL, `["echo_tool"]`, `[]`)
	creds := &fakeCreds{secret: &credentials.Secret{Kind: "basic", Fields: map[string]string{"username": "u", "password": "***"}}}
	d := New(creds)
	_, err := d.Handle(context.Background(), callReq("agent-1", `{"tool":"echo_tool"}`, g))
	var denied *drivers.DeniedError
	require.True(t, errors.As(err, &denied))
	require.Equal(t, "credential_kind_mismatch", denied.Reason)
	require.Equal(t, int64(0), ref.hits.Load())
}

func TestMissingAgentIdentityDenied(t *testing.T) {
	ref, srv := newRefServer(t, false)
	g := grantForServer(srv.URL, `["echo_tool"]`, `[]`)
	d := New(bearerCreds())
	req := callReq("", `{"tool":"echo_tool"}`, g)
	req.Agent = nil
	_, err := d.Handle(context.Background(), req)
	var denied *drivers.DeniedError
	require.True(t, errors.As(err, &denied))
	require.Equal(t, "agent_identity_missing", denied.Reason)
	require.Equal(t, int64(0), ref.hits.Load())
}

func TestWrapUntrustedEscapesAttrsNotBody(t *testing.T) {
	out := WrapUntrusted(`body with "quotes" & <tags> kept verbatim`, "mcp", map[string]string{
		"tool": `ev"il&<`,
	})
	require.True(t, strings.HasPrefix(out, `<skquad_untrusted source="mcp" tool="ev&quot;il&amp;&lt;">`))
	require.Contains(t, out, `body with "quotes" & <tags> kept verbatim`)
	require.True(t, strings.HasSuffix(out, "</skquad_untrusted>"))
}
