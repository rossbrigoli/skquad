package httpapi

// MIG-6 (S-312): tests for the agent-facing ssh route
// (POST /v1/ssh/{resourceID}). Mirrors the rest/mcp route test
// patterns: pipeline order (kill switch → authn → body cap → grant
// lookup → dispatch), closed tool-family check, the 202
// pending_confirmation mapping for the driver's conditional deny gate,
// and confirmation-header forwarding.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rossbrigoli/skquad/tool-gateway/internal/auth"
	"github.com/rossbrigoli/skquad/tool-gateway/internal/drivers"
	"github.com/rossbrigoli/skquad/tool-gateway/internal/policy"
)

// recordedSSHReq captures what the route handed to the driver.
type recordedSSHReq struct {
	Operation      string
	Payload        []byte
	ResourceID     string
	ConfirmationID string
}

// fakeSSHDriver is a controllable stand-in for the ssh driver.
type fakeSSHDriver struct {
	last    *recordedSSHReq
	resp    *drivers.Response
	respErr error
}

func (f *fakeSSHDriver) Name() string { return "ssh" }

func (f *fakeSSHDriver) Handle(_ context.Context, req *drivers.Request) (*drivers.Response, error) {
	f.last = &recordedSSHReq{
		Operation:      req.Operation,
		Payload:        append([]byte(nil), req.Payload...),
		ResourceID:     req.Resource,
		ConfirmationID: req.ConfirmationID,
	}
	if f.respErr != nil {
		return nil, f.respErr
	}
	return f.resp, nil
}

func newSSHRoutingServer(t *testing.T, drv drivers.Driver, grant *policy.Grant) (*Server, *cpStub) {
	t.Helper()
	st := newCPStub(t)
	st.set("agent-1", auth.HashCredential(testToken))
	if grant != nil {
		st.mu.Lock()
		snap := st.snapshots["agent-1"]
		snap.Grants = []policy.Grant{*grant}
		st.snapshots["agent-1"] = snap
		st.mu.Unlock()
	}
	srv, _, _ := newTestServer(t, st, 0, true)
	srv.deps.Drivers["ssh"] = drv
	return srv, st
}

func sshGrant(resourceID string) *policy.Grant {
	return &policy.Grant{
		ResourceID:   resourceID,
		ResourceType: "ssh",
		Ceiling:      json.RawMessage(`{"hosts_allow":["lab-*"]}`),
		Constraints:  json.RawMessage(`{"hosts_allow":["lab-1"]}`),
		Config:       json.RawMessage(`{"ssh_user":"ops","port":22,"auth_mode":"static_key"}`),
	}
}

func doSSH(t *testing.T, h http.Handler, resourceID, agentID, token, body, confID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/ssh/"+resourceID, strings.NewReader(body))
	if agentID != "" {
		req.Header.Set("X-Skquad-Agent-ID", agentID)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if confID != "" {
		req.Header.Set(ConfirmationIDHeader, confID)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestSSHRouteExecHappyPath(t *testing.T) {
	drv := &fakeSSHDriver{resp: &drivers.Response{StatusCode: 200, Body: map[string]any{
		"stdout": "up", "stderr": "", "exit_code": 0, "recording_id": "rec-1",
	}}}
	srv, _ := newSSHRoutingServer(t, drv, sshGrant("res-ssh-1"))

	body := `{"tool":"ssh_exec","host":"lab-1","command":"uptime","timeout_seconds":30}`
	rec := doSSH(t, srv, "res-ssh-1", "agent-1", testToken, body, "conf-9")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if drv.last == nil || drv.last.Operation != "ssh_exec" {
		t.Fatalf("driver got %+v", drv.last)
	}
	if drv.last.ResourceID != "res-ssh-1" {
		t.Fatalf("resource id=%q", drv.last.ResourceID)
	}
	if !strings.Contains(string(drv.last.Payload), `"command":"uptime"`) {
		t.Fatalf("payload not forwarded: %s", drv.last.Payload)
	}
	if drv.last.ConfirmationID != "conf-9" {
		t.Fatalf("confirmation header not forwarded: %q", drv.last.ConfirmationID)
	}
}

func TestSSHRouteToolFamilyClosed(t *testing.T) {
	drv := &fakeSSHDriver{resp: &drivers.Response{StatusCode: 200}}
	srv, _ := newSSHRoutingServer(t, drv, sshGrant("res-ssh-1"))

	for _, body := range []string{
		`{"tool":"ssh_wipe","host":"lab-1"}`,
		`{"host":"lab-1","command":"uptime"}`,
		`not json`,
	} {
		rec := doSSH(t, srv, "res-ssh-1", "agent-1", testToken, body, "")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body=%q status=%d want 400", body, rec.Code)
		}
		if drv.last != nil {
			t.Fatalf("driver must not be reached for %q", body)
		}
	}
}

func TestSSHRouteSessionToolsRoutable(t *testing.T) {
	drv := &fakeSSHDriver{resp: &drivers.Response{StatusCode: 200, Body: map[string]any{"session_id": "s-1"}}}
	srv, _ := newSSHRoutingServer(t, drv, sshGrant("res-ssh-1"))
	rec := doSSH(t, srv, "res-ssh-1", "agent-1", testToken, `{"tool":"ssh_session_open","host":"lab-1"}`, "")
	if rec.Code != http.StatusOK || drv.last == nil || drv.last.Operation != "ssh_session_open" {
		t.Fatalf("status=%d last=%+v", rec.Code, drv.last)
	}
}

func TestSSHRouteNoGrant(t *testing.T) {
	drv := &fakeSSHDriver{resp: &drivers.Response{StatusCode: 200}}
	srv, _ := newSSHRoutingServer(t, drv, nil)
	rec := doSSH(t, srv, "res-ssh-1", "agent-1", testToken, `{"tool":"ssh_exec","host":"lab-1","command":"uptime"}`, "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d want 403", rec.Code)
	}
	var env map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env["error"] != "no_grant" {
		t.Fatalf("body=%s", rec.Body.String())
	}
	if drv.last != nil {
		t.Fatal("driver reached without grant")
	}
}

func TestSSHRouteWrongResourceTypeGrant(t *testing.T) {
	grant := sshGrant("res-ssh-1")
	grant.ResourceType = "rest"
	drv := &fakeSSHDriver{resp: &drivers.Response{StatusCode: 200}}
	srv, _ := newSSHRoutingServer(t, drv, grant)
	rec := doSSH(t, srv, "res-ssh-1", "agent-1", testToken, `{"tool":"ssh_exec","host":"lab-1","command":"uptime"}`, "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d want 403", rec.Code)
	}
}

func TestSSHRouteUnauthenticated(t *testing.T) {
	drv := &fakeSSHDriver{resp: &drivers.Response{StatusCode: 200}}
	srv, _ := newSSHRoutingServer(t, drv, sshGrant("res-ssh-1"))
	rec := doSSH(t, srv, "res-ssh-1", "agent-1", "wrong-token", `{"tool":"ssh_exec","host":"lab-1","command":"uptime"}`, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401", rec.Code)
	}
}

func TestSSHRouteKillSwitch(t *testing.T) {
	drv := &fakeSSHDriver{resp: &drivers.Response{StatusCode: 200}}
	srv, _ := newSSHRoutingServer(t, drv, sshGrant("res-ssh-1"))
	srv.deps.Enabled.Store(false)
	rec := doSSH(t, srv, "res-ssh-1", "agent-1", testToken, `{"tool":"ssh_exec","host":"lab-1","command":"uptime"}`, "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want 503", rec.Code)
	}
}

func TestSSHRoutePendingConfirmationMapsTo202(t *testing.T) {
	drv := &fakeSSHDriver{respErr: drivers.Denied("confirmation_required:conf-42")}
	srv, _ := newSSHRoutingServer(t, drv, sshGrant("res-ssh-1"))
	rec := doSSH(t, srv, "res-ssh-1", "agent-1", testToken, `{"tool":"ssh_exec","host":"lab-1","command":"rm -rf /var/log"}`, "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s want 202", rec.Code, rec.Body.String())
	}
	var env map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env["confirmation_id"] != "conf-42" {
		t.Fatalf("missing confirmation id: %s", rec.Body.String())
	}
	if env["retry_with"] != ConfirmationIDHeader {
		t.Fatalf("missing retry hint: %s", rec.Body.String())
	}
}

func TestSSHRouteDenialMapsTo403WithReason(t *testing.T) {
	drv := &fakeSSHDriver{respErr: drivers.Denied("host_denied")}
	srv, _ := newSSHRoutingServer(t, drv, sshGrant("res-ssh-1"))
	rec := doSSH(t, srv, "res-ssh-1", "agent-1", testToken, `{"tool":"ssh_exec","host":"evil.host","command":"uptime"}`, "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "host_denied") {
		t.Fatalf("reason not surfaced: %s", rec.Body.String())
	}
}

func TestSSHRouteTransportFailureMapsTo502(t *testing.T) {
	drv := &fakeSSHDriver{respErr: context.DeadlineExceeded}
	srv, _ := newSSHRoutingServer(t, drv, sshGrant("res-ssh-1"))
	rec := doSSH(t, srv, "res-ssh-1", "agent-1", testToken, `{"tool":"ssh_exec","host":"lab-1","command":"uptime"}`, "")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d want 502", rec.Code)
	}
}

func TestSSHRouteBadResourceID(t *testing.T) {
	drv := &fakeSSHDriver{resp: &drivers.Response{StatusCode: 200}}
	srv, _ := newSSHRoutingServer(t, drv, sshGrant("res-ssh-1"))
	// A backslash in the id is not path-cleaned by the mux and must be
	// rejected by the id sanitizer (400, not a redirect).
	rec := doSSH(t, srv, "bad%5cid", "agent-1", testToken, `{"tool":"ssh_exec","host":"lab-1","command":"uptime"}`, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400", rec.Code)
	}
}
