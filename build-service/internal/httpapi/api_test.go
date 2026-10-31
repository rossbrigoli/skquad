package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/rossbrigoli/skquad/build-service/internal/config"
	"github.com/rossbrigoli/skquad/build-service/internal/principal"
	"github.com/rossbrigoli/skquad/build-service/internal/service"
)

const testToken = "build-token-123"

// stubStore records created jobs in memory.
type stubStore struct{ jobs map[string]*batchv1.Job }

func newStubStore() *stubStore { return &stubStore{jobs: map[string]*batchv1.Job{}} }

func (s *stubStore) CreateJob(_ context.Context, job *batchv1.Job) error {
	s.jobs[job.Name] = job
	return nil
}

func (s *stubStore) GetJob(_ context.Context, _, name string) (*batchv1.Job, error) {
	if j, ok := s.jobs[name]; ok {
		return j, nil
	}
	return nil, &notFoundErr{}
}

type notFoundErr struct{}

func (*notFoundErr) Error() string { return "not found" }

func (s *stubStore) ListJobs(_ context.Context, _, sel string) ([]batchv1.Job, error) {
	var out []batchv1.Job
	for _, j := range s.jobs {
		if sel == "" || selMatches(j.Labels, sel) {
			out = append(out, *j)
		}
	}
	return out, nil
}

func (s *stubStore) ListPods(_ context.Context, _, _ string) ([]corev1.Pod, error) {
	return nil, nil
}

func selMatches(labels map[string]string, sel string) bool {
	for _, kv := range strings.Split(sel, ",") {
		p := strings.SplitN(kv, "=", 2)
		if len(p) != 2 || labels[p[0]] != p[1] {
			return false
		}
	}
	return true
}

type stubRev struct{ sha string }

func (r stubRev) Resolve(_ context.Context, _, _ string) (string, error) { return r.sha, nil }

func newTestServer(t *testing.T) (*httptest.Server, *stubStore) {
	t.Helper()
	cfg := &config.Config{
		Listen:            ":0",
		Namespace:         "skquad-builds",
		KanikoImage:       "img",
		RegistrySecret:    "reg",
		PrincipalsFile:    "/tmp/p",
		GitHostsAllow:     []string{"github.com"},
		DestinationsAllow: []string{"docker.io/brigss007/agent-builds/"},
		Limits:            config.Limits{MaxConcurrentPerPrincipal: 5, MaxPerHourPerPrincipal: 5},
		Job:               config.JobConfig{ActiveDeadlineSeconds: 60, TTLSecondsAfterFinished: 60},
	}
	doc := `{"principals":[{"id":"sherlock","agent_id":"a1","token_sha256_b64":"` +
		principal.HashToken(testToken) +
		`","repos_allow":["rossbrigoli/*"],"destinations_allow":["docker.io/brigss007/agent-builds/"]}]}`
	pstore, err := principal.Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	store := newStubStore()
	svc := service.New(cfg, store, nil, stubRev{sha: strings.Repeat("a", 40)}, logger)
	srv := httptest.NewServer(NewServer(pstore, svc, logger).Handler())
	t.Cleanup(srv.Close)
	return srv, store
}

func doReq(t *testing.T, method, url, token, body string) *http.Response {
	t.Helper()
	var rdr *bytes.Reader
	if body != "" {
		rdr = bytes.NewReader([]byte(body))
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

const goodBody = `{"repo":"github.com/rossbrigoli/skquad","revision":"main","dockerfile":"Dockerfile","destination":"docker.io/brigss007/agent-builds/skquad:dev"}`

func TestHealthAndReady(t *testing.T) {
	srv, _ := newTestServer(t)
	if resp := doReq(t, "GET", srv.URL+"/healthz", "", ""); resp.StatusCode != 200 {
		t.Fatalf("healthz %d", resp.StatusCode)
	}
	if resp := doReq(t, "GET", srv.URL+"/readyz", "", ""); resp.StatusCode != 200 {
		t.Fatalf("readyz %d", resp.StatusCode)
	}
}

func TestAuthDeny(t *testing.T) {
	srv, _ := newTestServer(t)
	for _, tok := range []string{"", "wrong", "Bearer wrong"} {
		resp := doReq(t, "POST", srv.URL+"/v1/builds", tok, goodBody)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("token %q: got %d want 401", tok, resp.StatusCode)
		}
	}
	// GET paths also require auth.
	if resp := doReq(t, "GET", srv.URL+"/v1/builds", "", ""); resp.StatusCode != 401 {
		t.Fatalf("list unauth: %d", resp.StatusCode)
	}
}

func TestCreateAccepted(t *testing.T) {
	srv, store := newTestServer(t)
	resp := doReq(t, "POST", srv.URL+"/v1/builds", testToken, goodBody)
	if resp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create: %d %s", resp.StatusCode, string(b))
	}
	var b service.Build
	if err := json.NewDecoder(resp.Body).Decode(&b); err != nil {
		t.Fatal(err)
	}
	if b.Status != "pending" || b.JobName == "" {
		t.Fatalf("bad build payload: %+v", b)
	}
	if len(store.jobs) != 1 {
		t.Fatalf("job not created: %d", len(store.jobs))
	}
}

func TestCreateBadJSON(t *testing.T) {
	srv, _ := newTestServer(t)
	resp := doReq(t, "POST", srv.URL+"/v1/builds", testToken, `{"nope":1}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("got %d want 400", resp.StatusCode)
	}
}

func TestCreateValidationDenied(t *testing.T) {
	srv, _ := newTestServer(t)
	resp := doReq(t, "POST", srv.URL+"/v1/builds", testToken, `{"repo":"evil.com/a/b","revision":"main","dockerfile":"D","destination":"docker.io/brigss007/agent-builds/x:1"}`)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("got %d want 403", resp.StatusCode)
	}
}

func TestGetAndList(t *testing.T) {
	srv, _ := newTestServer(t)
	resp := doReq(t, "POST", srv.URL+"/v1/builds", testToken, goodBody)
	var b service.Build
	if err := json.NewDecoder(resp.Body).Decode(&b); err != nil {
		t.Fatal(err)
	}

	got := doReq(t, "GET", srv.URL+"/v1/builds/"+b.ID, testToken, "")
	if got.StatusCode != 200 {
		t.Fatalf("get: %d", got.StatusCode)
	}

	lst := doReq(t, "GET", srv.URL+"/v1/builds", testToken, "")
	if lst.StatusCode != 200 {
		t.Fatalf("list: %d", lst.StatusCode)
	}
	var out struct {
		Builds []service.Build `json:"builds"`
	}
	if err := json.NewDecoder(lst.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Builds) != 1 || out.Builds[0].ID != b.ID {
		t.Fatalf("list mismatch: %+v", out.Builds)
	}
}

func TestGetUnknownBuild404(t *testing.T) {
	srv, _ := newTestServer(t)
	resp := doReq(t, "GET", srv.URL+"/v1/builds/deadbeefdeadbeefdeadbeef", testToken, "")
	if resp.StatusCode != 404 {
		t.Fatalf("got %d want 404", resp.StatusCode)
	}
}
