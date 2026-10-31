package govern

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rossbrigoli/skquad/build-service/internal/authn"
	"github.com/rossbrigoli/skquad/build-service/internal/cppolicy"
)

// --- test doubles -------------------------------------------------------------

type fakePolicy struct {
	globs []string
	err   error
}

func (f fakePolicy) GitReposAllow(_ context.Context, _ string) ([]string, error) {
	return f.globs, f.err
}

type fakeProber struct {
	refs map[string]string
	err  error
}

func (f fakeProber) ListRefs(_ context.Context, _ string) (map[string]string, error) {
	return f.refs, f.err
}

type fakeActive struct {
	n   int
	err error
}

func (f fakeActive) CountActive(_ context.Context, _ string) (int, error) { return f.n, f.err }

func principal() *authn.Principal {
	return &authn.Principal{
		ID:                "sherlock",
		AgentID:           "agent-1",
		ReposAllow:        []string{"github.com/rossbrigoli/*"},
		DestinationsAllow: []string{"ghcr.io/rossbrigoli/agent-builds/"},
		Active:            true,
	}
}

func baseValidator(pol cppolicy.Lookup, prober fakeProber, act fakeActive) *Validator {
	return NewValidator(Deps{
		Policy:                    pol,
		Prober:                    prober,
		Active:                    act,
		GitHostsAllow:             []string{"github.com"},
		DestinationsAllow:         []string{"ghcr.io/rossbrigoli/agent-builds/"},
		MaxConcurrentPerPrincipal: 2,
		MaxPerHourPerPrincipal:    5,
	})
}

func goodRequest() Request {
	return Request{
		Repo:        "https://github.com/rossbrigoli/skquad.git",
		Revision:    "main",
		Dockerfile:  "Dockerfile",
		Destination: "ghcr.io/rossbrigoli/agent-builds/skquad:demo-1",
	}
}

var okRefs = map[string]string{
	"HEAD":            "0123456789abcdef0123456789abcdef01234567",
	"refs/heads/main": "0123456789abcdef0123456789abcdef01234567",
	"refs/tags/v1.0":  "ffffffffffffffffffffffffffffffffffffffff",
}

// --- allow path ---------------------------------------------------------------

func TestAllowHappyPath(t *testing.T) {
	v := baseValidator(fakePolicy{globs: []string{"github.com/rossbrigoli/*"}}, fakeProber{refs: okRefs}, fakeActive{})
	d := v.Validate(context.Background(), principal(), goodRequest())
	if !d.Allowed {
		t.Fatalf("expected allow, got %+v", d)
	}
	if d.Repo != "github.com/rossbrigoli/skquad" {
		t.Fatalf("canonical repo wrong: %q", d.Repo)
	}
	if d.RefMatch != "refs/heads/main" {
		t.Fatalf("ref match wrong: %q", d.RefMatch)
	}
}

func TestAllowFullSHA(t *testing.T) {
	req := goodRequest()
	req.Revision = "ffffffffffffffffffffffffffffffffffffffff"
	v := baseValidator(nil, fakeProber{}, fakeActive{})
	if d := v.Validate(context.Background(), principal(), req); !d.Allowed {
		t.Fatalf("full sha should be allowed by format: %+v", d)
	}
}

// --- deny paths ---------------------------------------------------------------

func TestDenyCodes(t *testing.T) {
	cases := []struct {
		name   string
		pol    cppolicy.Lookup
		prober fakeProber
		act    fakeActive
		mutate func(*Request)
		want   string
	}{
		{"empty repo", nil, fakeProber{refs: okRefs}, fakeActive{}, func(r *Request) { r.Repo = "" }, CodeInvalidRepo},
		{"bad scheme", nil, fakeProber{refs: okRefs}, fakeActive{}, func(r *Request) { r.Repo = "file:///etc/passwd" }, CodeInvalidRepo},
		{"url with creds", nil, fakeProber{refs: okRefs}, fakeActive{}, func(r *Request) { r.Repo = "https://user:pw@github.com/rossbrigoli/x" }, CodeInvalidRepo},
		{"host not allowed", nil, fakeProber{refs: okRefs}, fakeActive{}, func(r *Request) { r.Repo = "https://gitlab.example.org/rossbrigoli/x" }, CodeGitHostNotAllowed},
		{"repo outside principal list", nil, fakeProber{refs: okRefs}, fakeActive{}, func(r *Request) { r.Repo = "https://github.com/other/repo.git" }, CodeRepoNotAllowed},
		{"outside grant scope", fakePolicy{globs: []string{"github.com/rossbrigoli/other"}}, fakeProber{refs: okRefs}, fakeActive{}, nil, CodeOutsideGrantScope},
		{"policy unavailable", fakePolicy{err: cppolicy.ErrPolicyUnavailable}, fakeProber{refs: okRefs}, fakeActive{}, nil, CodePolicyUnavailable},
		{"empty revision", nil, fakeProber{refs: okRefs}, fakeActive{}, func(r *Request) { r.Revision = "" }, CodeInvalidRevision},
		{"revision with bad chars", nil, fakeProber{refs: okRefs}, fakeActive{}, func(r *Request) { r.Revision = "main;rm -rf" }, CodeInvalidRevision},
		{"revision not found", nil, fakeProber{refs: okRefs}, fakeActive{}, func(r *Request) { r.Revision = "nope" }, CodeRevisionNotFound},
		{"probe failure fail-closed", nil, fakeProber{err: errors.New("boom")}, fakeActive{}, nil, CodeRevisionCheckFailed},
		{"dockerfile absolute", nil, fakeProber{refs: okRefs}, fakeActive{}, func(r *Request) { r.Dockerfile = "/etc/Dockerfile" }, CodeInvalidDockerfile},
		{"dockerfile traversal", nil, fakeProber{refs: okRefs}, fakeActive{}, func(r *Request) { r.Dockerfile = "../evil/Dockerfile" }, CodeInvalidDockerfile},
		{"destination no tag", nil, fakeProber{refs: okRefs}, fakeActive{}, func(r *Request) { r.Destination = "ghcr.io/rossbrigoli/agent-builds/x" }, CodeInvalidDestination},
		{"destination latest forbidden", nil, fakeProber{refs: okRefs}, fakeActive{}, func(r *Request) { r.Destination = "ghcr.io/rossbrigoli/agent-builds/x:latest" }, CodeTagForbidden},
		{"destination outside global allow", nil, fakeProber{refs: okRefs}, fakeActive{}, func(r *Request) { r.Destination = "docker.io/brigss007/skquad-api-server:x" }, CodeDestinationDenied},
		{"destination platform image", nil, fakeProber{refs: okRefs}, fakeActive{}, func(r *Request) { r.Destination = "ghcr.io/rossbrigoli/skquad-api-server:x" }, CodeDestinationDenied},
		{"concurrent quota", nil, fakeProber{refs: okRefs}, fakeActive{n: 2}, nil, CodeQuotaConcurrent},
		{"count error fail-closed", nil, fakeProber{refs: okRefs}, fakeActive{err: errors.New("api")}, nil, CodeQuotaConcurrent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := goodRequest()
			if tc.mutate != nil {
				tc.mutate(&req)
			}
			v := baseValidator(tc.pol, tc.prober, tc.act)
			d := v.Validate(context.Background(), principal(), req)
			if d.Allowed || d.Code != tc.want {
				t.Fatalf("want deny %s, got %+v", tc.want, d)
			}
		})
	}
}

func TestHourlyRateWindow(t *testing.T) {
	v := baseValidator(nil, fakeProber{refs: okRefs}, fakeActive{})
	v.deps.MaxPerHourPerPrincipal = 2
	now := time.Unix(1730000000, 0)
	v.nowFn = func() time.Time { return now }
	ctx := context.Background()
	p := principal()
	if d := v.Validate(ctx, p, goodRequest()); !d.Allowed {
		t.Fatalf("first build should pass: %+v", d)
	}
	if d := v.Validate(ctx, p, goodRequest()); !d.Allowed {
		t.Fatalf("second build should pass: %+v", d)
	}
	if d := v.Validate(ctx, p, goodRequest()); d.Allowed || d.Code != CodeQuotaRate {
		t.Fatalf("third build must hit hourly quota: %+v", d)
	}
	// After an hour the window slides and builds pass again.
	now = now.Add(time.Hour + time.Minute)
	if d := v.Validate(ctx, p, goodRequest()); !d.Allowed {
		t.Fatalf("quota should have reset after an hour: %+v", d)
	}
}

func TestPrincipalWithoutAgentIDDeniedWhenPolicyWired(t *testing.T) {
	p := principal()
	p.AgentID = ""
	v := baseValidator(fakePolicy{globs: []string{"*"}}, fakeProber{refs: okRefs}, fakeActive{})
	if d := v.Validate(context.Background(), p, goodRequest()); d.Allowed || d.Code != CodeOutsideGrantScope {
		t.Fatalf("missing agent_id with policy wired must deny: %+v", d)
	}
}

func TestCanonicalRepoForms(t *testing.T) {
	forms := []string{
		"https://github.com/rossbrigoli/skquad",
		"https://github.com/rossbrigoli/skquad.git",
		"ssh://git@github.com/rossbrigoli/skquad.git",
		"git@github.com:rossbrigoli/skquad.git",
		"git@github.com:rossbrigoli/skquad",
	}
	for _, f := range forms {
		c, err := CanonicalRepo(f)
		if err != nil || c != "github.com/rossbrigoli/skquad" {
			t.Fatalf("form %q -> %q, %v", f, c, err)
		}
	}
	bad := []string{"", "github.com/rossbrigoli/skquad", "https://github.com/onlyorg", "ftp://x/y/z"}
	for _, f := range bad {
		if _, err := CanonicalRepo(f); err == nil {
			t.Fatalf("form %q must be rejected", f)
		}
	}
}
