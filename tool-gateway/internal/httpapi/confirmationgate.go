// TG-8 slice C2: gateway confirmation-gate enforcement
// (docs/tg8-grant-approvals-spec.md §C).
//
// Every typed dispatch (web/rest/mcp/git) runs confirmationGate BEFORE
// driver invocation. Ungated grants pass straight through (zero CP
// calls). Gated dispatches:
//
//   - No X-Skquad-Confirmation-Id header → POST /confirmation/check.
//     mode=auto (live standing grant) proceeds with the matched id
//     audited; mode=pending does NOT execute — the agent gets 202
//     pending_confirmation with the id to retry with after approval.
//   - Header present → POST /confirmation/consume {id, args_hash}.
//     allowed proceeds (mode once/standing audited); denied returns 403
//     with the CP reason verbatim under a stable code.
//   - CP unreachable/error → FAIL CLOSED: 503 confirmation_unavailable.
//     A gated call never executes without a positive CP answer.
//
// Every gated dispatch emits one audit event carrying the gate outcome
// (auto|once|standing|pending|denied|unavailable + ids) on the
// existing audit.Event path.
package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/rossbrigoli/skquad/tool-gateway/internal/audit"
	"github.com/rossbrigoli/skquad/tool-gateway/internal/confirmation"
	"github.com/rossbrigoli/skquad/tool-gateway/internal/drivers"
	"github.com/rossbrigoli/skquad/tool-gateway/internal/policy"
)

// ConfirmationIDHeader carries the confirmation id an agent retries
// with after the owner approves (one-shot or standing).
const ConfirmationIDHeader = "X-Skquad-Confirmation-Id"

// ConfirmationClient is the CP confirmation surface the gate needs.
type ConfirmationClient interface {
	Check(ctx context.Context, resourceID, agentID, tool, argsHash string) (*confirmation.CheckResult, error)
	Consume(ctx context.Context, id, argsHash string) (*confirmation.ConsumeResult, error)
}

// pendingConfirmationHint is the agent-facing retry instruction on 202.
const pendingConfirmationHint = "owner approval required; retry with header " + ConfirmationIDHeader + " after approval"

// confirmationGate evaluates the TG-8 §C gate for one dispatch.
// Returns (ctx, true) when the driver may run — ctx carries the
// confirmation-satisfied marker. Returns (nil, false) when the gate
// already wrote the terminal response; the caller must return.
//
// tool is the confirmation identity (MCP tool name, or the operation
// for web/rest/git); payload is the canonical body hashed for binding
// (git: the trailing path — the packfile stream is never buffered).
func (s *Server) confirmationGate(
	w http.ResponseWriter, r *http.Request,
	reqID, agentID string,
	grant *policy.Grant,
	operation, tool string,
	payload []byte,
) (context.Context, bool) {
	if !policy.ConfirmationRequired(grant, tool) {
		return r.Context(), true
	}
	resourceID := grant.ResourceID
	argsHash := confirmation.ArgsHash(resourceID, operation, payload)
	confID := strings.TrimSpace(r.Header.Get(ConfirmationIDHeader))

	gateAudit := func(outcome string, code int, confirmationID, standingID, reason string) {
		detail := map[string]any{"gate": outcome}
		if confirmationID != "" {
			detail["confirmation_id"] = confirmationID
		}
		if standingID != "" {
			detail["standing_grant_id"] = standingID
		}
		if reason != "" {
			detail["reason"] = reason
		}
		b, err := json.Marshal(detail)
		if err != nil {
			b = []byte(`{"gate":"` + outcome + `"}`)
		}
		decision := audit.DecisionAllow
		if outcome == "pending" || outcome == "denied" || outcome == "unavailable" {
			decision = audit.DecisionDeny
		}
		s.deps.Audit.Emit(audit.Event{
			Timestamp:  time.Now(),
			RequestID:  reqID,
			AgentID:    agentID,
			Resource:   resourceID,
			Operation:  operation,
			Decision:   decision,
			StatusCode: code,
			Detail:     string(b),
		})
	}

	if s.deps.Confirmation == nil {
		// Gated grant but no confirmation client wired: fail closed.
		gateAudit("unavailable", http.StatusServiceUnavailable, "", "", "confirmation client not configured")
		writeError(w, http.StatusServiceUnavailable, "confirmation_unavailable", "confirmation service unreachable; failing closed")
		return nil, false
	}

	if confID != "" {
		res, err := s.deps.Confirmation.Consume(r.Context(), confID, argsHash)
		if err != nil {
			gateAudit("unavailable", http.StatusServiceUnavailable, confID, "", "consume: "+err.Error())
			writeError(w, http.StatusServiceUnavailable, "confirmation_unavailable", "confirmation service unreachable; failing closed")
			return nil, false
		}
		if !res.Allowed {
			reason := res.Reason
			if reason == "" {
				reason = "denied_by_owner"
			}
			gateAudit("denied", http.StatusForbidden, confID, res.MatchedStandingGrantID, reason)
			writeJSON(w, http.StatusForbidden, map[string]string{
				"error":   "confirmation_denied",
				"reason":  confirmationDenialCode(reason),
				"message": reason,
			})
			return nil, false
		}
		mode := res.Mode
		if mode == "" {
			mode = "once"
		}
		gateAudit(mode, http.StatusOK, confID, res.MatchedStandingGrantID, "")
		return drivers.WithConfirmationSatisfied(r.Context()), true
	}

	res, err := s.deps.Confirmation.Check(r.Context(), resourceID, agentID, tool, argsHash)
	if err != nil {
		gateAudit("unavailable", http.StatusServiceUnavailable, "", "", "check: "+err.Error())
		writeError(w, http.StatusServiceUnavailable, "confirmation_unavailable", "confirmation service unreachable; failing closed")
		return nil, false
	}
	switch res.Mode {
	case "auto":
		gateAudit("auto", http.StatusOK, "", res.MatchedStandingGrantID, "")
		return drivers.WithConfirmationSatisfied(r.Context()), true
	case "pending":
		gateAudit("pending", http.StatusAccepted, res.ConfirmationID, "", "")
		writeJSON(w, http.StatusAccepted, map[string]string{
			"error":           "pending_confirmation",
			"confirmation_id": res.ConfirmationID,
			"hint":            pendingConfirmationHint,
		})
		return nil, false
	default:
		// Unreachable via the client's validation, but fail closed anyway.
		gateAudit("unavailable", http.StatusServiceUnavailable, "", "", "unknown mode "+res.Mode)
		writeError(w, http.StatusServiceUnavailable, "confirmation_unavailable", "confirmation service unreachable; failing closed")
		return nil, false
	}
}

// confirmationDenialCode extracts the stable code from a CP denial
// reason ("denied_by_owner: not today" ⇒ "denied_by_owner"). Unknown
// prefixes pass through verbatim — the code space is CP-owned.
func confirmationDenialCode(reason string) string {
	known := map[string]bool{
		"denied_replayed":         true,
		"args_hash_mismatch":      true,
		"approval_expired":        true,
		"standing_grant_not_live": true,
		"denied_by_owner":         true,
		"confirmation_expired":    true,
		"confirmation_pending":    true,
	}
	code := reason
	if i := strings.Index(reason, ":"); i >= 0 {
		code = strings.TrimSpace(reason[:i])
	}
	if known[code] {
		return code
	}
	return strings.TrimSpace(reason)
}
