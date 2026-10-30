// Package revision resolves a requested git revision to a FULL commit
// SHA before any build is scheduled, so every build is revision-pinned
// (docs §6.6 philosophy: reproducible, revertible via git revert).
//
// Resolution rules:
//
//   - Full SHA (40 or 64 hex): verified reachable via
//     `git fetch --depth=1 <url> <sha>` into a throwaway repo.
//     (GitHub and most forges allow fetching reachable SHAs.)
//   - Named ref (branch/tag): `git ls-remote <url> <ref>` must return
//     exactly one match; the SHA is taken from the remote, never from
//     the caller after that point.
//
// The git binary is injected (GitRunner interface) so deny paths and
// parsing are unit-testable without a real git, and the production
// runner pins env (GIT_TERMINAL_PROMPT=0, BatchMode ssh) exactly.
package revision

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// Errors surfaced to the service layer.
var (
	ErrNotFound     = errors.New("revision not found on remote")
	ErrAmbiguous    = errors.New("revision matches multiple remote refs")
	ErrProbeFailed  = errors.New("remote revision probe failed")
	ErrBadResolved  = errors.New("remote returned an unparseable SHA")
	ErrUnreachable  = errors.New("commit not reachable on remote")
)

var shaRe = regexp.MustCompile(`^[0-9a-f]{40}$|^[0-9a-f]{64}$`)

// IsFullSHA reports whether rev is a full commit SHA.
func IsFullSHA(rev string) bool { return shaRe.MatchString(rev) }

// GitRunner executes git commands. Production: ExecRunner. Tests: fake.
type GitRunner interface {
	// Run executes `git <args>` in dir with hardened env and returns
	// combined stdout (not stderr).
	Run(ctx context.Context, dir string, args ...string) (string, error)
}

// ExecRunner shells out to a real git binary.
type ExecRunner struct{ GitBin string }

func (r ExecRunner) Run(ctx context.Context, dir string, args ...string) (string, error) {
	bin := r.GitBin
	if bin == "" {
		bin = "git"
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_SSH_COMMAND=ssh -oBatchMode=yes",
	)
	var stdout strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = nil // discard stderr; errors carry exit status only
	if err := cmd.Run(); err != nil {
		return stdout.String(), err
	}
	return stdout.String(), nil
}

// Resolver resolves revisions against remote repositories.
type Resolver struct {
	Runner GitRunner
}

// Resolve returns the full commit SHA for rev in the repo at gitURL.
func (r *Resolver) Resolve(ctx context.Context, gitURL, rev string) (string, error) {
	if IsFullSHA(rev) {
		return r.resolveSHA(ctx, gitURL, rev)
	}
	return r.resolveRef(ctx, gitURL, rev)
}

// resolveRef: ls-remote for the named ref. Accept refs/heads/<rev>,
// refs/tags/<rev>, and the bare ref if unambiguous.
func (r *Resolver) resolveRef(ctx context.Context, gitURL, rev string) (string, error) {
	out, err := r.Runner.Run(ctx, "", "ls-remote", gitURL,
		rev, "refs/heads/"+rev, "refs/tags/"+rev)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrProbeFailed, err)
	}
	type hit struct{ sha, ref string }
	var hits []hit
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		if !shaRe.MatchString(fields[0]) {
			return "", ErrBadResolved
		}
		hits = append(hits, hit{sha: fields[0], ref: fields[1]})
	}
	if len(hits) == 0 {
		return "", ErrNotFound
	}
	// Deduplicate SHAs: a branch and tag with the same name pointing at
	// one commit is NOT ambiguous.
	uniq := map[string]bool{}
	for _, h := range hits {
		uniq[h.sha] = true
	}
	if len(uniq) > 1 {
		return "", ErrAmbiguous
	}
	return hits[0].sha, nil
}

// resolveSHA: verify reachability with a depth-1 fetch into a throwaway
// repo, then confirm FETCH_HEAD equals the requested SHA.
func (r *Resolver) resolveSHA(ctx context.Context, gitURL, sha string) (string, error) {
	tmp, err := os.MkdirTemp("", "skquad-rev-*")
	if err != nil {
		return "", fmt.Errorf("temp dir: %w", err)
	}
	defer os.RemoveAll(tmp)
	if err := os.Mkdir(filepath.Join(tmp, "r"), 0o755); err != nil {
		return "", err
	}
	repoDir := filepath.Join(tmp, "r")
	if _, err := r.Runner.Run(ctx, repoDir, "init", "-q"); err != nil {
		return "", fmt.Errorf("%w: init: %v", ErrProbeFailed, err)
	}
	if _, err := r.Runner.Run(ctx, repoDir, "fetch", "-q", "--depth=1", gitURL, sha); err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	out, err := r.Runner.Run(ctx, repoDir, "rev-parse", "FETCH_HEAD")
	if err != nil {
		return "", fmt.Errorf("%w: rev-parse: %v", ErrProbeFailed, err)
	}
	got := strings.TrimSpace(out)
	if !strings.EqualFold(got, sha) {
		return "", ErrBadResolved
	}
	return strings.ToLower(got), nil
}
