package httpapi

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/tool-gateway/internal/audit"
	"github.com/rossbrigoli/skquad/tool-gateway/internal/auth"
	"github.com/rossbrigoli/skquad/tool-gateway/internal/boundary"
	"github.com/rossbrigoli/skquad/tool-gateway/internal/drivers"
	webdriver "github.com/rossbrigoli/skquad/tool-gateway/internal/drivers/web"
	"github.com/rossbrigoli/skquad/tool-gateway/internal/policy"
)

// setWebGrant configures a snapshot for the agent carrying one web grant.
func (st *cpStub) setWebGrant(agent, credHash, config, ceiling, constraints string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	g := policy.Grant{ResourceID: "web-system-1", ResourceType: "web"}
	if config != "" {
		g.Config = json.RawMessage(config)
	}
	if ceiling != "" {
		g.Ceiling = json.RawMessage(ceiling)
	}
	if constraints != "" {
		g.Constraints = json.RawMessage(constraints)
	}
	st.snapshots[agent] = policy.Snapshot{
		AgentID:        agent,
		CredentialHash: credHash,
		Grants:         []policy.Grant{g},
	}
}

func newWebTestServer(t *testing.T, st *cpStub, enabled bool) (*Server, *bytes.Buffer) {
	t.Helper()
	pc := policy.NewClient(st.srv.URL, time.Hour, 2*time.Second)
	auditBuf := &bytes.Buffer{}
	srv := New(Deps{
		Policy:       pc,
		PolicyClient: pc,
		Boundary:     boundary.StaticVerifier{},
		Audit:        audit.NewStdoutEmitter(auditBuf),
		Enabled:      func() *atomic.Bool { b := &atomic.Bool{}; b.Store(enabled); return b }(),
		Drivers:      map[string]drivers.Driver{"web": webdriver.New()},
	})
	return srv, auditBuf
}

func doWebFetch(t *testing.T, h http.Handler, agentID, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/web/fetch", strings.NewReader(body))
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

func TestWebFetchRouteHappyPath(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "gateway-served")
	}))
	defer upstream.Close()

	st := newCPStub(t)
	st.setWebGrant("agent-1", auth.HashCredential(testToken), `{}`,
		`{"allow_private_network":true}`, `{"allow_private_network":true}`)
	srv, auditBuf := newWebTestServer(t, st, true)

	rec := doWebFetch(t, srv, "agent-1", testToken, `{"url":"`+upstream.URL+`/page"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var out struct {
		URL         string `json:"url"`
		Status      int    `json:"status"`
		ContentType string `json:"contentType"`
		BodyB64   string `json:"bodyB64"`
		Truncated bool   `json:"truncated"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Equal(t, 200, out.Status)
	require.Contains(t, out.ContentType, "text/plain")
	body, err := base64.StdEncoding.DecodeString(out.BodyB64)
	require.NoError(t, err)
	require.Equal(t, "gateway-served", string(body))

	ev := lastAuditEvent(t, auditBuf)
	require.Equal(t, audit.DecisionAllow, ev.Decision)
	require.Equal(t, "web-system-1", ev.Resource)
	require.Equal(t, "fetch", ev.Operation)
	require.Equal(t, "agent-1", ev.AgentID)
}

func TestWebFetchRouteNoGrant(t *testing.T) {
	st := newCPStub(t)
	st.set("agent-1", auth.HashCredential(testToken)) // snapshot without grants
	srv, auditBuf := newWebTestServer(t, st, true)

	rec := doWebFetch(t, srv, "agent-1", testToken, `{"url":"http://example.com"}`)
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Contains(t, rec.Body.String(), "no_grant")
	ev := lastAuditEvent(t, auditBuf)
	require.Equal(t, audit.DecisionDeny, ev.Decision)
	require.Equal(t, "no_web_grant", ev.Detail)
}

func TestWebFetchRouteDomainDeny(t *testing.T) {
	st := newCPStub(t)
	st.setWebGrant("agent-1", auth.HashCredential(testToken), `{"deny_domains":["floor.example"]}`, `{}`, `{}`)
	srv, auditBuf := newWebTestServer(t, st, true)

	rec := doWebFetch(t, srv, "agent-1", testToken, `{"url":"http://floor.example/"}`)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "domain_denied")
	ev := lastAuditEvent(t, auditBuf)
	require.Equal(t, audit.DecisionDeny, ev.Decision)
	require.Equal(t, "domain_denied", ev.Detail)
}

func TestWebFetchRouteSSRFDeny(t *testing.T) {
	st := newCPStub(t)
	st.setWebGrant("agent-1", auth.HashCredential(testToken), `{}`, `{}`, `{}`)
	srv, _ := newWebTestServer(t, st, true)

	// Loopback upstream with no allow_private anywhere → ssrf_blocked.
	rec := doWebFetch(t, srv, "agent-1", testToken, `{"url":"http://127.0.0.1:9/none"}`)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "ssrf_blocked")
}

func TestWebFetchRouteKillSwitch(t *testing.T) {
	st := newCPStub(t)
	st.setWebGrant("agent-1", auth.HashCredential(testToken), `{}`, `{}`, `{}`)
	srv, auditBuf := newWebTestServer(t, st, false)

	rec := doWebFetch(t, srv, "agent-1", testToken, `{"url":"http://example.com"}`)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Contains(t, rec.Body.String(), "gateway_disabled")
	require.Equal(t, "kill_switch", lastAuditEvent(t, auditBuf).Detail)
}

func TestWebFetchRouteAuthn(t *testing.T) {
	st := newCPStub(t)
	st.setWebGrant("agent-1", auth.HashCredential(testToken), `{}`, `{}`, `{}`)
	srv, _ := newWebTestServer(t, st, true)

	rec := doWebFetch(t, srv, "agent-1", "wrong-token", `{"url":"http://example.com"}`)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	rec = doWebFetch(t, srv, "", "", `{"url":"http://example.com"}`)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestWebFetchRouteBadRequest(t *testing.T) {
	st := newCPStub(t)
	st.setWebGrant("agent-1", auth.HashCredential(testToken), `{}`, `{}`, `{}`)
	srv, _ := newWebTestServer(t, st, true)

	rec := doWebFetch(t, srv, "agent-1", testToken, `{"url":"ftp://x.example"}`)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "bad_request")
}
