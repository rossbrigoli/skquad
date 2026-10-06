// Package browser implements the gateway's governed `browser` driver
// (docs/tg6-browser-protocol.md §6, docs/tool-gateway.md §6.4):
// session-brokered access to the in-cluster browser-service MCP surface.
//
// The agent never holds a browser session itself. The driver:
//
//   - Brokers sessions (protocol §2): on the first tool call for an
//     agent with no active session it POSTs /v1/sessions with the
//     (agent, task, resource) binding and the grant-derived policy
//     ceiling, authenticating with the gateway's internal token
//     (SKQUAD_BROWSER_INTERNAL_TOKEN).
//   - Maintains an in-memory, mutex-guarded registry agent → sessions,
//     enforcing max_sessions_per_agent (default 1; violation →
//     DeniedError "session_limit").
//   - Injects the session token into forwarded tool arguments and
//     STRIPS any agent-supplied `session` field first: agents can never
//     choose, spoof, or observe another session's token.
//   - Checks expiry before use: a stale binding is closed (best
//     effort) and transparently recreated.
//   - Maps JSON-RPC protocol errors to client-safe denials:
//     -32001 → "session_invalid" (unbind), -32002 →
//     "ceiling_exceeded", -32003 → "browser_busy". Transport
//     failures are retryable errors and close+unbind the session.
//   - Wraps all tool output as untrusted content (same S-PROMPT WP3
//     posture as the mcp driver).
//
// Egress note: the service URL is admin-registered (grant config), not
// agent-supplied, and is an in-cluster http endpoint; the driver talks
// to it directly. All agent-driven web egress is the browser service's
// problem (netguard sidecar), not this driver's.
package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/rossbrigoli/skquad/tool-gateway/internal/drivers"
	"github.com/rossbrigoli/skquad/tool-gateway/internal/drivers/mcp"
)

// Timeout bounds each upstream exchange (control + MCP).
const Timeout = 30 * time.Second

// allowedTools is the pinned v1 browser tool surface (protocol §3).
// Anything else is denied before any session or network activity.
var allowedTools = map[string]bool{
	"browser.navigate":      true,
	"browser.click":         true,
	"browser.type":          true,
	"browser.screenshot":    true,
	"browser.extract":       true,
	"browser.close_session": true,
}

// CallRequest is the agent-facing browser_call payload:
//
//	{"tool": "browser.navigate", "arguments": {"url": "..."}, "task_id": "..."}
//
// Any "session" field inside Arguments is agent-supplied and ALWAYS
// stripped/overwritten before forwarding.
type CallRequest struct {
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	TaskID    string          `json:"task_id,omitempty"`
}

// CallResult is returned to the caller on success (HTTP 200). Tool
// content is wrapped as untrusted.
type CallResult struct {
	Tool      string `json:"tool"`
	Content   string `json:"content,omitempty"`
	IsError   bool   `json:"isError"`
	SessionID string `json:"session_id,omitempty"`
}

// session is one brokered browser session bound to an agent.
type session struct {
	sessionID string
	token     string
	taskID    string
	expiresAt time.Time
}

// expired reports whether the session is past its hard cap. A zero
// expiry is treated as stale (fail-closed: we cannot prove liveness).
func (s *session) expired(now time.Time) bool {
	return s.expiresAt.IsZero() || !now.Before(s.expiresAt)
}

// Driver performs governed browser calls with session brokering.
// Registry state is in-memory only; a gateway restart orphans sessions
// to the service-side sweeper (protocol §2).
type Driver struct {
	internalToken string

	mu            sync.Mutex
	sessions      map[string][]*session // agentID → active sessions (creation order)
	agentLocks    map[string]*sync.Mutex
	agentLockRefs map[string]int

	// Injection points for tests.
	httpClient *http.Client
	now        func() time.Time
}

// New builds a browser driver bound to the gateway's internal service
// token. An empty token disables the driver fail-closed.
func New(internalToken string) *Driver {
	return &Driver{
		internalToken: strings.TrimSpace(internalToken),
		sessions:      map[string][]*session{},
		agentLocks:    map[string]*sync.Mutex{},
		agentLockRefs: map[string]int{},
		httpClient:    &http.Client{Timeout: Timeout},
		now:           time.Now,
	}
}

func (d *Driver) Name() string { return "browser" }

// Handle executes one governed browser tool call:
// policy fold → tool ACL → per-agent lock → session acquire
// (expiry/limit/create) → token injection → MCP forward → error
// mapping → untrusted wrap.
func (d *Driver) Handle(ctx context.Context, req *drivers.Request) (*drivers.Response, error) {
	if req.Grant == nil {
		return nil, drivers.Denied("no_grant")
	}
	if d.internalToken == "" {
		// Fail-closed: without the internal token we cannot broker.
		return nil, drivers.Denied("browser_unconfigured")
	}
	var in CallRequest
	if err := json.Unmarshal(req.Payload, &in); err != nil {
		return nil, fmt.Errorf("bad_request: %w", err)
	}
	in.Tool = strings.TrimSpace(in.Tool)
	if !allowedTools[in.Tool] {
		return nil, drivers.Denied("tool_denied")
	}
	argsObj := map[string]any{}
	if len(in.Arguments) > 0 {
		if err := json.Unmarshal(in.Arguments, &argsObj); err != nil {
			return nil, fmt.Errorf("bad_request: arguments must be a JSON object")
		}
	}
	agentID := ""
	if req.Agent != nil {
		agentID = req.Agent.AgentID
	}
	if agentID == "" {
		return nil, drivers.Denied("agent_identity_missing")
	}

	pol, err := EffectivePolicy(req.Grant.Config, req.Grant.Ceiling, req.Grant.Constraints)
	if err != nil {
		return nil, drivers.Denied("invalid_policy")
	}
	endpoint, err := mcpEndpoint(pol.ServiceURL)
	if err != nil {
		return nil, fmt.Errorf("bad_request: invalid browser service endpoint")
	}

	// Serialize everything for one agent: acquire + forward, so two
	// concurrent calls can never both create a session.
	unlock := d.lockAgent(agentID)
	defer unlock()

	sess, err := d.acquireSession(ctx, req, pol, agentID, in.TaskID)
	if err != nil {
		return nil, err
	}

	// Token injection: strip any agent-supplied session field, then
	// set ours. The agent value is never forwarded.
	delete(argsObj, "session")
	argsObj["session"] = sess.token
	forwardArgs, err := json.Marshal(argsObj)
	if err != nil {
		return nil, fmt.Errorf("bad_request: arguments not serializable")
	}

	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()

	c := newServiceClient(ctx, pol.ServiceURL, sess.token, d.httpClient)
	if err := c.initialize(endpoint); err != nil {
		// Transport/protocol failure: drop the binding so the next
		// call re-brokers; the service sweeper reaps the orphan.
		d.unbind(agentID, sess)
		return nil, err
	}
	resultRaw, rpcErr, err := c.toolsCall(endpoint, in.Tool, forwardArgs)
	if err != nil {
		d.unbind(agentID, sess)
		return nil, err
	}
	if rpcErr != nil {
		return d.mapRPCError(rpcErr, agentID, sess, in.Tool)
	}

	// close_session succeeded upstream: unbind locally.
	if in.Tool == "browser.close_session" {
		d.unbind(agentID, sess)
	}

	content, err := extractContent(resultRaw)
	if err != nil {
		return nil, err
	}
	wrapped := mcp.WrapUntrusted(content, "browser", map[string]string{
		"resource": req.Resource,
		"tool":     in.Tool,
	})
	return &drivers.Response{
		StatusCode: http.StatusOK,
		Body: &CallResult{
			Tool:      in.Tool,
			Content:   wrapped,
			SessionID: sess.sessionID,
		},
	}, nil
}

// acquireSession returns a live session for (agent, task), creating
// one when needed. Caller holds the agent lock.
func (d *Driver) acquireSession(ctx context.Context, req *drivers.Request, pol *Policy, agentID, taskID string) (*session, error) {
	now := d.now()
	live, stale := d.partitionSessions(agentID, now)

	// Stale bindings: unbind already happened in partition; close the
	// remote sessions best-effort (the service sweeper is the backstop).
	if len(stale) > 0 {
		ctrl := newServiceClient(ctx, pol.ServiceURL, d.internalToken, d.httpClient)
		for _, s := range stale {
			_ = ctrl.closeSession(s.sessionID)
		}
	}

	// Reuse rule: an explicit task must match the bound task; an
	// agent that doesn't name a task reuses its oldest live session.
	for _, s := range live {
		if taskID == "" || s.taskID == taskID {
			return s, nil
		}
	}
	if len(live) >= pol.MaxSessionsPerAgent {
		return nil, drivers.Denied("session_limit")
	}

	binding := map[string]any{
		"agent_id":    agentID,
		"task_id":     taskID,
		"resource_id": req.Resource,
		"policy": map[string]any{
			"deny_hosts":           pol.DenyHosts,
			"max_pages":            pol.MaxPages,
			"max_screenshot_bytes": pol.MaxScreenshotBytes,
			"idle_timeout_s":       pol.IdleTimeoutS,
			"max_session_minutes":  pol.MaxSessionMinutes,
		},
	}
	ctrl := newServiceClient(ctx, pol.ServiceURL, d.internalToken, d.httpClient)
	sr, err := ctrl.createSession(binding)
	if err != nil {
		if errors.Is(err, ErrBrowserBusy) {
			return nil, drivers.Denied("browser_busy")
		}
		return nil, err
	}
	sess := &session{
		sessionID: sr.SessionID,
		token:     sr.Token,
		taskID:    taskID,
		expiresAt: sr.ExpiresAt,
	}
	d.mu.Lock()
	d.sessions[agentID] = append(d.sessions[agentID], sess)
	d.mu.Unlock()
	return sess, nil
}

// partitionSessions splits the agent's registry entries into live and
// stale (expired) sessions, unbinding the stale ones atomically. The
// caller closes the stale remote sessions (best effort).
func (d *Driver) partitionSessions(agentID string, now time.Time) (live, stale []*session) {
	d.mu.Lock()
	defer d.mu.Unlock()
	kept := make([]*session, 0, len(d.sessions[agentID]))
	for _, s := range d.sessions[agentID] {
		if s.expired(now) {
			stale = append(stale, s)
		} else {
			kept = append(kept, s)
			live = append(live, s)
		}
	}
	d.sessions[agentID] = kept
	return live, stale
}

// mapRPCError converts an in-band JSON-RPC protocol error into a
// client-safe denial per protocol §3.
func (d *Driver) mapRPCError(rpcErr *rpcError, agentID string, sess *session, tool string) (*drivers.Response, error) {
	switch rpcErr.Code {
	case ErrCodeSessionInvalid:
		d.unbind(agentID, sess)
		return nil, drivers.Denied("session_invalid")
	case ErrCodeCeilingExceeded:
		return nil, drivers.Denied("ceiling_exceeded")
	case ErrCodeBrowserBusy:
		return nil, drivers.Denied("browser_busy")
	default:
		// Unknown protocol error: surface as a tool error (the call
		// reached the tool layer), mirroring the mcp driver posture.
		return &drivers.Response{
			StatusCode: http.StatusOK,
			Body: &CallResult{
				Tool:    tool,
				IsError: true,
				Content: sanitizeRPCMessage(rpcErr.Message),
			},
		}, nil
	}
}

// unbind removes a session from the agent's registry. It does NOT
// close the remote session (callers decide; the sweeper backstops).
func (d *Driver) unbind(agentID string, s *session) {
	d.mu.Lock()
	defer d.mu.Unlock()
	list := d.sessions[agentID]
	for i, e := range list {
		if e == s {
			d.sessions[agentID] = append(list[:i], list[i+1:]...)
			break
		}
	}
}

// lockAgent serializes operations for one agent. Locks are refcounted
// so the map does not grow unboundedly. Caller must call unlock.
func (d *Driver) lockAgent(agentID string) func() {
	d.mu.Lock()
	lk, ok := d.agentLocks[agentID]
	if !ok {
		lk = &sync.Mutex{}
		d.agentLocks[agentID] = lk
	}
	d.agentLockRefs[agentID]++
	d.mu.Unlock()
	lk.Lock()
	return func() {
		lk.Unlock()
		d.mu.Lock()
		d.agentLockRefs[agentID]--
		if d.agentLockRefs[agentID] == 0 {
			delete(d.agentLocks, agentID)
			delete(d.agentLockRefs, agentID)
		}
		d.mu.Unlock()
	}
}

// extractContent renders a tools/call result's content blocks into
// text (text blocks verbatim, others as compact JSON — same posture
// as the mcp driver).
func extractContent(raw json.RawMessage) (string, error) {
	var res struct {
		Content []json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", fmt.Errorf("browser_failed: unparseable tools/call result")
	}
	var sb strings.Builder
	for i, blockRaw := range res.Content {
		if i > 0 {
			sb.WriteString("\n")
		}
		var block struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(blockRaw, &block); err != nil {
			return "", fmt.Errorf("browser_failed: unparseable content block")
		}
		if block.Type == "text" {
			sb.WriteString(block.Text)
			continue
		}
		sb.WriteString("[" + block.Type + " content: " + string(blockRaw) + "]")
	}
	return sb.String(), nil
}

// sanitizeRPCMessage keeps unknown JSON-RPC error messages client-safe.
func sanitizeRPCMessage(msg string) string {
	msg = strings.Map(func(r rune) rune {
		if r < 0x20 {
			return ' '
		}
		return r
	}, msg)
	const maxLen = 512
	if len(msg) > maxLen {
		msg = msg[:maxLen] + "…"
	}
	return msg
}

// mcpEndpoint normalizes the service base URL to the streamable-HTTP
// MCP endpoint (append /mcp when absent).
func mcpEndpoint(base string) (string, error) {
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("invalid browser service base_url")
	}
	p := strings.TrimRight(u.Path, "/")
	if !strings.HasSuffix(p, "/mcp") {
		p = p + "/mcp"
	}
	u.Path = p
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

// compile-time interface check
var _ drivers.Driver = (*Driver)(nil)
