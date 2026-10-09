package apply

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

func (f *fakeRec) AppendOut([]byte) error      { f.chunks++; return nil }
func (f *fakeRec) Flush(context.Context) error { return nil }
func (f *fakeRec) Close(context.Context) error { f.closed = true; return nil }
func (f *fakeRec) RecordingID() string         { return f.id }

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

func replaceAll(s, old, new string) string {
	return string(bytesReplace([]byte(s), []byte(old), []byte(new)))
}

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
		Hosts:  []string{"staging-1.lab"},
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
		AnsibleBin:  fakeAnsible(t, `[{"stats":{"staging-1.lab":{"ok":3,"changed":1,"failures":0,"unreachable":0,"skipped":0}}}]`, 0),
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

// fakeAnsibleCapture is like fakeAnsible but additionally:
//   - touches markerPath so tests can assert the binary ran at all
//   - copies the inventory it was invoked with (argv: -i <inventory> <playbook>)
//     so tests can assert host-key-verification args without racing the
//     workdir cleanup that happens when Apply returns.
func fakeAnsibleCapture(t *testing.T, stdout string, exit int) (bin, marker, invCopy string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "fake-ansible-playbook")
	marker = filepath.Join(dir, "ansible-ran")
	invCopy = filepath.Join(dir, "inventory-captured.ini")
	script := "#!/bin/sh\n" +
		"touch " + marker + "\n" +
		"cp \"${2:-/dev/null}\" " + invCopy + " 2>/dev/null || true\n" +
		"cat > /dev/null || true\n" +
		"printf '%s' " + shellQuote(stdout) + "\n" +
		"exit " + itoaT(exit) + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, marker, invCopy
}

func fileExists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	return err == nil
}

// evilRepo builds a repo whose playbook trips the lint gate with an
// unpinned package task and a denied command.
func evilRepo(t *testing.T) (url, rev string) {
	t.Helper()
	dir := t.TempDir()
	src := dir + "/src"
	mustGit(t, "", "init", "-b", "main", src)
	mustGit(t, src, "config", "user.email", "t@t")
	mustGit(t, src, "config", "user.name", "t")
	if err := os.MkdirAll(src+"/playbooks", 0o755); err != nil {
		t.Fatal(err)
	}
	evil := "- hosts: targets\n  tasks:\n    - name: install webserver\n      ansible.builtin.apt:\n        name: nginx\n    - name: cleanup\n      shell: rm -rf /var/lib/nope\n"
	if err := os.WriteFile(src+"/playbooks/evil.yml", []byte(evil), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, src, "add", "-A")
	mustGit(t, src, "commit", "-m", "evil")
	return "file://" + src, gitRev(t, src, "HEAD")
}

// TestEngineApplyGatesNeverRunAnsible covers every pre-run refusal gate:
// the fake ansible marker must be ABSENT in all of them — nothing may
// partially apply.
func TestEngineApplyGatesNeverRunAnsible(t *testing.T) {
	url, _, _, unmergedRev, _ := gitFixture(t)
	evilURL, evilRev := evilRepo(t)
	cases := []struct {
		name        string
		gitURL      string
		rev         string
		mutate      func(*Request)
		wantRefusal string
	}{
		{
			name: "lint_unpinned_package", gitURL: evilURL, rev: evilRev,
			mutate:      func(r *Request) { r.Playbook = "evil.yml"; r.DenyPatterns = []string{"rm -rf *"} },
			wantRefusal: RefusalLintFailed,
		},
		{
			name: "unmerged_revision", gitURL: url, rev: unmergedRev,
			mutate:      func(r *Request) {},
			wantRefusal: RefusalNotMerged,
		},
		{
			name: "empty_hosts", gitURL: url, rev: unmergedRev,
			mutate:      func(r *Request) { r.Hosts = nil },
			wantRefusal: RefusalEmptyHosts,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bin, marker, _ := fakeAnsibleCapture(t, "[]", 0)
			e := &Engine{AnsibleBin: bin}
			req := baseRequest(t, tc.gitURL, tc.rev)
			req.StaticKeyPEM = "FAKEKEY"
			tc.mutate(&req)
			res, err := e.Apply(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			if res.Status != StatusRefused || res.RefusalReason != tc.wantRefusal {
				t.Fatalf("want refused/%s, got %+v", tc.wantRefusal, res)
			}
			if fileExists(t, marker) {
				t.Errorf("ansible must NOT have run on %s refusal", tc.wantRefusal)
			}
		})
	}
}

// TestEngineApplySuccessHardenedInventory is the success path with the
// inventory assertions: host-key checking is mandatory and bound to the
// supplied known_hosts material inside the ephemeral workdir.
func TestEngineApplySuccessHardenedInventory(t *testing.T) {
	url, _, mergedRev, _, _ := gitFixture(t)
	bin, marker, invCopy := fakeAnsibleCapture(t, `[{"stats":{"staging-1.lab":{"ok":2,"changed":1,"failures":0,"unreachable":0,"skipped":0}}}]`, 0)
	ca := &fakeCA{}
	e := &Engine{AnsibleBin: bin}
	req := baseRequest(t, url, mergedRev)
	req.CAMint = ca
	res, err := e.Apply(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusSucceeded {
		t.Fatalf("want succeeded, got %+v", res)
	}
	if !fileExists(t, marker) {
		t.Fatal("fake ansible never ran")
	}
	inv, err := os.ReadFile(invCopy)
	if err != nil {
		t.Fatalf("inventory was not passed to ansible: %v", err)
	}
	s := string(inv)
	if !strings.Contains(s, "StrictHostKeyChecking=yes") {
		t.Fatal("inventory missing StrictHostKeyChecking=yes: " + s)
	}
	if !strings.Contains(s, "UserKnownHostsFile=") || !strings.Contains(s, "skquad-apply-") {
		t.Fatalf("inventory missing known_hosts path in ephemeral workdir: %s", s)
	}
	if !strings.Contains(s, "[targets]") || !strings.Contains(s, "ansible_user=ops") {
		t.Fatalf("inventory host group wiring wrong: %s", s)
	}
	if !strings.Contains(s, "CertificateFile=") {
		t.Fatalf("CA mode must pass CertificateFile per host: %s", s)
	}
}

// TestEngineApplyMissingSSHAuthMaterial: no CA minter AND no static key
// is a hard configuration error — refused before ansible runs.
func TestEngineApplyMissingSSHAuthMaterial(t *testing.T) {
	url, _, mergedRev, _, _ := gitFixture(t)
	bin, marker, _ := fakeAnsibleCapture(t, "[]", 0)
	e := &Engine{AnsibleBin: bin}
	req := baseRequest(t, url, mergedRev)
	req.CAMint = nil
	req.StaticKeyPEM = ""
	res, err := e.Apply(context.Background(), req)
	if err == nil {
		t.Fatalf("want error for missing ssh auth material, got %+v", res)
	}
	if !strings.Contains(err.Error(), "no SSH auth material") {
		t.Errorf("unexpected error: %v", err)
	}
	if fileExists(t, marker) {
		t.Error("ansible must not run without ssh auth material")
	}
}

// failingRecorder errors on every method — recording is best-effort and
// must never fail the apply (same semantics as TG-10).
type failingRecorder struct{}

func (failingRecorder) AppendOut([]byte) error      { return errors.New("sink exploded") }
func (failingRecorder) Flush(context.Context) error { return errors.New("flush exploded") }
func (failingRecorder) Close(context.Context) error { return errors.New("close exploded") }
func (failingRecorder) RecordingID() string         { return "rec-fail" }

func TestEngineApplyRecorderFailureBestEffort(t *testing.T) {
	url, _, mergedRev, _, _ := gitFixture(t)
	stdout := `[{"stats":{"staging-1.lab":{"ok":1,"changed":1,"failures":0,"unreachable":0,"skipped":0}}}]`

	t.Run("recorder_methods_error", func(t *testing.T) {
		e := &Engine{
			AnsibleBin:  fakeAnsible(t, stdout, 0),
			NewRecorder: func(string, map[string]any) (Recorder, error) { return failingRecorder{}, nil },
		}
		req := baseRequest(t, url, mergedRev)
		req.StaticKeyPEM = "K"
		res, err := e.Apply(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if res.Status != StatusSucceeded {
			t.Fatalf("recorder errors must not fail the apply: %+v", res)
		}
	})

	t.Run("recorder_factory_error", func(t *testing.T) {
		e := &Engine{
			AnsibleBin:  fakeAnsible(t, stdout, 0),
			NewRecorder: func(string, map[string]any) (Recorder, error) { return nil, errors.New("no sink") },
		}
		req := baseRequest(t, url, mergedRev)
		req.StaticKeyPEM = "K"
		res, err := e.Apply(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if res.Status != StatusSucceeded {
			t.Fatalf("recorder factory errors must not fail the apply: %+v", res)
		}
	})
}

// TestEngineApplyContextCancelled: a pre-cancelled context fails fast
// (git verify cannot proceed) and ansible never runs.
func TestEngineApplyContextCancelled(t *testing.T) {
	url, _, mergedRev, _, _ := gitFixture(t)
	bin, marker, _ := fakeAnsibleCapture(t, "[]", 0)
	e := &Engine{AnsibleBin: bin}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := baseRequest(t, url, mergedRev)
	req.StaticKeyPEM = "K"
	res, err := e.Apply(ctx, req)
	if err == nil {
		t.Fatalf("want error on cancelled context, got %+v", res)
	}
	if fileExists(t, marker) {
		t.Error("ansible must not run on cancelled context")
	}
}

// TestEngineApplyTimeoutBoundsRun: a runaway ansible is killed at the
// request timeout and the run surfaces as failed, not hung.
func TestEngineApplyTimeoutBoundsRun(t *testing.T) {
	url, _, mergedRev, _, _ := gitFixture(t)
	dir := t.TempDir()
	bin := filepath.Join(dir, "slow-ansible")
	// exec so the context kill lands on `sleep` itself — a forked child
	// would keep the stdout pipe open and stall the run past the kill.
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	e := &Engine{AnsibleBin: bin}
	req := baseRequest(t, url, mergedRev)
	req.StaticKeyPEM = "K"
	req.Timeout = 2 * time.Second
	start := time.Now()
	res, err := e.Apply(context.Background(), req)
	elapsed := time.Since(start)
	if elapsed > 15*time.Second {
		t.Fatalf("timeout not enforced, took %v", elapsed)
	}
	if err == nil && (res == nil || res.Status != StatusFailed) {
		t.Fatalf("want failed result or error after timeout, got %+v (err %v)", res, err)
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
