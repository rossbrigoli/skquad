package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/rossbrigoli/skquad/build-service/internal/authn"
	"github.com/rossbrigoli/skquad/build-service/internal/cppolicy"
	"github.com/rossbrigoli/skquad/build-service/internal/govern"
	"github.com/rossbrigoli/skquad/build-service/internal/runner"
)

type fakeProber struct{ refs map[string]string }

func (f fakeProber) ListRefs(_ context.Context, _ string) (map[string]string, error) {
	return f.refs, nil
}

type fakePolicy struct {
	globs []string
	err   error
}

func (f fakePolicy) GitReposAllow(_ context.Context, _ string) ([]string, error) {
	return f.globs, f.err
}

func newTestServer(t *testing.T, pol cppolicy.Lookup) (*Server, *runner.FakeRunner) {
	t.Helper()
	doc := map[string]any{"principals": []map[string]any{
		{
			"id": "sherlock", "agent_id": "agent-sherlock",
			"token_b64sha256":    authn.HashToken("tok-sherlock"),
			"repos_allow":        []string{"github.com/rossbrigoli/*"},
			"destinations_allow": []string{"ghcr.io/rossbrigoli/agent-builds/"},
			"active":             true,
		},
		{
			"id": "other", "agent_id": "agent-other",
			"token_b64sha256":    authn.HashToken("tok-other"),
			"repos_allow":        []string{"github.com/rossbrigoli/other-repo"},
			"destinations_allow": []string{"ghcr.io/rossbrigoli/agent-builds/"},
			"active":             true,
		},
	}}
	raw, _ := json.Marshal(doc)
	p := filepath.Join(t.TempDir(), "principals.json")
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := authn.LoadStore(p)
	if err != nil {
		t.Fatal(err)
	}
	fr := runner.NewFakeRunner()
	v := govern.NewValidator(govern.Deps{
		Policy:                    pol,
		Prober:                    fakeProber{refs: map[string]string{"refs/heads/main": "0123456789abcdef0123456789abcdef01234567"}},
		Active:                    fr,
		GitHostsAllow:             []string{"github.com"},
		DestinationsAllow:         []string{"ghcr.io/rossbrigoli/agent-builds/"},
		MaxConcurrentPerPrincipal: 2,
		MaxPerHourPerPrincipal:    10,
	})
	srv := NewServer(Deps{Store: store, Validator: v, Runner: fr})
	return srv, fr
}

func goodBody() []byte {
	b, _ := json.Marshal(map[string]string{
		"repo":        "https://github.com/rossbrigoli/skquad.git",
		"revision":    "main",
		"dockerfile":  "Dockerfile",
		"destination": "ghcr.io/rossbrigoli/agent-builds/skquad:demo-1",
	})
	return b
}

func doJSON(t *testing.T, h http.Handler, method, path, token string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHealthz(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	rec := doJSON(t, srv.Handler(), "GET", "/healthz", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz: %d", rec.Code)
	}
}

func TestUnauthenticated(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	for _, tok := range []string{"", "wrong"} {
		rec := doJSON(t, srv.Handler(), "POST", "/api/v1/builds", tok, goodBody())
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("token %q: want 401 got %d", tok, rec.Code)
		}
	}
}

func TestSubmitAllowed(t *testing.T) {
	srv, fr := newTestServer(t, fakePolicy{globs: []string{"github.com/rossbrigoli/*"}})
	rec := doJSON(t, srv.Handler(), "POST", "/api/v1/builds", "tok-sherlock", goodBody())
	if rec.Code != http.StatusCreated {
		t.Fatalf("want 201 got %d: %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	id, _ := out["build_id"].(string)
	if !runner.BuildIDPattern.MatchString(id) {
		t.Fatalf("bad build id: %v", out)
	}
	if out["state"] != runner.StateQueued {
		t.Fatalf("state: %v", out["state"])
	}
	if st, _ := fr.Get(context.Background(), id); st == nil || st.Principal != "sherlock" {
		t.Fatal("job not recorded for principal")
	}
}

func TestSubmitDenyMapping(t *testing.T) {
	cases := []struct {
		name    string
		pol     cppolicy.Lookup
		body    map[string]string
		want    int
		wantErr string
	}{
		{
			name: "repo outside principal grant", pol: fakePolicy{globs: []string{"github.com/rossbrigoli/*"}},
			body: map[string]string{"repo": "https://github.com/someoneelse/x.git", "revision": "main", "dockerfile": "Dockerfile", "destination": "ghcr.io/rossbrigoli/agent-builds/x:1"},
			want: http.StatusForbidden, wantErr: govern.CodeRepoNotAllowed,
		},
		{
			name: "destination denied", pol: fakePolicy{globs: []string{"github.com/rossbrigoli/*"}},
			body: map[string]string{"repo": "https://github.com/rossbrigoli/skquad.git", "revision": "main", "dockerfile": "Dockerfile", "destination": "ghcr.io/rossbrigoli/skquad-api-server:x"},
			want: http.StatusForbidden, wantErr: govern.CodeDestinationDenied,
		},
		{
			name: "invalid revision format", pol: fakePolicy{globs: []string{"github.com/rossbrigoli/*"}},
			body: map[string]string{"repo": "https://github.com/rossbrigoli/skquad.git", "revision": "bad revision!", "dockerfile": "Dockerfile", "destination": "ghcr.io/rossbrigoli/agent-builds/x:1"},
			want: http.StatusBadRequest, wantErr: govern.CodeInvalidRevision,
		},
		{
			name: "policy unavailable is 502", pol: fakePolicy{err: cppolicy.ErrPolicyUnavailable},
			body: map[string]string{"repo": "https://github.com/rossbrigoli/skquad.git", "revision": "main", "dockerfile": "Dockerfile", "destination": "ghcr.io/rossbrigoli/agent-builds/x:1"},
			want: http.StatusBadGateway, wantErr: govern.CodePolicyUnavailable,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newTestServer(t, tc.pol)
			body, _ := json.Marshal(tc.body)
			rec := doJSON(t, srv.Handler(), "POST", "/api/v1/builds", "tok-sherlock", body)
			if rec.Code != tc.want {
				t.Fatalf("want %d got %d: %s", tc.want, rec.Code, rec.Body.String())
			}
			var eb errorBody
			_ = json.Unmarshal(rec.Body.Bytes(), &eb)
			if eb.Error != tc.wantErr {
				t.Fatalf("want error %q got %q", tc.wantErr, eb.Error)
			}
		})
	}
}

func TestGetOwnBuildAndAntiEnumeration(t *testing.T) {
	srv, fr := newTestServer(t, fakePolicy{globs: []string{"github.com/rossbrigoli/*"}})
	rec := doJSON(t, srv.Handler(), "POST", "/api/v1/builds", "tok-sherlock", goodBody())
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	id := out["build_id"].(string)

	fr.SetState(id, runner.StateSucceeded, "sha256:aaaabbbbccccddddeeeeffff000011112222333344445555666677778888999a")

	rec = doJSON(t, srv.Handler(), "GET", "/api/v1/builds/"+id, "tok-sherlock", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner GET: %d %s", rec.Code, rec.Body.String())
	}
	var st runner.Status
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.State != runner.StateSucceeded || st.Digest == "" {
		t.Fatalf("status wrong: %+v", st)
	}

	// Another principal cannot see it.
	rec = doJSON(t, srv.Handler(), "GET", "/api/v1/builds/"+id, "tok-other", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("non-owner GET must 404, got %d", rec.Code)
	}

	// Unknown id 404s; malformed id 400s.
	rec = doJSON(t, srv.Handler(), "GET", "/api/v1/builds/deadbeefcafebabe", "tok-sherlock", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown id: %d", rec.Code)
	}
	rec = doJSON(t, srv.Handler(), "GET", "/api/v1/builds/BAD-ID", "tok-sherlock", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed id: %d", rec.Code)
	}
}

func TestListScopedToPrincipal(t *testing.T) {
	srv, fr := newTestServer(t, fakePolicy{globs: []string{"github.com/rossbrigoli/*"}})
	if rec := doJSON(t, srv.Handler(), "POST", "/api/v1/builds", "tok-sherlock", goodBody()); rec.Code != http.StatusCreated {
		t.Fatalf("submit: %d", rec.Code)
	}
	// Give "other" a build directly in the fake.
	_ = fr.Create(context.Background(), runner.Spec{ID: "1111222233334444", Principal: "other", Repo: "github.com/rossbrigoli/other-repo", Revision: "main", Dockerfile: "D", Destination: "ghcr.io/rossbrigoli/agent-builds/o:1"})

	rec := doJSON(t, srv.Handler(), "GET", "/api/v1/builds", "tok-sherlock", nil)
	var out struct {
		Builds []runner.Status `json:"builds"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Builds) != 1 || out.Builds[0].Principal != "sherlock" {
		t.Fatalf("list leaked other principals: %+v", out.Builds)
	}
}

func TestRunnerErrorIsDenied(t *testing.T) {
	srv, fr := newTestServer(t, fakePolicy{globs: []string{"github.com/rossbrigoli/*"}})
	fr.CreateErr = errors.New("api down")
	rec := doJSON(t, srv.Handler(), "POST", "/api/v1/builds", "tok-sherlock", goodBody())
	if rec.Code != http.StatusForbidden {
		t.Fatalf("runner failure must not report success: %d %s", rec.Code, rec.Body.String())
	}
}
