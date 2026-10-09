package apply

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type fakeCA struct{ minted []string }

func (f *fakeCA) Mint(_ context.Context, user, host string, _ time.Duration) ([]byte, error) {
	f.minted = append(f.minted, user+"@"+host)
	return []byte("FAKECERT " + host), nil
}

type fakeRec struct {
	chunks int
	closed bool
	id     string
}

func (f *fakeRec) AppendOut([]byte) error   { f.chunks++; return nil }
func (f *fakeRec) Flush(context.Context) error { return nil }
func (f *fakeRec) Close(context.Context) error { f.closed = true; return nil }
func (f *fakeRec) RecordingID() string       { return f.id }

// fakeAnsible writes an executable that emits canned JSON + exit code.
func fakeAnsible(t *testing.T, stdout string, exit int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-ansible-playbook")
	script := "#!/bin/sh\ncat > /dev/null || true\nprintf '%s' " + shellQuote(stdout) + "\nexit " + itoaT(exit) + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func shellQuote(s string) string {
	return "'" + replaceAll(s, "'", `'\''`) + "'"
}

func replaceAll(s, old, new string) string { return string(bytesReplace([]byte(s), []byte(old), []byte(new))) }

func bytesReplace(s, old, repl []byte) []byte {
	out := []byte{}
	for i := 0; i < len(s); {
		if i+len(old) <= len(s) && string(s[i:i+len(old)]) == string(old) {
			out = append(out, repl...)
			i += len(old)
			continue
		}
		out = append(out, s[i])
		i++
	}
	return out
}

func itoaT(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}

func baseRequest(t *testing.T, url, rev string) Request {
	t.Helper()
	return Request{
		ApplyID: "app-1", ResourceID: "res-1", AgentID: "agent-1",
		Playbook: "web.yml", GitRev: rev, HostGroup: "staging",
		Hosts: []string{"staging-1.lab"},
		GitURL: url, DefaultBranch: "main", PlaybooksPath: "playbooks",
		SSHUser: "ops", KnownHosts: "staging-1.lab ssh-ed25519 AAAA",
		CertTTL: 15 * time.Minute,
	}
}

func TestEngineApplySuccess(t *testing.T) {
	url, _, mergedRev, _, _ := gitFixture(t)
	rec := &fakeRec{id: "app-1"}
	ca := &fakeCA{}
	e := &Engine{
		AnsibleBin: fakeAnsible(t, `[{"stats":{"staging-1.lab":{"ok":3,"changed":1,"failures":0,"unreachable":0,"skipped":0}}}]`, 0),
		NewRecorder: func(id string, _ map[string]any) (Recorder, error) { return rec, nil },
	}
	req := baseRequest(t, url, mergedRev)
	req.CAMint = ca
	res, err := e.Apply(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusSucceeded {
		t.Fatalf("want succeeded, got %+v", res)
	}
	if res.PerHost["staging-1.lab"].Changed != 1 {
		t.Errorf("per-host missing: %+v", res.PerHost)
	}
	if len(ca.minted) != 1 || ca.minted[0] != "ops@staging-1.lab" {
		t.Errorf("CA mint wrong: %v", ca.minted)
	}
	if rec.chunks == 0 || !rec.closed {
		t.Errorf("recording not written/closed: %+v", rec)
	}
}

func TestEngineApplyRefusesUnmerged(t *testing.T) {
	url, _, _, unmergedRev, _ := gitFixture(t)
	e := &Engine{AnsibleBin: fakeAnsible(t, "[]", 0)}
	res, err := e.Apply(context.Background(), baseRequest(t, url, unmergedRev))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusRefused || res.RefusalReason != RefusalNotMerged {
		t.Fatalf("want refused/artifact_not_merged, got %+v", res)
	}
}

func TestEngineApplyRefusesLintFailure(t *testing.T) {
	// web.yml in the fixture has no tasks — passes lint. Test the wiring
	// with a playbook containing a denied command via a dedicated repo:
	// We achieve that by creating a second fixture with evil content:
	dir := t.TempDir()
	src := dir + "/src"
	mustGit(t, "", "init", "-b", "main", src)
	mustGit(t, src, "config", "user.email", "t@t")
	mustGit(t, src, "config", "user.name", "t")
	if err := os.MkdirAll(src+"/playbooks", 0o755); err != nil {
		t.Fatal(err)
	}
	evil := "- hosts: targets\n  tasks:\n    - name: boom\n      shell: rm -rf /var/lib/nope\n"
	if err := os.WriteFile(src+"/playbooks/evil.yml", []byte(evil), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, src, "add", "-A")
	mustGit(t, src, "commit", "-m", "evil")
	evilRev := gitRev(t, src, "HEAD")

	req := baseRequest(t, "file://"+src, evilRev)
	req.Playbook = "evil.yml"
	req.DenyPatterns = []string{"rm -rf *"}
	e := &Engine{AnsibleBin: fakeAnsible(t, "[]", 0)}
	res, err := e.Apply(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusRefused || res.RefusalReason != RefusalLintFailed {
		t.Fatalf("want refused/lint_failed, got %+v", res)
	}
	if len(res.LintFindings) == 0 {
		t.Error("lint findings must be retained on refusal")
	}
}
func TestEngineApplyPlaybookTraversalRefused(t *testing.T) {
	url, _, mergedRev, _, _ := gitFixture(t)
	req := baseRequest(t, url, mergedRev)
	req.Playbook = "../.git/config"
	e := &Engine{AnsibleBin: fakeAnsible(t, "[]", 0)}
	res, err := e.Apply(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusRefused || res.RefusalReason != RefusalBadPlaybook {
		t.Fatalf("want refused/playbook_not_found, got %+v", res)
	}
}

func TestEngineApplyEmptyHosts(t *testing.T) {
	req := baseRequest(t, "https://example.invalid/x.git", "0000000000000000000000000000000000000000")
	req.Hosts = nil
	e := &Engine{}
	res, err := e.Apply(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusRefused || res.RefusalReason != RefusalEmptyHosts {
		t.Fatalf("want refused/empty_host_group, got %+v", res)
	}
}

func TestEngineApplyFailureExitCode(t *testing.T) {
	url, _, mergedRev, _, _ := gitFixture(t)
	e := &Engine{AnsibleBin: fakeAnsible(t, `[{"stats":{"staging-1.lab":{"ok":1,"changed":0,"failures":1,"unreachable":0,"skipped":0}}}]`, 2)}
	req := baseRequest(t, url, mergedRev)
	req.StaticKeyPEM = "FAKEKEY" // static key path
	res, err := e.Apply(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusFailed || res.ExitCode != 2 {
		t.Fatalf("want failed/exit 2, got %+v", res)
	}
	if res.PerHost["staging-1.lab"].Failed != 1 {
		t.Errorf("per-host failure not parsed: %+v", res.PerHost)
	}
}
