// Package service orchestrates the governed build path:
//
//	authenticate → validate → scope-check → pin revision → quota → schedule kaniko Job
//
// Every gate is a separate, individually-testable step, and every
// acceptance or denial produces an audit event. The Kubernetes surface
// is behind a narrow interface (JobStore) so the whole policy engine is
// unit-testable with a fake store and no cluster.
package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/rossbrigoli/skquad/build-service/internal/buildspec"
	"github.com/rossbrigoli/skquad/build-service/internal/config"
	"github.com/rossbrigoli/skquad/build-service/internal/kanikojob"
	"github.com/rossbrigoli/skquad/build-service/internal/principal"
	"github.com/rossbrigoli/skquad/build-service/internal/revision"
)

// HTTP-mappable error kinds.
type ErrorKind int

const (
	KindValidation ErrorKind = iota // 400
	KindForbidden                   // 403
	KindNotFound                    // 404
	KindQuota                       // 429
	KindUpstream                    // 502
	KindInternal                    // 500
)

// Error is a service error carrying a kind for the HTTP layer plus a
// safe (agent-visible) message.
type Error struct {
	Kind    ErrorKind
	Message string
}

func (e *Error) Error() string { return e.Message }

// JobStore is the narrow Kubernetes surface the service needs.
type JobStore interface {
	CreateJob(ctx context.Context, job *batchv1.Job) error
	GetJob(ctx context.Context, namespace, name string) (*batchv1.Job, error)
	ListJobs(ctx context.Context, namespace, labelSelector string) ([]batchv1.Job, error)
	// ListPods backs digest extraction (kaniko's digest lands in the
	// pod termination message, not the Job status).
	ListPods(ctx context.Context, namespace, labelSelector string) ([]corev1.Pod, error)
}

// ScopeChecker verifies the agent's live git-grant scope. Production:
// *policyclient.Client. Tests: fake.
type ScopeChecker interface {
	RepoAllowed(ctx context.Context, agentID, orgRepo string) error
}

// Build is the service-level view of one build.
type Build struct {
	ID          string     `json:"build_id"`
	Principal   string     `json:"principal"`
	Repo        string     `json:"repo"`
	Revision    string     `json:"revision"`
	ResolvedSHA string     `json:"resolved_sha"`
	Dockerfile  string     `json:"dockerfile"`
	Destination string     `json:"destination"`
	JobName     string     `json:"job_name"`
	Status      string     `json:"status"`
	Digest      string     `json:"digest,omitempty"`
	Error       string     `json:"error,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
}

// revisionResolver is the minimal interface the service uses to pin
// revisions (production: *revision.Resolver; tests: fakes).
type revisionResolver interface {
	Resolve(ctx context.Context, gitURL, rev string) (string, error)
}

// Service is the build orchestrator.
type Service struct {
	cfg    *config.Config
	store  JobStore
	scope  ScopeChecker // may be nil (static-only governance)
	revResolver revisionResolver
	logger *slog.Logger

	mu       sync.Mutex
	history  map[string][]time.Time // principalID → submit times (hour window)
}

// New wires the service. scope may be nil.
func New(cfg *config.Config, store JobStore, scope ScopeChecker, rev revisionResolver, logger *slog.Logger) *Service {
	return &Service{cfg: cfg, store: store, scope: scope, revResolver: rev, logger: logger, history: map[string][]time.Time{}}
}

// Submit runs the full governance pipeline and schedules the build.
func (s *Service) Submit(ctx context.Context, p *principal.Principal, req buildspec.Request) (*Build, error) {
	// 1. Syntax normalization (pure).
	spec, verr := buildspec.Parse(req)
	if verr != nil {
		s.audit(p, "build_denied", map[string]any{"reason": verr.Error()})
		return nil, &Error{Kind: KindValidation, Message: verr.Error()}
	}
	orgRepo := orgRepoOf(spec.Repo)
	destNoTag := spec.Destination[:strings.LastIndex(spec.Destination, ":")]

	// 2. Git host floor (SSRF guard for our own probes).
	if !containsFold(s.cfg.GitHostsAllow, hostOf(spec.Repo)) {
		s.audit(p, "build_denied", map[string]any{"repo": spec.Repo, "reason": "host_not_allowed"})
		return nil, &Error{Kind: KindForbidden, Message: "git host not allowed"}
	}

	// 3. Principal repo floor.
	if !buildspec.RepoAllowed(p.ReposAllow, orgRepo) {
		s.audit(p, "build_denied", map[string]any{"repo": spec.Repo, "reason": "principal_repos_allow"})
		return nil, &Error{Kind: KindForbidden, Message: "repo not in principal allowlist"}
	}

	// 4. Live git-grant scope from the control plane (fail-closed).
	if s.scope != nil && p.AgentID != "" {
		if err := s.scope.RepoAllowed(ctx, p.AgentID, orgRepo); err != nil {
			s.audit(p, "build_denied", map[string]any{"repo": spec.Repo, "reason": "cp_git_scope", "detail": err.Error()})
			return nil, &Error{Kind: KindForbidden, Message: "repo not covered by agent git grant"}
		}
	}

	// 5. Destination: global ∧ principal prefixes must both match.
	if !buildspec.DestinationAllowed(s.cfg.DestinationsAllow, destNoTag) {
		s.audit(p, "build_denied", map[string]any{"destination": spec.Destination, "reason": "global_destinations_allow"})
		return nil, &Error{Kind: KindForbidden, Message: "destination not in global allowlist"}
	}
	if !buildspec.DestinationAllowed(p.DestinationsAllow, destNoTag) {
		s.audit(p, "build_denied", map[string]any{"destination": spec.Destination, "reason": "principal_destinations_allow"})
		return nil, &Error{Kind: KindForbidden, Message: "destination not in principal allowlist"}
	}

	// 6. Revision pinning: resolve to a full SHA before scheduling.
	gitURL := gitURLFor(spec.Repo)
	fullSHA, err := s.revResolver.Resolve(ctx, gitURL, spec.Revision)
	if err != nil {
		switch {
		case errors.Is(err, revision.ErrNotFound):
			s.audit(p, "build_denied", map[string]any{"repo": spec.Repo, "revision": spec.Revision, "reason": "revision_not_found"})
			return nil, &Error{Kind: KindNotFound, Message: "revision not found on remote"}
		case errors.Is(err, revision.ErrUnreachable):
			s.audit(p, "build_denied", map[string]any{"repo": spec.Repo, "revision": spec.Revision, "reason": "revision_unreachable"})
			return nil, &Error{Kind: KindNotFound, Message: "revision not reachable on remote"}
		default:
			s.audit(p, "build_denied", map[string]any{"repo": spec.Repo, "revision": spec.Revision, "reason": "revision_probe_failed"})
			return nil, &Error{Kind: KindUpstream, Message: "revision probe failed"}
		}
	}

	// 7. Quotas: hourly (in-process window) then concurrent (Jobs API).
	if err := s.checkHourly(p); err != nil {
		return nil, err
	}
	if err := s.checkConcurrent(ctx, p); err != nil {
		return nil, err
	}

	// 8. Schedule.
	buildID := newBuildID()
	job, err := kanikojob.Render(s.cfg, buildID, p.ID, gitURL, fullSHA, spec.Dockerfile, spec.Destination)
	if err != nil {
		return nil, &Error{Kind: KindInternal, Message: "job render failed"}
	}
	if err := s.store.CreateJob(ctx, job); err != nil {
		s.audit(p, "build_error", map[string]any{"build_id": buildID, "reason": "job_create_failed", "detail": err.Error()})
		return nil, &Error{Kind: KindInternal, Message: "failed to schedule build"}
	}
	s.recordSubmit(p.ID)
	s.audit(p, "build_submitted", map[string]any{
		"build_id": buildID, "repo": spec.Repo, "revision": spec.Revision,
		"resolved_sha": fullSHA, "dockerfile": spec.Dockerfile,
		"destination": spec.Destination, "job": job.Name,
	})
	return &Build{
		ID: buildID, Principal: p.ID, Repo: spec.Repo, Revision: spec.Revision,
		ResolvedSHA: fullSHA, Dockerfile: spec.Dockerfile, Destination: spec.Destination,
		JobName: job.Name, Status: kanikojob.StatusPending, CreatedAt: time.Now().UTC(),
	}, nil
}

// Get returns one build owned by the principal (404 otherwise — no
// cross-principal visibility).
func (s *Service) Get(ctx context.Context, p *principal.Principal, buildID string) (*Build, error) {
	if !isBuildID(buildID) {
		return nil, &Error{Kind: KindNotFound, Message: "unknown build"}
	}
	sel := fmt.Sprintf("%s=%s,%s=%s",
		kanikojob.LabelBuildID, buildID,
		kanikojob.LabelPrincipal, labelSafe(p.ID))
	jobs, err := s.store.ListJobs(ctx, s.cfg.Namespace, sel)
	if err != nil {
		return nil, &Error{Kind: KindInternal, Message: "build lookup failed"}
	}
	if len(jobs) == 0 {
		return nil, &Error{Kind: KindNotFound, Message: "unknown build"}
	}
	pods, _ := s.store.ListPods(ctx, s.cfg.Namespace,
		fmt.Sprintf("batch.kubernetes.io/job-name=%s", jobs[0].Name))
	return toBuild(&jobs[0], firstPod(&pods)), nil
}

// List returns the principal's builds (active + recently finished;
// finished jobs disappear after TTL — that is the retention policy).
func (s *Service) List(ctx context.Context, p *principal.Principal) ([]Build, error) {
	sel := fmt.Sprintf("%s=%s", kanikojob.LabelApp, kanikojob.AppValue)
	if p != nil {
		sel += fmt.Sprintf(",%s=%s", kanikojob.LabelPrincipal, labelSafe(p.ID))
	}
	jobs, err := s.store.ListJobs(ctx, s.cfg.Namespace, sel)
	if err != nil {
		return nil, &Error{Kind: KindInternal, Message: "build list failed"}
	}
	out := make([]Build, 0, len(jobs))
	// One pod list for the whole page, indexed by owning job name.
	pods, _ := s.store.ListPods(ctx, s.cfg.Namespace,
		fmt.Sprintf("%s=%s", kanikojob.LabelApp, kanikojob.AppValue))
	byJob := map[string]*corev1.Pod{}
	for i := range pods {
		if owner := ownerJobName(&pods[i]); owner != "" {
			if _, dup := byJob[owner]; !dup {
				byJob[owner] = &pods[i]
			}
		}
	}
	for i := range jobs {
		out = append(out, *toBuild(&jobs[i], byJob[jobs[i].Name]))
	}
	return out, nil
}

func firstPod(pods *[]corev1.Pod) *corev1.Pod {
	if pods == nil || len(*pods) == 0 {
		return nil
	}
	return &(*pods)[0]
}

// ownerJobName resolves the owning kaniko Job name from the pod's
// batch job-name label (preferred) or the legacy job-name label.
func ownerJobName(pod *corev1.Pod) string {
	if n := pod.Labels["batch.kubernetes.io/job-name"]; n != "" {
		return n
	}
	return pod.Labels["job-name"]
}

func (s *Service) checkHourly(p *principal.Principal) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cut := time.Now().Add(-time.Hour)
	kept := s.history[p.ID][:0]
	for _, t := range s.history[p.ID] {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	s.history[p.ID] = kept
	if len(kept) >= s.cfg.Limits.MaxPerHourPerPrincipal {
		s.audit(p, "build_denied", map[string]any{"reason": "hourly_quota"})
		return &Error{Kind: KindQuota, Message: "hourly build quota exceeded"}
	}
	return nil
}

func (s *Service) checkConcurrent(ctx context.Context, p *principal.Principal) error {
	sel := fmt.Sprintf("%s=%s,%s=%s",
		kanikojob.LabelApp, kanikojob.AppValue,
		kanikojob.LabelPrincipal, labelSafe(p.ID))
	jobs, err := s.store.ListJobs(ctx, s.cfg.Namespace, sel)
	if err != nil {
		return &Error{Kind: KindInternal, Message: "quota check failed"}
	}
	active := 0
	for i := range jobs {
		st := kanikojob.MapStatus(&jobs[i])
		if st == kanikojob.StatusPending || st == kanikojob.StatusRunning {
			active++
		}
	}
	if active >= s.cfg.Limits.MaxConcurrentPerPrincipal {
		s.audit(p, "build_denied", map[string]any{"reason": "concurrent_quota", "active": active})
		return &Error{Kind: KindQuota, Message: "concurrent build limit reached"}
	}
	return nil
}

func (s *Service) recordSubmit(principalID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.history[principalID] = append(s.history[principalID], time.Now())
}

func (s *Service) audit(p *principal.Principal, event string, fields map[string]any) {
	l := s.logger
	if p != nil {
		l = l.With("principal", p.ID, "agent_id", p.AgentID)
	}
	args := make([]any, 0, 2*len(fields)+2)
	args = append(args, "audit_event", event)
	for k, v := range fields {
		args = append(args, k, v)
	}
	l.Info("audit", args...)
}

func toBuild(job *batchv1.Job, pod *corev1.Pod) *Build {
	b := &Build{
		ID:          job.Labels[kanikojob.LabelBuildID],
		Principal:   job.Labels[kanikojob.LabelPrincipal],
		JobName:     job.Name,
		Destination: job.Annotations["skquad.io/destination"],
		ResolvedSHA: job.Annotations["skquad.io/revision"],
		Status:      kanikojob.MapStatus(job),
		Digest:      kanikojob.ExtractDigestFromPod(pod),
	}
	if !job.CreationTimestamp.IsZero() {
		b.CreatedAt = job.CreationTimestamp.Time
	}
	if job.Status.CompletionTime != nil {
		t := job.Status.CompletionTime.Time
		b.FinishedAt = &t
	}
	if b.Status == kanikojob.StatusFailed {
		for _, c := range job.Status.Conditions {
			if c.Type == batchv1.JobFailed && c.Status == "True" {
				b.Error = c.Message
				break
			}
		}
	}
	return b
}

func orgRepoOf(hostOrgRepo string) string {
	i := strings.Index(hostOrgRepo, "/")
	if i < 0 {
		return hostOrgRepo
	}
	return hostOrgRepo[i+1:]
}

func hostOf(hostOrgRepo string) string {
	if i := strings.Index(hostOrgRepo, "/"); i >= 0 {
		return hostOrgRepo[:i]
	}
	return hostOrgRepo
}

func gitURLFor(hostOrgRepo string) string {
	return "https://" + hostOrgRepo + ".git"
}

func labelSafe(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-_.")
	if len(out) > 63 {
		out = strings.Trim(out[:63], "-_.")
	}
	if out == "" {
		return "none"
	}
	return out
}

func containsFold(list []string, want string) bool {
	for _, v := range list {
		if strings.EqualFold(strings.TrimSpace(v), want) {
			return true
		}
	}
	return false
}

func newBuildID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing is catastrophic enough; timestamp fallback
		// keeps uniqueness practical without panicking the API.
		return fmt.Sprintf("t%013x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func isBuildID(s string) bool {
	if len(s) != 24 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
