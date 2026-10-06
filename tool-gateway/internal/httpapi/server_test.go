package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rossbrigoli/skquad/tool-gateway/internal/audit"
	"github.com/rossbrigoli/skquad/tool-gateway/internal/auth"
	"github.com/rossbrigoli/skquad/tool-gateway/internal/boundary"
	"github.com/rossbrigoli/skquad/tool-gateway/internal/drivers"
	"github.com/rossbrigoli/skquad/tool-gateway/internal/policy"
)

// cpStub is a controllable stand-in for the CP /internal/v1/policy endpoint.
type cpStub struct {
	mu sync.Mutex

	// per-agent snapshot responses (nil map => 404 for everyone)
	snapshots map[string]policy.Snapshot
	fail      bool // when true => 503
	calls     int
	lastETag  string

	srv *httptest.Server
}

func newCPStub(t *testing.T) *cpStub {
	t.Helper()
	st := &cpStub{snapshots: map[string]policy.Snapshot{}}
	st.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/v1/policy" {
			http.NotFound(w, r)
			return
		}
		st.mu.Lock()
		defer st.mu.Unlock()
		st.calls++
		st.lastETag = r.Header.Get("If-None-Match")
		if st.fail {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		agent := r.URL.Query().Get("agent")
		snap, ok := st.snapshots[agent]
		if !ok {
			http.NotFound(w, r)
			return
		}
		etag := `"` + agent + `-v1"`
		if st.lastETag == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		_ = json.NewEncoder(w).Encode(snap)
	}))
	t.Cleanup(st.srv.Close)
	return st
}

func (st *cpStub) set(agent, credHash string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.snapshots[agent] = policy.Snapshot{AgentID: agent, CredentialHash: credHash}
}

func (st *cpStub) setFail(f bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.fail = f
}

func (st *cpStub) callCount() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.calls
}

func newTestServer(t *testing.T, st *cpStub, ttl time.Duration, enabled bool) (*Server, *policy.Client, *bytes.Buffer) {
	t.Helper()
	pc := policy.NewClient(st.srv.URL, ttl, 2*time.Second)
	auditBuf := &bytes.Buffer{}
	srv := New(Deps{
		Policy:       pc,
		PolicyClient: pc,
		Boundary:     boundary.StaticVerifier{},
		Audit:        audit.NewStdoutEmitter(auditBuf),
		Enabled:      func() *atomic.Bool { b := &atomic.Bool{}; b.Store(enabled); return b }(),
		Drivers:      map[string]drivers.Driver{"echo": drivers.Echo{}},
	})
	return srv, pc, auditBuf
}

func doEcho(t *testing.T, h http.Handler, agentID, token string, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/echo", strings.NewReader(body))
	if agentID != "" {
		req.Header.Set("X-Skquad-Agent-ID", agentID)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

const testToken = "s3cr3t-agent-token"

func TestEchoHappyPath(t *testing.T) {
	st := newCPStub(t)
	st.set("agent-1", auth.HashCredential(testToken))
	srv, _, auditBuf := newTestServer(t, st, 30*time.Second, true)

	rec := doEcho(t, srv, "agent-1", testToken, `{"msg":"hello"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if out["gateway"] != "tool-gateway" {
		t.Fatalf("gateway = %v", out["gateway"])
	}
	agent := out["agent"].(map[string]any)
	if agent["id"] != "agent-1" {
		t.Fatalf("agent id = %v, want agent-1", agent["id"])
	}
	payload := out["payload"].(map[string]any)
	if payload["msg"] != "hello" {
		t.Fatalf("payload = %v", payload)
	}

	// Audit event must record an allow with identity.
	ev := lastAuditEvent(t, auditBuf)
	if ev.Decision != audit.DecisionAllow || ev.AgentID != "agent-1" || ev.Resource != "echo" {
		t.Fatalf("audit event = %+v", ev)
	}
}

func TestAuthRejection(t *testing.T) {
	st := newCPStub(t)
	st.set("agent-1", auth.HashCredential(testToken))
	srv, _, auditBuf := newTestServer(t, st, 30*time.Second, true)

	cases := []struct {
		name    string
		agent   string
		token   string
		want401 bool
	}{
		{"missing agent id", "", testToken, true},
		{"missing token", "agent-1", "", true},
		{"wrong token", "agent-1", "wrong-token", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doEcho(t, srv, tc.agent, tc.token, `{}`)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 (body: %s)", rec.Code, rec.Body.String())
			}
		})
	}
	// Every rejection must be audited as deny.
	events := auditEvents(t, auditBuf)
	if len(events) != 3 {
		t.Fatalf("audit events = %d, want 3", len(events))
	}
	for _, ev := range events {
		if ev.Decision != audit.DecisionDeny {
			t.Fatalf("audit decision = %s, want deny", ev.Decision)
		}
	}
}

func TestFailClosed_CPErrorAfterTTL(t *testing.T) {
	st := newCPStub(t)
	st.set("agent-1", auth.HashCredential(testToken))
	// Tiny TTL so the cached entry expires immediately-ish.
	srv, _, _ := newTestServer(t, st, 10*time.Millisecond, true)

	// Warm the cache.
	if rec := doEcho(t, srv, "agent-1", testToken, `{}`); rec.Code != 200 {
		t.Fatalf("warm-up status = %d, want 200", rec.Code)
	}
	// CP goes down; after TTL, requests must be denied fail-closed.
	st.setFail(true)
	time.Sleep(20 * time.Millisecond)

	rec := doEcho(t, srv, "agent-1", testToken, `{}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body: %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "policy_unavailable") {
		t.Fatalf("body = %s, want policy_unavailable", rec.Body.String())
	}
}

func TestFailClosed_CPTransportDown(t *testing.T) {
	// Point the client at a closed port: transport error, no cache => deny.
	st := newCPStub(t)
	st.srv.Close() // simulate CP fully down
	pc := policy.NewClient(st.srv.URL, 30*time.Second, 500*time.Millisecond)
	srv := New(Deps{
		Policy:       pc,
		PolicyClient: pc,
		Boundary:     boundary.StaticVerifier{},
		Audit:        audit.NopEmitter{},
		Enabled:      func() *atomic.Bool { b := &atomic.Bool{}; b.Store(true); return b }(),
		Drivers:      map[string]drivers.Driver{"echo": drivers.Echo{}},
	})
	rec := doEcho(t, srv, "agent-1", testToken, `{}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
}

func TestFreshCacheServesWithoutCPCall(t *testing.T) {
	st := newCPStub(t)
	st.set("agent-1", auth.HashCredential(testToken))
	srv, _, _ := newTestServer(t, st, time.Hour, true)

	if rec := doEcho(t, srv, "agent-1", testToken, `{}`); rec.Code != 200 {
		t.Fatalf("first status = %d", rec.Code)
	}
	before := st.callCount()
	// CP fails now; fresh cache must still serve.
	st.setFail(true)
	if rec := doEcho(t, srv, "agent-1", testToken, `{}`); rec.Code != 200 {
		t.Fatalf("cached status = %d, want 200", rec.Code)
	}
	if st.callCount() != before {
		t.Fatalf("CP was called again despite fresh cache (calls %d -> %d)", before, st.callCount())
	}
}

func TestETagRevalidation(t *testing.T) {
	st := newCPStub(t)
	st.set("agent-1", auth.HashCredential(testToken))
	srv, _, _ := newTestServer(t, st, 10*time.Millisecond, true)

	if rec := doEcho(t, srv, "agent-1", testToken, `{}`); rec.Code != 200 {
		t.Fatalf("first status = %d", rec.Code)
	}
	time.Sleep(20 * time.Millisecond)
	// After TTL the client must revalidate with If-None-Match; the stub
	// answers 304 and the cached snapshot still authorizes.
	if rec := doEcho(t, srv, "agent-1", testToken, `{}`); rec.Code != 200 {
		t.Fatalf("revalidated status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if st.lastETag == "" {
		t.Fatal("expected If-None-Match to be sent on revalidation")
	}
}

func TestKillSwitch(t *testing.T) {
	st := newCPStub(t)
	st.set("agent-1", auth.HashCredential(testToken))
	srv, _, auditBuf := newTestServer(t, st, time.Hour, false)

	rec := doEcho(t, srv, "agent-1", testToken, `{}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "gateway_disabled") {
		t.Fatalf("body = %s, want gateway_disabled", rec.Body.String())
	}
	// Health endpoints stay reachable under the kill switch.
	for _, p := range []string{"/healthz", "/readyz", "/internal/verify-boundary"} {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if p == "/readyz" {
			// readyz may be 200 or 503 depending on probe state; it must NOT be the kill-switch 503 body.
			if strings.Contains(rec.Body.String(), "gateway_disabled") {
				t.Fatalf("readyz was killed: %s", rec.Body.String())
			}
			continue
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200", p, rec.Code)
		}
	}
	ev := lastAuditEvent(t, auditBuf)
	if ev.Detail != "kill_switch" {
		t.Fatalf("audit detail = %q, want kill_switch", ev.Detail)
	}
}

func TestReadyzFailClosedUntilCPReachable(t *testing.T) {
	st := newCPStub(t)
	pc := policy.NewClient(st.srv.URL, time.Minute, 2*time.Second)
	srv := New(Deps{
		Policy:       pc,
		PolicyClient: pc,
		Boundary:     boundary.StaticVerifier{},
		Audit:        audit.NopEmitter{},
		Enabled:      func() *atomic.Bool { b := &atomic.Bool{}; b.Store(true); return b }(),
		Drivers:      map[string]drivers.Driver{"echo": drivers.Echo{}},
	})

	// Before any CP contact: not ready.
	rec := get(t, srv, "/readyz")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("pre-probe readyz = %d, want 503", rec.Code)
	}

	// Any successful policy call flips reachability.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	pc.Probe(ctx, "nobody") // 404 still proves reachability

	rec = get(t, srv, "/readyz")
	if rec.Code != http.StatusOK {
		t.Fatalf("post-probe readyz = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
}

func TestReadyzBlockedByBoundaryVerifier(t *testing.T) {
	st := newCPStub(t)
	pc := policy.NewClient(st.srv.URL, time.Minute, 2*time.Second)
	pc.Probe(context.Background(), "nobody")
	srv := New(Deps{
		Policy:       pc,
		PolicyClient: pc,
		Boundary:     boundary.NoneVerifier{},
		Audit:        audit.NopEmitter{},
		Enabled:      func() *atomic.Bool { b := &atomic.Bool{}; b.Store(true); return b }(),
		Drivers:      map[string]drivers.Driver{"echo": drivers.Echo{}},
	})
	rec := get(t, srv, "/readyz")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz with unverified fence = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "fence") {
		t.Fatalf("body = %s, want fence reason", rec.Body.String())
	}
}

func TestVerifyBoundaryEndpoint(t *testing.T) {
	st := newCPStub(t)
	pc := policy.NewClient(st.srv.URL, time.Minute, 2*time.Second)
	srv := New(Deps{
		Policy:       pc,
		PolicyClient: pc,
		Boundary:     boundary.StaticVerifier{},
		Audit:        audit.NopEmitter{},
		Enabled:      func() *atomic.Bool { b := &atomic.Bool{}; b.Store(true); return b }(),
		Drivers:      map[string]drivers.Driver{"echo": drivers.Echo{}},
	})
	rec := get(t, srv, "/internal/verify-boundary")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var fs boundary.FenceState
	if err := json.Unmarshal(rec.Body.Bytes(), &fs); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if !fs.Applied || fs.Verifier != "static" {
		t.Fatalf("fence state = %+v", fs)
	}
}

func TestPayloadTooLarge(t *testing.T) {
	st := newCPStub(t)
	st.set("agent-1", auth.HashCredential(testToken))
	pc := policy.NewClient(st.srv.URL, time.Hour, 2*time.Second)
	srv := New(Deps{
		Policy:       pc,
		PolicyClient: pc,
		Boundary:     boundary.StaticVerifier{},
		Audit:        audit.NopEmitter{},
		Enabled:      func() *atomic.Bool { b := &atomic.Bool{}; b.Store(true); return b }(),
		Drivers:      map[string]drivers.Driver{"echo": drivers.Echo{}},
		MaxBodyBytes: 16,
	})
	rec := doEcho(t, srv, "agent-1", testToken, strings.Repeat("x", 17))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func auditEvents(t *testing.T, buf *bytes.Buffer) []audit.Event {
	t.Helper()
	var out []audit.Event
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var ev audit.Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("bad audit line %q: %v", line, err)
		}
		out = append(out, ev)
	}
	return out
}

func lastAuditEvent(t *testing.T, buf *bytes.Buffer) audit.Event {
	t.Helper()
	evs := auditEvents(t, buf)
	if len(evs) == 0 {
		t.Fatal("no audit events")
	}
	return evs[len(evs)-1]
}

// Guard: fmt import used by helper error strings in future edits.
