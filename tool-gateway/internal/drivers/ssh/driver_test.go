package ssh

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rossbrigoli/skquad/tool-gateway/internal/audit"
	"github.com/rossbrigoli/skquad/tool-gateway/internal/auth"
	"github.com/rossbrigoli/skquad/tool-gateway/internal/confirmation"
	"github.com/rossbrigoli/skquad/tool-gateway/internal/credentials"
	"github.com/rossbrigoli/skquad/tool-gateway/internal/drivers"
	"github.com/rossbrigoli/skquad/tool-gateway/internal/policy"
)

// ---- fakes ----

type orderedAudit struct {
	mu     sync.Mutex
	events []string // "action|detail"
}

func (a *orderedAudit) Emit(ev audit.Event) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, ev.Operation+"|"+ev.Detail)
}
func (a *orderedAudit) list() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string{}, a.events...)
}
func (a *orderedAudit) has(prefix string) bool {
	for _, e := range a.list() {
		if strings.HasPrefix(e, prefix) {
			return true
		}
	}
	return false
}

type fakeConf struct {
	checkMode  string // "auto" | "pending"
	checkErr   error
	consumeRes *confirmation.ConsumeResult
	consumeErr error
	checks     int
	consumes   int
}

func (f *fakeConf) Check(ctx context.Context, resourceID, agentID, tool, argsHash string) (*confirmation.CheckResult, error) {
	f.checks++
	if f.checkErr != nil {
		return nil, f.checkErr
	}
	res := &confirmation.CheckResult{Mode: f.checkMode}
	if f.checkMode == "auto" {
		res.MatchedStandingGrantID = "sg-1"
	} else {
		res.ConfirmationID = "conf-1"
	}
	return res, nil
}

func (f *fakeConf) Consume(ctx context.Context, id, argsHash string) (*confirmation.ConsumeResult, error) {
	f.consumes++
	if f.consumeErr != nil {
		return nil, f.consumeErr
	}
	if f.consumeRes != nil {
		return f.consumeRes, nil
	}
	return &confirmation.ConsumeResult{Allowed: true, Mode: "once"}, nil
}

type fakeCreds struct {
	secret *credentials.Secret
	err    error
	calls  int
}

func (f *fakeCreds) Resolve(ctx context.Context, resourceID, agentID string) (*credentials.Secret, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.secret, nil
}

// terminalCall records one terminal-service request.
type terminalCall struct {
	path   string
	body   map[string]any
	header string
}

func fakeTerminal(t *testing.T, status int, resp map[string]any, calls *[]terminalCall, mu *sync.Mutex) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		mu.Lock()
		*calls = append(*calls, terminalCall{path: r.URL.Path, body: m, header: r.Header.Get("Authorization")})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(resp)
	}))
}

const testCeiling = `{"hosts_allow":["staging-*","prod-web-*"],"hosts_deny":["prod-db*"],"cert_ttl_minutes":30,"exec_timeout_seconds":60,"max_concurrent_sessions":2}`
const testConfig = `{"ssh_user":"ops","port":22,"known_hosts":"staging-1 ssh-ed25519 AAAA","auth_mode":"ca"}`

func grant(ceiling, constraints, config string) *policy.Grant {
	return &policy.Grant{
		ResourceID:   "res-ssh",
		ResourceType: "ssh",
		Ceiling:      json.RawMessage(ceiling),
		Constraints:  json.RawMessage(constraints),
		Config:       json.RawMessage(config),
	}
}

func req(agentID, operation, payload string) *drivers.Request {
	return &drivers.Request{
		Agent:     &auth.AgentPrincipal{AgentID: agentID},
		Resource:  "res-ssh",
		Operation: operation,
		Payload:   []byte(payload),
		Grant:     grant(testCeiling, `{}`, testConfig),
	}
}

func newDriver(t *testing.T, url, token string, em audit.Emitter, conf ConfirmationClient, creds credentials.Resolver) *Driver {
	return New(url, token, em, conf, creds)
}

// ---- tests ----

func TestExecHappyPathAuditOrder(t *testing.T) {
	var mu sync.Mutex
	var calls []terminalCall
	tsrv := fakeTerminal(t, 200, map[string]any{"stdout": "ok", "exit_code": 0, "recording_id": "rec1"}, &calls, &mu)
	defer tsrv.Close()
	em := &orderedAudit{}
	d := newDriver(t, tsrv.URL, "tok", em, &fakeConf{}, &fakeCreds{})

	resp, err := d.Handle(context.Background(), req("a1", "ssh_exec", `{"host":"staging-1","command":"uptime"}`))
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status %d", resp.StatusCode)
	}
	// terminal called with bearer + required fields
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 1 || calls[0].path != "/v1/exec" {
		t.Fatalf("calls: %+v", calls)
	}
	if calls[0].header != "Bearer tok" {
		t.Errorf("auth header %q", calls[0].header)
	}
	if calls[0].body["host"] != "staging-1" || calls[0].body["user"] != "ops" || calls[0].body["command"] != "uptime" {
		t.Errorf("body: %+v", calls[0].body)
	}
	auth := calls[0].body["auth"].(map[string]any)
	if auth["mode"] != "ca" {
		t.Errorf("auth mode %v", auth["mode"])
	}
	if _, leaked := auth["private_key_pem"]; leaked {
		t.Error("private_key_pem must not be sent in ca mode")
	}
	// audit-before-execute: attempt recorded BEFORE the terminal call,
	// result after.
	events := em.list()
	attemptIdx, resultIdx := -1, -1
	for i, e := range events {
		if strings.HasPrefix(e, "ssh_exec_attempt|") {
			attemptIdx = i
		}
		if strings.HasPrefix(e, "ssh_exec_result|") {
			resultIdx = i
		}
	}
	if attemptIdx < 0 || resultIdx < 0 || attemptIdx > resultIdx {
		t.Fatalf("audit order wrong: %v", events)
	}
	// attempt must precede the terminal-service hit: since the terminal
	// call happened between the two audit emits, attemptIdx < resultIdx
	// proves ordering (result is emitted only after the call returns).
}

func TestExecHostDenied(t *testing.T) {
	var mu sync.Mutex
	var calls []terminalCall
	tsrv := fakeTerminal(t, 200, map[string]any{}, &calls, &mu)
	defer tsrv.Close()
	d := newDriver(t, tsrv.URL, "tok", &orderedAudit{}, &fakeConf{}, &fakeCreds{})
	_, err := d.Handle(context.Background(), req("a1", "ssh_exec", `{"host":"evil.host","command":"uptime"}`))
	if !isDenied(err, "host_denied") {
		t.Fatalf("want host_denied, got %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 0 {
		t.Error("terminal must not be called on host_denied")
	}
}

func TestExecCommandDenied(t *testing.T) {
	var mu sync.Mutex
	var calls []terminalCall
	tsrv := fakeTerminal(t, 200, map[string]any{}, &calls, &mu)
	defer tsrv.Close()
	// command_allow set and command not in it → hard CommandDeny
	g := grant(`{"hosts_allow":["staging-*"],"command_allow":["journalctl *","ls *"]}`, `{"command_allow":["journalctl *"]}`, testConfig)
	r := req("a1", "ssh_exec", `{"host":"staging-1","command":"rm -f /tmp/x"}`)
	r.Grant = g
	d := newDriver(t, tsrv.URL, "tok", &orderedAudit{}, &fakeConf{}, &fakeCreds{})
	_, err := d.Handle(context.Background(), r)
	if !isDenied(err, "command_denied") {
		t.Fatalf("want command_denied, got %v", err)
	}
}

func TestExecDenyPatternGatedPending(t *testing.T) {
	var mu sync.Mutex
	var calls []terminalCall
	tsrv := fakeTerminal(t, 200, map[string]any{}, &calls, &mu)
	defer tsrv.Close()
	conf := &fakeConf{checkMode: "pending"}
	d := newDriver(t, tsrv.URL, "tok", &orderedAudit{}, conf, &fakeCreds{})
	_, err := d.Handle(context.Background(), req("a1", "ssh_exec", `{"host":"staging-1","command":"rm -rf /var/log"}`))
	if err == nil || !strings.Contains(err.Error(), "confirmation_required:conf-1") {
		t.Fatalf("want confirmation_required:conf-1, got %v", err)
	}
	if conf.checks != 1 {
		t.Errorf("expected 1 check, got %d", conf.checks)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 0 {
		t.Error("gated pending must NOT call terminal")
	}
}

func TestExecDenyPatternStandingProceeds(t *testing.T) {
	var mu sync.Mutex
	var calls []terminalCall
	tsrv := fakeTerminal(t, 200, map[string]any{"exit_code": 0, "recording_id": "r"}, &calls, &mu)
	defer tsrv.Close()
	em := &orderedAudit{}
	conf := &fakeConf{checkMode: "auto"}
	d := newDriver(t, tsrv.URL, "tok", em, conf, &fakeCreds{})
	_, err := d.Handle(context.Background(), req("a1", "ssh_exec", `{"host":"staging-1","command":"rm -rf /var/log"}`))
	if err != nil {
		t.Fatalf("standing grant should proceed: %v", err)
	}
	if !em.has("ssh_gate_standing") && !strings.Contains(strings.Join(em.list(), "\n"), `"gate":"standing"`) {
		t.Errorf("standing gate audit missing: %v", em.list())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 1 {
		t.Error("standing grant must call terminal")
	}
}

func TestExecRetryWithConfirmationIDConsumes(t *testing.T) {
	var mu sync.Mutex
	var calls []terminalCall
	tsrv := fakeTerminal(t, 200, map[string]any{"exit_code": 0, "recording_id": "r"}, &calls, &mu)
	defer tsrv.Close()
	conf := &fakeConf{consumeRes: &confirmation.ConsumeResult{Allowed: true, Mode: "once"}}
	d := newDriver(t, tsrv.URL, "tok", &orderedAudit{}, conf, &fakeCreds{})
	r := req("a1", "ssh_exec", `{"host":"staging-1","command":"rm -rf /var/log"}`)
	r.ConfirmationID = "conf-42"
	if _, err := d.Handle(context.Background(), r); err != nil {
		t.Fatalf("consume-approved retry must proceed: %v", err)
	}
	if conf.consumes != 1 || conf.checks != 0 {
		t.Errorf("expected consume-only path: consumes=%d checks=%d", conf.consumes, conf.checks)
	}
}

func TestExecConfirmationUnavailableFailsClosed(t *testing.T) {
	var mu sync.Mutex
	var calls []terminalCall
	tsrv := fakeTerminal(t, 200, map[string]any{}, &calls, &mu)
	defer tsrv.Close()
	conf := &fakeConf{checkErr: errors.New("cp down")}
	d := newDriver(t, tsrv.URL, "tok", &orderedAudit{}, conf, &fakeCreds{})
	_, err := d.Handle(context.Background(), req("a1", "ssh_exec", `{"host":"staging-1","command":"rm -rf /x"}`))
	if !isDenied(err, "confirmation_unavailable") {
		t.Fatalf("want confirmation_unavailable, got %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 0 {
		t.Error("must not call terminal when gate unavailable")
	}
}

func TestStaticKeyCredentials(t *testing.T) {
	var mu sync.Mutex
	var calls []terminalCall
	tsrv := fakeTerminal(t, 200, map[string]any{"exit_code": 0, "recording_id": "r"}, &calls, &mu)
	defer tsrv.Close()
	creds := &fakeCreds{secret: &credentials.Secret{Fields: map[string]string{"private_key_pem": "PEMDATA"}}}
	cfg := `{"ssh_user":"ops","known_hosts":"h ssh-ed25519 AAAA","auth_mode":"static_key"}`
	r := req("a1", "ssh_exec", `{"host":"staging-1","command":"uptime"}`)
	r.Grant = grant(testCeiling, `{}`, cfg)
	d := newDriver(t, tsrv.URL, "tok", &orderedAudit{}, &fakeConf{}, creds)
	if _, err := d.Handle(context.Background(), r); err != nil {
		t.Fatalf("static_key exec: %v", err)
	}
	mu.Lock()
	auth := calls[0].body["auth"].(map[string]any)
	mu.Unlock()
	if auth["mode"] != "static_key" || auth["private_key_pem"] != "PEMDATA" {
		t.Errorf("static key not forwarded: %+v", auth)
	}
}

func TestStaticKeyResolutionFailure(t *testing.T) {
	var mu sync.Mutex
	var calls []terminalCall
	tsrv := fakeTerminal(t, 200, map[string]any{}, &calls, &mu)
	defer tsrv.Close()
	creds := &fakeCreds{err: errors.New("no secret")}
	cfg := `{"ssh_user":"ops","known_hosts":"h ssh-ed25519 AAAA","auth_mode":"static_key"}`
	r := req("a1", "ssh_exec", `{"host":"staging-1","command":"uptime"}`)
	r.Grant = grant(testCeiling, `{}`, cfg)
	d := newDriver(t, tsrv.URL, "tok", &orderedAudit{}, &fakeConf{}, creds)
	_, err := d.Handle(context.Background(), r)
	if !isDenied(err, "credentials_unavailable") {
		t.Fatalf("want credentials_unavailable, got %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 0 {
		t.Error("must not call terminal without credentials")
	}
}

func TestSessionLifecycle(t *testing.T) {
	var mu sync.Mutex
	var calls []terminalCall
	tsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		mu.Lock()
		calls = append(calls, terminalCall{path: r.URL.Path, body: m})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/events"):
			json.NewEncoder(w).Encode(map[string]any{"cursor": 3, "events": []any{}, "closed": false})
		default:
			json.NewEncoder(w).Encode(map[string]any{"session_id": "sess-1", "recording_id": "rec-1"})
		}
	}))
	defer tsrv.Close()
	d := newDriver(t, tsrv.URL, "tok", &orderedAudit{}, &fakeConf{}, &fakeCreds{})

	// open
	_, err := d.Handle(context.Background(), req("a1", "ssh_session_open", `{"host":"staging-1"}`))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// foreign agent cannot touch the session
	_, err = d.Handle(context.Background(), req("a2", "ssh_session_send", `{"session_id":"sess-1","stdin_b64":"aGk="}`))
	if !isDenied(err, "session_not_found") {
		t.Fatalf("foreign session must be not_found, got %v", err)
	}
	// owner can send
	if _, err := d.Handle(context.Background(), req("a1", "ssh_session_send", `{"session_id":"sess-1","stdin_b64":"aGk="}`)); err != nil {
		t.Fatalf("owner send: %v", err)
	}
	// events
	resp, err := d.Handle(context.Background(), req("a1", "ssh_session_events", `{"session_id":"sess-1","cursor":0}`))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("events: %v", err)
	}
	// close
	if _, err := d.Handle(context.Background(), req("a1", "ssh_session_close", `{"session_id":"sess-1"}`)); err != nil {
		t.Fatalf("close: %v", err)
	}
	// after close, session is gone
	_, err = d.Handle(context.Background(), req("a1", "ssh_session_send", `{"session_id":"sess-1","stdin_b64":"aGk="}`))
	if !isDenied(err, "session_not_found") {
		t.Fatalf("closed session must be not_found, got %v", err)
	}
}

func TestSessionLimit(t *testing.T) {
	var mu sync.Mutex
	n := 0
	tsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		id := "sess-" + string(rune('0'+n))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"session_id": id, "recording_id": "rec"})
	}))
	defer tsrv.Close()
	// ceiling max_concurrent_sessions = 2
	d := newDriver(t, tsrv.URL, "tok", &orderedAudit{}, &fakeConf{}, &fakeCreds{})
	for i := 0; i < 2; i++ {
		if _, err := d.Handle(context.Background(), req("a1", "ssh_session_open", `{"host":"staging-1"}`)); err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
	}
	_, err := d.Handle(context.Background(), req("a1", "ssh_session_open", `{"host":"staging-1"}`))
	if !isDenied(err, "session_limit") {
		t.Fatalf("want session_limit, got %v", err)
	}
	// a DIFFERENT agent has its own budget
	if _, err := d.Handle(context.Background(), req("a2", "ssh_session_open", `{"host":"staging-1"}`)); err != nil {
		t.Fatalf("other agent should get a slot: %v", err)
	}
}

func TestUnknownOperation(t *testing.T) {
	d := newDriver(t, "http://x", "tok", &orderedAudit{}, &fakeConf{}, &fakeCreds{})
	_, err := d.Handle(context.Background(), req("a1", "ssh_wat", `{}`))
	if !isDenied(err, "unknown_operation") {
		t.Fatalf("want unknown_operation, got %v", err)
	}
}

func TestTerminalErrorMapping(t *testing.T) {
	var mu sync.Mutex
	var calls []terminalCall
	tsrv := fakeTerminal(t, 502, map[string]any{"error": "host_verification_failed"}, &calls, &mu)
	defer tsrv.Close()
	d := newDriver(t, tsrv.URL, "tok", &orderedAudit{}, &fakeConf{}, &fakeCreds{})
	_, err := d.Handle(context.Background(), req("a1", "ssh_exec", `{"host":"staging-1","command":"uptime"}`))
	if !isDenied(err, "host_verification_failed") {
		t.Fatalf("want host_verification_failed passthrough, got %v", err)
	}
}

func isDenied(err error, reason string) bool {
	var d *drivers.DeniedError
	if errors.As(err, &d) {
		return d.Reason == reason
	}
	return false
}

var _ = time.Now
