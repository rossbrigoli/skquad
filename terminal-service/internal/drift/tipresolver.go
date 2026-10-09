package drift

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

var fullSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// GitTipResolver resolves the current tip SHA of a branch with
// `git ls-remote <url> refs/heads/<branch>` — read-only, no clone,
// no working tree. GitBin is injectable for tests.
type GitTipResolver struct {
	GitBin string
}

// TipRev returns the full 40-char SHA at the tip of branch in gitURL.
func (g *GitTipResolver) TipRev(ctx context.Context, gitURL, branch string) (string, error) {
	bin := g.GitBin
	if bin == "" {
		bin = "git"
	}
	if gitURL == "" || branch == "" {
		return "", fmt.Errorf("drift: gitURL and branch are required")
	}
	cmd := exec.CommandContext(ctx, bin, "ls-remote", gitURL, "refs/heads/"+branch)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("drift: ls-remote %s %s: %w", redactGitURL(gitURL), branch, err)
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		return "", fmt.Errorf("drift: branch %s not found in %s", branch, redactGitURL(gitURL))
	}
	sha := strings.Fields(line)[0]
	if !fullSHA.MatchString(sha) {
		return "", fmt.Errorf("drift: ls-remote returned non-SHA %q for %s", sha, branch)
	}
	return sha, nil
}

// redactGitURL strips any userinfo (user:pass@) before an error
// message could leak credentials into logs.
func redactGitURL(u string) string {
	if i := strings.Index(u, "://"); i >= 0 {
		rest := u[i+3:]
		if j := strings.Index(rest, "@"); j >= 0 {
			return u[:i+3] + rest[j+1:]
		}
	}
	return u
}
