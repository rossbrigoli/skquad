// TG-4b (S-244 series, card 518fc777): git smart-HTTP driver tests.
// Fake upstreams only; the token is always a fake ("FAKE-GIT-TOKEN")
// and tests assert it never leaks into results/logs.
package git

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/tool-gateway/internal/auth"
	"github.com/rossbrigoli/skquad/tool-gateway/internal/credentials"
	"github.com/rossbrigoli/skquad/tool-gateway/internal/drivers"
	"github.com/rossbrigoli/skquad/tool-gateway/internal/policy"
)

// ── helpers ───────────────────────────────────────────────────────────────

const fakeToken = "FAKE-GIT-TOKEN-0001112223334445556667"

func grantFor(config, ceiling, constraints, egressClass string) *policy.Grant {
	g := &policy.Grant{ResourceID: "git-hub-1", ResourceType: "git", EgressClass: egressClass}
	if config != "" {
		g.Config = json.RawMessage(config)
	}
	if ceiling != "" {
		g.Ceiling = json.RawMessage(ceiling)
	}
	if constraints != "" {
		g.Constraints = json.RawMessage(constraints)
	}
	return g
}

// openGrant: ceiling allows the acme org + push, grant opts into push.
// egress_class=internal so the loopback httptest upstream is
// reachable through the netguard guard (same posture as rest tests).
func openGrant(baseURL string) *policy.Grant {
	return grantFor(
		fmt.Sprintf(`{"base_url":%q}`, baseURL),
		`{"repos_allow":["acme/*","other/app"],"allow_push":true,"rate_per_min":60}`,
		`{"allow_push":true}`,
		"internal",
	)
}

func streamReq(agent string, g *policy.Grant, trailing string) *drivers.Request {
	return &drivers.Request{
		Agent:     &auth.AgentPrincipal{AgentID: agent},
		Resource:  g.ResourceID,
		Operation: "git",
		Grant:     g,
		Path:      trailing,
	}
}

func bearerCreds() *fakeCreds {
	return &fakeCreds{secret: &credentials.Secret{
		Kind:   "bearer",
		Fields: map[string]string{"token": fakeToken},
	}}
}

// fakeCreds is a stub credentials.Resolver counting calls (to prove
// denials happen BEFORE any secret fetch).
type fakeCreds struct {
	secret *credentials.Secret
	err    error
	calls  atomic.Int64
}

func (f *fakeCreds) Resolve(_ context.Context, _ string, _ string) (*credentials.Secret, error) {
	f.calls.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	return f.secret, nil
}

// flushCountingRecorder tracks Flush() calls so streaming tests can
// prove chunked passthrough (many flushes, not one final write).
type flushCountingRecorder struct {
	*httptest.ResponseRecorder
	flushes atomic.Int64
}

func (f *flushCountingRecorder) Flush() {
	f.flushes.Add(1)
	f.ResponseRecorder.Flush()
}

func serve(t *testing.T, d *Driver, req *drivers.Request, method, target string) (*flushCountingRecorder, *drivers.StreamResult, error) {
	t.Helper()
	rec := &flushCountingRecorder{ResponseRecorder: httptest.NewRecorder()}
	r := httptest.NewRequest(method, target, nil)
	if req.Payload != nil {
		r = httptest.NewRequest(method, target, bytes.NewReader(req.Payload))
	}
	res, err := d.ServeStream(rec, r, req)
	return rec, res, err
}

// ── 1. upload-pack passthrough (GET info/refs + POST body) ──────────────

func TestUploadPackInfoRefsPassthrough(t *testing.T) {
	const refsBody = "001f# service=git-upload-pack\n0000001e0000"
	var gotAuth, gotPath, gotQuery string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		_, _ = w.Write([]byte(refsBody))
	}))
	defer up.Close()

	d := New(bearerCreds())
	g := openGrant(up.URL)
	rec, res, err := serve(t, d, streamReq("agent-1", g, "acme/widgets.git/info/refs"),
		http.MethodGet, "http://gateway/git/git-hub-1/acme/widgets.git/info/refs?service=git-upload-pack")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, refsBody, rec.Body.String(), "body must pass through byte-identical")
	require.Equal(t, "Bearer "+fakeToken, gotAuth, "credential injected upstream")
	require.Equal(t, "/acme/widgets.git/info/refs", gotPath)
	require.Equal(t, "service=git-upload-pack", gotQuery)
	require.Equal(t, "acme/widgets", res.Repo)
	require.Equal(t, ServiceUploadPack, res.Service)
	require.Equal(t, http.StatusOK, res.StatusCode)
}

func TestUploadPackPostStreamed(t *testing.T) {
	body := bytes.Repeat([]byte("pack-data-FAKE-"), 5000) // ~75 KiB
	var got bytes.Buffer
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(&got, r.Body)
		w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
		_, _ = w.Write([]byte("response-pack"))
	}))
	defer up.Close()

	d := New(bearerCreds())
	g := openGrant(up.URL)
	req := streamReq("agent-1", g, "acme/widgets.git/git-upload-pack")
	req.Payload = body
	rec, res, err := serve(t, d, req, http.MethodPost, "http://gateway/git/git-hub-1/acme/widgets.git/git-upload-pack")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "response-pack", rec.Body.String())
	require.Equal(t, body, got.Bytes(), "request body streamed upstream intact")
	require.Equal(t, ServiceUploadPack, res.Service)
}

// ── 2. repos_allow default-deny + org/* glob ────────────────────────────

func TestRepoAllowlistDefaultDeny(t *testing.T) {
	var hits atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer up.Close()

	creds := bearerCreds()
	d := New(creds)
	// Ceiling sets NO repos_allow → default-deny everything.
	g := grantFor(fmt.Sprintf(`{"base_url":%q}`, up.URL), `{"allow_push":true}`, `{"allow_push":true}`, "internal")
	_, _, err := serve(t, d, streamReq("a", g, "acme/widgets.git/info/refs"),
		http.MethodGet, "http://g/x?service=git-upload-pack")
	require.ErrorIs(t, err, drivers.ErrDenied)
	require.Zero(t, hits.Load(), "denied before any upstream connection")
	require.Zero(t, creds.calls.Load(), "denied before credential resolution")
}

func TestRepoAllowlistGlobAndExact(t *testing.T) {
	var hits atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()

	d := New(bearerCreds())
	g := openGrant(up.URL) // repos_allow: acme/*, other/app

	// org/* glob match
	_, res, err := serve(t, d, streamReq("a", g, "acme/widgets.git/info/refs"),
		http.MethodGet, "http://g/x?service=git-upload-pack")
	require.NoError(t, err)
	require.Equal(t, "acme/widgets", res.Repo)

	// exact match
	_, _, err = serve(t, d, streamReq("a", g, "other/app.git/info/refs"),
		http.MethodGet, "http://g/x?service=git-upload-pack")
	require.NoError(t, err)

	// not in the allowlist
	_, _, err = serve(t, d, streamReq("a", g, "evil/corp.git/info/refs"),
		http.MethodGet, "http://g/x?service=git-upload-pack")
	require.ErrorIs(t, err, drivers.ErrDenied)
	require.Equal(t, int64(2), hits.Load(), "only the two allowed repos reached upstream")
}

func TestRepoAllowlistGrantNarrowsNotWidens(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()

	d := New(bearerCreds())
	// Grant narrows to acme/widgets only.
	g := grantFor(
		fmt.Sprintf(`{"base_url":%q}`, up.URL),
		`{"repos_allow":["acme/*"],"allow_push":true}`,
		`{"repos_allow":["acme/widgets"],"allow_push":true}`,
		"internal",
	)
	_, _, err := serve(t, d, streamReq("a", g, "acme/other.git/info/refs"),
		http.MethodGet, "http://g/x?service=git-upload-pack")
	require.ErrorIs(t, err, drivers.ErrDenied, "grant narrowing must hide repos the ceiling allowed")

	_, _, err = serve(t, d, streamReq("a", g, "acme/widgets.git/info/refs"),
		http.MethodGet, "http://g/x?service=git-upload-pack")
	require.NoError(t, err)
}

// ── 3. read-only grant rejects receive-pack BEFORE upstream ──────────────

func TestReadOnlyGrantRejectsReceivePack(t *testing.T) {
	var hits atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer up.Close()

	creds := bearerCreds()
	d := New(creds)
	// Ceiling allows push but the GRANT does not opt in → read-only.
	g := grantFor(fmt.Sprintf(`{"base_url":%q}`, up.URL), `{"repos_allow":["acme/*"],"allow_push":true}`, `{}`, "internal")
	_, _, err := serve(t, d, streamReq("a", g, "acme/widgets.git/git-receive-pack"),
		http.MethodPost, "http://g/x")
	require.ErrorIs(t, err, drivers.ErrDenied)
	require.Contains(t, err.Error(), "push_not_allowed")
	require.Zero(t, hits.Load(), "receive-pack denial must not touch the upstream")
	require.Zero(t, creds.calls.Load(), "receive-pack denial must not resolve credentials")

	// Ceiling itself denies push: same result even with a push-hungry grant.
	g2 := grantFor(fmt.Sprintf(`{"base_url":%q}`, up.URL), `{"repos_allow":["acme/*"],"allow_push":false}`, `{"allow_push":true}`, "internal")
	_, _, err = serve(t, d, streamReq("a", g2, "acme/widgets.git/git-receive-pack"),
		http.MethodPost, "http://g/x")
	require.ErrorIs(t, err, drivers.ErrDenied)
	require.Zero(t, hits.Load())
}

// ── 4. push-granted receive-pack proxied + report-status parsed ─────────

func TestReceivePackProxiedAndReportStatusParsed(t *testing.T) {
	// report-status: unpack ok, one ref advanced, one rejected.
	report := pkt("unpack ok\n") +
		pkt("ok refs/heads/main\n") +
		pkt("ng refs/heads/bad rejected by hook\n") +
		"0000"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/acme/widgets.git/git-receive-pack", r.URL.Path)
		w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
		_, _ = w.Write([]byte(report))
	}))
	defer up.Close()

	d := New(bearerCreds())
	g := openGrant(up.URL)
	req := streamReq("pusher", g, "acme/widgets.git/git-receive-pack")
	req.Payload = []byte("fake-pack-body")
	rec, res, err := serve(t, d, req, http.MethodPost, "http://g/x")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, report, rec.Body.String(), "report-status streamed through byte-identical")
	require.Equal(t, []string{"refs/heads/main"}, res.RefsAdvanced)
	require.Equal(t, []string{"refs/heads/bad rejected by hook"}, res.RefsRejected)
	require.Equal(t, ServiceReceivePack, res.Service)
}

// pkt renders a git pkt-line: 4-hex length (including the header) + payload.
func pkt(payload string) string {
	n := len(payload) + 4
	return fmt.Sprintf("%04x%s", n, payload)
}

// ── 5. cross-host redirect refused ───────────────────────────────────────

func TestCrossHostRedirectRefused(t *testing.T) {
	var secondHostHits atomic.Int64
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondHostHits.Add(1)
	}))
	defer second.Close()

	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, second.URL+"/stolen", http.StatusFound)
	}))
	defer first.Close()

	d := New(bearerCreds())
	g := openGrant(first.URL)
	_, _, err := serve(t, d, streamReq("a", g, "acme/widgets.git/info/refs"),
		http.MethodGet, "http://g/x?service=git-upload-pack")
	require.ErrorIs(t, err, drivers.ErrDenied)
	require.Contains(t, err.Error(), "redirect_denied")
	require.Zero(t, secondHostHits.Load(), "credentialed call must not follow a cross-host redirect")
}

// ── 6. credential injected upstream, absent from results/logs ───────────

func TestCredentialNeverLeaks(t *testing.T) {
	var gotAuth string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte("ok"))
	}))
	defer up.Close()

	// Capture the standard logger to prove the driver logs nothing.
	var logBuf bytes.Buffer
	orig := logWriterSwap(&logBuf)
	defer orig()

	d := New(bearerCreds())
	g := openGrant(up.URL)
	_, res, err := serve(t, d, streamReq("a", g, "acme/widgets.git/info/refs"),
		http.MethodGet, "http://g/x?service=git-upload-pack")
	require.NoError(t, err)
	require.Equal(t, "Bearer "+fakeToken, gotAuth, "credential injected upstream")

	// The result (which flows into the audit detail) must not contain
	// the token, and the captured log must be empty of it.
	b, _ := json.Marshal(res)
	require.NotContains(t, string(b), fakeToken)
	require.NotContains(t, logBuf.String(), fakeToken)
}

// logWriterSwap redirects the standard logger into buf and returns a
// restore function (caplog for the driver package).
func logWriterSwap(buf *bytes.Buffer) func() {
	orig := log.Writer()
	log.SetOutput(buf)
	return func() { log.SetOutput(orig) }
}

// ── 7. large-body streaming does not buffer ─────────────────────────────

func TestLargeBodyStreamsWithoutBuffering(t *testing.T) {
	const size = 4 << 20 // 4 MiB — far above the 32 KiB copy chunk
	bigBody := make([]byte, size)
	_, _ = rand.Read(bigBody)

	// Upstream: echo the request size back and stream a large
	// response in chunks with explicit flushes.
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, err := io.Copy(io.Discard, r.Body)
		require.NoError(t, err)
		require.Equal(t, int64(size), n, "full pack-sized request body streamed to upstream")
		w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		for off := 0; off < size; off += 64 << 10 {
			end := off + 64<<10
			if end > size {
				end = size
			}
			_, _ = w.Write(bigBody[off:end])
			fl.Flush()
		}
	}))
	defer up.Close()

	d := New(bearerCreds())
	g := openGrant(up.URL)
	req := streamReq("a", g, "acme/widgets.git/git-upload-pack")
	req.Payload = bigBody

	rec := &flushCountingRecorder{ResponseRecorder: httptest.NewRecorder()}
	r := httptest.NewRequest(http.MethodPost, "http://g/x", bytes.NewReader(bigBody))
	res, err := d.ServeStream(rec, r, req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, size, rec.Body.Len(), "full response streamed to client")
	require.GreaterOrEqual(t, rec.flushes.Load(), int64(16),
		"chunked passthrough must flush repeatedly (no whole-body buffering)")
	require.NotNil(t, res)
}

// ── path parsing ─────────────────────────────────────────────────────────

func TestParseGitPath(t *testing.T) {
	cases := []struct {
		path, query, repo, service string
		wantErr                    bool
	}{
		{"acme/repo.git/info/refs", "service=git-upload-pack", "acme/repo", ServiceUploadPack, false},
		{"acme/repo.git/info/refs", "service=git-receive-pack", "acme/repo", ServiceReceivePack, false},
		{"acme/repo.git/git-upload-pack", "", "acme/repo", ServiceUploadPack, false},
		{"acme/repo.git/git-receive-pack", "", "acme/repo", ServiceReceivePack, false},
		{"org/sub/deep.git/git-upload-pack", "", "org/sub/deep", ServiceUploadPack, false},
		// .git required for unambiguous smart-HTTP framing
		{"acme/repo/info/refs", "service=git-upload-pack", "", "", true},
		{"acme/repo/git-upload-pack", "", "", "", true},
		// missing service on info/refs
		{"acme/repo.git/info/refs", "", "", "", true},
		// traversal / malformed
		{"acme/../evil.git/git-upload-pack", "", "", "", true},
		{"acme/.git/git-upload-pack", "", "", "", true},
		{"", "", "", "", true},
		{"onlyone.git", "", "", "", true},
	}
	for _, c := range cases {
		repo, service, err := ParseGitPath(c.path, c.query)
		if c.wantErr {
			require.Error(t, err, "path %q must be rejected", c.path)
			continue
		}
		require.NoError(t, err, "path %q", c.path)
		require.Equal(t, c.repo, repo)
		require.Equal(t, c.service, service)
	}
}

// ── policy folding ───────────────────────────────────────────────────────

func TestEffectivePolicyFolding(t *testing.T) {
	p, err := EffectivePolicy(
		json.RawMessage(`{"base_url":"https://github.com/"}`),
		json.RawMessage(`{"repos_allow":["acme/*"],"allow_push":true,"rate_per_min":120}`),
		json.RawMessage(`{"repos_allow":["acme/app"],"allow_push":true,"rate_per_min":30}`),
	)
	require.NoError(t, err)
	require.Equal(t, "https://github.com", p.BaseURL)
	require.Equal(t, []string{"acme/app"}, p.ReposAllow)
	require.True(t, p.AllowPush)
	require.Equal(t, 30, p.RatePerMin)

	// Push requires BOTH layers.
	p2, err := EffectivePolicy(
		json.RawMessage(`{"base_url":"https://github.com"}`),
		json.RawMessage(`{"repos_allow":["acme/*"],"allow_push":true}`),
		json.RawMessage(`{}`),
	)
	require.NoError(t, err)
	require.False(t, p2.AllowPush, "grant must explicitly opt into push")

	// Missing base_url denies.
	_, err = EffectivePolicy(nil, json.RawMessage(`{"repos_allow":["a/b"]}`), nil)
	require.Error(t, err)

	// Malformed JSON denies (never widens).
	_, err = EffectivePolicy(json.RawMessage(`{`), nil, nil)
	require.Error(t, err)
}

func TestUnknownServiceDenied(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("must not reach upstream")
	}))
	defer up.Close()
	d := New(bearerCreds())
	g := openGrant(up.URL)
	_, _, err := serve(t, d, streamReq("a", g, "acme/widgets.git/git-daemon-pack"),
		http.MethodPost, "http://g/x")
	require.Error(t, err)
	require.Contains(t, err.Error(), "bad_request")
}

func TestCredentialsFailureFailsClosed(t *testing.T) {
	var hits atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer up.Close()
	creds := &fakeCreds{err: credentials.ErrUnavailable}
	d := New(creds)
	g := openGrant(up.URL)
	_, _, err := serve(t, d, streamReq("a", g, "acme/widgets.git/info/refs"),
		http.MethodGet, "http://g/x?service=git-upload-pack")
	require.ErrorIs(t, err, drivers.ErrDenied)
	require.Contains(t, err.Error(), "credentials_unavailable")
	require.Zero(t, hits.Load())
}

func TestUnsupportedAuthKindDenied(t *testing.T) {
	var hits atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer up.Close()
	creds := &fakeCreds{secret: &credentials.Secret{Kind: "ssh_key", Fields: map[string]string{"key": "x"}}}
	d := New(creds)
	g := openGrant(up.URL)
	_, _, err := serve(t, d, streamReq("a", g, "acme/widgets.git/info/refs"),
		http.MethodGet, "http://g/x?service=git-upload-pack")
	require.ErrorIs(t, err, drivers.ErrDenied)
	require.Contains(t, err.Error(), "unsupported_auth_kind")
	require.Zero(t, hits.Load())
}

func TestBasicAuthInjected(t *testing.T) {
	var gotAuth string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte("ok"))
	}))
	defer up.Close()
	creds := &fakeCreds{secret: &credentials.Secret{
		Kind:   "basic",
		Fields: map[string]string{"username": "x-access-token", "password": fakeToken},
	}}
	d := New(creds)
	g := openGrant(up.URL)
	_, _, err := serve(t, d, streamReq("a", g, "acme/widgets.git/info/refs"),
		http.MethodGet, "http://g/x?service=git-upload-pack")
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(gotAuth, "Basic "), "basic auth injected")
	decoded, derr := base64.StdEncoding.DecodeString(strings.TrimPrefix(gotAuth, "Basic "))
	require.NoError(t, derr)
	require.Equal(t, "x-access-token:"+fakeToken, string(decoded))
}
