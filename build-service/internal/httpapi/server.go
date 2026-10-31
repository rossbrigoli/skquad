// Package httpapi exposes the build-service HTTP API consumed by agents
// through the tool-gateway `rest` driver:
//
//	POST /api/v1/builds        submit a build request
//	GET  /api/v1/builds        list the caller's builds
//	GET  /api/v1/builds/{id}   fetch one build (status + digest)
//	GET  /healthz              liveness
//
// Authentication: `Authorization: Bearer <token>` — the per-agent
// credential the gateway injects for the registered build-service rest
// resource. The token hashes to exactly one active principal.
//
// Deny mapping: format problems -> 400, policy denials/quotas -> 403,
// upstream (CP policy / ls-remote) failures -> 502 (fail-closed).
// Error bodies are {"error":"<code>","reason":"<safe text>"}.
package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/rossbrigoli/skquad/build-service/internal/audit"
	"github.com/rossbrigoli/skquad/build-service/internal/authn"
	"github.com/rossbrigoli/skquad/build-service/internal/govern"
	"github.com/rossbrigoli/skquad/build-service/internal/runner"
)

// Deps wires the server.
type Deps struct {
	Store     *authn.Store
	Validator *govern.Validator
	Runner    runner.Runner
	Audit     audit.Emitter
}

// Server is the HTTP handler set.
type Server struct {
	deps Deps

	mu           sync.Mutex
	auditedFinal map[string]bool // build ids whose terminal result was audited
}

// NewServer builds the server.
func NewServer(deps Deps) *Server {
	if deps.Audit == nil {
		deps.Audit = audit.Nop{}
	}
	return &Server{deps: deps, auditedFinal: map[string]bool{}}
}

// Handler returns the routed http.Handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("POST /api/v1/builds", s.auth(s.handleSubmit))
	mux.HandleFunc("GET /api/v1/builds", s.auth(s.handleList))
	mux.HandleFunc("GET /api/v1/builds/{id}", s.auth(s.handleGet))
	return mux
}

type errorBody struct {
	Error  string `json:"error"`
	Reason string `json:"reason,omitempty"`
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleSubmit(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		s.denied(w, p, "", govern.CodeInvalidBuildArgs, "unreadable body")
		return
	}
	var req govern.Request
	if err := json.Unmarshal(body, &req); err != nil {
		s.denied(w, p, "", govern.CodeInvalidBuildArgs, "invalid JSON body")
		return
	}

	dec := s.deps.Validator.Validate(r.Context(), p, req)
	if !dec.Allowed {
		s.denied(w, p, req.Destination, dec.Code, dec.Reason)
		return
	}

	id, err := newBuildID()
	if err != nil {
		s.denied(w, p, req.Destination, "internal_error", "id generation failed")
		return
	}
	spec := runner.Spec{
		ID:          id,
		Principal:   p.ID,
		Repo:        dec.Repo,
		Revision:    req.Revision,
		Dockerfile:  req.Dockerfile,
		Destination: req.Destination,
		BuildArgs:   req.BuildArgs,
	}
	if err := s.deps.Runner.Create(r.Context(), spec); err != nil {
		s.denied(w, p, req.Destination, "runner_error", "failed to create build job")
		return
	}
	s.deps.Audit.Emit("build_submit", map[string]any{
		"decision":    "allow",
		"principal":   p.ID,
		"build_id":    id,
		"repo":        dec.Repo,
		"revision":    req.Revision,
		"destination": req.Destination,
	})
	writeJSON(w, http.StatusCreated, map[string]any{
		"build_id":    id,
		"state":       runner.StateQueued,
		"repo":        dec.Repo,
		"revision":    req.Revision,
		"destination": req.Destination,
	})
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	id := r.PathValue("id")
	if !runner.BuildIDPattern.MatchString(id) {
		writeErr(w, http.StatusBadRequest, "invalid_build_id", "")
		return
	}
	st, err := s.deps.Runner.Get(r.Context(), id)
	if err != nil || st == nil {
		writeErr(w, http.StatusNotFound, "build_not_found", "")
		return
	}
	// Anti-enumeration: another principal's build is indistinguishable
	// from "not found".
	if st.Principal != authnLabel(p) {
		writeErr(w, http.StatusNotFound, "build_not_found", "")
		return
	}
	if (st.State == runner.StateSucceeded || st.State == runner.StateFailed) && s.markAudited(id) {
		s.deps.Audit.Emit("build_result", map[string]any{
			"principal":   p.ID,
			"build_id":    id,
			"state":       st.State,
			"digest":      st.Digest,
			"destination": st.Destination,
		})
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	items, err := s.deps.Runner.List(r.Context(), p.ID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "runner_unavailable", "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"builds": items})
}

func (s *Server) denied(w http.ResponseWriter, p *authn.Principal, destination, code, reason string) {
	principal := "<anonymous>"
	if p != nil {
		principal = p.ID
	}
	s.deps.Audit.Emit("build_submit", map[string]any{
		"decision":    "deny",
		"principal":   principal,
		"code":        code,
		"destination": destination,
	})
	writeErr(w, statusForCode(code), code, reason)
}

func (s *Server) markAudited(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.auditedFinal[id] {
		return false
	}
	s.auditedFinal[id] = true
	return true
}

// statusForCode maps governance codes to HTTP statuses.
func statusForCode(code string) int {
	switch code {
	case govern.CodeInvalidRepo, govern.CodeInvalidRevision, govern.CodeInvalidDockerfile,
		govern.CodeInvalidDestination, govern.CodeInvalidBuildArgs, govern.CodeRevisionNotFound:
		return http.StatusBadRequest
	case govern.CodePolicyUnavailable, govern.CodeRevisionCheckFailed:
		return http.StatusBadGateway
	default:
		return http.StatusForbidden
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, reason string) {
	writeJSON(w, status, errorBody{Error: code, Reason: reason})
}

func newBuildID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func authnLabel(p *authn.Principal) string {
	if p == nil {
		return ""
	}
	return runner.SanitizeLabel(p.ID)
}

// context plumbing

type principalKey struct{}

func withPrincipal(ctx context.Context, p *authn.Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

func principalFrom(ctx context.Context) *authn.Principal {
	p, _ := ctx.Value(principalKey{}).(*authn.Principal)
	return p
}

// auth is the bearer-token authentication middleware.
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r.Header.Get("Authorization"))
		p := s.deps.Store.Authenticate(token)
		if p == nil {
			s.deps.Audit.Emit("build_unauthenticated", map[string]any{
				"path": r.URL.Path,
			})
			writeErr(w, http.StatusUnauthorized, "unauthenticated", "")
			return
		}
		next(w, r.WithContext(withPrincipal(r.Context(), p)))
	}
}

func bearerToken(authorization string) string {
	const prefix = "Bearer "
	if len(authorization) <= len(prefix) || !strings.EqualFold(authorization[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(authorization[len(prefix):])
}
