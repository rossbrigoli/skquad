// TG-4b (S-244 series, card 518fc777): gateway pipeline tests for
// the git smart-HTTP proxy route. Fake upstreams + fake tokens only.
package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rossbrigoli/skquad/tool-gateway/internal/audit"
	"github.com/rossbrigoli/skquad/tool-gateway/internal/auth"
	"github.com/rossbrigoli/skquad/tool-gateway/internal/boundary"
	"github.com/rossbrigoli/skquad/tool-gateway/internal/credentials"
	"github.com/rossbrigoli/skquad/tool-gateway/internal/drivers"
	gitdriver "github.com/rossbrigoli/skquad/tool-gateway/internal/drivers/git"
	"github.com/rossbrigoli/skquad/tool-gateway/internal/policy"
)

const gitFakeToken = "FAKE-GIT-TOKEN-route-test-999"

func gitGrantJSON(baseURL string) policy.Grant {
	return policy.Grant{
		ResourceID:   "git-hub-1",
		ResourceType: "git",
		EgressClass:  "internal", // loopback upstream in tests
		Config:       json.RawMessage(fmt.Sprintf(`{"base_url":%q}`, baseURL)),
		Ceiling:      json.RawMessage(`{"repos_allow":["acme/*"],"allow_push":true,"rate_per_min":120}`),
		Constraints:  json.RawMessage(`{"allow_push":true}`),
	}
}

func newGitRouteServer(t *testing.T, baseURL string, enabled bool) (*Server, *bytes.Buffer) {
	t.Helper()
	st := newCPStub(t)
	st.set("agent-1", auth.HashCredential(testToken))
	snap := st.snapshots["agent-1"]
	snap.Grants = []policy.Grant{gitGrantJSON(baseURL)}
	st.mu.Lock()
	st.snapshots["agent-1"] = snap
	st.mu.Unlock()

	pc := policy.NewClient(st.srv.URL, 30*time.Second, 2*time.Second)
	auditBuf := &bytes.Buffer{}
	srv := New(Deps{
		Policy:       pc,
		PolicyClient: pc,
		Boundary:     boundary.StaticVerifier{},
		Audit:        audit.NewStdoutEmitter(auditBuf),
		Enabled:      func() *atomic.Bool { b := &atomic.Bool{}; b.Store(enabled); return b }(),
		Drivers:      map[string]drivers.Driver{},
		StreamDrivers: map[string]drivers.StreamingDriver{
			"git": gitdriver.New(&gitRouteCreds{}),
		},
		MaxBodyBytes: 1 << 20, // 1 MiB — git streams must NOT be capped by this
	})
	return srv, auditBuf
}

// gitRouteCreds resolves the fake bearer PAT for any (resource, agent).
type gitRouteCreds struct{ calls atomic.Int64 }

func (c *gitRouteCreds) Resolve(_ context.Context, _, _ string) (*credentials.Secret, error) {
	c.calls.Add(1)
	return &credentials.Secret{Kind: "bearer", Fields: map[string]string{"token": gitFakeToken}}, nil
}

func doGit(t *testing.T, h http.Handler, method, path, agentID, token string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, body)
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

func TestGitRouteClonePassthrough(t *testing.T) {
	const refs = "001e# service=git-upload-pack\n0000"
	var gotAuth string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(refs))
	}))
	defer up.Close()

	srv, auditBuf := newGitRouteServer(t, up.URL, true)
	rec := doGit(t, srv, http.MethodGet,
		"/git/git-hub-1/acme/widgets.git/info/refs?service=git-upload-pack", "agent-1", testToken, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != refs {
		t.Fatalf("body = %q want %q", rec.Body.String(), refs)
	}
	if gotAuth != "Bearer "+gitFakeToken {
		t.Fatalf("upstream auth = %q", gotAuth)
	}
	ev := lastAuditEvent(t, auditBuf)
	if ev.Decision != audit.DecisionAllow || ev.Operation != "git_fetch" || ev.AgentID != "agent-1" {
		t.Fatalf("audit = %+v", ev)
	}
	if !strings.Contains(ev.Detail, "acme/widgets") || !strings.Contains(ev.Detail, "git-upload-pack") {
		t.Fatalf("audit detail missing repo/service: %q", ev.Detail)
	}
	if strings.Contains(auditBuf.String(), gitFakeToken) {
		t.Fatal("token leaked into audit")
	}
}

func TestGitRouteNoGrantDenied(t *testing.T) {
	var hits atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer up.Close()

	st := newCPStub(t)
	st.set("agent-none", auth.HashCredential(testToken)) // no grants
	pc := policy.NewClient(st.srv.URL, 30*time.Second, 2*time.Second)
	srv := New(Deps{
		Policy:        pc,
		Boundary:      boundary.StaticVerifier{},
		Audit:         audit.NopEmitter{},
		Enabled:       func() *atomic.Bool { b := &atomic.Bool{}; b.Store(true); return b }(),
		StreamDrivers: map[string]drivers.StreamingDriver{"git": gitdriver.New(&gitRouteCreds{})},
	})
	rec := doGit(t, srv, http.MethodGet,
		"/git/git-hub-1/acme/widgets.git/info/refs?service=git-upload-pack", "agent-none", testToken, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d want 403", rec.Code)
	}
	if hits.Load() != 0 {
		t.Fatal("upstream hit without grant")
	}
}

func TestGitRouteUnauthenticated(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("must not reach upstream")
	}))
	defer up.Close()
	srv, _ := newGitRouteServer(t, up.URL, true)
	rec := doGit(t, srv, http.MethodGet,
		"/git/git-hub-1/acme/widgets.git/info/refs?service=git-upload-pack", "agent-1", "wrong-token", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d want 401", rec.Code)
	}
}

func TestGitRouteKillSwitch(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("must not reach upstream")
	}))
	defer up.Close()
	srv, _ := newGitRouteServer(t, up.URL, false)
	rec := doGit(t, srv, http.MethodGet,
		"/git/git-hub-1/acme/widgets.git/info/refs?service=git-upload-pack", "agent-1", testToken, nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d want 503", rec.Code)
	}
}

func TestGitRoutePushAuditsRefs(t *testing.T) {
	report := pktLine("unpack ok\n") + pktLine("ok refs/heads/main\n") +
		pktLine("ok refs/tags/v1\n") + "0000"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(report))
	}))
	defer up.Close()

	srv, auditBuf := newGitRouteServer(t, up.URL, true)
	body := strings.NewReader("fake-pack-body")
	rec := doGit(t, srv, http.MethodPost, "/git/git-hub-1/acme/widgets.git/git-receive-pack", "agent-1", testToken, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != report {
		t.Fatalf("report not passed through: %q", rec.Body.String())
	}
	ev := lastAuditEvent(t, auditBuf)
	if ev.Operation != "git_push" || ev.Decision != audit.DecisionAllow {
		t.Fatalf("audit = %+v", ev)
	}
	var detail map[string]any
	if err := json.Unmarshal([]byte(ev.Detail), &detail); err != nil {
		t.Fatalf("detail not JSON: %v", err)
	}
	refs, _ := detail["refs_advanced"].([]any)
	if len(refs) != 2 {
		t.Fatalf("refs_advanced = %v", detail["refs_advanced"])
	}
	if strings.Contains(auditBuf.String(), gitFakeToken) {
		t.Fatal("token leaked into audit")
	}
}

func TestGitRouteLargeBodyNotCapped(t *testing.T) {
	// MaxBodyBytes is 1 MiB in this server; a 4 MiB push must still
	// stream through (the git route never reads/caps the body).
	const size = 4 << 20
	big := make([]byte, size)
	_, _ = rand.Read(big)
	var got int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, err := io.Copy(io.Discard, r.Body)
		got = n
		if err == nil || err == io.EOF {
			_, _ = w.Write([]byte("0000"))
			return
		}
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer up.Close()

	srv, _ := newGitRouteServer(t, up.URL, true)
	rec := doGit(t, srv, http.MethodPost, "/git/git-hub-1/acme/widgets.git/git-receive-pack", "agent-1", testToken, bytes.NewReader(big))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d want 200 (no 413 on git streams)", rec.Code)
	}
	if got != size {
		t.Fatalf("upstream received %d want %d", got, size)
	}
}

func TestGitRouteReadOnlyGrantPushDenied(t *testing.T) {
	var hits atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer up.Close()

	st := newCPStub(t)
	st.set("agent-ro", auth.HashCredential(testToken))
	snap := st.snapshots["agent-ro"]
	g := gitGrantJSON(up.URL)
	g.Constraints = json.RawMessage(`{}`) // read-only grant (no push opt-in)
	snap.Grants = []policy.Grant{g}
	st.mu.Lock()
	st.snapshots["agent-ro"] = snap
	st.mu.Unlock()

	pc := policy.NewClient(st.srv.URL, 30*time.Second, 2*time.Second)
	srv := New(Deps{
		Policy:        pc,
		Boundary:      boundary.StaticVerifier{},
		Audit:         audit.NopEmitter{},
		Enabled:       func() *atomic.Bool { b := &atomic.Bool{}; b.Store(true); return b }(),
		StreamDrivers: map[string]drivers.StreamingDriver{"git": gitdriver.New(&gitRouteCreds{})},
	})
	rec := doGit(t, srv, http.MethodPost, "/git/git-hub-1/acme/widgets.git/git-receive-pack", "agent-ro", testToken, strings.NewReader("x"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d want 403", rec.Code)
	}
	if hits.Load() != 0 {
		t.Fatal("read-only grant must not reach upstream")
	}
}

func TestGitRouteBadResourceID(t *testing.T) {
	srv, _ := newGitRouteServer(t, "http://127.0.0.1:1", true)
	rec := doGit(t, srv, http.MethodGet, "/git/..%2Fevil/acme/x.git/info/refs?service=git-upload-pack", "agent-1", testToken, nil)
	// chi/Go mux may 404 the route or 400 the id; either is a deny.
	if rec.Code == http.StatusOK {
		t.Fatal("bad resource id must not pass")
	}
}

func pktLine(payload string) string {
	return fmt.Sprintf("%04x%s", len(payload)+4, payload)
}
