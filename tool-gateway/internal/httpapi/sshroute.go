package httpapi

// MIG-6 (S-312): agent-facing HTTP route for the TG-10 ssh driver.
//
// The ssh driver (internal/drivers/ssh) shipped with TG-10 M1 but had no
// route: agents could discover ssh_exec via fetch-at-wake yet had no
// governed path to dispatch it. This closes the loop:
//
//	POST /v1/ssh/{resourceID}   body = the tool arguments JSON:
//	  {"tool":"ssh_exec","host":"...","command":"...","timeout_seconds":30}
//
// Pipeline mirrors the rest/mcp handlers: kill switch → authn → body
// cap → grant lookup (typed ssh resource, id-addressed) → driver
// dispatch → audit. The ssh tool name is carried in the body's "tool"
// field and must be one of the fixed ssh tool-family names; anything
// else is rejected 400 before the driver sees it.
//
// Confirmation semantics differ from rest/mcp/git HERE, deliberately:
// the ssh gate is CONDITIONAL (only deny-pattern hits are gated) and is
// owned by the driver itself (pattern-scoped tool identity,
// docs/tool-gateway.md §6.6). Running the generic confirmationGate on
// top would double-hash the payload under a different identity and
// break the driver's Consume binding. So this route does NOT call
// confirmationGate; it forwards X-Skquad-Confirmation-Id to the
// driver via Request.ConfirmationID, which is the approved-once /
// standing retry path the driver's gate consumes.
//
// A driver denial of the form "confirmation_required:<id>" maps to
// 202 pending_confirmation carrying the id the agent retries with —
// the same contract the generic gate publishes.

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/rossbrigoli/skquad/tool-gateway/internal/audit"
	"github.com/rossbrigoli/skquad/tool-gateway/internal/auth"
	"github.com/rossbrigoli/skquad/tool-gateway/internal/drivers"
	"github.com/rossbrigoli/skquad/tool-gateway/internal/policy"
)

// sshToolFamily is the closed set of operations routable to the ssh
// driver. Mirrors the driver's Handle switch; unknown names never reach
// the driver.
var sshToolFamily = map[string]bool{
	"ssh_exec":           true,
	"ssh_session_open":   true,
	"ssh_session_send":   true,
	"ssh_session_events": true,
	"ssh_session_close":  true,
	"ssh_apply":          true,
	"ssh_apply_status":   true,
}

// handleSSHCall runs the governed pipeline for one ssh tool dispatch.
func (s *Server) handleSSHCall(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	reqID := newRequestID()
	resourceID := strings.TrimSpace(r.PathValue("resourceID"))
	resource := "ssh:" + resourceID
	operation := ""
	agentID := ""

	emit := func(decision string, code int, detail string) {
		s.deps.Audit.Emit(audit.Event{
			Timestamp:  time.Now(),
			RequestID:  reqID,
			AgentID:    agentID,
			Resource:   resource,
			Operation:  orDefault(operation, "ssh_call"),
			Decision:   decision,
			StatusCode: code,
			LatencyMS:  time.Since(start).Milliseconds(),
			Detail:     detail,
		})
	}

	if resourceID == "" || strings.ContainsAny(resourceID, "/\\") || resourceID == "." || resourceID == ".." {
		emit(audit.DecisionDeny, http.StatusBadRequest, "bad_resource_id")
		writeError(w, http.StatusBadRequest, "bad_request", "invalid resource id")
		return
	}

	// 0. Kill switch.
	if s.deps.Enabled != nil && !s.deps.Enabled.Load() {
		emit(audit.DecisionDeny, http.StatusServiceUnavailable, "kill_switch")
		writeError(w, http.StatusServiceUnavailable, "gateway_disabled", "tool gateway is disabled by kill switch")
		return
	}

	// 1. Authn.
	principal, err := auth.Middleware(r.Context(), s.deps.Policy,
		r.Header.Get("X-Skquad-Agent-ID"), r.Header.Get("Authorization"))
	if err != nil {
		switch {
		case errors.Is(err, policy.ErrPolicyUnavailable):
			emit(audit.DecisionDeny, http.StatusBadGateway, "policy_unavailable")
			writeError(w, http.StatusBadGateway, "policy_unavailable", "policy could not be resolved; failing closed")
		default:
			emit(audit.DecisionDeny, http.StatusUnauthorized, err.Error())
			writeError(w, http.StatusUnauthorized, "unauthorized", err.Error())
		}
		return
	}
	agentID = principal.AgentID

	// 2. Read + cap body.
	body, err := io.ReadAll(io.LimitReader(r.Body, s.deps.MaxBodyBytes+1))
	if err != nil {
		emit(audit.DecisionError, http.StatusBadRequest, "body read error")
		writeError(w, http.StatusBadRequest, "bad_request", "failed reading request body")
		return
	}
	if int64(len(body)) > s.deps.MaxBodyBytes {
		emit(audit.DecisionDeny, http.StatusRequestEntityTooLarge, "payload too large")
		writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "request body exceeds limit")
		return
	}

	// 3. Tool extraction BEFORE dispatch: denied calls must still audit
	// what was attempted. Strict membership check — the closed family.
	var in struct {
		Tool string `json:"tool"`
	}
	if err := json.Unmarshal(body, &in); err != nil || !sshToolFamily[in.Tool] {
		emit(audit.DecisionDeny, http.StatusBadRequest, "unknown_ssh_tool")
		writeError(w, http.StatusBadRequest, "bad_request", "body must be JSON with a known ssh tool name")
		return
	}
	operation = in.Tool

	// 4. Grant lookup: the agent must hold a live grant to THIS ssh
	// resource (id-addressed, same contract as rest/mcp).
	var grant *policy.Grant
	if principal.Snapshot != nil {
		for i := range principal.Snapshot.Grants {
			g := principal.Snapshot.Grants[i]
			if g.ResourceType == "ssh" && g.ResourceID == resourceID {
				grant = &principal.Snapshot.Grants[i]
				break
			}
		}
	}
	if grant == nil {
		emit(audit.DecisionDeny, http.StatusForbidden, "no_ssh_grant")
		writeError(w, http.StatusForbidden, "no_grant", "agent has no grant to this ssh resource")
		return
	}

	// 5. Driver dispatch. The ssh driver owns the conditional
	// deny-pattern confirmation gate (§6.6); the confirmation retry
	// header is forwarded, not evaluated here.
	drv, ok := s.deps.Drivers["ssh"]
	if !ok {
		emit(audit.DecisionError, http.StatusNotFound, "no ssh driver")
		writeError(w, http.StatusNotFound, "not_found", "no ssh driver registered")
		return
	}
	resp, err := drv.Handle(r.Context(), &drivers.Request{
		Agent:          principal,
		Resource:       grant.ResourceID,
		Operation:      operation,
		Payload:        body,
		Grant:          grant,
		ConfirmationID: strings.TrimSpace(r.Header.Get(ConfirmationIDHeader)),
	})
	if err != nil {
		var denied *drivers.DeniedError
		if errors.As(err, &denied) {
			if confID, isPending := strings.CutPrefix(denied.Reason, "confirmation_required:"); isPending && confID != "" {
				emit(audit.DecisionDeny, http.StatusAccepted, "pending_confirmation:"+confID)
				writeJSON(w, http.StatusAccepted, map[string]any{
					"error":           map[string]any{"code": "pending_confirmation", "message": "owner approval required for this command"},
					"confirmation_id": confID,
					"retry_with":      ConfirmationIDHeader,
				})
				return
			}
			emit(audit.DecisionDeny, http.StatusForbidden, denied.Reason)
			writeError(w, http.StatusForbidden, "denied", denied.Reason)
			return
		}
		if errors.Is(err, drivers.ErrDenied) {
			emit(audit.DecisionDeny, http.StatusForbidden, "denied")
			writeError(w, http.StatusForbidden, "denied", "blocked by policy")
			return
		}
		emit(audit.DecisionError, http.StatusBadGateway, err.Error())
		writeError(w, http.StatusBadGateway, "ssh_failed", "ssh call failed (blocked by policy or unreachable target)")
		return
	}

	code := resp.StatusCode
	if code == 0 {
		code = http.StatusOK
	}
	emit(audit.DecisionAllow, code, "")
	s.reportUsage(agentID, grant)
	writeJSON(w, code, resp.Body)
}

// orDefault keeps audit operations non-empty for pre-tool rejections.
func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
