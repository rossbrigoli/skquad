// Package httpapi exposes the build service HTTP surface:
//
//	GET  /healthz, /readyz     — unauthenticated liveness/readiness
//	POST /v1/builds            — submit a governed build request
//	GET  /v1/builds            — list the caller's builds
//	GET  /v1/builds/{id}       — fetch one build (own only)
//
// Authentication: bearer token, matched against principal token hashes
// (constant time). In the production wiring this endpoint is reached
// only through the tool-gateway `rest` driver, which injects the
// per-agent credential from CP custody — so the agent never handles
// this service's token directly, and the gateway's path_allow ACL plus
// this service's principal auth form two independent floors.
package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/rossbrigoli/skquad/build-service/internal/buildspec"
	"github.com/rossbrigoli/skquad/build-service/internal/principal"
	"github.com/rossbrigoli/skquad/build-service/internal/service"
)

// MaxBodyBytes bounds request bodies (build requests are tiny).
const MaxBodyBytes = 16 << 10

// Server holds the HTTP wiring.
type Server struct {
	store  *principal.Store
	svc    *service.Service
	logger *slog.Logger
	ready  func() bool
}

// NewServer builds the handler set.
func NewServer(store *principal.Store, svc *service.Service, logger *slog.Logger) *Server {
	return &Server{store: store, svc: svc, logger: logger, ready: func() bool { return true }}
}

// Handler returns the routed http.Handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if s.ready() {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Error(w, "not ready", http.StatusServiceUnavailable)
	})
	mux.HandleFunc("POST /v1/builds", s.requirePrincipal(s.handleCreate))
	mux.HandleFunc("GET /v1/builds", s.requirePrincipal(s.handleList))
	mux.HandleFunc("GET /v1/builds/{id}", s.requirePrincipal(s.handleGet))
	return mux
}

type ctxKey int

const principalKey ctxKey = 1

func (s *Server) requirePrincipal(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := bearerToken(r)
		p := s.store.Authenticate(tok)
		if p == nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthenticated"})
			return
		}
		next(w, r.WithContext(contextWithPrincipal(r.Context(), p)))
	}
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	return ""
}

func principalFrom(r *http.Request) *principal.Principal {
	p, _ := r.Context().Value(principalKey).(*principal.Principal)
	return p
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r)
	var req buildspec.Request
	dec := json.NewDecoder(io.LimitReader(r.Body, MaxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	build, err := s.svc.Submit(r.Context(), p, req)
	if err != nil {
		s.mapError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, build)
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r)
	builds, err := s.svc.List(r.Context(), p)
	if err != nil {
		s.mapError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"builds": builds})
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r)
	build, err := s.svc.Get(r.Context(), p, r.PathValue("id"))
	if err != nil {
		s.mapError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, build)
}

func (s *Server) mapError(w http.ResponseWriter, err error) {
	var se *service.Error
	if errors.As(err, &se) {
		status := http.StatusInternalServerError
		switch se.Kind {
		case service.KindValidation:
			status = http.StatusBadRequest
		case service.KindForbidden:
			status = http.StatusForbidden
		case service.KindNotFound:
			status = http.StatusNotFound
		case service.KindQuota:
			status = http.StatusTooManyRequests
		case service.KindUpstream:
			status = http.StatusBadGateway
		}
		writeJSON(w, status, map[string]string{"error": se.Message})
		return
	}
	s.logger.Error("unhandled error", "err", err)
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
