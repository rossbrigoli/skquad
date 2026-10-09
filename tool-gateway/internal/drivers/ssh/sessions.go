package ssh

// sessions.go — gateway-side registry for interactive ssh sessions.
//
// The gateway OWNS the agent→session mapping: agents receive the
// terminal-service session id but can only act on sessions bound to
// their own agent identity (foreign/unknown ids → session_not_found,
// never a passthrough). Per-agent+resource concurrency is capped by
// the effective policy (MaxConcurrentSessions). Idle sessions are
// reaped best-effort (kill call to the terminal-service).

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/rossbrigoli/skquad/tool-gateway/internal/confirmation"
	"github.com/rossbrigoli/skquad/tool-gateway/internal/drivers"
)

// EmergencyMaxTTLMinutes caps emergency-lane session credentials
// (docs/tool-gateway.md §6.7: TTL forced ≤ 30 minutes). The
// terminal-service enforces the matching hard session deadline.
const EmergencyMaxTTLMinutes = 30

type sshSession struct {
	agentID     string
	resourceID  string
	sessionID   string
	recordingID string
	lastActive  time.Time
	// TG-11 emergency lane: when set, every deny-pattern line typed
	// into the session requires a ONE-TIME approval; standing grants
	// are bypassed (fail closed) and the incident id rides every
	// audit event.
	emergency    bool
	incidentID   string
	host         string
	pendingStdin []byte // gated bytes not yet released to the PTY
}

type sessionRegistry struct {
	mu    sync.Mutex
	byID  map[string]*sshSession
	idleT time.Duration
}

func newSessionRegistry() *sessionRegistry {
	return &sessionRegistry{byID: map[string]*sshSession{}, idleT: 30 * time.Minute}
}

func (r *sessionRegistry) bind(s *sshSession) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byID[s.sessionID] = s
}

// lookup returns the session only when it belongs to agentID.
func (r *sessionRegistry) lookup(agentID, sessionID string) (*sshSession, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.byID[sessionID]
	if !ok || s.agentID != agentID {
		return nil, false
	}
	s.lastActive = time.Now()
	return s, true
}

func (r *sessionRegistry) drop(sessionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byID, sessionID)
}

func (r *sessionRegistry) countFor(agentID, resourceID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	n := 0
	for id, s := range r.byID {
		if now.Sub(s.lastActive) > r.idleT {
			delete(r.byID, id)
			continue
		}
		if s.agentID == agentID && s.resourceID == resourceID {
			n++
		}
	}
	return n
}

func (d *Driver) sessionOpen(ctx context.Context, req *drivers.Request) (*drivers.Response, error) {
	if d.terminalURL == "" || d.token == "" {
		return nil, drivers.Denied("terminal_service_unconfigured")
	}
	var p struct {
		Host       string `json:"host"`
		Emergency  *bool  `json:"emergency"`
		IncidentID string `json:"incident_id"`
	}
	if err := json.Unmarshal(req.Payload, &p); err != nil || p.Host == "" {
		return nil, drivers.Denied("bad_request")
	}
	emergency := p.Emergency != nil && *p.Emergency
	if code := validateEmergencyPair(emergency, p.IncidentID); code != "" {
		d.emitAudit(req, "", "ssh_session_open_refused", map[string]any{"host": p.Host, "error": code})
		return nil, drivers.Denied(code)
	}
	pol, err := EffectivePolicy(req.Grant.Ceiling, req.Grant.Constraints)
	if err != nil {
		return nil, drivers.Denied("ceiling_exceeded")
	}
	if !pol.HostAllowed(p.Host) {
		return nil, drivers.Denied("host_denied")
	}
	agentID := ""
	if req.Agent != nil {
		agentID = req.Agent.AgentID
	}
	if d.sessions.countFor(agentID, req.Grant.ResourceID) >= pol.MaxConcurrentSessions {
		return nil, drivers.Denied("session_limit")
	}
	cfg, err := parseResourceConfig(req.Grant.Config)
	if err != nil {
		return nil, drivers.Denied("resource_config_invalid")
	}
	// Emergency lane: force the ephemeral-cert TTL down to ≤ 30 min
	// regardless of what the ceiling/grant negotiated.
	if emergency && pol.CertTTLMinutes > EmergencyMaxTTLMinutes {
		clamped := *pol
		clamped.CertTTLMinutes = EmergencyMaxTTLMinutes
		pol = &clamped
	}
	body, err := d.buildExecBody(req, execPayload{Host: p.Host}, cfg, pol, agentID)
	if err != nil {
		return nil, err
	}
	if emergency {
		var m map[string]any
		if err := json.Unmarshal(body, &m); err != nil {
			return nil, drivers.Denied("terminal_request_failed")
		}
		m["emergency"] = true
		m["incident_id"] = p.IncidentID
		if body, err = json.Marshal(m); err != nil {
			return nil, drivers.Denied("terminal_request_failed")
		}
	}
	auditDetail := func(extra map[string]any) map[string]any {
		if emergency {
			extra["emergency"] = true
			extra["incident_id"] = p.IncidentID
		}
		return extra
	}
	d.emitAudit(req, agentID, "ssh_session_open_attempt", auditDetail(map[string]any{"host": p.Host, "user": cfg.SSHUser}))

	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	status, raw, err := d.terminalPOST(callCtx, "/v1/sessions", body)
	if err != nil {
		return nil, drivers.Denied("terminal_unreachable")
	}
	if status < 200 || status >= 300 {
		reason := safeTerminalError(raw)
		d.emitAudit(req, agentID, "ssh_session_open_result", auditDetail(map[string]any{"host": p.Host, "error": reason}))
		return nil, drivers.Denied(reason)
	}
	var out struct {
		SessionID   string `json:"session_id"`
		RecordingID string `json:"recording_id"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.SessionID == "" {
		return nil, drivers.Denied("terminal_response_invalid")
	}
	d.sessions.bind(&sshSession{
		agentID: agentID, resourceID: req.Grant.ResourceID,
		sessionID: out.SessionID, recordingID: out.RecordingID, lastActive: time.Now(),
		emergency: emergency, incidentID: p.IncidentID, host: p.Host,
	})
	d.emitAudit(req, agentID, "ssh_session_open", auditDetail(map[string]any{
		"host": p.Host, "session_id": out.SessionID, "recording_id": out.RecordingID,
	}))
	respBody := map[string]any{
		"session_id": out.SessionID, "recording_id": out.RecordingID,
	}
	if emergency {
		respBody["emergency"] = true
		respBody["incident_id"] = p.IncidentID
		respBody["max_ttl_minutes"] = EmergencyMaxTTLMinutes
	}
	return &drivers.Response{StatusCode: 200, Body: respBody}, nil
}

func (d *Driver) sessionSend(ctx context.Context, req *drivers.Request) (*drivers.Response, error) {
	var p struct {
		SessionID string `json:"session_id"`
		StdinB64  string `json:"stdin_b64"`
	}
	if err := json.Unmarshal(req.Payload, &p); err != nil || p.SessionID == "" {
		return nil, drivers.Denied("bad_request")
	}
	agentID := ""
	if req.Agent != nil {
		agentID = req.Agent.AgentID
	}
	sess, ok := d.sessions.lookup(agentID, p.SessionID)
	if !ok {
		return nil, drivers.Denied("session_not_found")
	}
	var body []byte
	if sess.emergency {
		var err error
		if body, err = d.gateEmergencySend(ctx, req, sess, p.StdinB64); err != nil {
			return nil, err
		}
	} else {
		var err error
		if body, err = json.Marshal(map[string]string{"stdin_b64": p.StdinB64}); err != nil {
			return nil, drivers.Denied("bad_request")
		}
	}
	callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	status, raw, err := d.terminalPOST(callCtx, "/v1/sessions/"+sess.sessionID+"/send", body)
	if err != nil {
		return nil, drivers.Denied("terminal_unreachable")
	}
	if status < 200 || status >= 300 {
		return nil, drivers.Denied(safeTerminalError(raw))
	}
	return &drivers.Response{StatusCode: 200, Body: map[string]any{}}, nil
}

func (d *Driver) sessionEvents(ctx context.Context, req *drivers.Request) (*drivers.Response, error) {
	var p struct {
		SessionID string `json:"session_id"`
		Cursor    int    `json:"cursor"`
	}
	if err := json.Unmarshal(req.Payload, &p); err != nil || p.SessionID == "" {
		return nil, drivers.Denied("bad_request")
	}
	agentID := ""
	if req.Agent != nil {
		agentID = req.Agent.AgentID
	}
	sess, ok := d.sessions.lookup(agentID, p.SessionID)
	if !ok {
		return nil, drivers.Denied("session_not_found")
	}
	callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	url := "/v1/sessions/" + sess.sessionID + "/events?cursor=" + strconv.Itoa(p.Cursor)
	status, raw, err := d.terminalGET(callCtx, url)
	if err != nil {
		return nil, drivers.Denied("terminal_unreachable")
	}
	if status < 200 || status >= 300 {
		return nil, drivers.Denied(safeTerminalError(raw))
	}
	var out struct {
		Cursor int             `json:"cursor"`
		Events json.RawMessage `json:"events"`
		Closed bool            `json:"closed"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, drivers.Denied("terminal_response_invalid")
	}
	if out.Closed {
		d.sessions.drop(sess.sessionID)
		d.emitAudit(req, agentID, "ssh_session_closed", incidentDetail(sess, map[string]any{
			"session_id": sess.sessionID, "recording_id": sess.recordingID,
		}))
	}
	return &drivers.Response{StatusCode: 200, Body: map[string]any{
		"cursor": out.Cursor, "events": out.Events, "closed": out.Closed,
	}}, nil
}

func (d *Driver) sessionClose(ctx context.Context, req *drivers.Request) (*drivers.Response, error) {
	var p struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(req.Payload, &p); err != nil || p.SessionID == "" {
		return nil, drivers.Denied("bad_request")
	}
	agentID := ""
	if req.Agent != nil {
		agentID = req.Agent.AgentID
	}
	sess, ok := d.sessions.lookup(agentID, p.SessionID)
	if !ok {
		return nil, drivers.Denied("session_not_found")
	}
	callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	status, raw, err := d.terminalPOST(callCtx, "/v1/sessions/"+sess.sessionID+"/kill", []byte(`{}`))
	if err != nil {
		return nil, drivers.Denied("terminal_unreachable")
	}
	if status < 200 || status >= 300 {
		return nil, drivers.Denied(safeTerminalError(raw))
	}
	d.sessions.drop(sess.sessionID)
	d.emitAudit(req, agentID, "ssh_session_close", incidentDetail(sess, map[string]any{
		"session_id": sess.sessionID, "recording_id": sess.recordingID,
	}))
	return &drivers.Response{StatusCode: 200, Body: map[string]any{}}, nil
}

// incidentDetail stamps the incident id (and the emergency flag) onto
// session audit detail so every audit event for an emergency session
// carries the incident reference (docs §6.7).
func incidentDetail(sess *sshSession, detail map[string]any) map[string]any {
	if sess != nil && sess.emergency {
		detail["emergency"] = true
		detail["incident_id"] = sess.incidentID
	}
	return detail
}

// validateEmergencyPair mirrors the terminal-service check (each module
// is self-contained; both must agree on the codes). Returns "" when
// valid, else the stable refusal code.
//
// DECISION: incident_id WITHOUT emergency is REFUSED (not ignored) —
// silently dropping a security-relevant field could hide an attempted
// emergency that lost its guardrails; failing closed forces the caller
// to state intent explicitly.
func validateEmergencyPair(emergency bool, incidentID string) string {
	switch {
	case emergency && incidentID == "":
		return "emergency_without_incident_id"
	case !emergency && incidentID != "":
		return "incident_id_without_emergency"
	case emergency:
		if utf8Len(incidentID) > 120 || containsControl(incidentID) {
			return "invalid_incident_id"
		}
	}
	return ""
}

func utf8Len(s string) int {
	n := 0
	for range s {
		n++
	}
	return n
}

func containsControl(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

// firstDenyLine scans the complete lines of a PTY input buffer and
// returns the first deny-pattern hit. Only CommandGate is enforced on
// interactive stdin; a CommandDeny verdict (allow-list miss) is NOT
// enforced here — interactive shells run builtins/prompt chatter that
// no allow-list covers, and the emergency lane doc scopes the gate to
// deny patterns. Partial lines (no terminator) cannot execute, so
// they are not gated on their own; the terminator arrives with a
// later chunk and the whole buffered candidate is checked then.
func firstDenyLine(pol *Policy, buf []byte) (string, bool) {
	for _, line := range strings.Split(string(buf), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if verdict, pattern := pol.CheckCommand(line); verdict == CommandGate {
			return pattern, true
		}
	}
	return "", false
}

// gateEmergencySend inspects one interactive send for an emergency
// session. Clean input passes through (with any previously-buffered
// gated bytes). A deny-pattern hit holds the bytes and drives the
// TG-8 gate with a SESSION-SCOPED tool identity:
//
//	tool     = "ssh_session#<session_id>#deny:<pattern>"
//	argsHash = ArgsHash(resource, "ssh_session_send", canonical{session_id, stdin_b64})
//
// The session id is unique per session, so pre-existing standing
// grants (issued as `ssh_exec#<host>#deny:<pattern>`, or "approve
// this and future" under any other identity) can never match —
// standing lookup is effectively bypassed. Defense in depth on top:
// a Check that still answers "auto" (standing short-circuit) and a
// Consume that returns mode "standing" are both DENIED with
// standing_grant_not_allowed_in_emergency. Only a one-time approval
// (consume mode "once") releases the bytes.
func (d *Driver) gateEmergencySend(ctx context.Context, req *drivers.Request, sess *sshSession, stdinB64 string) ([]byte, error) {
	if d.conf == nil {
		return nil, drivers.Denied("confirmation_unavailable")
	}
	pol, err := EffectivePolicy(req.Grant.Ceiling, req.Grant.Constraints)
	if err != nil {
		return nil, drivers.Denied("ceiling_exceeded")
	}
	data, err := base64.StdEncoding.DecodeString(stdinB64)
	if err != nil {
		return nil, drivers.Denied("bad_base64")
	}
	candidate := make([]byte, 0, len(sess.pendingStdin)+len(data))
	// Approval-retry dedupe: the agent retries the byte-identical
	// request after approval, but those bytes already sit in
	// pendingStdin from the gated pass — don't append them twice.
	if bytes.HasSuffix(sess.pendingStdin, data) {
		candidate = append(candidate, sess.pendingStdin...)
	} else {
		candidate = append(candidate, sess.pendingStdin...)
		candidate = append(candidate, data...)
	}

	pattern, hit := firstDenyLine(pol, candidate)
	if !hit {
		sess.pendingStdin = nil
		return json.Marshal(map[string]string{"stdin_b64": base64.StdEncoding.EncodeToString(candidate)})
	}

	canonical, err := json.Marshal(map[string]string{"session_id": sess.sessionID, "stdin_b64": stdinB64})
	if err != nil {
		return nil, drivers.Denied("confirmation_failed")
	}
	argsHash := confirmation.ArgsHash(req.Grant.ResourceID, "ssh_session_send", canonical)
	tool := "ssh_session#" + sess.sessionID + "#deny:" + pattern

	if req.ConfirmationID != "" {
		res, err := d.conf.Consume(ctx, req.ConfirmationID, argsHash)
		if err != nil {
			return nil, drivers.Denied("confirmation_unavailable")
		}
		if !res.Allowed {
			return nil, drivers.Denied("confirmation_denied")
		}
		if res.Mode == "standing" {
			// A standing approval must never unlock an emergency
			// deny-pattern line. Fail closed.
			return nil, drivers.Denied("standing_grant_not_allowed_in_emergency")
		}
		sess.pendingStdin = nil
		d.emitAudit(req, sess.agentID, "ssh_session_send_approved", incidentDetail(sess, map[string]any{
			"session_id": sess.sessionID, "deny_pattern": pattern,
		}))
		return json.Marshal(map[string]string{"stdin_b64": base64.StdEncoding.EncodeToString(candidate)})
	}

	res, err := d.conf.Check(ctx, req.Grant.ResourceID, sess.agentID, tool, argsHash)
	if err != nil {
		return nil, drivers.Denied("confirmation_unavailable")
	}
	if res.Mode != "pending" {
		// "auto" means some standing grant matched the emergency
		// identity — should be impossible with the session-scoped
		// tool, but emergency sessions do not trust standing grants:
		// fail closed.
		return nil, drivers.Denied("standing_grant_not_allowed_in_emergency")
	}
	sess.pendingStdin = candidate
	d.emitAudit(req, sess.agentID, "ssh_session_send_gated", incidentDetail(sess, map[string]any{
		"session_id": sess.sessionID, "deny_pattern": pattern, "confirmation_id": res.ConfirmationID,
	}))
	return nil, drivers.Denied("confirmation_required:" + res.ConfirmationID)
}

func (d *Driver) terminalPOST(ctx context.Context, path string, body []byte) (int, []byte, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, d.terminalURL+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+d.token)
	resp, err := d.http.Do(httpReq)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, raw, nil
}

func (d *Driver) terminalGET(ctx context.Context, path string) (int, []byte, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, d.terminalURL+path, nil)
	if err != nil {
		return 0, nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+d.token)
	resp, err := d.http.Do(httpReq)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, raw, nil
}

var _ = io.Discard
