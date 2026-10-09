package ssh

// apply.go — TG-11 slice C: gateway `ssh_apply` / `ssh_apply_status`
// driver (docs/tool-gateway.md §6.7). Governed mutation lane: the agent
// applies a MERGED, fully-pinned playbook revision through the
// terminal-service apply engine. Mirrors the TG-10 ssh driver structure
// (ceiling fold → checks → confirmation gate → audit-before-execute →
// terminal-service call), with two differences:
//
//  1. The confirmation gate is UNCONDITIONAL for applies. Identity:
//
//       tool     = "ssh_apply#<host_group>#<playbook>"
//       argsHash = ArgsHash(resource, "ssh_apply", canonical{host_group, playbook})
//
//     so tier-driven approvers (owner / owner+admin) approve exactly
//     that (group, playbook) pair; "approve once" pins the identical
//     retry via args_hash.
//
//  2. The repo URL is NEVER taken from the tool call — it comes only
//     from the resource's endpoint_config.artifact (git_url /
//     default_branch / playbooks_path). The payload cannot redirect the
//     executor.
//
// Async semantics: POST /v1/applies starts the job; the gateway waits
// up to min(timeout_seconds, 900s) (default 120s) polling
// GET /v1/applies/{id}. On expiry it returns the apply_id with a
// continuation hint (ssh_apply_status) instead of pinning a dispatch
// slot for a long playbook run.

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"github.com/rossbrigoli/skquad/tool-gateway/internal/audit"
	"github.com/rossbrigoli/skquad/tool-gateway/internal/confirmation"
	"github.com/rossbrigoli/skquad/tool-gateway/internal/drivers"
)

// auditEvent builds a gate-level audit event (the confirm gate runs
// before we hold enough flow state for emitAudit's full shape).
func auditEvent(resourceID, agentID, operation, detail string) audit.Event {
	return audit.Event{
		Timestamp: time.Now(), AgentID: agentID, Resource: resourceID,
		Operation: operation, Decision: audit.DecisionAllow, StatusCode: 200,
		Detail: detail,
	}
}

// Apply wait bounds (gateway-side; §6.7 async job API).
const (
	DefaultApplyWaitSeconds = 120
	MaxApplyWaitSeconds     = 900
)

// applyPollInterval is how often the gateway polls the terminal-service
// for apply completion while waiting. A var so tests can shrink it.
var applyPollInterval = 1 * time.Second

// fullGitRev is the ONLY accepted git_rev form: a full 40-char lowercase
// hex SHA. Branch names and short SHAs are rejected — mutable or
// ambiguous refs break the merge-approval binding (§6.7).
var fullGitRev = regexp.MustCompile(`^[0-9a-f]{40}$`)

// applyPayload is the agent-facing ssh_apply argument set. NOTE: there
// is deliberately no git_url/default_branch/playbooks_path field — the
// repo identity is resource-owned, not call-owned.
type applyPayload struct {
	Playbook       string `json:"playbook"`
	GitRev         string `json:"git_rev"`
	HostGroup      string `json:"host_group"`
	CheckOnly      bool   `json:"check_only"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}

// artifactConfig is the endpoint_config.artifact section (§6.7).
type artifactConfig struct {
	GitURL        string `json:"git_url"`
	DefaultBranch string `json:"default_branch"`
	PlaybooksPath string `json:"playbooks_path"`
}

// applyCeiling is the apply-relevant slice of the ssh ceiling.
type applyCeiling struct {
	HostGroups map[string]struct {
		Hosts []string `json:"hosts"`
		Tier  string   `json:"tier"`
	} `json:"host_groups"`
	MirrorsAllow []string        `json:"mirrors_allow"`
	RequireTip   map[string]bool `json:"require_tip"`
}

// applyConstraints is the apply-relevant slice of the grant constraints.
// Pointer/nil distinctions matter: unset ≠ empty for the allow lists,
// and CheckOnly *true* marks a dry-run-only grant.
type applyConstraints struct {
	ApplyEnabled    *bool    `json:"apply_enabled"`
	PlaybooksAllow  []string `json:"playbooks_allow"`
	HostGroupsAllow []string `json:"host_groups_allow"`
	CheckOnly       *bool    `json:"check_only"`
}

func (d *Driver) apply(ctx context.Context, req *drivers.Request) (*drivers.Response, error) {
	if d.terminalURL == "" || d.token == "" {
		return nil, drivers.Denied("terminal_service_unconfigured")
	}
	var p applyPayload
	if err := json.Unmarshal(req.Payload, &p); err != nil ||
		strings.TrimSpace(p.Playbook) == "" || p.GitRev == "" || strings.TrimSpace(p.HostGroup) == "" {
		return nil, drivers.Denied("bad_request")
	}
	if !fullGitRev.MatchString(p.GitRev) {
		return nil, drivers.Denied("invalid_git_rev")
	}
	// Playbook must be a relative, traversal-free path within
	// playbooks_path (the engine joins them; refuse escapes here too).
	pb := strings.TrimSpace(p.Playbook)
	if strings.HasPrefix(pb, "/") || strings.Contains(pb, "..") {
		return nil, drivers.Denied("invalid_playbook")
	}

	// Base TG-10 policy fold (cert TTL, deny patterns, host deny).
	pol, err := EffectivePolicy(req.Grant.Ceiling, req.Grant.Constraints)
	if err != nil {
		return nil, drivers.Denied("ceiling_exceeded")
	}

	// Apply capability: the grant must explicitly enable it. Discovery
	// hides the tools without it; this is the dispatch-time fail-closed
	// twin (a hand-crafted call must not sneak through).
	var con applyConstraints
	if len(req.Grant.Constraints) > 0 {
		if err := json.Unmarshal(req.Grant.Constraints, &con); err != nil {
			return nil, drivers.Denied("constraints_invalid")
		}
	}
	if con.ApplyEnabled == nil || !*con.ApplyEnabled {
		return nil, drivers.Denied("apply_not_permitted")
	}

	// Ceiling: host_groups / mirrors_allow / require_tip.
	var ce applyCeiling
	if len(req.Grant.Ceiling) > 0 {
		if err := json.Unmarshal(req.Grant.Ceiling, &ce); err != nil {
			return nil, drivers.Denied("ceiling_invalid")
		}
	}
	grp, ok := ce.HostGroups[p.HostGroup]
	if !ok || len(grp.Hosts) == 0 {
		return nil, drivers.Denied("unknown_host_group")
	}
	// host_groups_allow: least-privilege by group (glob on group name).
	if con.HostGroupsAllow != nil && !anyGlob(con.HostGroupsAllow, p.HostGroup) {
		return nil, drivers.Denied("host_group_denied")
	}
	// playbooks_allow: path globs within playbooks_path.
	if con.PlaybooksAllow != nil && !anyGlob(con.PlaybooksAllow, pb) {
		return nil, drivers.Denied("playbook_denied")
	}
	// check_only-only grants refuse real mutation.
	if con.CheckOnly != nil && *con.CheckOnly && !p.CheckOnly {
		return nil, drivers.Denied("check_only_grant")
	}
	// Defense in depth: every group host must still pass the TG-10 host
	// allow/deny fold (groups cannot widen reach past hosts_allow).
	for _, h := range grp.Hosts {
		if !pol.HostAllowed(h) {
			return nil, drivers.Denied("host_denied")
		}
	}

	// Artifact config: repo identity comes ONLY from the resource.
	art, err := parseArtifactConfig(req.Grant.Config)
	if err != nil {
		return nil, err
	}
	cfg, err := parseResourceConfig(req.Grant.Config)
	if err != nil {
		return nil, drivers.Denied("resource_config_invalid")
	}

	agentID := ""
	if req.Agent != nil {
		agentID = req.Agent.AgentID
	}

	// UNCONDITIONAL confirmation gate (TG-8 fold).
	if err := d.applyConfirmGate(ctx, req.Grant.ResourceID, agentID, p.HostGroup, pb, req.ConfirmationID); err != nil {
		return nil, err
	}

	// require_tip: ceiling require_tip[tier]; default true for high tier.
	tier := grp.Tier
	if tier == "" {
		tier = "medium"
	}
	requireTip := ce.RequireTip[tier]
	if ce.RequireTip == nil && tier == "high" {
		requireTip = true
	}

	wait := clampInt(p.TimeoutSeconds, DefaultApplyWaitSeconds, 1, MaxApplyWaitSeconds)

	d.emitAudit(req, agentID, "ssh_apply_attempt", map[string]any{
		"playbook": pb, "git_rev": p.GitRev, "host_group": p.HostGroup,
		"check_only": p.CheckOnly, "require_tip": requireTip,
	})

	body, err := d.buildApplyBody(req, p, pb, grp.Hosts, cfg, art, ce, pol, agentID, requireTip, wait)
	if err != nil {
		return nil, err
	}

	postCtx, cancelPost := context.WithTimeout(ctx, 20*time.Second)
	defer cancelPost()
	status, raw, err := d.terminalPOST(postCtx, "/v1/applies", body)
	if err != nil {
		d.emitAudit(req, agentID, "ssh_apply_result", map[string]any{"error": "terminal_unreachable"})
		return nil, drivers.Denied("terminal_unreachable")
	}
	if status < 200 || status >= 300 {
		reason := safeTerminalError(raw)
		d.emitAudit(req, agentID, "ssh_apply_result", map[string]any{"error": reason})
		return nil, drivers.Denied(reason)
	}
	var started struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(raw, &started); err != nil || started.ID == "" {
		return nil, drivers.Denied("terminal_response_invalid")
	}

	// Wait for completion up to `wait`; then hand off the apply_id.
	deadline := time.Now().Add(time.Duration(wait) * time.Second)
	for {
		getCtx, cancelGet := context.WithTimeout(ctx, 15*time.Second)
		st, sraw, err := d.terminalGET(getCtx, "/v1/applies/"+started.ID)
		cancelGet()
		if err != nil {
			d.emitAudit(req, agentID, "ssh_apply_result", map[string]any{"apply_id": started.ID, "error": "terminal_unreachable"})
			return nil, drivers.Denied("terminal_unreachable")
		}
		if st < 200 || st >= 300 {
			reason := safeTerminalError(sraw)
			d.emitAudit(req, agentID, "ssh_apply_result", map[string]any{"apply_id": started.ID, "error": reason})
			return nil, drivers.Denied(reason)
		}
		var out applyStatusOut
		if err := json.Unmarshal(sraw, &out); err != nil {
			return nil, drivers.Denied("terminal_response_invalid")
		}
		if isTerminalApplyStatus(out.Status) {
			d.emitAudit(req, agentID, "ssh_apply_result", map[string]any{
				"apply_id": started.ID, "status": out.Status,
				"exit_code": out.ExitCode, "recording_id": out.RecordingID,
			})
			return &drivers.Response{StatusCode: 200, Body: out.toBody(started.ID)}, nil
		}
		if time.Now().After(deadline) {
			d.emitAudit(req, agentID, "ssh_apply_pending", map[string]any{"apply_id": started.ID, "status": out.Status})
			return &drivers.Response{StatusCode: 200, Body: map[string]any{
				"apply_id":     started.ID,
				"status":       out.Status,
				"continuation": "ssh_apply_status",
				"hint":         "Apply still running past the wait window. Poll ssh_apply_status(apply_id) for the result.",
			}}, nil
		}
		select {
		case <-ctx.Done():
			return nil, drivers.Denied("terminal_unreachable")
		case <-time.After(applyPollInterval):
		}
	}
}

func (d *Driver) applyStatus(ctx context.Context, req *drivers.Request) (*drivers.Response, error) {
	if d.terminalURL == "" || d.token == "" {
		return nil, drivers.Denied("terminal_service_unconfigured")
	}
	var p struct {
		ApplyID string `json:"apply_id"`
	}
	if err := json.Unmarshal(req.Payload, &p); err != nil || strings.TrimSpace(p.ApplyID) == "" {
		return nil, drivers.Denied("bad_request")
	}
	agentID := ""
	if req.Agent != nil {
		agentID = req.Agent.AgentID
	}
	callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	status, raw, err := d.terminalGET(callCtx, "/v1/applies/"+strings.TrimSpace(p.ApplyID))
	if err != nil {
		return nil, drivers.Denied("terminal_unreachable")
	}
	if status == 404 {
		return nil, drivers.Denied("apply_not_found")
	}
	if status < 200 || status >= 300 {
		return nil, drivers.Denied(safeTerminalError(raw))
	}
	var out applyStatusOut
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, drivers.Denied("terminal_response_invalid")
	}
	d.emitAudit(req, agentID, "ssh_apply_status", map[string]any{"apply_id": p.ApplyID, "status": out.Status})
	return &drivers.Response{StatusCode: 200, Body: out.toBody(p.ApplyID)}, nil
}

// applyStatusOut normalizes the terminal-service GET /v1/applies/{id}
// response.
type applyStatusOut struct {
	Status        string          `json:"status"`
	StartedAt     *string         `json:"started_at"`
	FinishedAt    *string         `json:"finished_at"`
	ExitCode      *int            `json:"exit_code"`
	RefusalReason string          `json:"refusal_reason"`
	Error         string          `json:"error"`
	RecordingID   string          `json:"recording_id"`
	PerHost       json.RawMessage `json:"per_host"`
	LintFindings  json.RawMessage `json:"lint_findings"`
}

func (o applyStatusOut) toBody(applyID string) map[string]any {
	body := map[string]any{"apply_id": applyID, "status": o.Status}
	if o.StartedAt != nil {
		body["started_at"] = *o.StartedAt
	}
	if o.FinishedAt != nil {
		body["finished_at"] = *o.FinishedAt
	}
	if o.ExitCode != nil {
		body["exit_code"] = *o.ExitCode
	}
	if o.RefusalReason != "" {
		body["refusal_reason"] = o.RefusalReason
	}
	if o.Error != "" {
		body["error"] = o.Error
	}
	if o.RecordingID != "" {
		body["recording_id"] = o.RecordingID
	}
	if len(o.PerHost) > 0 {
		body["per_host"] = o.PerHost
	}
	if len(o.LintFindings) > 0 {
		body["lint_findings"] = o.LintFindings
	}
	return body
}

func isTerminalApplyStatus(s string) bool {
	switch s {
	case "succeeded", "failed", "refused":
		return true
	}
	return false
}

// buildApplyBody assembles the terminal-service POST /v1/applies body.
// Credential wiring mirrors buildExecBody: BYO static key ONLY for
// auth_mode=static_key, fail closed on resolution failure.
func (d *Driver) buildApplyBody(req *drivers.Request, p applyPayload, playbook string, hosts []string, cfg resourceConfig, art artifactConfig, ce applyCeiling, pol *Policy, agentID string, requireTip bool, wait int) ([]byte, error) {
	body := map[string]any{
		"resource_id":      req.Grant.ResourceID,
		"agent_id":         agentID,
		"playbook":         playbook,
		"git_rev":          p.GitRev,
		"host_group":       p.HostGroup,
		"hosts":            hosts,
		"check_only":       p.CheckOnly,
		"require_tip":      requireTip,
		"git_url":          art.GitURL,
		"default_branch":   art.DefaultBranch,
		"ssh_user":         cfg.SSHUser,
		"known_hosts":      cfg.KnownHosts,
		"cert_ttl_seconds": pol.CertTTLMinutes * 60,
		"mirrors_allow":    ce.MirrorsAllow,
		"deny_patterns":    pol.CommandDeny,
		"timeout_seconds":  wait,
	}
	if art.PlaybooksPath != "" {
		body["playbooks_path"] = art.PlaybooksPath
	}
	if cfg.AuthMode == "static_key" {
		if d.creds == nil {
			return nil, drivers.Denied("credentials_unavailable")
		}
		secret, err := d.creds.Resolve(context.Background(), req.Grant.ResourceID, agentID)
		if err != nil || secret == nil || secret.Fields["private_key_pem"] == "" {
			return nil, drivers.Denied("credentials_unavailable")
		}
		body["static_key_pem"] = secret.Fields["private_key_pem"]
	}
	return json.Marshal(body)
}

// applyConfirmGate runs the unconditional TG-8 gate for applies.
// Proceeds only on an explicit positive answer.
func (d *Driver) applyConfirmGate(ctx context.Context, resourceID, agentID, hostGroup, playbook, confID string) error {
	if d.conf == nil {
		return drivers.Denied("confirmation_unavailable")
	}
	canonical, err := json.Marshal(map[string]string{"host_group": hostGroup, "playbook": playbook})
	if err != nil {
		return drivers.Denied("confirmation_failed")
	}
	argsHash := confirmation.ArgsHash(resourceID, "ssh_apply", canonical)
	tool := "ssh_apply#" + hostGroup + "#" + playbook

	if confID != "" {
		res, err := d.conf.Consume(ctx, confID, argsHash)
		if err != nil {
			return drivers.Denied("confirmation_unavailable")
		}
		if !res.Allowed {
			return drivers.Denied("confirmation_denied")
		}
		if res.Mode == "standing" && res.MatchedStandingGrantID != "" {
			d.audit.Emit(auditEvent(resourceID, agentID, "ssh_apply", `{"gate":"standing","standing_grant_id":"`+res.MatchedStandingGrantID+`"}`))
		}
		return nil
	}
	res, err := d.conf.Check(ctx, resourceID, agentID, tool, argsHash)
	if err != nil {
		return drivers.Denied("confirmation_unavailable")
	}
	switch res.Mode {
	case "auto":
		d.audit.Emit(auditEvent(resourceID, agentID, "ssh_apply", `{"gate":"standing","standing_grant_id":"`+res.MatchedStandingGrantID+`"}`))
		return nil
	case "pending":
		return drivers.Denied("confirmation_required:" + res.ConfirmationID)
	default:
		return drivers.Denied("confirmation_unavailable")
	}
}

func parseArtifactConfig(raw json.RawMessage) (artifactConfig, error) {
	var art artifactConfig
	if len(raw) == 0 {
		return art, drivers.Denied("artifact_not_configured")
	}
	var obj struct {
		Artifact *artifactConfig `json:"artifact"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil || obj.Artifact == nil || strings.TrimSpace(obj.Artifact.GitURL) == "" {
		return art, drivers.Denied("artifact_not_configured")
	}
	art = *obj.Artifact
	if art.DefaultBranch == "" {
		art.DefaultBranch = "main"
	}
	return art, nil
}

// anyGlob reports whether s matches at least one of the (anchored,
// case-insensitive) globs.
func anyGlob(patterns []string, s string) bool {
	for _, pat := range patterns {
		if globMatch(pat, s) {
			return true
		}
	}
	return false
}
