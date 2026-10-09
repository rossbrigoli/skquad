// Package httpapi exposes the terminal service's internal HTTP surface:
// /v1/exec, /v1/sessions*, healthz/readyz. Every /v1 call must carry
// the shared internal bearer token (constant-time compared). This
// service is reachable only from the tool-gateway (netpol-enforced);
// the token is the second floor.
package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"
	"unicode"

	"github.com/rossbrigoli/skquad/terminal-service/internal/apply"
	"github.com/rossbrigoli/skquad/terminal-service/internal/recorder"
	"github.com/rossbrigoli/skquad/terminal-service/internal/sshexec"
)

// LiveSession is the subset of an interactive session the API needs;
// *sshexec.Session satisfies it. Tests inject a fake to avoid needing a
// real SSH server for registry/limit coverage.
type LiveSession interface {
	Send(b []byte) error
	Close()
	Closed() bool
}

// SessionStarter opens interactive sessions. Production wiring uses
// sshexec.OpenSession.
type SessionStarter interface {
	Open(ctx context.Context, req sshexec.Request, onOut func([]byte)) (LiveSession, error)
}

// sshStarter adapts sshexec.OpenSession to SessionStarter.
type sshStarter struct{}

func (sshStarter) Open(ctx context.Context, req sshexec.Request, onOut func([]byte)) (LiveSession, error) {
	return sshexec.OpenSession(ctx, req, onOut)
}

// Config wires the API.
type Config struct {
	InternalToken       string
	MaxSessions         int
	SessionIdleTimeout  time.Duration
	RecorderSinkFactory func(ctx context.Context, recordingID string) (recorder.Sink, error)
	CAMint              sshexec.CAMinter
	Sessions            SessionStarter
	// ApplyEngine runs artifact applies; production wiring uses
	// *apply.Engine (mirrors the Sessions pattern).
	ApplyEngine ApplyEngine
	// MaxApplies bounds the async apply registry.
	MaxApplies int
}

// Server is the HTTP handler set.
type Server struct {
	cfg      Config
	logger   *slog.Logger
	sessions *registry
	applies  *applyRegistry
}

// NewServer validates config and returns the server.
func NewServer(cfg Config, logger *slog.Logger) (http.Handler, error) {
	if cfg.InternalToken == "" {
		return nil, errors.New("internal token required")
	}
	if cfg.MaxSessions <= 0 {
		cfg.MaxSessions = 8
	}
	if cfg.SessionIdleTimeout <= 0 {
		cfg.SessionIdleTimeout = 30 * time.Minute
	}
	if cfg.MaxApplies <= 0 {
		cfg.MaxApplies = 16
	}
	s := &Server{
		cfg:      cfg,
		logger:   logger,
		sessions: newRegistry(cfg.MaxSessions, cfg.SessionIdleTimeout),
		applies:  newApplyRegistry(cfg.MaxApplies),
	}
	if cfg.Sessions == nil {
		cfg.Sessions = sshStarter{}
	}
	if cfg.ApplyEngine == nil {
		cfg.ApplyEngine = &apply.Engine{}
	}
	s.cfg = cfg
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	mux.Handle("/v1/exec", s.auth(http.HandlerFunc(s.handleExec)))
	mux.Handle("/v1/sessions", s.auth(http.HandlerFunc(s.handleSessionOpen)))
	mux.Handle("/v1/sessions/", s.auth(http.HandlerFunc(s.handleSessionRoutes)))
	mux.Handle("/v1/applies", s.auth(http.HandlerFunc(s.handleApplyCreate)))
	mux.Handle("/v1/applies/", s.auth(http.HandlerFunc(s.handleApplyRoutes)))
	return mux, nil
}

func (s *Server) auth(next http.Handler) http.Handler {
	want := []byte(s.cfg.InternalToken)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(bearerToken(r))
		if subtle.ConstantTimeCompare(got, want) != 1 {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) > len(prefix) && h[:len(prefix)] == prefix {
		return h[len(prefix):]
	}
	return ""
}

const maxBodyBytes = 1 << 20 // 1 MiB

type execRequest struct {
	ResourceID     string `json:"resource_id"`
	AgentID        string `json:"agent_id"`
	TaskID         string `json:"task_id"`
	Host           string `json:"host"`
	User           string `json:"user"`
	Command        string `json:"command"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	// Emergency + IncidentID: TG-11 emergency interactive lane
	// (docs/tool-gateway.md §6.7). Required pair: one without the
	// other is refused fail-closed at open.
	Emergency  bool   `json:"emergency"`
	IncidentID string `json:"incident_id"`
	Auth       struct {
		Mode           string `json:"mode"`
		PrivateKeyPEM  string `json:"private_key_pem"`
		CertTTLMinutes int    `json:"cert_ttl_minutes"`
		KnownHosts     string `json:"known_hosts"`
	} `json:"auth"`
}

// EmergencyMaxSessionTTL is the hard cap on emergency-lane sessions
// (docs §6.7: "session TTL forced ≤ 30 minutes"). Enforced here as a
// hard deadline from open (not idle), so an emergency session cannot be
// kept alive by poking it.
const EmergencyMaxSessionTTL = 30 * time.Minute

// validateEmergencyPair checks the emergency/incident_id pair and the
// incident_id format. Returns "" when valid, else the stable refusal
// code. DECISION (docs §6.7 closeout): incident_id WITHOUT emergency is
// REFUSED, not ignored — silently dropping a security-relevant field is
// the failure mode we must not have; if the caller meant emergency, the
// pair must say so explicitly.
func validateEmergencyPair(emergency bool, incidentID string) string {
	switch {
	case emergency && incidentID == "":
		return "emergency_without_incident_id"
	case !emergency && incidentID != "":
		return "incident_id_without_emergency"
	case emergency:
		if utf8Len(incidentID) > 120 || stringsContainsControl(incidentID) {
			return "invalid_incident_id"
		}
	}
	return ""
}

func utf8Len(s string) int {
	n := 0
	for range s {
		n++
	}
	return n
}

func stringsContainsControl(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

func (s *Server) handleExec(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	var req execRequest
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil || len(body) > maxBodyBytes {
		writeErr(w, http.StatusRequestEntityTooLarge, "body_too_large")
		return
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request")
		return
	}
	if req.Host == "" || req.User == "" || req.Command == "" {
		writeErr(w, http.StatusBadRequest, "host, user and command are required")
		return
	}
	recID := newID()
	rec, recErr := s.newRecorder(r.Context(), recID, recorder.Meta{
		ResourceID: req.ResourceID, AgentID: req.AgentID, TaskID: req.TaskID,
		Host: req.Host, User: req.User, Command: req.Command,
	})
	timeout := time.Duration(req.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	if timeout > 300*time.Second {
		timeout = 300 * time.Second
	}
	sshReq := sshexec.Request{
		Host: req.Host, User: req.User, Command: req.Command, Timeout: timeout,
		Auth: sshexec.Auth{
			Mode:          req.Auth.Mode,
			PrivateKeyPEM: req.Auth.PrivateKeyPEM,
			CertTTL:       time.Duration(req.Auth.CertTTLMinutes) * time.Minute,
			KnownHosts:    req.Auth.KnownHosts,
		},
		CAMint: s.cfg.CAMint,
	}
	res, err := sshexec.Exec(r.Context(), sshReq)
	if rec != nil {
		if res != nil {
			_ = rec.AppendOut([]byte(res.Stdout))
			_ = rec.AppendOut([]byte(res.Stderr))
		}
		_ = rec.Close(context.Background())
	}
	if err != nil {
		switch {
		case errors.Is(err, sshexec.ErrHostVerificationRequired):
			writeErr(w, http.StatusBadRequest, "host_verification_required")
		case errors.Is(err, sshexec.ErrHostVerificationFailed):
			writeErr(w, http.StatusBadGateway, "host_verification_failed")
		case errors.Is(err, sshexec.ErrBadKey):
			writeErr(w, http.StatusBadRequest, "invalid_private_key")
		case errors.Is(err, sshexec.ErrTimeout):
			// timeout still yields a structured 200 below
		default:
			if caUnavailable(err) {
				writeErr(w, http.StatusBadGateway, "ca_unavailable")
				return
			}
			s.logger.Warn("exec failed", "host", req.Host, "err", err)
			writeErr(w, http.StatusBadGateway, "ssh_failed")
			return
		}
	}
	out := map[string]any{"exit_code": -1}
	if res != nil {
		out["stdout"] = res.Stdout
		out["stderr"] = res.Stderr
		out["exit_code"] = res.ExitCode
	}
	out["recording_id"] = recID
	if recErr != nil {
		out["recording_error"] = recErr.Error()
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleSessionOpen(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	var req execRequest
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil || len(body) > maxBodyBytes {
		writeErr(w, http.StatusRequestEntityTooLarge, "body_too_large")
		return
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request")
		return
	}
	if req.Host == "" || req.User == "" {
		writeErr(w, http.StatusBadRequest, "host and user are required")
		return
	}
	if code := validateEmergencyPair(req.Emergency, req.IncidentID); code != "" {
		writeErr(w, http.StatusBadRequest, code)
		return
	}
	// Emergency lane: clamp the ephemeral-cert TTL too, so the SSH
	// credential cannot outlive the 30-minute session ceiling even if
	// the gateway's clamp were bypassed (defense in depth).
	if req.Emergency && req.Auth.CertTTLMinutes > 30 {
		req.Auth.CertTTLMinutes = 30
	}
	recID := newID()
	rec, recErr := s.newRecorder(r.Context(), recID, recorder.Meta{
		ResourceID: req.ResourceID, AgentID: req.AgentID, TaskID: req.TaskID,
		Host: req.Host, User: req.User,
		Emergency: req.Emergency, IncidentID: req.IncidentID,
	})
	sessID := newID()
	sess := &session{
		id:          sessID,
		recordingID: recID,
		events:      &eventLog{},
		lastActive:  time.Now(),
		rec:         rec,
		emergency:   req.Emergency,
		incidentID:  req.IncidentID,
	}
	if req.Emergency {
		sess.deadline = time.Now().Add(EmergencyMaxSessionTTL)
	}
	if err := s.sessions.add(sess); err != nil {
		writeErr(w, http.StatusTooManyRequests, "session_limit")
		return
	}
	sshReq := sshexec.Request{
		Host: req.Host, User: req.User,
		Auth: sshexec.Auth{
			Mode:          req.Auth.Mode,
			PrivateKeyPEM: req.Auth.PrivateKeyPEM,
			CertTTL:       time.Duration(req.Auth.CertTTLMinutes) * time.Minute,
			KnownHosts:    req.Auth.KnownHosts,
		},
		CAMint: s.cfg.CAMint,
	}
	onOut := func(chunk []byte) {
		sess.appendEvent("out", chunk)
		if rec != nil {
			_ = rec.AppendOut(chunk)
		}
	}
	sshSess, err := s.cfg.Sessions.Open(r.Context(), sshReq, onOut)
	if err != nil {
		s.sessions.remove(sessID)
		if rec != nil {
			_ = rec.Close(context.Background())
		}
		switch {
		case errors.Is(err, sshexec.ErrHostVerificationRequired):
			writeErr(w, http.StatusBadRequest, "host_verification_required")
		case errors.Is(err, sshexec.ErrHostVerificationFailed):
			writeErr(w, http.StatusBadGateway, "host_verification_failed")
		case errors.Is(err, sshexec.ErrBadKey):
			writeErr(w, http.StatusBadRequest, "invalid_private_key")
		default:
			if caUnavailable(err) {
				writeErr(w, http.StatusBadGateway, "ca_unavailable")
				return
			}
			s.logger.Warn("session open failed", "host", req.Host, "err", err)
			writeErr(w, http.StatusBadGateway, "ssh_failed")
		}
		return
	}
	sess.attach(sshSess)
	out := map[string]string{
		"session_id":   sessID,
		"recording_id": recID,
	}
	if req.Emergency {
		out["emergency"] = "true"
		out["incident_id"] = req.IncidentID
		out["expires_at"] = sess.deadline.UTC().Format(time.RFC3339)
	}
	if recErr != nil {
		out["recording_error"] = recErr.Error()
	}
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) handleSessionRoutes(w http.ResponseWriter, r *http.Request) {
	// /v1/sessions/{id}/{action}
	path := trim(r.URL.Path, "/v1/sessions/")
	parts := splitPath(path)
	if len(parts) != 2 {
		writeErr(w, http.StatusNotFound, "not_found")
		return
	}
	id, action := parts[0], parts[1]
	sess, ok := s.sessions.get(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "session_not_found")
		return
	}
	switch {
	case action == "send" && r.Method == http.MethodPost:
		var body struct {
			StdinB64 string `json:"stdin_b64"`
		}
		raw, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
		if err != nil || len(raw) > maxBodyBytes {
			writeErr(w, http.StatusRequestEntityTooLarge, "body_too_large")
			return
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request")
			return
		}
		data, err := base64.StdEncoding.DecodeString(body.StdinB64)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad_base64")
			return
		}
		if sess.ssh == nil || sess.ssh.Closed() {
			writeErr(w, http.StatusConflict, "session_closed")
			return
		}
		if err := sess.ssh.Send(data); err != nil {
			writeErr(w, http.StatusConflict, "session_closed")
			return
		}
		sess.appendEvent("in", data)
		if rec := s.recorderFor(sess); rec != nil {
			_ = rec.AppendIn(data)
		}
		s.sessions.touch(sess)
		writeJSON(w, http.StatusOK, map[string]any{})
	case action == "events" && r.Method == http.MethodGet:
		cursor := 0
		if c := r.URL.Query().Get("cursor"); c != "" {
			if n, err := strconv.Atoi(c); err == nil && n > 0 {
				cursor = n
			}
		}
		events, next := sess.events.since(cursor)
		closed := sess.ssh == nil || sess.ssh.Closed()
		writeJSON(w, http.StatusOK, map[string]any{
			"cursor": next,
			"events": events,
			"closed": closed,
		})
	case action == "kill" && r.Method == http.MethodPost:
		s.closeSession(sess)
		writeJSON(w, http.StatusOK, map[string]any{})
	default:
		writeErr(w, http.StatusNotFound, "not_found")
	}
}

func (s *Server) recorderFor(sess *session) *recorder.Recorder {
	return sess.rec
}

func (s *Server) closeSession(sess *session) {
	if sess.ssh != nil {
		sess.ssh.Close()
	}
	if sess.rec != nil {
		_ = sess.rec.Close(context.Background())
	}
	s.sessions.remove(sess.id)
}

func (s *Server) newRecorder(ctx context.Context, id string, meta recorder.Meta) (*recorder.Recorder, error) {
	if s.cfg.RecorderSinkFactory == nil {
		return nil, nil
	}
	sink, err := s.cfg.RecorderSinkFactory(ctx, id)
	if err != nil {
		return nil, err
	}
	return recorder.New(sink, id, meta)
}

// ---- session registry ----

type session struct {
	id          string
	recordingID string
	events      *eventLog
	lastActive  time.Time
	ssh         LiveSession
	rec         *recorder.Recorder
	// emergency lane (TG-11): hard deadline from open + incident stamp.
	emergency  bool
	incidentID string
	deadline   time.Time // zero = no hard deadline
	mu         sync.Mutex
}

func (s *session) attach(sshSess LiveSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ssh = sshSess
}

func (s *session) appendEvent(stream string, data []byte) {
	s.events.append(stream, data)
	s.mu.Lock()
	s.lastActive = time.Now()
	s.mu.Unlock()
}

// expiredAt reports idle expiry OR the emergency hard deadline
// (TG-11 §6.7: TTL forced ≤ 30 min, counted from open, not reset by
// activity). Session fields are guarded by s.mu; the sweeper holds no
// session lock here, but lastActive/deadline writes happen under
// session mu — read them via a locked snapshot helper instead.
func (r *registry) expiredAt(s *session, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if now.Sub(s.lastActive) > r.idleTTL {
		return true
	}
	return !s.deadline.IsZero() && !now.Before(s.deadline)
}

type registry struct {
	mu       sync.Mutex
	byID     map[string]*session
	max      int
	idleTTL  time.Duration
	stopOnce sync.Once
	stop     chan struct{}
}

func newRegistry(max int, idle time.Duration) *registry {
	r := &registry{byID: map[string]*session{}, max: max, idleTTL: idle, stop: make(chan struct{})}
	go r.sweeper()
	return r
}

func (r *registry) add(s *session) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.byID) >= r.max {
		return errors.New("session_limit")
	}
	r.byID[s.id] = s
	return nil
}

func (r *registry) get(id string) (*session, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.byID[id]
	return s, ok
}

func (r *registry) remove(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byID, id)
}

func (r *registry) touch(s *session) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s.mu.Lock()
	s.lastActive = time.Now()
	s.mu.Unlock()
}

func (r *registry) sweeper() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-r.stop:
			return
		case now := <-t.C:
			r.mu.Lock()
			for id, s := range r.byID {
				expired := r.expiredAt(s, now)
				s.mu.Lock()
				closed := s.ssh != nil && s.ssh.Closed()
				s.mu.Unlock()
				if expired || closed {
					if s.ssh != nil {
						s.ssh.Close()
					}
					if s.rec != nil {
						_ = s.rec.Close(context.Background())
					}
					delete(r.byID, id)
				}
			}
			r.mu.Unlock()
		}
	}
}

// Stop halts the sweeper (tests).
func (r *registry) Stop() { r.stopOnce.Do(func() { close(r.stop) }) }

// ---- event log (cursor-based) ----

// Event is one incremental output/input event.
type Event struct {
	TMS    int64  `json:"t_ms"`
	Stream string `json:"stream"`
	Data   string `json:"data_b64"`
}

type eventLog struct {
	mu     sync.Mutex
	events []Event
}

func (l *eventLog) append(stream string, data []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, Event{
		TMS:    time.Now().UnixMilli(),
		Stream: stream,
		Data:   base64.StdEncoding.EncodeToString(data),
	})
}

// since returns events after cursor (exclusive) and the new cursor.
func (l *eventLog) since(cursor int) ([]Event, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if cursor > len(l.events) {
		cursor = len(l.events)
	}
	out := make([]Event, len(l.events)-cursor)
	copy(out, l.events[cursor:])
	return out, len(l.events)
}

// ---- helpers ----

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func trim(s, prefix string) string {
	if len(s) >= len(prefix) && s[:len(prefix)] == prefix {
		return s[len(prefix):]
	}
	return s
}

func splitPath(p string) []string {
	out := []string{}
	cur := ""
	for _, c := range p {
		if c == '/' {
			if cur != "" {
				out = append(out, cur)
				cur = ""
			}
			continue
		}
		cur += string(c)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

func caUnavailable(err error) bool {
	return err != nil && (errors.Is(err, errCAUnavailable) || contains(err.Error(), "ca_unavailable") || contains(err.Error(), "ca_mint_failed"))
}

var errCAUnavailable = errors.New("ca_unavailable")

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
