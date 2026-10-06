// Internal MCP enumerate endpoint (TG-5 slice B1, docs/tool-gateway.md §6.3).
//
//	POST /internal/mcp/enumerate
//
// The control-plane holds the resource config + credential but has no
// egress to arbitrary MCP servers, so it asks the gateway to perform
// `initialize` + `tools/list` on its behalf at registration time.
//
// Request:
//
//	{
//	  "resource_id": "mcp-1",            // required, opaque id (no slashes)
//	  "url": "https://mcp.example.com/mcp", // upstream base/endpoint URL
//	  "auth": {"kind": "bearer", "token": "***"}, // CP-held credential
//	  "allow_private": false              // optional; internal-class egress
//	                                    // (private/loopback upstreams).
//	                                    // Mirrors the driver's egress
//	                                    // class: default deny.
//	}
//
// Response 200:
//
//	{
//	  "resource_id": "mcp-1",
//	  "tools": [{"name": "...", "description": "...", "inputSchema": {...}}, ...],
//	  "hash": "<sha256 hex of canonical tool set>"
//	}
//
// The hash canonicalization is documented normatively in
// drivers/mcp/enumerate.go (CanonicalToolSetHash) — the CP reproduces
// it for drift detection.
//
// Error taxonomy (structured, client-safe; never echoes the token):
//
//	400 bad_request             malformed body / url / auth kind / resource id
//	401 unauthorized            missing/invalid internal token
//	503 gateway_disabled        kill switch engaged
//	503 internal_unconfigured   no internal token configured (fail-closed)
//	502 mcp_enumerate_failed    upstream unreachable / handshake failed
//	502 redirect_denied         upstream tried a cross-host redirect (refused)
//	502 egress_denied           upstream blocked by netguard SSRF policy
//
// Auth: trusted-internal shared secret, header
// `X-Skquad-Internal-Token` (fallback: `Authorization: Bearer`),
// constant-time compared against SKQUAD_GATEWAY_INTERNAL_TOKEN. This
// mirrors the CP's requireGatewayCallback pattern (the existing
// gateway→CP direction) but is a distinct token/env var so trust
// flows CP→gateway without reusing the callback secret. Unset secret
// = endpoint disabled (503), never open access.
package httpapi

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rossbrigoli/skquad/tool-gateway/internal/audit"
	mcpdriver "github.com/rossbrigoli/skquad/tool-gateway/internal/drivers/mcp"
)

// InternalTokenHeader carries the CP→gateway shared internal token.
const InternalTokenHeader = "X-Skquad-Internal-Token"

type mcpEnumerateRequest struct {
	ResourceID string `json:"resource_id"`
	URL        string `json:"url"`
	Auth       struct {
		Kind  string `json:"kind"`
		Token string `json:"token"`
	} `json:"auth"`
	AllowPrivate bool `json:"allow_private"`
}

// handleMCPEnumerate performs CP-delegated enumeration of an upstream
// MCP server. Audit records resource id, tool count and hash — never
// the credential, never the schemas.
func (s *Server) handleMCPEnumerate(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	reqID := newRequestID()

	emit := func(resource string, decision string, code int, detail string) {
		s.deps.Audit.Emit(audit.Event{
			Timestamp:  time.Now(),
			RequestID:  reqID,
			Resource:   resource,
			Operation:  "mcp_enumerate",
			Decision:   decision,
			StatusCode: code,
			LatencyMS:  time.Since(start).Milliseconds(),
			Detail:     detail,
		})
	}

	// 0. Kill switch: enumeration opens upstream connections, so a
	//    disabled gateway refuses it too (health-only).
	if s.deps.Enabled != nil && !s.deps.Enabled.Load() {
		emit("mcp", audit.DecisionDeny, http.StatusServiceUnavailable, "kill_switch")
		writeError(w, http.StatusServiceUnavailable, "gateway_disabled", "tool gateway is disabled by kill switch")
		return
	}

	// 1. Internal auth (fail-closed): a configured secret is
	//    required; constant-time compare; missing/invalid → 401.
	expected := strings.TrimSpace(s.deps.InternalToken)
	if expected == "" {
		emit("mcp", audit.DecisionDeny, http.StatusServiceUnavailable, "internal_unconfigured")
		writeError(w, http.StatusServiceUnavailable, "internal_unconfigured", "internal endpoint token is not configured")
		return
	}
	actual := strings.TrimSpace(r.Header.Get(InternalTokenHeader))
	if actual == "" {
		actual = strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	}
	if actual == "" || subtle.ConstantTimeCompare([]byte(actual), []byte(expected)) != 1 {
		emit("mcp", audit.DecisionDeny, http.StatusUnauthorized, "internal_token_invalid")
		writeError(w, http.StatusUnauthorized, "unauthorized", "internal token is missing or invalid")
		return
	}

	// 2. Validate request shape (strictly AFTER auth — no validation
	//    detail leaks to unauthenticated callers).
	var in mcpEnumerateRequest
	body, err := io.ReadAll(io.LimitReader(r.Body, s.deps.MaxBodyBytes+1))
	if err != nil {
		emit("mcp", audit.DecisionError, http.StatusBadRequest, "body read error")
		writeError(w, http.StatusBadRequest, "bad_request", "failed reading request body")
		return
	}
	if int64(len(body)) > s.deps.MaxBodyBytes {
		emit("mcp", audit.DecisionDeny, http.StatusRequestEntityTooLarge, "payload too large")
		writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "request body exceeds limit")
		return
	}
	if err := json.Unmarshal(body, &in); err != nil {
		emit("mcp", audit.DecisionDeny, http.StatusBadRequest, "bad_json")
		writeError(w, http.StatusBadRequest, "bad_request", "invalid enumerate request body")
		return
	}
	in.ResourceID = strings.TrimSpace(in.ResourceID)
	if in.ResourceID == "" || strings.ContainsAny(in.ResourceID, "/\\") || in.ResourceID == "." || in.ResourceID == ".." {
		emit("mcp:"+in.ResourceID, audit.DecisionDeny, http.StatusBadRequest, "bad_resource_id")
		writeError(w, http.StatusBadRequest, "bad_request", "invalid resource id")
		return
	}
	if !strings.EqualFold(strings.TrimSpace(in.Auth.Kind), "bearer") {
		emit("mcp:"+in.ResourceID, audit.DecisionDeny, http.StatusBadRequest, "unsupported_auth_kind")
		writeError(w, http.StatusBadRequest, "bad_request", "auth.kind must be bearer")
		return
	}
	if strings.TrimSpace(in.Auth.Token) == "" {
		emit("mcp:"+in.ResourceID, audit.DecisionDeny, http.StatusBadRequest, "missing_credential")
		writeError(w, http.StatusBadRequest, "bad_request", "auth.token is required")
		return
	}
	resource := "mcp:" + in.ResourceID

	// 3. Enumerate upstream (guarded: netguard dial-time SSRF pin +
	//    cross-host redirect refusal — same posture as the governed
	//    call path).
	res, err := mcpdriver.Enumerate(r.Context(), in.URL, in.Auth.Token, in.AllowPrivate, nil)
	if err != nil {
		kind, msg := classifyEnumerateFailure(err)
		emit(resource, audit.DecisionError, http.StatusBadGateway, kind)
		writeError(w, http.StatusBadGateway, kind, msg)
		return
	}

	emit(resource, audit.DecisionAllow, http.StatusOK, "tools="+strconv.Itoa(len(res.Tools))+" hash="+res.Hash)
	writeJSON(w, http.StatusOK, map[string]any{
		"resource_id": in.ResourceID,
		"tools":       res.Tools,
		"hash":        res.Hash,
	})
}

// classifyEnumerateFailure maps upstream errors to a stable,
// secret-free taxonomy the CP can branch on for registration
// failures. The raw upstream error is never echoed.
func classifyEnumerateFailure(err error) (kind string, message string) {
	msg := err.Error()
	switch {
	case strings.HasPrefix(msg, "bad_request"):
		return "bad_request", "invalid enumerate request"
	case strings.Contains(msg, "denied redirect"):
		return "redirect_denied", "upstream attempted a cross-host redirect (refused)"
	case strings.Contains(msg, "ssrf_guard"):
		return "egress_denied", "upstream blocked by network egress policy"
	default:
		return "mcp_enumerate_failed", "could not enumerate upstream MCP server (unreachable or handshake failed)"
	}
}
