package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rossbrigoli/skquad/tool-gateway/internal/auth"
	"github.com/rossbrigoli/skquad/tool-gateway/internal/drivers"
	"github.com/rossbrigoli/skquad/tool-gateway/internal/policy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---- fake browser service (docs/tg6-browser-protocol.md) ----

type fakeService struct {
	mu sync.Mutex

	// config
	internalToken     string
	createStatus      int               // when != 0, POST /v1/sessions answers this status
	rpcErrByTool      map[string][2]any // tool → {code, message}
	expiresAt         time.Time
	invalidateOnClose bool

	// captured state
	createBodies []map[string]any
	createAuth   []string
	mcpTools     []string
	mcpArgs      []map[string]any
	mcpAuth      []string
	closedIDs    []string
	sessionSeq   int
	validTokens  map[string]bool
}

func newFakeService(internalToken string) *fakeService {
	return &fakeService{
		internalToken: internalToken,
		rpcErrByTool:  map[string][2]any{},
		validTokens:   map[string]bool{},
	}
}

func (f *fakeService) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/sessions", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			f.handleCreate(w, r)
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
	})
	mux.HandleFunc("/v1/sessions/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/v1/sessions/")
		f.mu.Lock()
		f.closedIDs = append(f.closedIDs, id)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/mcp", f.handleMCP)
	return mux
}

func (f *fakeService) handleCreate(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createAuth = append(f.createAuth, r.Header.Get("Authorization"))
	if f.createStatus != 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.createStatus)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "browser_busy"})
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+f.internalToken {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	f.createBodies = append(f.createBodies, body)
	f.sessionSeq++
	id := fmt.Sprintf("bs_%032d", f.sessionSeq)
	token := fmt.Sprintf("tok%d", f.sessionSeq)
	f.validTokens[token] = true
	exp := f.expiresAt
	if exp.IsZero() {
		exp = time.Now().Add(time.Hour)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"session_id": id,
		"token":      token,
		"expires_at": exp.Format(time.RFC3339),
	})
}

func (f *fakeService) handleMCP(w http.ResponseWriter, r *http.Request) {
	var rpc struct {
		Method string `json:"method"`
		Params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		} `json:"params"`
		ID json.Number `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&rpc); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	writeResult := func(result any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": rpc.ID, "result": result,
		})
	}
	writeErr := func(code int, msg string) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": rpc.ID,
			"error": map[string]any{"code": code, "message": msg},
		})
	}

	switch rpc.Method {
	case "initialize":
		writeResult(map[string]any{
			"protocolVersion": "2025-03-26",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "skquad-browser-service", "version": "test"},
		})
	case "tools/call":
		f.mu.Lock()
		token, _ := rpc.Params.Arguments["session"].(string)
		valid := token != "" && f.validTokens[token]
		if !valid {
			f.mu.Unlock()
			writeErr(ErrCodeSessionInvalid, "missing or invalid session token")
			return
		}
		if spec, bad := f.rpcErrByTool[rpc.Params.Name]; bad {
			f.mu.Unlock()
			writeErr(spec[0].(int), spec[1].(string))
			return
		}
		f.mcpTools = append(f.mcpTools, rpc.Params.Name)
		f.mcpArgs = append(f.mcpArgs, rpc.Params.Arguments)
		f.mcpAuth = append(f.mcpAuth, r.Header.Get("Authorization"))
		if f.invalidateOnClose && rpc.Params.Name == "browser.close_session" {
			f.validTokens[token] = false
		}
		f.mu.Unlock()
		writeResult(map[string]any{
			"content": []map[string]any{{"type": "text", "text": `{"ok":true}`}},
			"isError": false,
		})
	default:
		writeErr(-32601, "method not found")
	}
}

func (f *fakeService) snapshotCreates() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]map[string]any, len(f.createBodies))
	copy(out, f.createBodies)
	return out
}

// ---- test helpers ----

func browserGrant(svcURL, ceiling, constraints string) *policy.Grant {
	cfg := `{"base_url":"` + svcURL + `","driver":"browser"}`
	return &policy.Grant{
		ResourceID:   "browser-svc",
		ResourceType: "mcp",
		Config:       json.RawMessage(cfg),
		Ceiling:      json.RawMessage(ceiling),
		Constraints:  json.RawMessage(constraints),
	}
}

func callReq(agentID, payload string, grant *policy.Grant) *drivers.Request {
	return &drivers.Request{
		Agent:     &auth.AgentPrincipal{AgentID: agentID},
		Resource:  "browser-svc",
		Operation: "mcp_call",
		Payload:   []byte(payload),
		Grant:     grant,
	}
}

func newTestDriver(f *fakeService, t *testing.T) (*Driver, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	d := New(f.internalToken)
	return d, srv
}

func deniedReason(t *testing.T, err error) string {
	t.Helper()
	var de *drivers.DeniedError
	require.True(t, errors.As(err, &de), "expected DeniedError, got %v", err)
	return de.Reason
}

// ---- tests ----

func TestFirstCallCreatesSessionAndInjectsToken(t *testing.T) {
	f := newFakeService("internal-tok")
	d, srv := newTestDriver(f, t)
	req := callReq("agent-1", `{"tool":"browser.navigate","arguments":{"url":"https://example.com"},"task_id":"task-9"}`, browserGrant(srv.URL, `{"deny_hosts":["*.internal"]}`, ""))

	resp, err := d.Handle(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// Session created exactly once with binding + grant-derived policy.
	creates := f.snapshotCreates()
	require.Len(t, creates, 1)
	assert.Equal(t, "agent-1", creates[0]["agent_id"])
	assert.Equal(t, "task-9", creates[0]["task_id"])
	assert.Equal(t, "browser-svc", creates[0]["resource_id"])
	pol := creates[0]["policy"].(map[string]any)
	assert.Equal(t, float64(DefaultMaxScreenshotBytes), pol["max_screenshot_bytes"])
	assert.Equal(t, float64(DefaultIdleTimeoutS), pol["idle_timeout_s"])
	assert.Equal(t, float64(DefaultMaxSessionMinutes), pol["max_session_minutes"])
	assert.Equal(t, float64(DefaultMaxPages), pol["max_pages"])
	assert.Equal(t, []any{"*.internal"}, pol["deny_hosts"])
	assert.Equal(t, "Bearer internal-tok", f.createAuth[0])

	// Forwarded args carry OUR session token; result wrapped untrusted.
	require.Len(t, f.mcpArgs, 1)
	assert.Equal(t, "tok1", f.mcpArgs[0]["session"])
	assert.Equal(t, "https://example.com", f.mcpArgs[0]["url"])
	body := resp.Body.(*CallResult)
	assert.True(t, strings.HasPrefix(body.Content, `<skquad_untrusted source="browser"`))
	assert.Contains(t, body.Content, `{"ok":true}`)
}

func TestSecondCallReusesSession(t *testing.T) {
	f := newFakeService("internal-tok")
	d, srv := newTestDriver(f, t)
	g := browserGrant(srv.URL, "", "")

	_, err := d.Handle(context.Background(), callReq("agent-1", `{"tool":"browser.navigate","arguments":{"url":"https://a.test"}}`, g))
	require.NoError(t, err)
	_, err = d.Handle(context.Background(), callReq("agent-1", `{"tool":"browser.click","arguments":{"selector":"#x"}}`, g))
	require.NoError(t, err)

	assert.Len(t, f.snapshotCreates(), 1, "second call must reuse the session")
	require.Len(t, f.mcpArgs, 2)
	assert.Equal(t, f.mcpArgs[0]["session"], f.mcpArgs[1]["session"])
}

func TestExpiryClosesAndRecreates(t *testing.T) {
	f := newFakeService("internal-tok")
	base := time.Date(2026, 10, 7, 1, 0, 0, 0, time.UTC)
	f.expiresAt = base.Add(time.Minute)
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	d := New("internal-tok")
	var clk time.Time = base
	d.now = func() time.Time { return clk }
	g := browserGrant(srv.URL, "", "")

	_, err := d.Handle(context.Background(), callReq("agent-1", `{"tool":"browser.navigate","arguments":{"url":"https://a.test"}}`, g))
	require.NoError(t, err)

	clk = base.Add(2 * time.Minute) // past expires_at
	_, err = d.Handle(context.Background(), callReq("agent-1", `{"tool":"browser.navigate","arguments":{"url":"https://b.test"}}`, g))
	require.NoError(t, err)

	assert.Len(t, f.snapshotCreates(), 2, "stale session must be recreated")
	assert.Equal(t, []string{"bs_" + strings.Repeat("0", 31) + "1"}, f.closedIDs, "stale session closed")
	require.Len(t, f.mcpArgs, 2)
	assert.Equal(t, "tok2", f.mcpArgs[1]["session"])
}

func TestSessionLimitDenied(t *testing.T) {
	f := newFakeService("internal-tok")
	d, srv := newTestDriver(f, t)
	g := browserGrant(srv.URL, `{"max_sessions_per_agent":1}`, "")

	_, err := d.Handle(context.Background(), callReq("agent-1", `{"tool":"browser.navigate","arguments":{"url":"https://a.test"},"task_id":"task-A"}`, g))
	require.NoError(t, err)

	_, err = d.Handle(context.Background(), callReq("agent-1", `{"tool":"browser.navigate","arguments":{"url":"https://b.test"},"task_id":"task-B"}`, g))
	require.Error(t, err)
	assert.Equal(t, "session_limit", deniedReason(t, err))
	assert.Len(t, f.snapshotCreates(), 1, "no second session created")
}

func TestSessionLimitZeroDeniesEverything(t *testing.T) {
	f := newFakeService("internal-tok")
	d, srv := newTestDriver(f, t)
	g := browserGrant(srv.URL, `{"max_sessions_per_agent":0}`, "")

	_, err := d.Handle(context.Background(), callReq("agent-1", `{"tool":"browser.navigate","arguments":{"url":"https://a.test"}}`, g))
	require.Error(t, err)
	assert.Equal(t, "session_limit", deniedReason(t, err))
	assert.Empty(t, f.snapshotCreates())
}

func TestAgentSuppliedSessionOverwritten(t *testing.T) {
	f := newFakeService("internal-tok")
	d, srv := newTestDriver(f, t)
	g := browserGrant(srv.URL, "", "")

	_, err := d.Handle(context.Background(), callReq("agent-1",
		`{"tool":"browser.navigate","arguments":{"url":"https://a.test","session":"evil-attacker-token"}}`, g))
	require.NoError(t, err)
	require.Len(t, f.mcpArgs, 1)
	assert.Equal(t, "tok1", f.mcpArgs[0]["session"], "agent-supplied session must be overwritten")
}

func TestRPCErrorMapping(t *testing.T) {
	cases := []struct {
		code   int
		reason string
	}{
		{ErrCodeSessionInvalid, "session_invalid"},
		{ErrCodeCeilingExceeded, "ceiling_exceeded"},
		{ErrCodeBrowserBusy, "browser_busy"},
	}
	for _, tc := range cases {
		t.Run(tc.reason, func(t *testing.T) {
			f := newFakeService("internal-tok")
			f.rpcErrByTool["browser.navigate"] = [2]any{tc.code, "nope"}
			d, srv := newTestDriver(f, t)
			g := browserGrant(srv.URL, "", "")

			_, err := d.Handle(context.Background(), callReq("agent-1", `{"tool":"browser.navigate","arguments":{"url":"https://a.test"}}`, g))
			require.Error(t, err)
			assert.Equal(t, tc.reason, deniedReason(t, err))

			if tc.code == ErrCodeSessionInvalid {
				// session_invalid unbinds: the next call re-brokers.
				_, err = d.Handle(context.Background(), callReq("agent-1", `{"tool":"browser.click","arguments":{"selector":"#x"}}`, g))
				require.NoError(t, err)
				assert.Len(t, f.snapshotCreates(), 2)
			}
		})
	}
}

func TestCloseSessionUnbinds(t *testing.T) {
	f := newFakeService("internal-tok")
	f.invalidateOnClose = true
	d, srv := newTestDriver(f, t)
	g := browserGrant(srv.URL, "", "")

	_, err := d.Handle(context.Background(), callReq("agent-1", `{"tool":"browser.navigate","arguments":{"url":"https://a.test"}}`, g))
	require.NoError(t, err)
	_, err = d.Handle(context.Background(), callReq("agent-1", `{"tool":"browser.close_session"}`, g))
	require.NoError(t, err)
	require.Contains(t, f.mcpTools, "browser.close_session")

	// Unbound: the next call must create a fresh session, not reuse the closed one.
	_, err = d.Handle(context.Background(), callReq("agent-1", `{"tool":"browser.navigate","arguments":{"url":"https://c.test"}}`, g))
	require.NoError(t, err)
	assert.Len(t, f.snapshotCreates(), 2)
}

func TestConcurrentCallsSameAgentOneSession(t *testing.T) {
	f := newFakeService("internal-tok")
	d, srv := newTestDriver(f, t)
	g := browserGrant(srv.URL, "", "")

	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = d.Handle(context.Background(), callReq("agent-1",
				fmt.Sprintf(`{"tool":"browser.navigate","arguments":{"url":"https://a%d.test"}}`, i), g))
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		require.NoError(t, err, "call %d", i)
	}
	assert.Len(t, f.snapshotCreates(), 1, "concurrent calls must share one session")
	sess := map[string]bool{}
	for _, a := range f.mcpArgs {
		sess[fmt.Sprint(a["session"])] = true
	}
	assert.Len(t, sess, 1)
}

func TestServiceBusyPassthrough(t *testing.T) {
	f := newFakeService("internal-tok")
	f.createStatus = http.StatusServiceUnavailable
	d, srv := newTestDriver(f, t)
	g := browserGrant(srv.URL, "", "")

	_, err := d.Handle(context.Background(), callReq("agent-1", `{"tool":"browser.navigate","arguments":{"url":"https://a.test"}}`, g))
	require.Error(t, err)
	assert.Equal(t, "browser_busy", deniedReason(t, err))
}

func TestUnknownToolDenied(t *testing.T) {
	f := newFakeService("internal-tok")
	d, srv := newTestDriver(f, t)
	g := browserGrant(srv.URL, "", "")

	_, err := d.Handle(context.Background(), callReq("agent-1", `{"tool":"browser.eval","arguments":{}}`, g))
	require.Error(t, err)
	assert.Equal(t, "tool_denied", deniedReason(t, err))
	assert.Empty(t, f.snapshotCreates())
}

func TestUnconfiguredInternalTokenFailsClosed(t *testing.T) {
	srv := httptest.NewServer(newFakeService("internal-tok").handler())
	defer srv.Close()
	d := New("")
	g := browserGrant(srv.URL, "", "")
	_, err := d.Handle(context.Background(), callReq("agent-1", `{"tool":"browser.navigate","arguments":{"url":"https://a.test"}}`, g))
	require.Error(t, err)
	assert.Equal(t, "browser_unconfigured", deniedReason(t, err))
}

func TestTransportFailureUnbinds(t *testing.T) {
	f := newFakeService("internal-tok")
	d, srv := newTestDriver(f, t)
	g := browserGrant(srv.URL, "", "")

	_, err := d.Handle(context.Background(), callReq("agent-1", `{"tool":"browser.navigate","arguments":{"url":"https://a.test"}}`, g))
	require.NoError(t, err)
	require.Len(t, f.snapshotCreates(), 1)

	srv.Close() // kill the service mid-life
	_, err = d.Handle(context.Background(), callReq("agent-1", `{"tool":"browser.navigate","arguments":{"url":"https://a.test"}}`, g))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "browser_failed") // retryable transport error, not a denial
	assert.False(t, errors.Is(err, drivers.ErrDenied))
}

func TestPolicyFolding(t *testing.T) {
	p, err := EffectivePolicy(
		json.RawMessage(`{"base_url":"http://browser:8090"}`),
		json.RawMessage(`{"max_pages":50,"max_screenshot_bytes":2097152,"deny_hosts":["a.test"]}`),
		json.RawMessage(`{"max_pages":10,"deny_hosts":["b.test"],"max_sessions_per_agent":2}`),
	)
	require.NoError(t, err)
	assert.Equal(t, 10, p.MaxPages, "min across layers")
	assert.Equal(t, DefaultMaxScreenshotBytes, p.MaxScreenshotBytes)
	assert.Equal(t, 2, p.MaxSessionsPerAgent)
	assert.ElementsMatch(t, []string{"a.test", "b.test"}, p.DenyHosts, "deny union")

	_, err = EffectivePolicy(json.RawMessage(`{}`), nil, nil)
	require.Error(t, err, "missing base_url denies")
}
