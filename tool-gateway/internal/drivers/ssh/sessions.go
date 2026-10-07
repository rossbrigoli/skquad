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
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/rossbrigoli/skquad/tool-gateway/internal/drivers"
)

type sshSession struct {
	agentID     string
	resourceID  string
	sessionID   string
	recordingID string
	lastActive  time.Time
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
		Host string `json:"host"`
	}
	if err := json.Unmarshal(req.Payload, &p); err != nil || p.Host == "" {
		return nil, drivers.Denied("bad_request")
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
	body, err := d.buildExecBody(req, execPayload{Host: p.Host}, cfg, pol, agentID)
	if err != nil {
		return nil, err
	}
	d.emitAudit(req, agentID, "ssh_session_open_attempt", map[string]any{"host": p.Host, "user": cfg.SSHUser})

	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	status, raw, err := d.terminalPOST(callCtx, "/v1/sessions", body)
	if err != nil {
		return nil, drivers.Denied("terminal_unreachable")
	}
	if status < 200 || status >= 300 {
		reason := safeTerminalError(raw)
		d.emitAudit(req, agentID, "ssh_session_open_result", map[string]any{"host": p.Host, "error": reason})
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
	})
	d.emitAudit(req, agentID, "ssh_session_open", map[string]any{
		"host": p.Host, "session_id": out.SessionID, "recording_id": out.RecordingID,
	})
	return &drivers.Response{StatusCode: 200, Body: map[string]any{
		"session_id": out.SessionID, "recording_id": out.RecordingID,
	}}, nil
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
	body, _ := json.Marshal(map[string]string{"stdin_b64": p.StdinB64})
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
		d.emitAudit(req, agentID, "ssh_session_closed", map[string]any{
			"session_id": sess.sessionID, "recording_id": sess.recordingID,
		})
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
	d.emitAudit(req, agentID, "ssh_session_close", map[string]any{
		"session_id": sess.sessionID, "recording_id": sess.recordingID,
	})
	return &drivers.Response{StatusCode: 200, Body: map[string]any{}}, nil
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
