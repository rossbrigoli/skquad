package drift

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rossbrigoli/skquad/terminal-service/internal/apply"
)

// --- fakes ---

type fakeLister struct {
	resources []ArtifactResource
	err       error
	calls    int
}

func (f *fakeLister) ListArtifactResources(context.Context) ([]ArtifactResource, error) {
	f.calls++
	return f.resources, f.err
}

type fakePoster struct
{
	posted []Report
	err    error
}

func (f *fakePoster) PostDriftReport(_ context.Context, r Report) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	f.posted = append(f.posted, r)
	return "report-" + r.ResourceID + "-" + r.HostGroup, nil
}

type fakeEngine struct {
	result *apply.Result
	err    error
	reqs   []apply.Request
}

func (f *fakeEngine) Apply(_ context.Context, req apply.Request) (*apply.Result, error) {
	f.reqs = append(f.reqs, req)
	if f.err != nil {
		return nil, f.err
	}
	return f.result, nil
}

type fakeTips struct {
	rev string
	err error
}

func (f *fakeTips) TipRev(_ context.Context, _, _ string) (string, error) {
	return f.rev, f.err
}

func baseResource() ArtifactResource {
	return ArtifactResource{
		ResourceID:    "res-1",
		Name:          "web-servers",
		GitURL:        "https://git.example.com/ops/pb.git",
		DefaultBranch: "main",
		PlaybooksPath: "playbooks",
		DriftPlaybook: "site.yml",
		SSHUser:       "deploy",
		KnownHosts:    "host1 ssh-ed25519 AAAA",
		HostGroups: []HostGroup{
			{Name: "web", Hosts: []string{"host1", "host2"}},
			{Name: "db", Hosts: []string{"db1"}},
		},
	}
}

const tipSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// --- tests ---

// funcEngine returns per-request results — realistic because the apply
// engine's PerHost map only ever contains the hosts of that request's
// group.
type funcEngine struct {
	fn   func(apply.Request) *apply.Result
	reqs []apply.Request
}

func (f *funcEngine) Apply(_ context.Context, req apply.Request) (*apply.Result, error) {
	f.reqs = append(f.reqs, req)
	return f.fn(req), nil
}

func TestRunnerClassifiesDriftedHosts(t *testing.T) {
	eng := &funcEngine{fn: func(req apply.Request) *apply.Result {
		switch req.HostGroup {
		case "web":
			return &apply.Result{Status: apply.StatusSucceeded, PerHost: map[string]apply.HostResult{
				"host1": {OK: 3},
				"host2": {OK: 2, Changed: 1},
			}}
		default: // db
			return &apply.Result{Status: apply.StatusSucceeded, PerHost: map[string]apply.HostResult{
				"db1": {Unreachable: 1},
			}}
		}
	}}
	poster := &fakePoster{}
	r := &Runner{
		Resources:       &fakeLister{resources: []ArtifactResource{baseResource()}},
		Poster:          poster,
		Engine:          eng,
		Tips:            &fakeTips{rev: tipSHA},
		DefaultPlaybook: "site.yml",
	}
	sum, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Checks != 2 || sum.Drifted != 2 {
		t.Fatalf("summary = %+v, want 2 checks / 2 drifted", sum)
	}
	web := poster.posted[0]
	if web.HostGroup != "web" || len(web.DriftedHosts) != 1 || web.DriftedHosts[0] != "host2" {
		t.Fatalf("web report = %+v", web)
	}
	if web.InSync {
		t.Fatalf("web report must not be in_sync")
	}
	db := poster.posted[1]
	if db.HostGroup != "db" || len(db.DriftedHosts) != 1 || db.DriftedHosts[0] != "db1" {
		t.Fatalf("db report = %+v (unreachable must count as drifted)", db)
	}
}

func TestRunnerInSyncProducesNoDrift(t *testing.T) {
	eng := &fakeEngine{result: &apply.Result{
		Status:  apply.StatusSucceeded,
		PerHost: map[string]apply.HostResult{"host1": {OK: 5}, "host2": {OK: 1, Skipped: 2}},
	}}
	poster := &fakePoster{}
	r := &Runner{
		Resources:       &fakeLister{resources: []ArtifactResource{baseResource()}},
		Poster:          poster,
		Engine:          eng,
		Tips:            &fakeTips{rev: tipSHA},
		DefaultPlaybook: "site.yml",
	}
	sum, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Checks != 2 || sum.Drifted != 0 {
		t.Fatalf("summary = %+v, want 2 checks / 0 drifted", sum)
	}
	for _, p := range poster.posted {
		if !p.InSync || len(p.DriftedHosts) != 0 {
			t.Fatalf("expected in-sync report, got %+v", p)
		}
	}
}

func TestRunnerRequestShape(t *testing.T) {
	eng := &fakeEngine{result: &apply.Result{Status: apply.StatusSucceeded, PerHost: map[string]apply.HostResult{"host1": {OK: 1}}}}
	r := &Runner{
		Resources:       &fakeLister{resources: []ArtifactResource{baseResource()}},
		Poster:          &fakePoster{},
		Engine:          eng,
		Tips:            &fakeTips{rev: tipSHA},
		DefaultPlaybook: "fallback.yml",
		CheckTimeout:    42 * time.Second,
	}
	if _, err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	req := eng.reqs[0]
	if !req.CheckOnly {
		t.Fatalf("drift must run CheckOnly")
	}
	if req.GitRev != tipSHA {
		t.Fatalf("GitRev = %q, want resolved tip %s", req.GitRev, tipSHA)
	}
	if req.RequireTip {
		t.Fatalf("RequireTip must be false — the runner pins the resolved tip itself")
	}
	if req.Playbook != "site.yml" {
		t.Fatalf("resource drift_playbook must win over the default, got %q", req.Playbook)
	}
	if req.AgentID != "drift-check" {
		t.Fatalf("AgentID = %q", req.AgentID)
	}
	if req.GitURL != baseResource().GitURL || req.SSHUser != "deploy" || req.KnownHosts == "" {
		t.Fatalf("transport fields not carried: %+v", req)
	}
	if req.Timeout != 42*time.Second {
		t.Fatalf("Timeout = %v", req.Timeout)
	}
}

func TestRunnerDefaultPlaybookFallback(t *testing.T) {
	res := baseResource()
	res.DriftPlaybook = ""
	res.HostGroups = res.HostGroups[:1]
	eng := &fakeEngine{result: &apply.Result{Status: apply.StatusSucceeded, PerHost: map[string]apply.HostResult{"host1": {OK: 1}}}}
	r := &Runner{
		Resources:       &fakeLister{resources: []ArtifactResource{res}},
		Poster:          &fakePoster{},
		Engine:          eng,
		Tips:            &fakeTips{rev: tipSHA},
		DefaultPlaybook: "fallback.yml",
	}
	if _, err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if eng.reqs[0].Playbook != "fallback.yml" {
		t.Fatalf("playbook = %q, want fallback.yml", eng.reqs[0].Playbook)
	}
}

func TestRunnerTipFailureSkipsResource(t *testing.T) {
	eng := &fakeEngine{result: &apply.Result{Status: apply.StatusSucceeded}}
	poster := &fakePoster{}
	r := &Runner{
		Resources:       &fakeLister{resources: []ArtifactResource{baseResource()}},
		Poster:          poster,
		Engine:          eng,
		Tips:            &fakeTips{err: errors.New("ls-remote failed")},
		DefaultPlaybook: "site.yml",
	}
	sum, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run must not fail on per-resource tip errors: %v", err)
	}
	if sum.Skipped != 1 || sum.Checks != 0 || len(poster.posted) != 0 || len(eng.reqs) != 0 {
		t.Fatalf("summary = %+v posted=%d engineReqs=%d, want skip-only", sum, len(poster.posted), len(eng.reqs))
	}
}

func TestRunnerRefusedCheckIsSkippedNotReported(t *testing.T) {
	eng := &fakeEngine{result: &apply.Result{Status: apply.StatusRefused, RefusalReason: apply.RefusalLintFailed}}
	poster := &fakePoster{}
	r := &Runner{
		Resources:       &fakeLister{resources: []ArtifactResource{baseResource()}},
		Poster:          poster,
		Engine:          eng,
		Tips:            &fakeTips{rev: tipSHA},
		DefaultPlaybook: "site.yml",
	}
	sum, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Checks != 0 || len(poster.posted) != 0 || sum.Skipped != 2 {
		t.Fatalf("refused checks must not produce reports: %+v posted=%d", sum, len(poster.posted))
	}
}

func TestRunnerListingFailurePropagates(t *testing.T) {
	r := &Runner{
		Resources: &fakeLister{err: errors.New("cp unreachable")},
		Poster:    &fakePoster{},
		Engine:    &fakeEngine{},
		Tips:      &fakeTips{rev: tipSHA},
	}
	if _, err := r.Run(context.Background()); err == nil {
		t.Fatalf("listing failure must propagate (CronJob should show a failed run)")
	}
}

func TestRunnerPostFailureSkipsGroupContinues(t *testing.T) {
	res := baseResource()
	res2 := baseResource()
	res2.ResourceID = "res-2"
	res2.HostGroups = res2.HostGroups[:1]
	poster := &fakePoster{err: errors.New("ingest 500")}
	r := &Runner{
		Resources:       &fakeLister{resources: []ArtifactResource{res}},
		Poster:          poster,
		Engine:          &fakeEngine{result: &apply.Result{Status: apply.StatusSucceeded, PerHost: map[string]apply.HostResult{"host1": {Changed: 1}}}},
		Tips:            &fakeTips{rev: tipSHA},
		DefaultPlaybook: "site.yml",
	}
	sum, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Checks != 0 || sum.Skipped != 2 {
		t.Fatalf("post failures must count as skipped, not posted: %+v", sum)
	}
}

func TestNewReportNormalizesHosts(t *testing.T) {
	rep := NewReport("r", "g", "p", tipSHA, []string{"b.example", "a.example", "b.example", ""})
	if len(rep.DriftedHosts) != 2 || rep.DriftedHosts[0] != "a.example" || rep.DriftedHosts[1] != "b.example" {
		t.Fatalf("hosts not normalized/sorted: %v", rep.DriftedHosts)
	}
	if rep.InSync {
		t.Fatalf("must not be in_sync")
	}
	in := NewReport("r", "g", "p", tipSHA, nil)
	if !in.InSync || len(in.DriftedHosts) != 0 {
		t.Fatalf("empty report must be in_sync: %+v", in)
	}
}
