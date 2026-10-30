package service

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/rossbrigoli/skquad/build-service/internal/buildspec"
	"github.com/rossbrigoli/skquad/build-service/internal/config"
	"github.com/rossbrigoli/skquad/build-service/internal/kanikojob"
	"github.com/rossbrigoli/skquad/build-service/internal/principal"
	"github.com/rossbrigoli/skquad/build-service/internal/revision"
)

// ── fakes ──────────────────────────────────────────────────────────────

type fakeStore struct {
	mu       sync.Mutex
	jobs     map[string]*batchv1.Job
	pods     map[string][]corev1.Pod
	createOK int
}

func newFakeStore() *fakeStore {
	return &fakeStore{jobs: map[string]*batchv1.Job{}, pods: map[string][]corev1.Pod{}}
}

func (f *fakeStore) CreateJob(_ context.Context, job *batchv1.Job) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jobs[job.Name] = job
	f.createOK++
	return nil
}

func (f *fakeStore) GetJob(_ context.Context, ns, name string) (*batchv1.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if j, ok := f.jobs[name]; ok {
		return j, nil
	}
	return nil, errors.New("not found")
}

func (f *fakeStore) ListJobs(_ context.Context, ns, sel string) ([]batchv1.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []batchv1.Job
	for _, j := range f.jobs {
		if matchesSelector(j.Labels, sel) {
			out = append(out, *j)
		}
	}
	return out, nil
}

func (f *fakeStore) ListPods(_ context.Context, ns, sel string) ([]corev1.Pod, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []corev1.Pod
	for _, ps := range f.pods {
		for _, p := range ps {
			if matchesSelector(p.Labels, sel) {
				out = append(out, p)
			}
		}
	}
	return out, nil
}

// matchesSelector: simple equality-only matcher for "k=v,k2=v2".
func matchesSelector(labels map[string]string, sel string) bool {
	if sel == "" {
		return true
	}
	for _, kv := range strings.Split(sel, ",") {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) != 2 || labels[parts[0]] != parts[1] {
			return false
		}
	}
	return true
}

type fakeScope struct {
	allowed map[string][]string // agentID → repos_allow
	err     error
}

func (f *fakeScope) RepoAllowed(_ context.Context, agentID, orgRepo string) error {
	if f.err != nil {
		return f.err
	}
	if buildspec.RepoAllowed(f.allowed[agentID], orgRepo) {
		return nil
	}
	return errors.New("not covered")
}

type fakeRev struct {
	sha string
	err error
}

func (f *fakeRev) Resolve(_ context.Context, _, _ string) (string, error) { return f.sha, f.err }

// ── helpers ────────────────────────────────────────────────────────────

func testConfig() *config.Config {
	return &config.Config{
		Listen:            ":0",
		Namespace:         "skquad-builds",
		KanikoImage:       "gcr.io/kaniko-project/executor:v1.24.0",
		RegistrySecret:    "reg",
		PrincipalsFile:    "/tmp/p",
		GitHostsAllow:     []string{"github.com"},
		DestinationsAllow: []string{"docker.io/brigss007/agent-builds/"},
		Limits:            config.Limits{MaxConcurrentPerPrincipal: 1, MaxPerHourPerPrincipal: 2},
		Job:               config.JobConfig{ActiveDeadlineSeconds: 600, TTLSecondsAfterFinished: 600},
	}
}

func testPrincipal() *principal.Principal {
	return &principal.Principal{
		ID:                "sherlock",
		AgentID:           "agent-1",
		TokenSHA256B64:    "AAAA",
		ReposAllow:        []string{"rossbrigoli/*"},
		DestinationsAllow: []string{"docker.io/brigss007/agent-builds/"},
	}
}

func goodRequest() buildspec.Request {
	return buildspec.Request{
		Repo:        "github.com/rossbrigoli/skquad",
		Revision:    "main",
		Dockerfile:  "build-service/Dockerfile",
		Destination: "docker.io/brigss007/agent-builds/skquad-build-service:dev-1",
	}
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})) }

func newSvc(cfg *config.Config, store JobStore, scope ScopeChecker, rev resolver) *Service {
	// rev is passed as the concrete resolver interface to allow fakes.
	s := New(cfg, store, scope, nil, quietLogger())
	s.revResolver = rev
	return s
}

// resolver is the minimal interface the service uses from *revision.Resolver.
type resolver interface {
	Resolve(ctx context.Context, gitURL, rev string) (string, error)
}

var _ = revision.IsFullSHA // keep import for parity checks

const fullA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// ── tests ──────────────────────────────────────────────────────────────

func TestSubmitHappyPath(t *testing.T) {
	store := newFakeStore()
	scope := &fakeScope{allowed: map[string][]string{"agent-1": {"rossbrigoli/*"}}}
	svc := newSvc(testConfig(), store, scope, &fakeRev{sha: fullA})

	b, err := svc.Submit(context.Background(), testPrincipal(), goodRequest())
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if b.Status != kanikojob.StatusPending || b.ResolvedSHA != fullA || b.Principal != "sherlock" {
		t.Fatalf("bad build: %+v", b)
	}
	if len(store.jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(store.jobs))
	}
	job := store.jobs[b.JobName]
	if job.Labels[kanikojob.LabelBuildID] != b.ID {
		t.Fatal("job missing build-id label")
	}
}

func TestSubmitDenyValidation(t *testing.T) {
	svc := newSvc(testConfig(), newFakeStore(), nil, &fakeRev{sha: fullA})
	_, err := svc.Submit(context.Background(), testPrincipal(), buildspec.Request{})
	var se *Error
	if !errors.As(err, &se) || se.Kind != KindValidation {
		t.Fatalf("expected KindValidation, got %v", err)
	}
}

func TestSubmitDenyHost(t *testing.T) {
	req := goodRequest()
	req.Repo = "gitlab.com/rossbrigoli/skquad"
	svc := newSvc(testConfig(), newFakeStore(), nil, &fakeRev{sha: fullA})
	_, err := svc.Submit(context.Background(), testPrincipal(), req)
	var se *Error
	if !errors.As(err, &se) || se.Kind != KindForbidden {
		t.Fatalf("expected KindForbidden for non-allowlisted host, got %v", err)
	}
}

func TestSubmitDenyPrincipalRepo(t *testing.T) {
	p := testPrincipal()
	p.ReposAllow = []string{"otherorg/*"}
	svc := newSvc(testConfig(), newFakeStore(), nil, &fakeRev{sha: fullA})
	_, err := svc.Submit(context.Background(), p, goodRequest())
	var se *Error
	if !errors.As(err, &se) || se.Kind != KindForbidden {
		t.Fatalf("expected KindForbidden, got %v", err)
	}
}

func TestSubmitDenyCPScopeFailClosed(t *testing.T) {
	scope := &fakeScope{err: errors.New("cp unreachable")}
	svc := newSvc(testConfig(), newFakeStore(), scope, &fakeRev{sha: fullA})
	_, err := svc.Submit(context.Background(), testPrincipal(), goodRequest())
	var se *Error
	if !errors.As(err, &se) || se.Kind != KindForbidden {
		t.Fatalf("policy fetch failure must deny (fail-closed), got %v", err)
	}
}

func TestSubmitDenyDestinationGlobal(t *testing.T) {
	cfg := testConfig()
	cfg.DestinationsAllow = []string{"ghcr.io/rossbrigoli/agent-builds/"}
	svc := newSvc(cfg, newFakeStore(), nil, &fakeRev{sha: fullA})
	_, err := svc.Submit(context.Background(), testPrincipal(), goodRequest())
	var se *Error
	if !errors.As(err, &se) || se.Kind != KindForbidden {
		t.Fatalf("expected global destination deny, got %v", err)
	}
}

func TestSubmitDenyDestinationPrincipal(t *testing.T) {
	p := testPrincipal()
	p.DestinationsAllow = []string{"ghcr.io/rossbrigoli/agent-builds/"}
	svc := newSvc(testConfig(), newFakeStore(), nil, &fakeRev{sha: fullA})
	_, err := svc.Submit(context.Background(), p, goodRequest())
	var se *Error
	if !errors.As(err, &se) || se.Kind != KindForbidden {
		t.Fatalf("expected principal destination deny, got %v", err)
	}
}

func TestSubmitRevisionNotFound(t *testing.T) {
	svc := newSvc(testConfig(), newFakeStore(), nil, &fakeRev{err: revision.ErrNotFound})
	_, err := svc.Submit(context.Background(), testPrincipal(), goodRequest())
	var se *Error
	if !errors.As(err, &se) || se.Kind != KindNotFound {
		t.Fatalf("expected KindNotFound, got %v", err)
	}
}

func TestSubmitRevisionProbeFailed(t *testing.T) {
	svc := newSvc(testConfig(), newFakeStore(), nil, &fakeRev{err: revision.ErrProbeFailed})
	_, err := svc.Submit(context.Background(), testPrincipal(), goodRequest())
	var se *Error
	if !errors.As(err, &se) || se.Kind != KindUpstream {
		t.Fatalf("expected KindUpstream, got %v", err)
	}
}

func TestSubmitHourlyQuota(t *testing.T) {
	cfg := testConfig()
	cfg.Limits.MaxPerHourPerPrincipal = 2
	cfg.Limits.MaxConcurrentPerPrincipal = 100
	store := newFakeStore()
	svc := newSvc(cfg, store, nil, &fakeRev{sha: fullA})
	p := testPrincipal()
	for i := 0; i < 2; i++ {
		if _, err := svc.Submit(context.Background(), p, goodRequest()); err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
	}
	_, err := svc.Submit(context.Background(), p, goodRequest())
	var se *Error
	if !errors.As(err, &se) || se.Kind != KindQuota {
		t.Fatalf("expected KindQuota on 3rd hourly build, got %v", err)
	}
}

func TestSubmitConcurrentQuota(t *testing.T) {
	cfg := testConfig()
	cfg.Limits.MaxPerHourPerPrincipal = 100
	cfg.Limits.MaxConcurrentPerPrincipal = 1
	store := newFakeStore()
	svc := newSvc(cfg, store, nil, &fakeRev{sha: fullA})
	p := testPrincipal()
	if _, err := svc.Submit(context.Background(), p, goodRequest()); err != nil {
		t.Fatal(err)
	}
	// First job is still pending → second submit must hit the concurrency gate.
	_, err := svc.Submit(context.Background(), p, goodRequest())
	var se *Error
	if !errors.As(err, &se) || se.Kind != KindQuota {
		t.Fatalf("expected KindQuota on concurrent submit, got %v", err)
	}
}

func TestGetOwnershipAndDigest(t *testing.T) {
	store := newFakeStore()
	svc := newSvc(testConfig(), store, nil, &fakeRev{sha: fullA})
	b, err := svc.Submit(context.Background(), testPrincipal(), goodRequest())
	if err != nil {
		t.Fatal(err)
	}
	// Attach a succeeded job + terminated pod with digest.
	job := store.jobs[b.JobName]
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	digest := "sha256:" + strings.Repeat("e", 64)
	store.pods[job.Name] = []corev1.Pod{{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kaniko-pod",
			Namespace: "skquad-builds",
			Labels: map[string]string{
				"app":                              kanikojob.AppValue,
				"batch.kubernetes.io/job-name":     job.Name,
			},
		},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name:  "kaniko",
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: "pushed " + digest}},
		}}},
	}}

	got, err := svc.Get(context.Background(), testPrincipal(), b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != kanikojob.StatusSucceeded || got.Digest != digest {
		t.Fatalf("bad get: %+v", got)
	}

	// Another principal must NOT see it.
	other := testPrincipal()
	other.ID = "mallory"
	if _, err := svc.Get(context.Background(), other, b.ID); err == nil {
		t.Fatal("cross-principal get must 404")
	}

	// Unknown/garbage id → not found.
	if _, err := svc.Get(context.Background(), testPrincipal(), "zzz"); err == nil {
		t.Fatal("garbage id must 404")
	}
}

func TestListScopedToPrincipal(t *testing.T) {
	store := newFakeStore()
	svc := newSvc(testConfig(), store, nil, &fakeRev{sha: fullA})
	if _, err := svc.Submit(context.Background(), testPrincipal(), goodRequest()); err != nil {
		t.Fatal(err)
	}
	mine, err := svc.List(context.Background(), testPrincipal())
	if err != nil || len(mine) != 1 {
		t.Fatalf("mine: %d err %v", len(mine), err)
	}
	other := testPrincipal()
	other.ID = "mallory"
	theirs, err := svc.List(context.Background(), other)
	if err != nil || len(theirs) != 0 {
		t.Fatalf("theirs must be empty: %d err %v", len(theirs), err)
	}
}

func TestHourlyWindowDecays(t *testing.T) {
	cfg := testConfig()
	cfg.Limits.MaxPerHourPerPrincipal = 1
	cfg.Limits.MaxConcurrentPerPrincipal = 100
	svc := New(cfg, newFakeStore(), nil, nil, quietLogger())
	svc.revResolver = &fakeRev{sha: fullA}
	p := testPrincipal()
	if _, err := svc.Submit(context.Background(), p, goodRequest()); err != nil {
		t.Fatal(err)
	}
	// Manually age the recorded submit beyond the window.
	svc.mu.Lock()
	svc.history[p.ID] = []time.Time{time.Now().Add(-2 * time.Hour)}
	svc.mu.Unlock()
	if _, err := svc.Submit(context.Background(), p, goodRequest()); err != nil {
		t.Fatalf("aged window must allow new build: %v", err)
	}
}
