// Package httpapi wires the tool-gateway request pipeline:
//
//	kill switch -> authn -> policy lookup -> driver dispatch -> audit emit
//
// Endpoints:
//
//	GET  /healthz                 liveness (process up)
//	GET  /readyz                  fail-closed readiness: CP policy path proven
//	                              reachable at least once AND boundary fence
//	                              confirmed; otherwise 503
//	GET  /internal/verify-boundary  fence state (TG-1 stub, interface in place)
//	POST /v1/echo                 pipeline-proving driver
package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/rossbrigoli/skquad/tool-gateway/internal/audit"
	"github.com/rossbrigoli/skquad/tool-gateway/internal/auth"
	"github.com/rossbrigoli/skquad/tool-gateway/internal/boundary"
	"github.com/rossbrigoli/skquad/tool-gateway/internal/drivers"
	"github.com/rossbrigoli/skquad/tool-gateway/internal/policy"
)

// Deps are the server's collaborators, injected for testability.
type Deps struct {
	Policy       policy.Lookup
	PolicyClient *policy.Client // optional: for readiness reachability signal
	Boundary     boundary.Verifier
	Audit        audit.Emitter
	Enabled      *atomic.Bool // kill switch; false => refuse all non-health traffic
	Drivers      map[string]drivers.Driver
	// StreamDrivers holds streaming proxy drivers (TG-4b git) that
	// own the response wire; the pipeline runs kill switch/authn/grant
	// lookup, then hands the raw ResponseWriter over.
	StreamDrivers map[string]drivers.StreamingDriver
	// MaxBodyBytes caps request payloads read by drivers.
	MaxBodyBytes int64
}

// Server implements http.Handler for the gateway.
type Server struct {
	deps Deps
	mux  *http.ServeMux
}

// healthzPaths are exempt from the kill switch.
var healthzPaths = map[string]bool{
	"/healthz":                  true,
	"/readyz":                   true,
	"/internal/verify-boundary": true,
}

func New(deps Deps) *Server {
	if deps.Audit == nil {
		deps.Audit = audit.NopEmitter{}
	}
	if deps.MaxBodyBytes == 0 {
		deps.MaxBodyBytes = 1 << 20 // 1 MiB default cap for TG-1
	}
	s := &Server{deps: deps, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /readyz", s.handleReadyz)
	s.mux.HandleFunc("GET /internal/verify-boundary", s.handleVerifyBoundary)
	s.mux.HandleFunc("POST /v1/echo", s.handleEcho)
	s.mux.HandleFunc("POST /v1/web/fetch", s.handleWebFetch)
	s.mux.HandleFunc("POST /v1/rest/{resourceID}", s.handleRestCall)
	// TG-4b: git smart-HTTP proxy (clone/fetch/push). GET and POST,
	// trailing path is "<org>/<repo>.git/<service>" (+ query).
	s.mux.HandleFunc("GET /git/{resourceID}/{trailing...}", s.handleGitProxy)
	s.mux.HandleFunc("POST /git/{resourceID}/{trailing...}", s.handleGitProxy)
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleReadyz implements the fail-closed launch gate: ready only when
// (i) the CP policy path has been proven reachable and (ii) the boundary
// verifier confirms the fence. Any unknown state is NOT ready.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	cpReachable := s.deps.PolicyClient != nil && s.deps.PolicyClient.Reachable()
	fence := s.deps.Boundary.Verify(r.Context())
	status := map[string]any{
		"cp_policy_reachable": cpReachable,
		"fence_applied":       fence.Applied,
		"verifier":            fence.Verifier,
	}
	if cpReachable && fence.Applied {
		writeJSON(w, http.StatusOK, appendReason(status, "ready"))
		return
	}
	var reasons []string
	if !cpReachable {
		reasons = append(reasons, "control-plane policy path not yet reachable")
	}
	if !fence.Applied {
		reasons = append(reasons, "network boundary fence not confirmed")
	}
	writeJSON(w, http.StatusServiceUnavailable, appendReason(status, strings.Join(reasons, "; ")))
}

func appendReason(m map[string]any, reason string) map[string]any {
	m["reason"] = reason
	return m
}

func (s *Server) handleVerifyBoundary(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.deps.Boundary.Verify(r.Context()))
}

// handleEcho runs the full pipeline for the echo driver.
func (s *Server) handleEcho(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	reqID := newRequestID()
	resource := "echo"
	operation := "echo"
	agentID := ""

	emit := func(decision string, code int, detail string) {
		s.deps.Audit.Emit(audit.Event{
			Timestamp:  time.Now(),
			RequestID:  reqID,
			AgentID:    agentID,
			Resource:   resource,
			Operation:  operation,
			Decision:   decision,
			StatusCode: code,
			LatencyMS:  time.Since(start).Milliseconds(),
			Detail:     detail,
		})
	}

	// 0. Kill switch.
	if s.deps.Enabled != nil && !s.deps.Enabled.Load() {
		emit(audit.DecisionDeny, http.StatusServiceUnavailable, "kill_switch")
		writeError(w, http.StatusServiceUnavailable, "gateway_disabled", "tool gateway is disabled by kill switch")
		return
	}

	// 1. Authn (credential verified against CP-delivered hash; policy
	//    lookup happens inside and is fail-closed).
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

	// 3. Driver dispatch.
	drv, ok := s.deps.Drivers[resource]
	if !ok {
		emit(audit.DecisionError, http.StatusNotFound, "no driver")
		writeError(w, http.StatusNotFound, "not_found", "no driver registered")
		return
	}
	resp, err := drv.Handle(r.Context(), &drivers.Request{
		Agent:     principal,
		Resource:  resource,
		Operation: operation,
		Payload:   body,
	})
	if err != nil {
		emit(audit.DecisionError, http.StatusBadGateway, err.Error())
		writeError(w, http.StatusBadGateway, "driver_error", "driver failed")
		return
	}

	// 4. Audit allow + respond.
	code := resp.StatusCode
	if code == 0 {
		code = http.StatusOK
	}
	emit(audit.DecisionAllow, code, "")
	writeJSON(w, code, resp.Body)
}

// handleWebFetch runs the pipeline for the TG-3 `web` driver:
// kill switch → authn → body cap → grant lookup (resource_type=web) →
// driver dispatch → audit. Denials map to 403 with a client-safe
// reason; upstream failures map to 502 fetch_failed (never echoing
// internal detail).
func (s *Server) handleWebFetch(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	reqID := newRequestID()
	resource := "web"
	operation := "fetch"
	agentID := ""

	emit := func(decision string, code int, detail string) {
		s.deps.Audit.Emit(audit.Event{
			Timestamp:  time.Now(),
			RequestID:  reqID,
			AgentID:    agentID,
			Resource:   resource,
			Operation:  operation,
			Decision:   decision,
			StatusCode: code,
			LatencyMS:  time.Since(start).Milliseconds(),
			Detail:     detail,
		})
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

	// 3. Grant lookup: the agent must hold a live grant to a `web`
	// resource. First match wins (the CP surfaces live grants only).
	var grant *policy.Grant
	if principal.Snapshot != nil {
		for i := range principal.Snapshot.Grants {
			if principal.Snapshot.Grants[i].ResourceType == "web" {
				grant = &principal.Snapshot.Grants[i]
				break
			}
		}
	}
	if grant == nil {
		resource = "web:none"
		emit(audit.DecisionDeny, http.StatusForbidden, "no_web_grant")
		writeError(w, http.StatusForbidden, "no_grant", "agent has no grant to a web resource")
		return
	}
	resource = grant.ResourceID

	// 4. Driver dispatch.
	drv, ok := s.deps.Drivers["web"]
	if !ok {
		emit(audit.DecisionError, http.StatusNotFound, "no web driver")
		writeError(w, http.StatusNotFound, "not_found", "no web driver registered")
		return
	}
	resp, err := drv.Handle(r.Context(), &drivers.Request{
		Agent:     principal,
		Resource:  grant.ResourceID,
		Operation: operation,
		Payload:   body,
		Grant:     grant,
	})
	if err != nil {
		var denied *drivers.DeniedError
		if errors.As(err, &denied) {
			emit(audit.DecisionDeny, http.StatusForbidden, denied.Reason)
			writeError(w, http.StatusForbidden, "denied", denied.Reason)
			return
		}
		if errors.Is(err, drivers.ErrDenied) {
			emit(audit.DecisionDeny, http.StatusForbidden, "denied")
			writeError(w, http.StatusForbidden, "denied", "blocked by policy")
			return
		}
		if strings.HasPrefix(err.Error(), "bad_request") {
			emit(audit.DecisionDeny, http.StatusBadRequest, "bad_request")
			writeError(w, http.StatusBadRequest, "bad_request", "invalid fetch request")
			return
		}
		emit(audit.DecisionError, http.StatusBadGateway, err.Error())
		writeError(w, http.StatusBadGateway, "fetch_failed", "fetch failed (blocked by network policy or unreachable target)")
		return
	}

	code := resp.StatusCode
	if code == 0 {
		code = http.StatusOK
	}
	emit(audit.DecisionAllow, code, "")
	writeJSON(w, code, resp.Body)
}

// handleRestCall runs the pipeline for the TG-4 `rest` driver:
// kill switch → authn → body cap → grant lookup (typed rest resource
// addressed by id) → driver dispatch → audit. The resource id comes
// from the URL path and must match one of the agent's live rest grants;
// the driver then enforces method/path/header/cap/egress policy from
// the folded ceiling ∧ constraints. Denials map to 403 with a
// client-safe reason; upstream failures map to 502 rest_failed (never
// echoing internal detail).
func (s *Server) handleRestCall(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	reqID := newRequestID()
	resourceID := strings.TrimSpace(r.PathValue("resourceID"))
	if resourceID == "" || strings.ContainsAny(resourceID, "/\\") || resourceID == "." || resourceID == ".." {
		s.deps.Audit.Emit(audit.Event{
			Timestamp: time.Now(), RequestID: reqID, Resource: "rest:" + resourceID,
			Operation: "rest_call", Decision: audit.DecisionDeny,
			StatusCode: http.StatusBadRequest, Detail: "bad_resource_id",
		})
		writeError(w, http.StatusBadRequest, "bad_request", "invalid resource id")
		return
	}
	operation := "rest_call"
	agentID := ""
	resource := "rest:" + resourceID

	emit := func(decision string, code int, detail string) {
		s.deps.Audit.Emit(audit.Event{
			Timestamp:  time.Now(),
			RequestID:  reqID,
			AgentID:    agentID,
			Resource:   resource,
			Operation:  operation,
			Decision:   decision,
			StatusCode: code,
			LatencyMS:  time.Since(start).Milliseconds(),
			Detail:     detail,
		})
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

	// 3. Grant lookup: the agent must hold a live grant to THIS rest
	// resource (id-addressed, unlike the singleton web resource).
	var grant *policy.Grant
	if principal.Snapshot != nil {
		for i := range principal.Snapshot.Grants {
			g := principal.Snapshot.Grants[i]
			if g.ResourceType == "rest" && g.ResourceID == resourceID {
				grant = &principal.Snapshot.Grants[i]
				break
			}
		}
	}
	if grant == nil {
		emit(audit.DecisionDeny, http.StatusForbidden, "no_rest_grant")
		writeError(w, http.StatusForbidden, "no_grant", "agent has no grant to this rest resource")
		return
	}

	// 4. Driver dispatch.
	drv, ok := s.deps.Drivers["rest"]
	if !ok {
		emit(audit.DecisionError, http.StatusNotFound, "no rest driver")
		writeError(w, http.StatusNotFound, "not_found", "no rest driver registered")
		return
	}
	resp, err := drv.Handle(r.Context(), &drivers.Request{
		Agent:     principal,
		Resource:  grant.ResourceID,
		Operation: operation,
		Payload:   body,
		Grant:     grant,
	})
	if err != nil {
		var denied *drivers.DeniedError
		if errors.As(err, &denied) {
			emit(audit.DecisionDeny, http.StatusForbidden, denied.Reason)
			writeError(w, http.StatusForbidden, "denied", denied.Reason)
			return
		}
		if errors.Is(err, drivers.ErrDenied) {
			emit(audit.DecisionDeny, http.StatusForbidden, "denied")
			writeError(w, http.StatusForbidden, "denied", "blocked by policy")
			return
		}
		if strings.HasPrefix(err.Error(), "bad_request") {
			emit(audit.DecisionDeny, http.StatusBadRequest, "bad_request")
			writeError(w, http.StatusBadRequest, "bad_request", "invalid rest request")
			return
		}
		emit(audit.DecisionError, http.StatusBadGateway, err.Error())
		writeError(w, http.StatusBadGateway, "rest_failed", "rest call failed (blocked by policy or unreachable target)")
		return
	}

	code := resp.StatusCode
	if code == 0 {
		code = http.StatusOK
	}
	emit(audit.DecisionAllow, code, "")
	writeJSON(w, code, resp.Body)
}

// handleGitProxy runs the pipeline for the TG-4b `git` driver:
// kill switch → authn → grant lookup (typed git resource addressed
// by id) → streaming dispatch. The body is NEVER read into memory
// here — the driver streams it straight to the upstream (packfiles).
// The driver owns the response wire after the grant check; denials
// before that map to 403 with a client-safe reason, parse failures
// to 400, upstream failures to 502. Audit records repo/service and,
// for pushes, the refs advanced/rejected (never credentials).
func (s *Server) handleGitProxy(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	reqID := newRequestID()
	resourceID := strings.TrimSpace(r.PathValue("resourceID"))
	resource := "git:" + resourceID
	operation := "git"
	agentID := ""

	emit := func(decision string, code int, detail string) {
		s.deps.Audit.Emit(audit.Event{
			Timestamp:  time.Now(),
			RequestID:  reqID,
			AgentID:    agentID,
			Resource:   resource,
			Operation:  operation,
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

	// 2. Grant lookup: the agent must hold a live grant to THIS git
	// resource (id-addressed, same posture as rest).
	var grant *policy.Grant
	if principal.Snapshot != nil {
		for i := range principal.Snapshot.Grants {
			g := principal.Snapshot.Grants[i]
			if g.ResourceType == "git" && g.ResourceID == resourceID {
				grant = &principal.Snapshot.Grants[i]
				break
			}
		}
	}
	if grant == nil {
		emit(audit.DecisionDeny, http.StatusForbidden, "no_git_grant")
		writeError(w, http.StatusForbidden, "no_grant", "agent has no grant to this git resource")
		return
	}

	// 3. Streaming dispatch. NOTE: no body read/cap — the driver
	// streams the packfile through (a cap here would break pushes).
	drv, ok := s.deps.StreamDrivers["git"]
	if !ok {
		emit(audit.DecisionError, http.StatusNotFound, "no git driver")
		writeError(w, http.StatusNotFound, "not_found", "no git driver registered")
		return
	}
	tw := &writeTracker{ResponseWriter: w}
	result, err := drv.ServeStream(tw, r, &drivers.Request{
		Agent:     principal,
		Resource:  grant.ResourceID,
		Operation: operation,
		Grant:     grant,
		Path:      r.PathValue("trailing"),
	})
	if result != nil {
		if result.Service == "git-receive-pack" {
			operation = "git_push"
		} else if result.Service != "" {
			operation = "git_fetch"
		}
	}
	if err != nil {
		var denied *drivers.DeniedError
		switch {
		case errors.As(err, &denied):
			emit(audit.DecisionDeny, http.StatusForbidden, gitAuditDetail(result, denied.Reason))
			writeError(w, http.StatusForbidden, "denied", denied.Reason)
		case strings.HasPrefix(err.Error(), "bad_request"):
			emit(audit.DecisionDeny, http.StatusBadRequest, "bad_request")
			writeError(w, http.StatusBadRequest, "bad_request", "invalid git request")
		default:
			// The driver may have already streamed response bytes when
			// the stream broke; we cannot write an envelope then. The
			// audit still records the failure.
			emit(audit.DecisionError, http.StatusBadGateway, gitAuditDetail(result, "stream_failed"))
			if !tw.wrote {
				writeError(w, http.StatusBadGateway, "git_failed", "git proxy failed (blocked by policy or unreachable target)")
			}
		}
		return
	}

	emit(audit.DecisionAllow, result.StatusCode, gitAuditDetail(result, ""))
}

// gitAuditDetail renders the secret-free audit detail for a git
// proxy call: repo, service and (push) refs advanced/rejected.
func gitAuditDetail(result *drivers.StreamResult, reason string) string {
	payload := map[string]any{}
	if reason != "" {
		payload["reason"] = reason
	}
	if result != nil {
		if result.Repo != "" {
			payload["repo"] = result.Repo
		}
		if result.Service != "" {
			payload["service"] = result.Service
		}
		if len(result.RefsAdvanced) > 0 {
			payload["refs_advanced"] = result.RefsAdvanced
		}
		if len(result.RefsRejected) > 0 {
			payload["refs_rejected"] = result.RefsRejected
		}
	}
	if len(payload) == 0 {
		return ""
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	return string(b)
}

// writeTracker records whether the streaming driver has written
// anything to the response yet, so a mid-stream failure can skip an
// error envelope that would corrupt the already-started stream.
type writeTracker struct {
	http.ResponseWriter
	wrote bool
}

func (t *writeTracker) WriteHeader(code int) {
	t.wrote = true
	t.ResponseWriter.WriteHeader(code)
}

func (t *writeTracker) Write(b []byte) (int, error) {
	t.wrote = true
	return t.ResponseWriter.Write(b)
}

// Flush keeps the http.Flusher contract alive for the driver's
// chunked passthrough.
func (t *writeTracker) Flush() {
	if f, ok := t.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// StartProbeLoop keeps the CP-reachability signal fresh for readiness by
// periodically probing the policy endpoint with a throwaway agent id.
func StartProbeLoop(ctx context.Context, c *policy.Client, agentID string, gap time.Duration) {
	// Probe immediately so a healthy CP can flip readiness fast.
	c.Probe(ctx, agentID)
	go func() {
		t := time.NewTicker(gap)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				c.Probe(ctx, agentID)
			}
		}
	}()
}

func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(b[:])
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, kind, message string) {
	writeJSON(w, code, map[string]string{"error": kind, "message": message})
}
