package drift

import (
	"context"
	"os"
	"os/exec"
	"testing"
)

// gitTipFixture builds a throwaway local repo and returns its file://
// URL plus the current main tip SHA.
func gitTipFixture(t *testing.T) (url, tip string) {
	t.Helper()
	dir := t.TempDir()
	src := dir + "/repo"
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		if len(args) > 0 && args[0] != "init" {
			cmd.Dir = src
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-b", "main", src)
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	if err := os.WriteFile(src+"/site.yml", []byte("- hosts: all\n  tasks: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-m", "initial")
	out, err := exec.Command("git", "-C", src, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return "file://" + src, string(out[:40])
}

func TestGitTipResolver(t *testing.T) {
	url, tip := gitTipFixture(t)
	r := &GitTipResolver{}
	got, err := r.TipRev(context.Background(), url, "main")
	if err != nil {
		t.Fatalf("TipRev: %v", err)
	}
	if got != tip {
		t.Fatalf("TipRev = %q, want %q", got, tip)
	}
}

func TestGitTipResolverMissingBranch(t *testing.T) {
	url, _ := gitTipFixture(t)
	r := &GitTipResolver{}
	if _, err := r.TipRev(context.Background(), url, "nope"); err == nil {
		t.Fatalf("missing branch must error")
	}
}

func TestGitTipResolverValidatesInput(t *testing.T) {
	r := &GitTipResolver{}
	if _, err := r.TipRev(context.Background(), "", "main"); err == nil {
		t.Fatalf("empty url must error")
	}
	if _, err := r.TipRev(context.Background(), "https://x.invalid/repo.git", ""); err == nil {
		t.Fatalf("empty branch must error")
	}
}

func TestRedactGitURL(t *testing.T) {
	if got := redactGitURL("https://user:pass@git.example.com/a/b.git"); got != "https://git.example.com/a/b.git" {
		t.Fatalf("redact = %q", got)
	}
	if got := redactGitURL("git@git.example.com:a/b.git"); got != "git@git.example.com:a/b.git" {
		t.Fatalf("non-URL scheme left as-is: %q", got)
	}
}
