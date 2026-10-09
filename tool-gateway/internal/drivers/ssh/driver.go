package ssh

// driver.go — TG-10 gateway ssh driver: governed SSH brokering.
//
// Flow (ssh_exec):
//
//	ceiling fold → host check → command policy →
//	  [deny-match → pattern-scoped confirmation gate (TG-8)] →
//	  AUDIT-BEFORE-EXECUTE ("ssh_exec_attempt") →
//	  terminal-service /v1/exec → AUDIT ("ssh_exec_result")
//
// The confirmation gate is CONDITIONAL (only deny-pattern hits are
// gated) and uses a pattern-scoped tool identity:
//
//	tool     = "ssh_exec#<host>#deny:<pattern>"
//	argsHash = ArgsHash(resource, "ssh_exec", canonical{host,command})
//
// so "Approve This and Future" creates a standing grant scoped to
// exactly that pattern+host (CP standing grants match on
// resource+agent+tool), while "approve once" pins the exact command
// via args_hash: an approved-once confirmation is consumed only by a
// byte-identical {host,command} retry.
//
// Sessions (ssh_session_*) broker terminal-service PTY sessions 1:1
// per agent+resource. The gateway owns session ids; agents can only
// touch their own sessions.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/rossbrigoli/skquad/tool-gateway/internal/audit"
	"github.com/rossbrigoli/skquad/tool-gateway/internal/confirmation"
	"github.com/rossbrigoli/skquad/tool-gateway/internal/credentials"
	"github.com/rossbrigoli/skquad/tool-gateway/internal/drivers"
)

// ConfirmationClient is the TG-8 confirmation surface the deny gate needs.
type ConfirmationClient interface {
	Check(ctx context.Context, resourceID, agentID, tool, argsHash string) (*confirmation.CheckResult, error)
	Consume(ctx context.Context, id, argsHash string) (*confirmation.ConsumeResult, error)
}

// Driver is the ssh driver.
type Driver struct {
	terminalURL string
	token       string
	audit       audit.Emitter
	conf        ConfirmationClient
	creds       credentials.Resolver
	http        *http.Client
	sessions    *sessionRegistry
}

// New builds the driver. All deps required; missing deps → driver
// fails closed on every call (never unauthenticated, never unaudited).
func New(terminalURL, token string, em audit.Emitter, conf ConfirmationClient, creds credentials.Resolver) *Driver {
	return &Driver{
		terminalURL: strings.TrimRight(terminalURL, "/"),
		token:       token,
		audit:       em,
		conf:        conf,
		creds:       creds,
		http:        &http.Client{},
		sessions:    newSessionRegistry(),
	}
}

// Name implements drivers.Driver.
func (d *Driver) Name() string { return "ssh" }

// Handle dispatches ssh operations.
func (d *Driver) Handle(ctx context.Context, req *drivers.Request) (*drivers.Response, error) {
	switch req.Operation {
	case "ssh_exec":
		return d.exec(ctx, req)
	case "ssh_session_open":
		return d.sessionOpen(ctx, req)
	case "ssh_session_send":
		return d.sessionSend(ctx, req)
	case "ssh_session_events":
		return d.sessionEvents(ctx, req)
	case "ssh_session_close":
		return d.sessionClose(ctx, req)
	case "ssh_apply":
		return d.apply(ctx, req)
	case "ssh_apply_status":
		return d.applyStatus(ctx, req)
	default:
		return nil, drivers.Denied("unknown_operation")
	}
}

type execPayload struct {
	ResourceID     string `json:"resource_id"`
	Host           string `json:"host"`
	Command        string `json:"command"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}

type resourceConfig struct {
	SSHUser    string `json:"ssh_user"`
	Port       int    `json:"port"`
	KnownHosts string `json:"known_hosts"`
	AuthMode   string `json:"auth_mode"`
}

func (d *Driver) exec(ctx context.Context, req *drivers.Request) (*drivers.Response, error) {
	if d.terminalURL == "" || d.token == "" {
		return nil, drivers.Denied("terminal_service_unconfigured")
	}
	var p execPayload
	if err := json.Unmarshal(req.Payload, &p); err != nil || p.Host == "" || p.Command == "" {
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
	gated := false
	switch verdict, pattern := pol.CheckCommand(p.Command); verdict {
	case CommandDeny:
		return nil, drivers.Denied("command_denied")
	case CommandGate:
		gated = true
		if err := d.confirmGate(ctx, req.Grant.ResourceID, agentID, p.Host, p.Command, pattern, req.ConfirmationID); err != nil {
			return nil, err
		}
	case CommandPass:
	}

	cfg, err := parseResourceConfig(req.Grant.Config)
	if err != nil {
		return nil, drivers.Denied("resource_config_invalid")
	}

	// AUDIT-BEFORE-EXECUTE: the attempt event is emitted (attempted,
	// best-effort) BEFORE any terminal-service call exists. A crash
	// mid-call still leaves the attempt on record.
	d.emitAudit(req, agentID, "ssh_exec_attempt", map[string]any{
		"host": p.Host, "user": cfg.SSHUser, "command": p.Command, "gated": gated,
	})

	body, err := d.buildExecBody(req, p, cfg, pol, agentID)
	if err != nil {
		return nil, err
	}

	timeout := time.Duration(pol.ExecTimeoutSeconds) * time.Second
	callCtx, cancel := context.WithTimeout(ctx, timeout+15*time.Second)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(callCtx, http.MethodPost, d.terminalURL+"/v1/exec", bytes.NewReader(body))
	if err != nil {
		return nil, drivers.Denied("terminal_request_failed")
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+d.token)

	resp, err := d.http.Do(httpReq)
	if err != nil {
		d.emitAudit(req, agentID, "ssh_exec_result", map[string]any{"host": p.Host, "error": "terminal_unreachable"})
		return nil, drivers.Denied("terminal_unreachable")
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		reason := safeTerminalError(raw)
		d.emitAudit(req, agentID, "ssh_exec_result", map[string]any{"host": p.Host, "error": reason})
		return nil, drivers.Denied(reason)
	}
	var out struct {
		Stdout      string `json:"stdout"`
		Stderr      string `json:"stderr"`
		ExitCode    int    `json:"exit_code"`
		RecordingID string `json:"recording_id"`
		DurationMS  int64  `json:"duration_ms"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, drivers.Denied("terminal_response_invalid")
	}
	d.emitAudit(req, agentID, "ssh_exec_result", map[string]any{
		"host": p.Host, "exit_code": out.ExitCode, "recording_id": out.RecordingID,
	})
	return &drivers.Response{StatusCode: 200, Body: map[string]any{
		"stdout": out.Stdout, "stderr": out.Stderr, "exit_code": out.ExitCode,
		"recording_id": out.RecordingID, "duration_ms": out.DurationMS,
	}}, nil
}

// buildExecBody assembles the terminal-service request, resolving BYO
// static-key credentials ONLY for auth_mode=static_key (fail closed on
// resolution failure; never silently downgrade to ca).
func (d *Driver) buildExecBody(req *drivers.Request, p execPayload, cfg resourceConfig, pol *Policy, agentID string) ([]byte, error) {
	auth := map[string]any{
		"mode":             "ca",
		"cert_ttl_minutes": pol.CertTTLMinutes,
		"known_hosts":      cfg.KnownHosts,
	}
	if cfg.AuthMode == "static_key" {
		if d.creds == nil {
			return nil, drivers.Denied("credentials_unavailable")
		}
		secret, err := d.creds.Resolve(context.Background(), req.Grant.ResourceID, agentID)
		if err != nil || secret == nil || secret.Fields["private_key_pem"] == "" {
			return nil, drivers.Denied("credentials_unavailable")
		}
		auth["mode"] = "static_key"
		auth["private_key_pem"] = secret.Fields["private_key_pem"]
	}
	body := map[string]any{
		"resource_id": req.Grant.ResourceID,
		"agent_id":    agentID,
		"task_id":     taskID(req),
		"host":        p.Host,
		"user":        cfg.SSHUser,
		"command":     p.Command,
		"auth":        auth,
	}
	if p.TimeoutSeconds > 0 {
		body["timeout_seconds"] = p.TimeoutSeconds
	}
	if cfg.Port > 0 {
		body["port"] = cfg.Port
	}
	return json.Marshal(body)
}

// confirmGate runs the pattern-scoped TG-8 gate. Proceeds only on an
// explicit positive answer; everything else denies WITHOUT executing.
func (d *Driver) confirmGate(ctx context.Context, resourceID, agentID, host, command, pattern, confID string) error {
	if d.conf == nil {
		return drivers.Denied("confirmation_unavailable")
	}
	canonical, err := json.Marshal(map[string]string{"host": host, "command": command})
	if err != nil {
		return drivers.Denied("confirmation_failed")
	}
	argsHash := confirmation.ArgsHash(resourceID, "ssh_exec", canonical)
	tool := "ssh_exec#" + host + "#deny:" + pattern

	// Retry-after-approval: if the agent presents a confirmation id
	// (X-Skquad-Confirmation-Id header, surfaced on Request), Consume
	// it directly — this is the approved-once/standing path. The hash
	// pins the exact {host,command}.
	if confID != "" {
		res, err := d.conf.Consume(ctx, confID, argsHash)
		if err != nil {
			return drivers.Denied("confirmation_unavailable")
		}
		if !res.Allowed {
			return drivers.Denied("confirmation_denied")
		}
		if res.Mode == "standing" && res.MatchedStandingGrantID != "" {
			d.audit.Emit(audit.Event{
				Timestamp: time.Now(), AgentID: agentID, Resource: resourceID,
				Operation: "ssh_exec", Decision: audit.DecisionAllow, StatusCode: 200,
				Detail: `{"gate":"standing","standing_grant_id":"` + res.MatchedStandingGrantID + `"}`,
			})
		}
		return nil
	}

	res, err := d.conf.Check(ctx, resourceID, agentID, tool, argsHash)
	if err != nil {
		return drivers.Denied("confirmation_unavailable")
	}
	switch res.Mode {
	case "auto":
		d.audit.Emit(audit.Event{
			Timestamp: time.Now(), AgentID: agentID, Resource: resourceID,
			Operation: "ssh_exec", Decision: audit.DecisionAllow, StatusCode: 200,
			Detail: `{"gate":"standing","standing_grant_id":"` + res.MatchedStandingGrantID + `"}`,
		})
		return nil
	case "pending":
		return drivers.Denied("confirmation_required:" + res.ConfirmationID)
	default:
		return drivers.Denied("confirmation_unavailable")
	}
}

func parseResourceConfig(raw json.RawMessage) (resourceConfig, error) {
	var cfg resourceConfig
	if len(raw) == 0 {
		return cfg, errors.New("missing config")
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return cfg, err
	}
	if cfg.SSHUser == "" {
		return cfg, errors.New("ssh_user required")
	}
	if strings.TrimSpace(cfg.KnownHosts) == "" {
		return cfg, errors.New("known_hosts required")
	}
	if cfg.AuthMode == "" {
		cfg.AuthMode = "ca"
	}
	return cfg, nil
}

func (d *Driver) emitAudit(req *drivers.Request, agentID, action string, detail map[string]any) {
	if d.audit == nil {
		return
	}
	b, err := json.Marshal(detail)
	if err != nil {
		b = []byte("{}")
	}
	d.audit.Emit(audit.Event{
		Timestamp: time.Now(),
		AgentID:   agentID,
		Resource:  req.Grant.ResourceID,
		Operation: action,
		Decision:  audit.DecisionAllow,
		Detail:    string(b),
	})
}

func safeTerminalError(raw []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(raw, &e)
	known := map[string]bool{
		"host_verification_failed": true, "host_verification_required": true,
		"ca_unavailable": true, "session_limit": true, "invalid_private_key": true,
	}
	if known[e.Error] {
		return e.Error
	}
	return "terminal_error"
}

func taskID(req *drivers.Request) string {
	return fmt.Sprintf("ssh-%d", time.Now().UnixMilli())
}
