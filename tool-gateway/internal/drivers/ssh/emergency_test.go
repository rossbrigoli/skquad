package ssh

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/rossbrigoli/skquad/tool-gateway/internal/confirmation"
)

// emergencyTerminal fakes the terminal-service session endpoints and
// records every /send body so we can assert exactly what reached the
// PTY.
type emergencyTerminal struct {
	mu       sync.Mutex
	sends    []map[string]any
	openBody map[string]any
}

func (e *emergencyTerminal) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		e.mu.Lock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/send"):
			e.sends = append(e.sends, m)
		case strings.HasSuffix(r.URL.Path, "/sessions"):
			e.openBody = m
		}
		e.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"session_id": "sess-e1", "recording_id": "rec-e1"})
	})
}

func (e *emergencyTerminal) sendCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.sends)
}

const emergencyCeiling = `{"hosts_allow":["staging-*"],"cert_ttl_minutes":60}`

func openEmergency(t *testing.T, d *Driver, incident string) {
	t.Helper()
	r := req("a1", "ssh_session_open", `{"host":"staging-1","emergency":true,"incident_id":"`+incident+`"}`)
	r.Grant = grant(emergencyCeiling, `{}`, testConfig)
	if _, err := d.Handle(context.Background(), r); err != nil {
		t.Fatalf("emergency open: %v", err)
	}
}

func TestEmergencyPairValidation(t *testing.T) {
	et := &emergencyTerminal{}
	tsrv := httptest.NewServer(et.handler())
	defer tsrv.Close()
	d := newDriver(t, tsrv.URL, "tok", &orderedAudit{}, &fakeConf{checkMode: "pending"}, &fakeCreds{})

	cases := []struct {
		payload string
		want    string
	}{
		{`{"host":"staging-1","emergency":true}`, "emergency_without_incident_id"},
		{`{"host":"staging-1","incident_id":"INC-1"}`, "incident_id_without_emergency"},
		{`{"host":"staging-1","emergency":true,"incident_id":"INC-\u0001x"}`, "invalid_incident_id"},
		{`{"host":"staging-1","emergency":true,"incident_id":"` + strings.Repeat("x", 121) + `"}`, "invalid_incident_id"},
	}
	for _, tc := range cases {
		r := req("a1", "ssh_session_open", tc.payload)
		r.Grant = grant(emergencyCeiling, `{}`, testConfig)
		_, err := d.Handle(context.Background(), r)
		if !isDenied(err, tc.want) {
			t.Errorf("payload %s: want %s, got %v", tc.payload, tc.want, err)
		}
	}
}

func TestEmergencyOpenTTLClampAndPropagation(t *testing.T) {
	et := &emergencyTerminal{}
	tsrv := httptest.NewServer(et.handler())
	defer tsrv.Close()
	em := &orderedAudit{}
	d := newDriver(t, tsrv.URL, "tok", em, &fakeConf{checkMode: "pending"}, &fakeCreds{})
	openEmergency(t, d, "INC-42")

	et.mu.Lock()
	defer et.mu.Unlock()
	if et.openBody["emergency"] != true || et.openBody["incident_id"] != "INC-42" {
		t.Errorf("terminal open body missing emergency/incident_id: %+v", et.openBody)
	}
	auth := et.openBody["auth"].(map[string]any)
	if got := auth["cert_ttl_minutes"]; got != float64(EmergencyMaxTTLMinutes) {
		t.Errorf("cert TTL not clamped to %d: %v", EmergencyMaxTTLMinutes, got)
	}
	events := strings.Join(em.list(), "\n")
	if !strings.Contains(events, `"incident_id":"INC-42"`) {
		t.Errorf("open audit events missing incident_id: %s", events)
	}
}

func TestEmergencySendDenyRequiresOneTimeApproval(t *testing.T) {
	et := &emergencyTerminal{}
	tsrv := httptest.NewServer(et.handler())
	defer tsrv.Close()
	conf := &fakeConf{checkMode: "pending"}
	em := &orderedAudit{}
	d := newDriver(t, tsrv.URL, "tok", em, conf, &fakeCreds{})
	openEmergency(t, d, "INC-1")

	// Clean line passes straight through.
	clean := base64.StdEncoding.EncodeToString([]byte("uptime\n"))
	r := req("a1", "ssh_session_send", `{"session_id":"sess-e1","stdin_b64":"`+clean+`"}`)
	r.Grant = grant(emergencyCeiling, `{}`, testConfig)
	if _, err := d.Handle(context.Background(), r); err != nil {
		t.Fatalf("clean send: %v", err)
	}
	if et.sendCount() != 1 {
		t.Fatalf("clean send must reach terminal, sends=%d", et.sendCount())
	}

	// Deny-pattern line is held: confirmation_required, nothing sent.
	deny := base64.StdEncoding.EncodeToString([]byte("sudo reboot\n"))
	r2 := req("a1", "ssh_session_send", `{"session_id":"sess-e1","stdin_b64":"`+deny+`"}`)
	r2.Grant = grant(emergencyCeiling, `{}`, testConfig)
	_, err := d.Handle(context.Background(), r2)
	if err == nil || !strings.Contains(err.Error(), "confirmation_required:conf-1") {
		t.Fatalf("deny line must require confirmation, got %v", err)
	}
	if et.sendCount() != 1 {
		t.Errorf("gated line must NOT reach terminal, sends=%d", et.sendCount())
	}

	// Retry with the one-time confirmation id releases the held bytes.
	r3 := req("a1", "ssh_session_send", `{"session_id":"sess-e1","stdin_b64":"`+deny+`"}`)
	r3.Grant = grant(emergencyCeiling, `{}`, testConfig)
	r3.ConfirmationID = "conf-1"
	if _, err := d.Handle(context.Background(), r3); err != nil {
		t.Fatalf("approved retry must proceed: %v", err)
	}
	if et.sendCount() != 2 {
		t.Errorf("approved retry must reach terminal, sends=%d", et.sendCount())
	}
	et.mu.Lock()
	got := et.sends[1]["stdin_b64"]
	et.mu.Unlock()
	if got != deny {
		t.Errorf("released bytes mismatch: %v", got)
	}
}

// mapConf answers Check with "auto" only for tools explicitly listed as
// having a standing grant — simulating a pre-existing "approve this and
// future" for the normal exec identity.
type mapConf struct {
	standing map[string]bool
	checks   int
	consumes int
	lastTool string
}

func (f *mapConf) Check(ctx context.Context, resourceID, agentID, tool, argsHash string) (*confirmation.CheckResult, error) {
	f.checks++
	f.lastTool = tool
	if f.standing[tool] {
		return &confirmation.CheckResult{Mode: "auto", MatchedStandingGrantID: "sg-1"}, nil
	}
	return &confirmation.CheckResult{Mode: "pending", ConfirmationID: "conf-9"}, nil
}

func (f *mapConf) Consume(ctx context.Context, id, argsHash string) (*confirmation.ConsumeResult, error) {
	f.consumes++
	return &confirmation.ConsumeResult{Allowed: true, Mode: "once"}, nil
}

func TestEmergencySendStandingGrantBypassed(t *testing.T) {
	et := &emergencyTerminal{}
	tsrv := httptest.NewServer(et.handler())
	defer tsrv.Close()
	// A standing grant EXISTS for the ordinary exec identity on this
	// host+pattern. The emergency session must NOT be unlocked by it.
	conf := &mapConf{standing: map[string]bool{
		"ssh_exec#staging-1#deny:sudo *": true,
	}}
	em := &orderedAudit{}
	d := newDriver(t, tsrv.URL, "tok", em, conf, &fakeCreds{})
	openEmergency(t, d, "INC-2")

	deny := base64.StdEncoding.EncodeToString([]byte("sudo reboot\n"))
	r := req("a1", "ssh_session_send", `{"session_id":"sess-e1","stdin_b64":"`+deny+`"}`)
	r.Grant = grant(emergencyCeiling, `{}`, testConfig)
	_, err := d.Handle(context.Background(), r)
	if err == nil || !strings.Contains(err.Error(), "confirmation_required:conf-9") {
		t.Fatalf("standing grant must NOT bypass emergency gate, got %v", err)
	}
	if !strings.HasPrefix(conf.lastTool, "ssh_session#sess-e1#deny:") {
		t.Errorf("emergency gate must use session-scoped identity, got %q", conf.lastTool)
	}
	if et.sendCount() != 0 {
		t.Errorf("gated line must not reach terminal, sends=%d", et.sendCount())
	}
	// One-time approval still works.
	r.ConfirmationID = "conf-9"
	if _, err := d.Handle(context.Background(), r); err != nil {
		t.Fatalf("one-time approval must proceed: %v", err)
	}
	if conf.consumes != 1 || et.sendCount() != 1 {
		t.Errorf("approved send wrong: consumes=%d sends=%d", conf.consumes, et.sendCount())
	}
}

func TestEmergencySendStandingShortCircuitFailsClosed(t *testing.T) {
	et := &emergencyTerminal{}
	tsrv := httptest.NewServer(et.handler())
	defer tsrv.Close()
	// CP answers "auto" (standing short-circuit) even for the
	// emergency identity → must be refused, never trusted.
	conf := &fakeConf{checkMode: "auto"}
	d := newDriver(t, tsrv.URL, "tok", &orderedAudit{}, conf, &fakeCreds{})
	openEmergency(t, d, "INC-3")

	deny := base64.StdEncoding.EncodeToString([]byte("sudo reboot\n"))
	r := req("a1", "ssh_session_send", `{"session_id":"sess-e1","stdin_b64":"`+deny+`"}`)
	r.Grant = grant(emergencyCeiling, `{}`, testConfig)
	_, err := d.Handle(context.Background(), r)
	if !isDenied(err, "standing_grant_not_allowed_in_emergency") {
		t.Fatalf("auto mode must fail closed in emergency, got %v", err)
	}
	if et.sendCount() != 0 {
		t.Errorf("must not send, sends=%d", et.sendCount())
	}
}

func TestEmergencyConsumeStandingFailsClosed(t *testing.T) {
	et := &emergencyTerminal{}
	tsrv := httptest.NewServer(et.handler())
	defer tsrv.Close()
	conf := &fakeConf{consumeRes: &confirmation.ConsumeResult{Allowed: true, Mode: "standing"}}
	d := newDriver(t, tsrv.URL, "tok", &orderedAudit{}, conf, &fakeCreds{})
	openEmergency(t, d, "INC-4")

	deny := base64.StdEncoding.EncodeToString([]byte("sudo reboot\n"))
	r := req("a1", "ssh_session_send", `{"session_id":"sess-e1","stdin_b64":"`+deny+`"}`)
	r.Grant = grant(emergencyCeiling, `{}`, testConfig)
	r.ConfirmationID = "conf-x"
	_, err := d.Handle(context.Background(), r)
	if !isDenied(err, "standing_grant_not_allowed_in_emergency") {
		t.Fatalf("standing consume must be refused in emergency, got %v", err)
	}
	if et.sendCount() != 0 {
		t.Errorf("must not send, sends=%d", et.sendCount())
	}
}

func TestEmergencyIncidentStampedOnAllAuditEvents(t *testing.T) {
	et := &emergencyTerminal{}
	tsrv := httptest.NewServer(et.handler())
	defer tsrv.Close()
	conf := &fakeConf{checkMode: "pending"}
	em := &orderedAudit{}
	d := newDriver(t, tsrv.URL, "tok", em, conf, &fakeCreds{})
	openEmergency(t, d, "INC-55")

	deny := base64.StdEncoding.EncodeToString([]byte("sudo reboot\n"))
	r := req("a1", "ssh_session_send", `{"session_id":"sess-e1","stdin_b64":"`+deny+`"}`)
	r.Grant = grant(emergencyCeiling, `{}`, testConfig)
	_, _ = d.Handle(context.Background(), r) // gated
	r.ConfirmationID = "conf-1"
	_, _ = d.Handle(context.Background(), r) // approved
	rc := req("a1", "ssh_session_close", `{"session_id":"sess-e1"}`)
	rc.Grant = grant(emergencyCeiling, `{}`, testConfig)
	_, _ = d.Handle(context.Background(), rc)

	want := []string{
		"ssh_session_open_attempt", "ssh_session_open",
		"ssh_session_send_gated", "ssh_session_send_approved", "ssh_session_close",
	}
	events := em.list()
	for _, w := range want {
		found := false
		for _, e := range events {
			if strings.HasPrefix(e, w+"|") {
				if !strings.Contains(e, `"incident_id":"INC-55"`) {
					t.Errorf("event %s missing incident_id: %s", w, e)
				}
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected audit event %s, events: %v", w, events)
		}
	}
}
