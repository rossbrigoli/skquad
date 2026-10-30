package buildspec

import (
	"strings"
	"testing"
)

func TestParseHappy(t *testing.T) {
	cases := []struct {
		in   Request
		repo string
		rev  string
		df   string
		dest string
	}{
		{
			in:   Request{Repo: "github.com/rossbrigoli/skquad", Revision: "main", Dockerfile: "Dockerfile", Destination: "docker.io/brigss007/agent-builds/skquad:v1.2.3"},
			repo: "github.com/rossbrigoli/skquad", rev: "main", df: "Dockerfile",
			dest: "docker.io/brigss007/agent-builds/skquad:v1.2.3",
		},
		{
			in:   Request{Repo: "https://github.com/rossbrigoli/skquad.git", Revision: "v1.0.0", Dockerfile: "build/Dockerfile", Destination: "ghcr.io/rossbrigoli/agent-builds/x:dev-abc_1"},
			repo: "github.com/rossbrigoli/skquad", rev: "v1.0.0", df: "build/Dockerfile",
			dest: "ghcr.io/rossbrigoli/agent-builds/x:dev-abc_1",
		},
		{
			in:   Request{Repo: "GitHub.com/RossBrigoli/Skquad", Revision: strings.Repeat("a", 40), Dockerfile: "Dockerfile", Destination: "docker.io/brigss007/agent-builds/x:latest"},
			repo: "github.com/RossBrigoli/Skquad", rev: strings.Repeat("a", 40), df: "Dockerfile",
			dest: "docker.io/brigss007/agent-builds/x:latest",
		},
	}
	for i, c := range cases {
		got, verr := Parse(c.in)
		if verr != nil {
			t.Fatalf("case %d: unexpected violations: %v", i, verr)
		}
		if got.Repo != c.repo || got.Revision != c.rev || got.Dockerfile != c.df || got.Destination != c.dest {
			t.Fatalf("case %d: got %+v want repo=%s rev=%s df=%s dest=%s", i, got, c.repo, c.rev, c.df, c.dest)
		}
	}
}

func TestParseDenyPaths(t *testing.T) {
	cases := []struct {
		name  string
		in    Request
		field string
		code  string
	}{
		{"empty repo", Request{Repo: "", Revision: "main", Dockerfile: "D", Destination: "docker.io/a/b/x:1"}, "repo", "empty"},
		{"userinfo", Request{Repo: "https://tok@github.com/a/b", Revision: "main", Dockerfile: "D", Destination: "docker.io/a/b/x:1"}, "repo", "userinfo_forbidden"},
		{"too few segments", Request{Repo: "github.com/skquad", Revision: "main", Dockerfile: "D", Destination: "docker.io/a/b/x:1"}, "repo", "not_host_org_repo"},
		{"too many segments ok? no, 4 segs allowed? reject", Request{Repo: "a/b/c/d", Revision: "main", Dockerfile: "D", Destination: "docker.io/a/b/x:1"}, "repo", "not_host_org_repo"},
		{"port in host", Request{Repo: "github.com:8080/a/b", Revision: "main", Dockerfile: "D", Destination: "docker.io/a/b/x:1"}, "repo", "port_forbidden"},
		{"empty revision", Request{Repo: "github.com/a/b", Revision: "", Dockerfile: "D", Destination: "docker.io/a/b/x:1"}, "revision", "empty"},
		{"abbreviated sha", Request{Repo: "github.com/a/b", Revision: "abc1234", Dockerfile: "D", Destination: "docker.io/a/b/x:1"}, "revision", "abbreviated_sha"},
		{"ref with space", Request{Repo: "github.com/a/b", Revision: "my branch", Dockerfile: "D", Destination: "docker.io/a/b/x:1"}, "revision", "unsafe_chars"},
		{"ref with caret", Request{Repo: "github.com/a/b", Revision: "main^", Dockerfile: "D", Destination: "docker.io/a/b/x:1"}, "revision", "unsafe_chars"},
		{"ref leading dash", Request{Repo: "github.com/a/b", Revision: "-x", Dockerfile: "D", Destination: "docker.io/a/b/x:1"}, "revision", "bad_edges"},
		{"ref double dot", Request{Repo: "github.com/a/b", Revision: "a..b", Dockerfile: "D", Destination: "docker.io/a/b/x:1"}, "revision", "double_dot"},
		{"ref at-brace", Request{Repo: "github.com/a/b", Revision: "HEAD@{0}", Dockerfile: "D", Destination: "docker.io/a/b/x:1"}, "revision", "bad_sequence"},
		{"absolute dockerfile", Request{Repo: "github.com/a/b", Revision: "main", Dockerfile: "/etc/passwd", Destination: "docker.io/a/b/x:1"}, "dockerfile", "absolute_path"},
		{"dotdot dockerfile", Request{Repo: "github.com/a/b", Revision: "main", Dockerfile: "../Dockerfile", Destination: "docker.io/a/b/x:1"}, "dockerfile", "dot_segment"},
		{"empty dockerfile", Request{Repo: "github.com/a/b", Revision: "main", Dockerfile: "", Destination: "docker.io/a/b/x:1"}, "dockerfile", "empty"},
		{"dest no tag", Request{Repo: "github.com/a/b", Revision: "main", Dockerfile: "D", Destination: "docker.io/a/b/x"}, "destination", "missing_tag"},
		{"dest digest pinned", Request{Repo: "github.com/a/b", Revision: "main", Dockerfile: "D", Destination: "docker.io/a/b/x@sha256:" + strings.Repeat("b", 64)}, "destination", "digest_forbidden"},
		{"dest bad tag chars", Request{Repo: "github.com/a/b", Revision: "main", Dockerfile: "D", Destination: "docker.io/a/b/x:bad!tag"}, "destination", "bad_tag"},
		{"dest no path", Request{Repo: "github.com/a/b", Revision: "main", Dockerfile: "D", Destination: "x:1"}, "destination", "missing_repository_path"},
		{"dest uppercase segment", Request{Repo: "github.com/a/b", Revision: "main", Dockerfile: "D", Destination: "docker.io/Brigss/agent-builds/x:1"}, "destination", "bad_name_segment"},
	}
	for _, c := range cases {
		_, verr := Parse(c.in)
		if verr == nil {
			t.Fatalf("%s: expected violation, got none", c.name)
		}
		found := false
		for _, v := range verr.Violations {
			if v.Field == c.field && v.Code == c.code {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s: expected %s:%s, got %+v", c.name, c.field, c.code, verr.Violations)
		}
	}
}

func TestRepoAllowedGlob(t *testing.T) {
	cases := []struct {
		pattern string
		repo    string
		want    bool
	}{
		{"rossbrigoli/*", "rossbrigoli/skquad", true},
		{"rossbrigoli/*", "rossbrigoli/other", true},
		{"rossbrigoli/*", "evilcorp/skquad", false},
		{"rossbrigoli/skquad", "rossbrigoli/skquad", true},
		{"rossbrigoli/skquad", "rossbrigoli/other", false},
		{"*", "a/b", false},
		{"rossbrigoli/*", "rossbrigoli/a/b", false},
		{"ross*li/skquad", "rossbrigoli/skquad", true},
	}
	for _, c := range cases {
		got := RepoAllowed([]string{c.pattern}, c.repo)
		if got != c.want {
			t.Errorf("RepoAllowed(%q,%q)=%v want %v", c.pattern, c.repo, got, c.want)
		}
	}
}

func TestDestinationAllowed(t *testing.T) {
	prefixes := []string{"docker.io/brigss007/agent-builds/"}
	if !DestinationAllowed(prefixes, "docker.io/brigss007/agent-builds/skquad") {
		t.Fatal("expected allow")
	}
	if DestinationAllowed(prefixes, "docker.io/brigss007/agent-builds-evil/x") {
		t.Fatal("boundary bypass via sibling prefix must fail")
	}
	if DestinationAllowed(prefixes, "docker.io/evil/agent-builds/x") {
		t.Fatal("different namespace must fail")
	}
}
