package apply

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// gitVerify clones the registered repo into workDir and enforces the
// approval binding (§6.7 "Approval = merged revision"):
//
//  1. rev must exist and be a full commit.
//  2. rev must be an ancestor of origin/<defaultBranch> — i.e. it was
//     MERGED. An agent-authored branch that was never merged is refused,
//     no matter what the agent claims.
//  3. when requireTip is set (high tier default), rev must EQUAL the
//     current tip of origin/<defaultBranch>, so a merged-but-superseded
//     revision cannot be applied.
//
// Returns the resolved full SHA on success.
func gitVerify(ctx context.Context, gitBin, gitURL, rev, defaultBranch, workDir string, requireTip bool) (string, string, error) {
	if gitBin == "" {
		gitBin = "git"
	}
	repoDir := workDir + "/repo"
	// --filter=blob:none keeps the clone light but preserves full ref
	// history, which the ancestor check needs.
	if out, err := runGit(ctx, gitBin, workDir, "clone", "--quiet", "--filter=blob:none", gitURL, repoDir); err != nil {
		return "", RefusalRevNotFound, fmt.Errorf("clone failed: %v (%s)", err, out)
	}
	// rev must resolve to a real commit object.
	out, err := runGit(ctx, gitBin, repoDir, "rev-parse", "--verify", "--quiet", rev+"^{commit}")
	if err != nil || strings.TrimSpace(out) == "" {
		return "", RefusalRevNotFound, nil
	}
	full := strings.TrimSpace(out)
	if !strings.EqualFold(full, rev) {
		// The provided rev resolved to a DIFFERENT commit (e.g. an
		// abbreviated match that drifted). Refuse: the approval is bound
		// to the exact SHA presented.
		return "", RefusalNotMerged, nil
	}
	// Merged = ancestor of the registered default branch tip.
	remoteRef := "origin/" + defaultBranch
	if _, err := runGit(ctx, gitBin, repoDir, "merge-base", "--is-ancestor", full, remoteRef); err != nil {
		return "", RefusalNotMerged, nil
	}
	if requireTip {
		tipOut, err := runGit(ctx, gitBin, repoDir, "rev-parse", remoteRef)
		if err != nil || strings.TrimSpace(tipOut) != full {
			return "", RefusalNotTip, nil
		}
	}
	return full, "", nil
}

func runGit(ctx context.Context, gitBin, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, gitBin, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	// Never prompt for credentials or open editors; the registered URL
	// must be non-interactive (https with token in URL is NOT allowed —
	// see resource validation; ssh agent or credential-less public repos).
	cmd.Env = append(cmd.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_SSH_COMMAND=ssh -oBatchMode=yes",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), errors.New(strings.TrimSpace(redactURL(string(out))))
	}
	return string(out), nil
}

// redactURL strips any userinfo that might have slipped into the clone URL
// in error output (defense against token leakage into logs/recordings).
func redactURL(s string) string {
	if i := strings.Index(s, "://"); i > 0 {
		rest := s[i+3:]
		if at := strings.IndexAny(rest, "@/ \n"); at >= 0 && strings.Contains(rest[:at], ":") {
			return s[:i+3] + "***:***" + rest[at:]
		}
	}
	return s
}
