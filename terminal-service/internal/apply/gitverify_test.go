package apply

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func readFileForTest(t *testing.T, path string) ([]byte, error) {
	t.Helper()
	return os.ReadFile(path)
}

// gitFixture builds a local repo with a main branch and returns
// (file:// URL, firstRev, mergedFeatureRev, unmergedRev, tipRev).
func gitFixture(t *testing.T) (url, firstRev, mergedRev, unmergedRev, tipRev string) {
	t.Helper()
	dir := t.TempDir()
	src := dir + "/src"
	mustGit(t, "", "init", "-b", "main", src)
	mustGit(t, src, "config", "user.email", "t@t")
	mustGit(t, src, "config", "user.name", "t")
	if err := os.MkdirAll(src+"/playbooks", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src+"/playbooks/web.yml", []byte("- hosts: targets\n  tasks: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, src, "add", "-A")
	mustGit(t, src, "commit", "-m", "initial")
	firstRev = gitRev(t, src, "HEAD")

	mustGit(t, src, "checkout", "-b", "feature")
	if err := os.WriteFile(src+"/playbooks/feature.yml", []byte("- hosts: targets\n  tasks: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, src, "add", "-A")
	mustGit(t, src, "commit", "-m", "feature")
	mergedRev = gitRev(t, src, "HEAD")

	mustGit(t, src, "checkout", "main")
	mustGit(t, src, "merge", "--no-edit", "feature")

	mustGit(t, src, "checkout", "-b", "rogue")
	if err := os.WriteFile(src+"/playbooks/rogue.yml", []byte("- hosts: targets\n  tasks: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, src, "add", "-A")
	mustGit(t, src, "commit", "-m", "rogue never merged")
	unmergedRev = gitRev(t, src, "HEAD")

	mustGit(t, src, "checkout", "main")
	if err := os.WriteFile(src+"/playbooks/tip.yml", []byte("- hosts: targets\n  tasks: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, src, "add", "-A")
	mustGit(t, src, "commit", "-m", "later tip")
	tipRev = gitRev(t, src, "HEAD")

	return "file://" + src, firstRev, mergedRev, unmergedRev, tipRev
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func gitRev(t *testing.T, dir, ref string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "rev-parse", ref).Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func TestGitVerify(t *testing.T) {
	url, firstRev, mergedRev, unmergedRev, tipRev := gitFixture(t)
	ctx := context.Background()
	work := t.TempDir()
	mk := func(s string) string {

		d := work + "/" + s
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		return d
	}

	// merged revision, no tip requirement
	if full, refusal, err := gitVerify(ctx, "git", url, mergedRev, "main", mk("a"), false); err != nil || refusal != "" || full != mergedRev {
		t.Fatalf("merged rev: full=%q refusal=%q err=%v", full, refusal, err)
	}
	// unmerged rogue branch
	if _, refusal, err := gitVerify(ctx, "git", url, unmergedRev, "main", mk("b"), false); err != nil || refusal != RefusalNotMerged {
		t.Fatalf("rogue rev: refusal=%q err=%v", refusal, err)
	}
	// require tip: older merged rev is not the tip
	if _, refusal, err := gitVerify(ctx, "git", url, mergedRev, "main", mk("c"), true); err != nil || refusal != RefusalNotTip {
		t.Fatalf("old merged rev with requireTip: refusal=%q err=%v", refusal, err)
	}
	// require tip: current tip passes
	if full, refusal, err := gitVerify(ctx, "git", url, tipRev, "main", mk("d"), true); err != nil || refusal != "" || full != tipRev {
		t.Fatalf("tip rev: full=%q refusal=%q err=%v", full, refusal, err)
	}
	// nonexistent rev
	if _, refusal, err := gitVerify(ctx, "git", url, strings.Repeat("a", 40), "main", mk("e"), false); err != nil || refusal != RefusalRevNotFound {
		t.Fatalf("missing rev: refusal=%q err=%v", refusal, err)
	}
	// first rev (merged by definition) without tip requirement
	if _, refusal, err := gitVerify(ctx, "git", url, firstRev, "main", mk("f"), false); err != nil || refusal != "" {
		t.Fatalf("first rev: refusal=%q err=%v", refusal, err)
	}
}

func TestRedactURL(t *testing.T) {
	got := redactURL("fatal: unable to access 'https://tok:***@github.com/x/y.git'")
	if strings.Contains(got, "tok:") && !strings.Contains(got, "***:***") {
		t.Errorf("token not redacted: %s", got)
	}
	if strings.Contains(redactURL("https://github.com/x/y.git"), "***") {
		t.Error("false redaction on clean URL")
	}
}
