// TG-5 slice B1 tests: enumeration + canonical hash semantics.
// Reuses the reference MCP server from driver_test.go.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const fakeToken = "FAKE-MCP-TOKEN"

// ── (1) enumerate returns the full real tool set ─────────────────────────

func TestEnumerateReturnsAllTools(t *testing.T) {
	_, srv := newRefServer(t, false)

	res, err := Enumerate(context.Background(), srv.URL, fakeToken, true, nil)
	require.NoError(t, err)
	require.Len(t, res.Tools, 5)

	names := make([]string, 0, len(res.Tools))
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
		require.NotEmpty(t, tl.Description, "tool %s missing description", tl.Name)
		require.NotEmpty(t, tl.InputSchema, "tool %s missing inputSchema", tl.Name)
		var obj map[string]any
		require.NoError(t, json.Unmarshal(tl.InputSchema, &obj), "inputSchema must be valid JSON")
	}
	require.ElementsMatch(t, []string{"echo_tool", "confirm_tool", "dangerous", "boom", "list_files"}, names)
	require.Len(t, res.Hash, 64, "hash must be 64 hex chars")
	require.Equal(t, "ref-mcp", res.ServerName)
	require.Equal(t, ProtocolVersion, res.ProtocolVersion)

	// The upstream saw the bearer credential we passed (and only it).
	ref2, srv2 := newRefServer(t, false)
	_, err = Enumerate(context.Background(), srv2.URL, fakeToken, true, nil)
	require.NoError(t, err)
	ref2.mu.Lock()
	defer ref2.mu.Unlock()
	require.Equal(t, "Bearer "+fakeToken, ref2.lastAuth)
}

// SSE-transport upstreams enumerate identically (client handles both).
func TestEnumerateOverSSE(t *testing.T) {
	_, srv := newRefServer(t, true)
	res, err := Enumerate(context.Background(), srv.URL, fakeToken, true, nil)
	require.NoError(t, err)
	require.Len(t, res.Tools, 5)
}

// ── (2) hash: stable for same set, flips on add/remove/modify ────────────

func tool(name, desc, schema string) ToolInfo {
	return ToolInfo{Name: name, Description: desc, InputSchema: json.RawMessage(schema)}
}

func TestCanonicalHashStableSameSet(t *testing.T) {
	base := []ToolInfo{
		tool("alpha", "first", `{"type":"object","properties":{"x":{"type":"string"}}}`),
		tool("beta", "second", `{"type":"object","properties":{"y":{"type":"integer"}}}`),
		tool("gamma", "third", `{"type":"object"}`),
	}
	h1, err := CanonicalToolSetHash(base)
	require.NoError(t, err)

	// Same set, different order → same hash (sorted by name).
	shuffled := []ToolInfo{base[2], base[0], base[1]}
	h2, err := CanonicalToolSetHash(shuffled)
	require.NoError(t, err)
	require.Equal(t, h1, h2, "hash must be order-independent")

	// Repeated computation is deterministic.
	h3, err := CanonicalToolSetHash(base)
	require.NoError(t, err)
	require.Equal(t, h1, h3)
}

func TestCanonicalHashChangesOnEveryMutation(t *testing.T) {
	base := []ToolInfo{
		tool("alpha", "first", `{"type":"object","properties":{"x":{"type":"string"}}}`),
		tool("beta", "second", `{"type":"object","properties":{"y":{"type":"integer"}}}`),
	}
	h0, err := CanonicalToolSetHash(base)
	require.NoError(t, err)

	added := append(append([]ToolInfo{}, base...), tool("delta", "new", `{"type":"object"}`))
	hAdded, err := CanonicalToolSetHash(added)
	require.NoError(t, err)
	require.NotEqual(t, h0, hAdded, "adding a tool must flip the hash")

	removed := base[:1]
	hRemoved, err := CanonicalToolSetHash(removed)
	require.NoError(t, err)
	require.NotEqual(t, h0, hRemoved, "removing a tool must flip the hash")

	renamed := []ToolInfo{tool("alpha2", "first", `{"type":"object","properties":{"x":{"type":"string"}}}`), base[1]}
	hRenamed, err := CanonicalToolSetHash(renamed)
	require.NoError(t, err)
	require.NotEqual(t, h0, hRenamed, "renaming must flip the hash")

	descChanged := []ToolInfo{tool("alpha", "CHANGED", string(base[0].InputSchema)), base[1]}
	hDesc, err := CanonicalToolSetHash(descChanged)
	require.NoError(t, err)
	require.NotEqual(t, h0, hDesc, "description change must flip the hash")

	// Nested schema change (deep property) must flip the hash.
	schemaChanged := []ToolInfo{
		tool("alpha", "first", `{"type":"object","properties":{"x":{"type":"number"}}}`),
		base[1],
	}
	hSchema, err := CanonicalToolSetHash(schemaChanged)
	require.NoError(t, err)
	require.NotEqual(t, h0, hSchema, "nested schema change must flip the hash")
}

func TestCanonicalHashSchemaKeyOrderInsensitive(t *testing.T) {
	// Same schema, keys written in different order → same canonical form.
	a := []ToolInfo{tool("t", "d", `{"type":"object","required":["x"],"properties":{"x":{"type":"string"}}}`)}
	b := []ToolInfo{tool("t", "d", `{"properties":{"x":{"type":"string"}},"required":["x"],"type":"object"}`)}
	ha, err := CanonicalToolSetHash(a)
	require.NoError(t, err)
	hb, err := CanonicalToolSetHash(b)
	require.NoError(t, err)
	require.Equal(t, ha, hb, "schema key order must not affect the hash")

	// But array ORDER inside the schema is significant.
	c := []ToolInfo{tool("t", "d", `{"type":"object","required":["x","y"]}`)}
	d := []ToolInfo{tool("t", "d", `{"type":"object","required":["y","x"]}`)}
	hc, _ := CanonicalToolSetHash(c)
	hd, _ := CanonicalToolSetHash(d)
	require.NotEqual(t, hc, hd, "schema array order must be significant")
}

func TestCanonicalHashMissingFields(t *testing.T) {
	// Missing description + missing schema canonicalize to "" and {}.
	withBlank := []ToolInfo{{Name: "solo"}}
	explicit := []ToolInfo{tool("solo", "", `{}`)}
	h1, err := CanonicalToolSetHash(withBlank)
	require.NoError(t, err)
	h2, err := CanonicalToolSetHash(explicit)
	require.NoError(t, err)
	require.Equal(t, h1, h2, "absent description/schema must equal explicit empty ones")

	// Empty tool set still hashes deterministically (empty-string hash).
	empty1, err := CanonicalToolSetHash(nil)
	require.NoError(t, err)
	empty2, err := CanonicalToolSetHash([]ToolInfo{})
	require.NoError(t, err)
	require.Equal(t, empty1, empty2)
}

// ── (3) unreachable upstream → structured error, no panic ────────────────

func TestEnumerateUnreachable(t *testing.T) {
	// Port with nothing listening (closed httptest server).
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	_, err := Enumerate(context.Background(), url, fakeToken, true, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "mcp_failed")
}

func TestEnumerateBadHandshake(t *testing.T) {
	// Upstream answers 200 with garbage (not JSON-RPC) → protocol error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, "this is not json-rpc")
	}))
	defer srv.Close()
	_, err := Enumerate(context.Background(), srv.URL, fakeToken, true, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "mcp_failed")
}

func TestEnumerateRPCErrorOnInitialize(t *testing.T) {
	// Upstream rejects the handshake with a JSON-RPC error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"unauthorized"}}`)
	}))
	defer srv.Close()
	_, err := Enumerate(context.Background(), srv.URL, fakeToken, true, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "initialize rejected")
}

// ── (5) cross-host redirect refused ──────────────────────────────────────

func TestEnumerateCrossHostRedirectRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://redirect-target.invalid:9999/mcp", http.StatusFound)
	}))
	defer srv.Close()

	_, err := Enumerate(context.Background(), srv.URL, fakeToken, true, nil)
	require.Error(t, err, "cross-host redirect must be refused")
	require.Contains(t, err.Error(), "denied redirect")
	// The token must not be echoed in the error.
	require.NotContains(t, err.Error(), fakeToken)
}

// Same-host redirect is allowed (only cross-host is refused).
func TestEnumerateSameHostRedirectAllowed(t *testing.T) {
	var mux *http.ServeMux
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/mcp" {
			http.Redirect(w, r, "/mcp/", http.StatusFound)
			return
		}
		mux.ServeHTTP(w, r)
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()
	// Minimal initialize+tools/list responder at /mcp/.
	mux = http.NewServeMux()
	mux.HandleFunc("/mcp/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		if strings.Contains(string(body), "initialize") {
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-03-26","serverInfo":{"name":"hop"}}}`)
			return
		}
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"t1","description":"d","inputSchema":{"type":"object"}}]}}`)
	})

	res, err := Enumerate(context.Background(), srv.URL, fakeToken, true, nil)
	require.NoError(t, err)
	require.Len(t, res.Tools, 1)
}

// ── SSRF guard: private/loopback refused unless allow_private ─────────────

func TestEnumerateLoopbackRefusedWithoutAllowPrivate(t *testing.T) {
	_, srv := newRefServer(t, false)
	_, err := Enumerate(context.Background(), srv.URL, fakeToken, false, nil)
	require.Error(t, err, "loopback upstream must be refused by default")
	require.Contains(t, err.Error(), "ssrf_guard")
}

func TestEnumerateValidatesInput(t *testing.T) {
	_, err := Enumerate(context.Background(), "ftp://nope/mcp", fakeToken, true, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "bad_request")

	_, err = Enumerate(context.Background(), "https://x.example/mcp", "", true, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "bad_request")
}
