package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/rossbrigoli/skquad/terminal-service/internal/apply"
)

// ApplyStatusPending is the pre-run state of a queued apply.
const ApplyStatusPending = "pending"

// ApplyEngine runs artifact applies (§6.7). *apply.Engine satisfies it;
// tests inject a stub — mirrors the SessionStarter pattern.
type ApplyEngine interface {
	Apply(ctx context.Context, req apply.Request) (*apply.Result, error)
}

// applyJob tracks one async apply in the registry.
type applyJob struct {
	mu         sync.Mutex
	id         string
	status     string // pending|running|succeeded|failed|refused
	startedAt  time.Time
	finishedAt time.Time
	result     *apply.Result
	errMsg     string
}

// applyRegistry is a bounded in-memory job store (same style as the
// session registry). Finished jobs stay queryable until evicted by the
// bound; there is no persistence — the recorder is the audit trail.
type applyRegistry struct {
	mu   sync.Mutex
	byID map[string]*applyJob
	max  int
}

func newApplyRegistry(max int) *applyRegistry {
	return &applyRegistry{byID: map[string]*applyJob{}, max: max}
}

func (r *applyRegistry) add(j *applyJob) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.byID) >= r.max {
		return errors.New("apply_limit")
	}
	r.byID[j.id] = j
	return nil
}

func (r *applyRegistry) get(id string) (*applyJob, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	j, ok := r.byID[id]
	return j, ok
}

// applyRequestBody mirrors apply.Request over the wire.
type applyRequestBody struct {
	ResourceID     string   `json:"resource_id"`
	AgentID        string   `json:"agent_id"`
	Playbook       string   `json:"playbook"`
	GitRev         string   `json:"git_rev"`
	HostGroup      string   `json:"host_group"`
	Hosts          []string `json:"hosts"`
	CheckOnly      bool     `json:"check_only"`
	RequireTip     bool     `json:"require_tip"`
	GitURL         string   `json:"git_url"`
	DefaultBranch  string   `json:"default_branch"`
	PlaybooksPath  string   `json:"playbooks_path"`
	SSHUser        string   `json:"ssh_user"`
	KnownHosts     string   `json:"known_hosts"`
	CertTTLSeconds int      `json:"cert_ttl_seconds"`
	StaticKeyPEM   string   `json:"static_key_pem"`
	MirrorsAllow   []string `json:"mirrors_allow"`
	DenyPatterns   []string `json:"deny_patterns"`
	TimeoutSeconds int      `json:"timeout_seconds"`
}

// handleApplyCreate accepts an apply, validates it, and runs it
// asynchronously: 202 + {id, status}. The run's context is detached
// from the request (the client polls) but bounded by
// apply.DefaultApplyTimeout.
func (s *Server) handleApplyCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil || len(body) > maxBodyBytes {
		writeErr(w, http.StatusRequestEntityTooLarge, "body_too_large")
		return
	}
	var in applyRequestBody
	if err := json.Unmarshal(body, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request")
		return
	}
	switch {
	case in.Playbook == "":
		writeErr(w, http.StatusBadRequest, "playbook is required")
		return
	case in.GitRev == "":
		writeErr(w, http.StatusBadRequest, "git_rev is required")
		return
	case in.GitURL == "":
		writeErr(w, http.StatusBadRequest, "git_url is required")
		return
	case in.DefaultBranch == "":
		writeErr(w, http.StatusBadRequest, "default_branch is required")
		return
	case in.SSHUser == "":
		writeErr(w, http.StatusBadRequest, "ssh_user is required")
		return
	case len(in.Hosts) == 0:
		writeErr(w, http.StatusBadRequest, "hosts must not be empty")
		return
	}
	req := apply.Request{
		ResourceID:    in.ResourceID,
		AgentID:       in.AgentID,
		Playbook:      in.Playbook,
		GitRev:        in.GitRev,
		HostGroup:     in.HostGroup,
		Hosts:         in.Hosts,
		CheckOnly:     in.CheckOnly,
		RequireTip:    in.RequireTip,
		GitURL:        in.GitURL,
		DefaultBranch: in.DefaultBranch,
		PlaybooksPath: in.PlaybooksPath,
		SSHUser:       in.SSHUser,
		KnownHosts:    in.KnownHosts,
		CertTTL:       time.Duration(in.CertTTLSeconds) * time.Second,
		StaticKeyPEM:  in.StaticKeyPEM,
		MirrorsAllow:  in.MirrorsAllow,
		DenyPatterns:  in.DenyPatterns,
	}
	// Bounded by the engine's global cap; a shorter client timeout is honored.
	timeout := apply.DefaultApplyTimeout
	if in.TimeoutSeconds > 0 && time.Duration(in.TimeoutSeconds)*time.Second < timeout {
		timeout = time.Duration(in.TimeoutSeconds) * time.Second
	}
	req.Timeout = timeout

	id := newID()
	job := &applyJob{id: id, status: ApplyStatusPending, startedAt: time.Now()}
	if err := s.applies.add(job); err != nil {
		writeErr(w, http.StatusTooManyRequests, "apply_limit")
		return
	}
	req.ApplyID = id
	go s.runApply(job, req, timeout)
	writeJSON(w, http.StatusAccepted, map[string]string{"id": id, "status": ApplyStatusPending})
}

// runApply executes the engine detached from the HTTP request context,
// bounded by timeout, and records the terminal state in the job.
func (s *Server) runApply(job *applyJob, req apply.Request, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	job.mu.Lock()
	job.status = apply.StatusRunning
	job.startedAt = time.Now()
	job.mu.Unlock()

	res, err := s.cfg.ApplyEngine.Apply(ctx, req)
	job.mu.Lock()
	defer job.mu.Unlock()
	job.finishedAt = time.Now()
	if err != nil {
		job.status = apply.StatusFailed
		job.errMsg = err.Error()
		s.logger.Warn("apply failed", "apply_id", job.id, "err", err)
		return
	}
	job.result = res
	job.status = res.Status // succeeded | failed | refused
}

// handleApplyRoutes serves GET /v1/applies/{id}.
func (s *Server) handleApplyRoutes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	parts := splitPath(trim(r.URL.Path, "/v1/applies/"))
	if len(parts) != 1 {
		writeErr(w, http.StatusNotFound, "not_found")
		return
	}
	job, ok := s.applies.get(parts[0])
	if !ok {
		writeErr(w, http.StatusNotFound, "apply_not_found")
		return
	}
	job.mu.Lock()
	defer job.mu.Unlock()
	out := map[string]any{
		"id":         job.id,
		"status":     job.status,
		"started_at": job.startedAt,
	}
	if !job.finishedAt.IsZero() {
		out["finished_at"] = job.finishedAt
	}
	if job.errMsg != "" {
		out["error"] = job.errMsg
	}
	if job.result != nil {
		out["exit_code"] = job.result.ExitCode
		if job.result.RefusalReason != "" {
			out["refusal_reason"] = job.result.RefusalReason
		}
		if job.result.RecordingID != "" {
			out["recording_id"] = job.result.RecordingID
		}
		if job.result.PerHost != nil {
			out["per_host"] = job.result.PerHost
		}
		if job.result.LintFindings != nil {
			out["lint_findings"] = job.result.LintFindings
		}
	}
	writeJSON(w, http.StatusOK, out)
}
