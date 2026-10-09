package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/rossbrigoli/skquad/terminal-service/internal/apply"
)

// stubApplyEngine mirrors the SessionStarter stub pattern: tests inject
// behavior without a real git/ansible pipeline.
type stubApplyEngine struct {
	fn func(ctx context.Context, req apply.Request) (*apply.Result, error)
}

func (s *stubApplyEngine) Apply(ctx context.Context, req apply.Request) (*apply.Result, error) {
	return s.fn(ctx, req)
}

const rev40 = "0000000000000000000000000000000000000001"

func validApplyBody() string {
	return `{"resource_id":"r","agent_id":"a","playbook":"web.yml","git_rev":"` + rev40 +
		`","host_group":"staging","hosts":["h1.lab"],"git_url":"https://example.invalid/x.git",` +
		`"default_branch":"main","playbooks_path":"playbooks","ssh_user":"ops",` +
		`"known_hosts":"h1.lab ssh-ed25519 AAAA","static_key_pem":"K"}`
}

func TestApplySubmitAndPollSucceeded(t *testing.T) {
	var gotReq apply.Request
	eng := &stubApplyEngine{fn: func(_ context.Context, req apply.Request) (*apply.Result, error) {
		time.Sleep(50 * time.Millisecond)
		gotReq = req
		return &apply.Result{
			Status:   apply.StatusSucceeded,
			ExitCode: 0,
			PerHost:  map[string]apply.HostResult{"h1.lab": {OK: 2, Changed: 1}},
		}, nil
	}}
	srv := testServer(t, Config{ApplyEngine: eng})
	defer srv.Close()

	resp := doReq(t, "POST", srv.URL+"/v1/applies", "test-token", validApplyBody())
	if resp.StatusCode != http.StatusAccepted {
		b, _ := json.Marshal(resp)
		t.Fatalf("want 202, got %d (%s)", resp.StatusCode, b)
	}
	var sub map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&sub); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if sub["status"] != ApplyStatusPending || sub["id"] == "" {
		t.Fatalf("bad 202 body: %+v", sub)
	}

	var got map[string]any
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		r := doReq(t, "GET", srv.URL+"/v1/applies/"+sub["id"], "test-token", "")
		if r.StatusCode != http.StatusOK {
			r.Body.Close()
			t.Fatalf("GET: want 200, got %d", r.StatusCode)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if got["status"] == apply.StatusSucceeded {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got["status"] != apply.StatusSucceeded {
		t.Fatalf("never reached succeeded: %+v", got)
	}
	if got["exit_code"].(float64) != 0 {
		t.Errorf("exit_code: %v", got["exit_code"])
	}
	ph, ok := got["per_host"].(map[string]any)
	if !ok {
		t.Fatalf("per_host missing: %+v", got)
	}
	h1 := ph["h1.lab"].(map[string]any)
	if h1["changed"].(float64) != 1 || h1["ok"].(float64) != 2 {
		t.Errorf("per_host counts wrong: %v", h1)
	}
	if _, ok := got["finished_at"]; !ok {
		t.Error("finished_at must be set when terminal")
	}
	// Engine contract: server-assigned ApplyID + bounded timeout.
	if gotReq.ApplyID != sub["id"] {
		t.Errorf("engine got ApplyID %q, want %q", gotReq.ApplyID, sub["id"])
	}
	if gotReq.Timeout != apply.DefaultApplyTimeout {
		t.Errorf("engine timeout %v, want %v", gotReq.Timeout, apply.DefaultApplyTimeout)
	}
}

func TestApplyValidationError400(t *testing.T) {
	srv := testServer(t, Config{})
	defer srv.Close()
	cases := []string{
		`{"playbook":"p.yml","git_rev":"` + rev40 + `","git_url":"u","default_branch":"main","ssh_user":"ops","hosts":[]}`,
		`{"git_rev":"` + rev40 + `","git_url":"u","default_branch":"main","ssh_user":"ops","hosts":["h"]}`,
		`{"playbook":"p.yml","git_url":"u","default_branch":"main","ssh_user":"ops","hosts":["h"]}`,
		`{"playbook":"p.yml","git_rev":"` + rev40 + `","default_branch":"main","ssh_user":"ops","hosts":["h"]}`,
		`{"playbook":"p.yml","git_rev":"` + rev40 + `","git_url":"u","ssh_user":"ops","hosts":["h"]}`,
		`{"playbook":"p.yml","git_rev":"` + rev40 + `","git_url":"u","default_branch":"main","hosts":["h"]}`,
		`not json`,
	}
	for i, body := range cases {
		resp := doReq(t, "POST", srv.URL+"/v1/applies", "test-token", body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("case %d: want 400, got %d (body %s)", i, resp.StatusCode, body)
		}
		resp.Body.Close()
	}
}

func TestApplyUnknownID404(t *testing.T) {
	srv := testServer(t, Config{})
	defer srv.Close()
	for _, p := range []string{"/v1/applies/deadbeef", "/v1/applies/"} {
		resp := doReq(t, "GET", srv.URL+p, "test-token", "")
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: want 404, got %d", p, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

func TestApplyAuthRequired(t *testing.T) {
	srv := testServer(t, Config{})
	defer srv.Close()
	resp := doReq(t, "POST", srv.URL+"/v1/applies", "", validApplyBody())
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("POST no token: want 401, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = doReq(t, "GET", srv.URL+"/v1/applies/whatever", "wrong", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("GET wrong token: want 401, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestApplyRegistryFull429(t *testing.T) {
	release := make(chan struct{})
	eng := &stubApplyEngine{fn: func(_ context.Context, _ apply.Request) (*apply.Result, error) {
		<-release
		return &apply.Result{Status: apply.StatusSucceeded}, nil
	}}
	srv := testServer(t, Config{MaxApplies: 1, ApplyEngine: eng})
	defer srv.Close()
	defer close(release)

	resp := doReq(t, "POST", srv.URL+"/v1/applies", "test-token", validApplyBody())
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("first apply: want 202, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp2 := doReq(t, "POST", srv.URL+"/v1/applies", "test-token", validApplyBody())
	if resp2.StatusCode != http.StatusTooManyRequests {
		t.Errorf("second apply: want 429, got %d", resp2.StatusCode)
	}
	resp2.Body.Close()
}

func TestApplyRegistryEvictsOldestFinished(t *testing.T) {
	eng := &stubApplyEngine{fn: func(_ context.Context, _ apply.Request) (*apply.Result, error) {
		return &apply.Result{Status: apply.StatusSucceeded}, nil
	}}
	srv := testServer(t, Config{MaxApplies: 1, ApplyEngine: eng})
	defer srv.Close()

	resp := doReq(t, "POST", srv.URL+"/v1/applies", "test-token", validApplyBody())
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("first apply: want 202, got %d", resp.StatusCode)
	}
	var first map[string]string
	json.NewDecoder(resp.Body).Decode(&first)
	resp.Body.Close()

	// Wait for the first job to finish so it becomes evictable.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		r := doReq(t, "GET", srv.URL+"/v1/applies/"+first["id"], "test-token", "")
		var got map[string]any
		json.NewDecoder(r.Body).Decode(&got)
		r.Body.Close()
		if got["status"] == apply.StatusSucceeded {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Registry is at its bound but the only job is finished: the next
	// apply must succeed by evicting it, not 429.
	resp2 := doReq(t, "POST", srv.URL+"/v1/applies", "test-token", validApplyBody())
	if resp2.StatusCode != http.StatusAccepted {
		t.Fatalf("second apply after finished job: want 202, got %d", resp2.StatusCode)
	}
	resp2.Body.Close()

	// The evicted job is no longer queryable.
	r := doReq(t, "GET", srv.URL+"/v1/applies/"+first["id"], "test-token", "")
	if r.StatusCode != http.StatusNotFound {
		t.Errorf("evicted job: want 404, got %d", r.StatusCode)
	}
	r.Body.Close()
}

func TestApplyEngineErrorSurfacesAsFailed(t *testing.T) {
	eng := &stubApplyEngine{fn: func(_ context.Context, _ apply.Request) (*apply.Result, error) {
		return nil, context.DeadlineExceeded
	}}
	srv := testServer(t, Config{ApplyEngine: eng})
	defer srv.Close()
	resp := doReq(t, "POST", srv.URL+"/v1/applies", "test-token", validApplyBody())
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202, got %d", resp.StatusCode)
	}
	var sub map[string]string
	json.NewDecoder(resp.Body).Decode(&sub)
	resp.Body.Close()

	var got map[string]any
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		r := doReq(t, "GET", srv.URL+"/v1/applies/"+sub["id"], "test-token", "")
		json.NewDecoder(r.Body).Decode(&got)
		r.Body.Close()
		if got["status"] == apply.StatusFailed {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got["status"] != apply.StatusFailed {
		t.Fatalf("want failed, got %+v", got)
	}
	if got["error"] == "" || got["error"] != context.DeadlineExceeded.Error() {
		t.Errorf("error message missing: %v", got["error"])
	}
}
