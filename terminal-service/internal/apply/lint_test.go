package apply

import (
	"strings"
	"testing"
)

func TestLintPlaybook_PinnedPackages(t *testing.T) {
	pb := `
- hosts: targets
  tasks:
    - name: install pinned apt pkg
      ansible.builtin.apt:
        name: nginx=1.24.0-1
        state: present
    - name: install pinned yum pkgs
      yum:
        name:
          - curl=7.88.1-1
          - wget=1.21.3-1
    - name: zypper with version param
      zypper:
        name: vim
        version: 9.0.1500-1.1
`
	findings, err := LintPlaybook([]byte(pb), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		if f.Fatal {
			t.Errorf("pinned playbook got fatal finding: %+v", f)
		}
	}
}

func TestLintPlaybook_UnpinnedPackage(t *testing.T) {
	cases := []struct {
		name string
		pb   string
	}{
		{"apt bare string", "- hosts: targets\n  tasks:\n    - apt: nginx\n"},
		{"apt list unpinned", "- hosts: targets\n  tasks:\n    - apt:\n        name: [nginx, curl]\n"},
		{"apt wildcard version", "- hosts: targets\n  tasks:\n    - apt:\n        name: nginx=*\n"},
		{"mixed list", "- hosts: targets\n  tasks:\n    - yum:\n        name: [curl=7.1, wget]\n"},
	}
	for _, tc := range cases {
		findings, err := LintPlaybook([]byte(tc.pb), nil, nil)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		found := false
		for _, f := range findings {
			if f.Rule == "unpinned_package" && f.Fatal {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: want fatal unpinned_package, got %+v", tc.name, findings)
		}
	}
}

func TestLintPlaybook_MirrorOverrides(t *testing.T) {
	pbAllowed := `
- hosts: targets
  tasks:
    - name: add pinned pkg from allowed mirror
      apt:
        name: nginx=1.24.0-1
        deb: https://pypi.internal.lab/nginx.deb
`
	if f, err := LintPlaybook([]byte(pbAllowed), []string{"pypi.internal.lab"}, nil); err != nil {
		t.Fatal(err)
	} else {
		for _, x := range f {
			if x.Fatal {
				t.Errorf("allowed mirror rejected: %+v", x)
			}
		}
	}

	pbDenied := `
- hosts: targets
  tasks:
    - name: sketchy mirror
      apt:
        name: nginx=1.24.0-1
        sources:
          - "deb http://evil.example.com/repo stable main"
`
	f, err := LintPlaybook([]byte(pbDenied), []string{"pypi.internal.lab"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, x := range f {
		if x.Rule == "mirror_not_allowed" && x.Fatal {
			found = true
		}
	}
	if !found {
		t.Errorf("want fatal mirror_not_allowed, got %+v", f)
	}

	// No allowlist configured ⇒ explicit overrides refused.
	f, err = LintPlaybook([]byte(pbAllowed), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	found = false
	for _, x := range f {
		if x.Rule == "mirror_not_allowed" && x.Fatal {
			found = true
		}
	}
	if !found {
		t.Errorf("no-allowlist should refuse overrides, got %+v", f)
	}
}

func TestLintPlaybook_DeniedCommands(t *testing.T) {
	denies := []string{"rm -rf *", "mkfs*", "dd of=*"}
	pb := `
- hosts: targets
  tasks:
    - name: chained payload
      shell: "systemctl status nginx; rm -rf /var/tmp/scratch"
    - name: block nested evil
      block:
        - name: deep
          command: mkfs.ext4 /dev/sdb
    - name: argv form
      command:
        argv: ["dd", "of=/dev/sda"]
    - name: innocent
      command: systemctl reload nginx
`
	f, err := LintPlaybook([]byte(pb), nil, denies)
	if err != nil {
		t.Fatal(err)
	}
	rules := map[string]int{}
	for _, x := range f {
		if x.Rule == "denied_command" && x.Fatal {
			rules[x.Task]++
		}
	}
	if rules["chained payload"] != 1 {
		t.Errorf("chained payload not caught: %+v", f)
	}
	if rules["deep"] != 1 {
		t.Errorf("block-nested mkfs not caught: %+v", f)
	}
	if rules["argv form"] != 1 {
		t.Errorf("argv dd not caught: %+v", f)
	}
	for _, x := range f {
		if x.Task == "innocent" {
			t.Errorf("innocent command flagged: %+v", x)
		}
	}
}

func TestLintPlaybook_Unparseable(t *testing.T) {
	f, err := LintPlaybook([]byte("\t- not: [valid"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(f) != 1 || f[0].Rule != "unparseable_playbook" || !f[0].Fatal {
		t.Errorf("want single fatal unparseable_playbook, got %+v", f)
	}
}

func TestDenyGlobMatchAnywhere(t *testing.T) {
	if !denyGlobMatch("rm -rf *", "ls /; rm -rf /tmp/x") {
		t.Error("deny must match mid-command")
	}
	if denyGlobMatch("rm -rf *", "echo hello") {
		t.Error("false positive")
	}
	if !denyGlobMatch("MKFS*", "mkfs.ext4 /dev/sdb") {
		t.Error("case-insensitive match failed")
	}
}

func TestParsePerHost(t *testing.T) {
	stdout := `[
		{"stats": {"h1": {"ok": 3, "changed": 1, "failures": 0, "unreachable": 0, "skipped": 2}}},
		{"stats": {"h1": {"ok": 1, "changed": 0, "failures": 2, "unreachable": 0, "skipped": 0},
		          "h2": {"ok": 0, "changed": 0, "failures": 0, "unreachable": 1, "skipped": 0}}}
	]`
	per := parsePerHost(stdout)
	if per == nil {
		t.Fatal("parse returned nil")
	}
	h1 := per["h1"]
	if h1.OK != 4 || h1.Changed != 1 || h1.Failed != 2 || h1.Skipped != 2 {
		t.Errorf("h1 aggregation wrong: %+v", h1)
	}
	if per["h2"].Unreachable != 1 {
		t.Errorf("h2 unreachable wrong: %+v", per["h2"])
	}
	if parsePerHost("not json") != nil {
		t.Error("non-json must parse to nil (no per-host data)")
	}
}

func TestBuildInventory(t *testing.T) {
	dir := t.TempDir()
	kh := dir + "/kh"
	if err := writeFileSecure(kh, []byte("host1 ssh-ed25519 AAAA\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path, err := buildInventory(dir, kh, "ops", map[string]string{"host1": `-o CertificateFile=/x/c.pem`}, []string{"host1", "host2"})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := readFileForTest(t, path)
	s := string(data)
	for _, want := range []string{"[targets]", `host1 ansible_ssh_common_args="-o CertificateFile=/x/c.pem"`, "host2", "ansible_user=ops"} {
		if !strings.Contains(s, want) {
			t.Errorf("inventory missing %q:\n%s", want, s)
		}
	}
}
