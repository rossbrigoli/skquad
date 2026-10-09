package ssh

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rossbrigoli/skquad/tool-gateway/internal/auth"
	"github.com/rossbrigoli/skquad/tool-gateway/internal/confirmation"
	"github.com/rossbrigoli/skquad/tool-gateway/internal/drivers"
	"github.com/rossbrigoli/skquad/tool-gateway/internal/policy"
)

// ---- apply-specific fakes ----

// applyConf records the exact tool identity and argsHash the gate uses,
// so tests can assert the ssh_apply#<group>#<playbook> contract.
type applyConf struct {
	mode   string // "auto" | "pending"
	tools  []string
	hashes []string
	mu     sync.Mutex
}

func (f *applyConf) Check(ctx context.Context, resourceID, agentID, tool, argsHash string) (*confirmation.CheckResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tools = append(f.tools, tool)
	f.hashes = append(f.hashes, argsHash)
	if f.mode == "auto" {
		return &confirmation.CheckResult{Mode: "auto", MatchedStandingGrantID: "sg-apply"}, nil
	}
	return &confirmation.CheckResult{Mode: "pending", ConfirmationID: "conf-1"}, nil
}

func (f *applyConf) Consume(ctx context.Context, id, argsHash string) (*confirmation.ConsumeResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hashes = append(f.hashes, argsHash)
	return &confirmation.ConsumeResult{Allowed: true, Mode: "once"}, nil
}

func (f *applyConf) snapshot() ([]string, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.tools...), append([]string{}, f.hashes...)
}

// applyTerminal is a stateful fake terminal-service for the apply API.
// getSeq supplies successive GET /v1/applies/{id} responses; the last
// one repeats forever.
type applyTerminal struct {
	srv        *httptest.Server
	mu         sync.Mutex
	posts      []map[string]any
	gets       int
	postStatus int
	postResp   map[string]any
	getSeq     []map[string]any
}

func newApplyTerminal(t *testing.T, postStatus int, postResp map[string]any, getSeq ...map[string]any) *applyTerminal {
	at := &applyTerminal{postStatus: postStatus, postResp: postResp, getSeq: getSeq}
	at.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && r.URL.Path == "/v1/applies" {
			body, _ := io.ReadAll(r.Body)
			var m map[string]any
			_ = json.Unmarshal(body, &m)
			at.mu.Lock()
			at.posts = append(at.posts, m)
			at.mu.Unlock()
			w.WriteHeader(at.postStatus)
			json.NewEncoder(w).Encode(at.postResp)
			return
		}
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/applies/") {
			at.mu.Lock()
			idx := at.gets
			if idx >= len(at.getSeq) {
				idx = len(at.getSeq) - 1
			}
			var resp map[string]any
			if idx >= 0 {
				resp = at.getSeq[idx]
			}
			at.gets++
			at.mu.Unlock()
			if resp == nil {
				w.WriteHeader(http.StatusNotFound)
				json.NewEncoder(w).Encode(map[string]any{"error": "not_found"})
				return
			}
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(resp)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]any{"error": "not_found"})
	}))
	t.Cleanup(at.srv.Close)
	return at
}

func (at *applyTerminal) postCalls() []map[string]any {
	at.mu.Lock()
	defer at.mu.Unlock()
	return append([]map[string]any{}, at.posts...)
}

func (at *applyTerminal) getCalls() int {
	at.mu.Lock()
	defer at.mu.Unlock()
	return at.gets
}

// shrinkPoll sets a tiny poll interval for the duration of a test.
func shrinkPoll(t *testing.T) {
	old := applyPollInterval
	applyPollInterval = 5 * time.Millisecond
	t.Cleanup(func() { applyPollInterval = old })
}

// ---- apply fixtures ----

const fullRev = "0123456789abcdef0123456789abcdef01234567"

// applyCeilingJSON: host_groups carry hosts+tiers; hosts must also pass the
// TG-10 hosts_allow fold (defense in depth).
const applyCeilingJSON = `{"hosts_allow":["staging-*"],"hosts_deny":["prod-db*"],` +
	`"host_groups":{"staging":{"hosts":["staging-1"],"tier":"medium"},"db":{"hosts":["prod-db1"],"tier":"high"}}}`

const applyConstraintsJSON = `{"apply_enabled":true}`

const applyConfig = `{"ssh_user":"ops","known_hosts":"staging-1 ssh-ed25519 AAAA","auth_mode":"ca",` +
	`"artifact":{"git_url":"https://git.example.com/team/playbooks.git","default_branch":"main","playbooks_path":"playbooks"}}`

func applyReq(operation, payload string) *drivers.Request {
	return applyReqGrant(operation, payload, applyCeilingJSON, applyConstraintsJSON, applyConfig)
}

func applyReqGrant(operation, payload, ceiling, constraints, config string) *drivers.Request {
	return &drivers.Request{
		Agent:     &auth.AgentPrincipal{AgentID: "a1"},
		Resource:  "res-ssh",
		Operation: operation,
		Payload:   []byte(payload),
		Grant: &policy.Grant{
			ResourceID:   "res-ssh",
			ResourceType: "ssh",
			Ceiling:      json.RawMessage(ceiling),
			Constraints:  json.RawMessage(constraints),
			Config:       json.RawMessage(config),
		},
	}
}

const validApply = `{"playbook":"deploy.yml","git_rev":"` + fullRev + `","host_group":"staging"}`

// ---- tests ----

func TestApplyValidation(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    string
	}{
		{"missing_playbook", `{"git_rev":"` + fullRev + `","host_group":"staging"}`, "bad_request"},
		{"blank_playbook", `{"playbook":"  ","git_rev":"` + fullRev + `","host_group":"staging"}`, "bad_request"},
		{"missing_host_group", `{"playbook":"deploy.yml","git_rev":"` + fullRev + `"}`, "bad_request"},
		{"missing_git_rev", `{"playbook":"deploy.yml","host_group":"staging"}`, "bad_request"},
		{"short_sha", `{"playbook":"deploy.yml","git_rev":"abc1234","host_group":"staging"}`, "invalid_git_rev"},
		{"branch_name", `{"playbook":"deploy.yml","git_rev":"main","host_group":"staging"}`, "invalid_git_rev"},
		{"upper_hex", `{"playbook":"deploy.yml","git_rev":"` + strings.ToUpper(fullRev) + `","host_group":"staging"}`, "invalid_git_rev"},
		{"absolute_playbook", `{"playbook":"/etc/passwd","git_rev":"` + fullRev + `","host_group":"staging"}`, "invalid_playbook"},
		{"traversal_playbook", `{"playbook":"../secrets.yml","git_rev":"` + fullRev + `","host_group":"staging"}`, "invalid_playbook"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			at := newApplyTerminal(t, 202, map[string]any{"id": "ap-1"})
			d := newDriver(t, at.srv.URL, "tok", &orderedAudit{}, &applyConf{mode: "auto"}, &fakeCreds{})
			_, err := d.Handle(context.Background(), applyReq("ssh_apply", tc.payload))
			if !isDenied(err, tc.want) {
				t.Fatalf("want %s, got %v", tc.want, err)
			}
			if len(at.postCalls()) != 0 {
				t.Error("terminal must not be called on validation failure")
			}
		})
	}
}

func TestApplyPayloadCannotRedirect(t *testing.T) {
	at := newApplyTerminal(t, 202, map[string]any{"id": "ap-1"},
		map[string]any{"status": "succeeded", "exit_code": 0})
	d := newDriver(t, at.srv.URL, "tok", &orderedAudit{}, &applyConf{mode: "auto"}, &fakeCreds{})
	evil := `{"playbook":"deploy.yml","git_rev":"` + fullRev + `","host_group":"staging",` +
		`"git_url":"https://evil.example.com/pwn.git","default_branch":"attacker","playbooks_path":"/tmp"}`
	if _, err := d.Handle(context.Background(), applyReq("ssh_apply", evil)); err != nil {
		t.Fatalf("apply: %v", err)
	}
	posts := at.postCalls()
	if len(posts) != 1 {
		t.Fatalf("posts: %+v", posts)
	}
	body := posts[0]
	if body["git_url"] != "https://git.example.com/team/playbooks.git" {
		t.Errorf("git_url not resource-owned: %v", body["git_url"])
	}
	if body["default_branch"] != "main" {
		t.Errorf("default_branch not resource-owned: %v", body["default_branch"])
	}
	if body["playbooks_path"] != "playbooks" {
		t.Errorf("playbooks_path not resource-owned: %v", body["playbooks_path"])
	}
}

func TestApplyConfirmationGate(t *testing.T) {
	t.Run("pending_denies_without_terminal", func(t *testing.T) {
		at := newApplyTerminal(t, 202, map[string]any{"id": "ap-1"})
		conf := &applyConf{mode: "pending"}
		d := newDriver(t, at.srv.URL, "tok", &orderedAudit{}, conf, &fakeCreds{})
		_, err := d.Handle(context.Background(), applyReq("ssh_apply", validApply))
		if err == nil || !strings.Contains(err.Error(), "confirmation_required:conf-1") {
			t.Fatalf("want confirmation_required:conf-1, got %v", err)
		}
		if len(at.postCalls()) != 0 {
			t.Error("pending gate must NOT call terminal")
		}
		tools, hashes := conf.snapshot()
		if len(tools) != 1 || tools[0] != "ssh_apply#staging#deploy.yml" {
			t.Errorf("tool identity wrong: %v", tools)
		}
		canonical, _ := json.Marshal(map[string]string{"host_group": "staging", "playbook": "deploy.yml"})
		want := confirmation.ArgsHash("res-ssh", "ssh_apply", canonical)
		if len(hashes) != 1 || hashes[0] != want {
			t.Errorf("argsHash mismatch: got %v want %s", hashes, want)
		}
	})
	t.Run("approved_proceeds", func(t *testing.T) {
		at := newApplyTerminal(t, 202, map[string]any{"id": "ap-2"},
			map[string]any{"status": "succeeded", "exit_code": 0})
		conf := &applyConf{mode: "auto"}
		d := newDriver(t, at.srv.URL, "tok", &orderedAudit{}, conf, &fakeCreds{})
		if _, err := d.Handle(context.Background(), applyReq("ssh_apply", validApply)); err != nil {
			t.Fatalf("approved apply must proceed: %v", err)
		}
		if len(at.postCalls()) != 1 {
			t.Errorf("terminal should have been called once: %+v", at.postCalls())
		}
	})
}

func TestApplyCeilingBeforeConfirmation(t *testing.T) {
	cases := []struct {
		name        string
		ceiling     string
		constraints string
		payload     string
		want        string
	}{
		{"unknown_host_group", applyCeilingJSON, applyConstraintsJSON,
			`{"playbook":"deploy.yml","git_rev":"` + fullRev + `","host_group":"nope"}`, "unknown_host_group"},
		{"group_outside_allow", applyCeilingJSON, `{"apply_enabled":true,"host_groups_allow":["web-*"]}`,
			validApply, "host_group_denied"},
		{"playbook_outside_allow", applyCeilingJSON, `{"apply_enabled":true,"playbooks_allow":["web/*.yml"]}`,
			validApply, "playbook_denied"},
		{"group_host_fails_tg10_fold", applyCeilingJSON, applyConstraintsJSON,
			`{"playbook":"deploy.yml","git_rev":"` + fullRev + `","host_group":"db"}`, "host_denied"},
		{"apply_not_enabled", applyCeilingJSON, `{}`, validApply, "apply_not_permitted"},
		{"bad_constraints", applyCeilingJSON, `{"apply_enabled":"yes"}`, validApply, "constraints_invalid"},
		{"no_artifact_config", applyCeilingJSON, applyConstraintsJSON, `{"playbook":"deploy.yml","git_rev":"` + fullRev + `","host_group":"staging"}`, "artifact_not_configured"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "no_artifact_config" {
				tc.payload = validApply // artifact missing from config below
			}
			at := newApplyTerminal(t, 202, map[string]any{"id": "ap-1"})
			conf := &applyConf{mode: "auto"}
			config := applyConfig
			if tc.name == "no_artifact_config" {
				config = `{"ssh_user":"ops","known_hosts":"h ssh-ed25519 AAAA","auth_mode":"ca"}`
			}
			d := newDriver(t, at.srv.URL, "tok", &orderedAudit{}, conf, &fakeCreds{})
			_, err := d.Handle(context.Background(), applyReqGrant("ssh_apply", tc.payload, tc.ceiling, tc.constraints, config))
			if !isDenied(err, tc.want) {
				t.Fatalf("want %s, got %v", tc.want, err)
			}
			tools, _ := conf.snapshot()
			if len(tools) != 0 {
				t.Errorf("ceiling denial must happen BEFORE confirmation: %v", tools)
			}
			if len(at.postCalls()) != 0 {
				t.Error("terminal must not be called on ceiling denial")
			}
		})
	}
}

func TestApplySuccessPath(t *testing.T) {
	perHost := json.RawMessage(`{"staging-1":{"changed":true}}`)
	at := newApplyTerminal(t, 202, map[string]any{"id": "ap-42", "status": "queued"},
		map[string]any{"status": "running"},
		map[string]any{
			"status": "succeeded", "exit_code": 0, "recording_id": "rec-9",
			"per_host": perHost,
		})
	shrinkPoll(t)
	em := &orderedAudit{}
	d := newDriver(t, at.srv.URL, "tok", em, &applyConf{mode: "auto"}, &fakeCreds{})
	resp, err := d.Handle(context.Background(), applyReq("ssh_apply", validApply))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if respBody(t, resp)["apply_id"] != "ap-42" || respBody(t, resp)["status"] != "succeeded" {
		t.Errorf("body: %+v", resp.Body)
	}
	if respBody(t, resp)["recording_id"] != "rec-9" {
		t.Errorf("recording_id: %v", respBody(t, resp)["recording_id"])
	}
	if respBody(t, resp)["per_host"] == nil {
		t.Error("per-host summary missing")
	}
	if !em.has("ssh_apply_attempt|") {
		t.Errorf("audit-before-execute missing: %v", em.list())
	}
	if !em.has("ssh_apply_result|") {
		t.Errorf("result audit missing: %v", em.list())
	}
	posts := at.postCalls()
	if len(posts) != 1 || posts[0]["host_group"] != "staging" {
		t.Fatalf("post body: %+v", posts)
	}
	if posts[0]["git_rev"] != fullRev {
		t.Errorf("git_rev: %v", posts[0]["git_rev"])
	}
}

func TestApplyWaitExpiry(t *testing.T) {
	at := newApplyTerminal(t, 202, map[string]any{"id": "ap-slow"},
		map[string]any{"status": "running"})
	shrinkPoll(t)
	d := newDriver(t, at.srv.URL, "tok", &orderedAudit{}, &applyConf{mode: "auto"}, &fakeCreds{})
	payload := `{"playbook":"deploy.yml","git_rev":"` + fullRev + `","host_group":"staging","timeout_seconds":1}`
	resp, err := d.Handle(context.Background(), applyReq("ssh_apply", payload))
	if err != nil {
		t.Fatalf("wait expiry must NOT be an error: %v", err)
	}
	if respBody(t, resp)["apply_id"] != "ap-slow" {
		t.Errorf("apply_id missing: %+v", resp.Body)
	}
	if respBody(t, resp)["continuation"] != "ssh_apply_status" {
		t.Errorf("continuation: %v", respBody(t, resp)["continuation"])
	}
	hint, _ := respBody(t, resp)["hint"].(string)
	if !strings.Contains(hint, "ssh_apply_status") {
		t.Errorf("hint must mention ssh_apply_status: %q", hint)
	}
	if at.getCalls() < 2 {
		t.Errorf("expected multiple polls, got %d", at.getCalls())
	}
}

func TestApplyStatusOp(t *testing.T) {
	t.Run("known_id", func(t *testing.T) {
		at := newApplyTerminal(t, 202, map[string]any{},
			map[string]any{"status": "succeeded", "exit_code": 0, "per_host": json.RawMessage(`{"h":"ok"}`)})
		d := newDriver(t, at.srv.URL, "tok", &orderedAudit{}, &applyConf{mode: "auto"}, &fakeCreds{})
		resp, err := d.Handle(context.Background(), applyReq("ssh_apply_status", `{"apply_id":"ap-7"}`))
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		if respBody(t, resp)["apply_id"] != "ap-7" || respBody(t, resp)["status"] != "succeeded" {
			t.Errorf("body: %+v", resp.Body)
		}
	})
	t.Run("unknown_id", func(t *testing.T) {
		at := newApplyTerminal(t, 202, map[string]any{}) // GET falls through to 404
		d := newDriver(t, at.srv.URL, "tok", &orderedAudit{}, &applyConf{mode: "auto"}, &fakeCreds{})
		_, err := d.Handle(context.Background(), applyReq("ssh_apply_status", `{"apply_id":"nope"}`))
		if !isDenied(err, "apply_not_found") {
			t.Fatalf("want apply_not_found, got %v", err)
		}
	})
	t.Run("missing_id", func(t *testing.T) {
		at := newApplyTerminal(t, 202, map[string]any{})
		d := newDriver(t, at.srv.URL, "tok", &orderedAudit{}, &applyConf{mode: "auto"}, &fakeCreds{})
		_, err := d.Handle(context.Background(), applyReq("ssh_apply_status", `{}`))
		if !isDenied(err, "bad_request") {
			t.Fatalf("want bad_request, got %v", err)
		}
	})
}

func TestApplyTerminalFailureMapping(t *testing.T) {
	cases := []struct {
		name       string
		postStatus int
		errBody    map[string]any
		want       string
	}{
		{"known_error_passthrough", 502, map[string]any{"error": "host_verification_failed"}, "host_verification_failed"},
		{"unknown_error_sanitized", 400, map[string]any{"error": "some raw internal detail"}, "terminal_error"},
		{"server_error", 500, map[string]any{}, "terminal_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			at := newApplyTerminal(t, tc.postStatus, tc.errBody)
			d := newDriver(t, at.srv.URL, "tok", &orderedAudit{}, &applyConf{mode: "auto"}, &fakeCreds{})
			_, err := d.Handle(context.Background(), applyReq("ssh_apply", validApply))
			if !isDenied(err, tc.want) {
				t.Fatalf("want %s, got %v", tc.want, err)
			}
		})
	}
	t.Run("poll_failure_maps", func(t *testing.T) {
		// POST ok, GET returns known error → denied with that error.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.Method == http.MethodPost {
				w.WriteHeader(202)
				json.NewEncoder(w).Encode(map[string]any{"id": "ap-x"})
				return
			}
			w.WriteHeader(503)
			json.NewEncoder(w).Encode(map[string]any{"error": "ca_unavailable"})
		}))
		defer srv.Close()
		shrinkPoll(t)
		d := newDriver(t, srv.URL, "tok", &orderedAudit{}, &applyConf{mode: "auto"}, &fakeCreds{})
		_, err := d.Handle(context.Background(), applyReq("ssh_apply", validApply))
		if !isDenied(err, "ca_unavailable") {
			t.Fatalf("want ca_unavailable, got %v", err)
		}
	})
}

func TestApplyCheckOnlySemantics(t *testing.T) {
	t.Run("dry_run_only_grant_refuses_real_apply", func(t *testing.T) {
		at := newApplyTerminal(t, 202, map[string]any{"id": "ap-1"})
		con := `{"apply_enabled":true,"check_only":true}`
		d := newDriver(t, at.srv.URL, "tok", &orderedAudit{}, &applyConf{mode: "auto"}, &fakeCreds{})
		_, err := d.Handle(context.Background(), applyReqGrant("ssh_apply", validApply, applyCeilingJSON, con, applyConfig))
		if !isDenied(err, "check_only_grant") {
			t.Fatalf("want check_only_grant, got %v", err)
		}
	})
	t.Run("check_only_flag_forwarded", func(t *testing.T) {
		at := newApplyTerminal(t, 202, map[string]any{"id": "ap-co"},
			map[string]any{"status": "succeeded", "exit_code": 0})
		con := `{"apply_enabled":true,"check_only":true}`
		d := newDriver(t, at.srv.URL, "tok", &orderedAudit{}, &applyConf{mode: "auto"}, &fakeCreds{})
		payload := `{"playbook":"deploy.yml","git_rev":"` + fullRev + `","host_group":"staging","check_only":true}`
		if _, err := d.Handle(context.Background(), applyReqGrant("ssh_apply", payload, applyCeilingJSON, con, applyConfig)); err != nil {
			t.Fatalf("check_only apply: %v", err)
		}
		posts := at.postCalls()
		if len(posts) != 1 {
			t.Fatalf("posts: %+v", posts)
		}
		if posts[0]["check_only"] != true {
			t.Errorf("check_only not forwarded: %v", posts[0]["check_only"])
		}
	})
}

func respBody(t *testing.T, resp *drivers.Response) map[string]any {
	t.Helper()
	m, ok := resp.Body.(map[string]any)
	if !ok {
		t.Fatalf("response body is not a map: %#v", resp.Body)
	}
	return m
}
