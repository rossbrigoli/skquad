package rest

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
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

func grantFor(config, ceiling, constraints string) *policy.Grant {
	g := &policy.Grant{ResourceID: "rest-system-1", ResourceType: "rest"}
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

func callReq(agent, payload string, g *policy.Grant) *drivers.Request {
	return &drivers.Request{
		Agent:     &auth.AgentPrincipal{AgentID: agent},
		Resource:  g.ResourceID,
		Operation: "call",
		Payload:   []byte(payload),
		Grant:     g,
	}
}

func raw(s string) json.RawMessage { return json.RawMessage(s) }

// fakeCreds is a stub credentials.Resolver. It counts resolutions so
// tests can prove ACL denials happen BEFORE any secret fetch, and
// records the resolved agent id (TG-4c isolation assertions).
type fakeCreds struct {
	secret    *credentials.Secret
	err       error
	calls     atomic.Int64
	lastAgent atomic.Value // string
}

func (f *fakeCreds) Resolve(_ context.Context, _ string, agentID string) (*credentials.Secret, error) {
	f.calls.Add(1)
	f.lastAgent.Store(agentID)
	if f.err != nil {
		return nil, f.err
	}
	return f.secret, nil
}

// keyedCreds serves a DIFFERENT secret per agent id (TG-4c, S-259
// isolation): the resolution is keyed by the calling agent, so agent
// A can never receive agent B's material.
type keyedCreds struct {
	byAgent map[string]*credentials.Secret
	calls   atomic.Int64
}

func (k *keyedCreds) Resolve(_ context.Context, _, agentID string) (*credentials.Secret, error) {
	k.calls.Add(1)
	s, ok := k.byAgent[agentID]
	if !ok {
		return nil, credentials.ErrUnavailable
	}
	return s, nil
}

// openGrant builds a permissive-but-internal grant for the given base
// URL: all methods, all paths, private egress allowed (httptest runs
// on loopback).
func openGrant(baseURL, authKind string) *policy.Grant {
	cfg := fmt.Sprintf(`{"base_url":%q,"auth_kind":%q}`, baseURL, authKind)
	ce := fmt.Sprintf(`{"methods":["GET","POST","PUT","PATCH","DELETE"],"path_allow":["/**"],"egress_class":"internal"}`)
	con := `{"egress_class":"internal"}`
	return grantFor(cfg, ce, con)
}

func respOf(t *testing.T, r *drivers.Response) *Response {
	t.Helper()
	out, ok := r.Body.(*Response)
	require.True(t, ok, "driver body must be *rest.Response")
	return out
}

// ── EffectivePolicy folding ───────────────────────────────────────────────

func TestEffectivePolicyRequiresBaseURL(t *testing.T) {
	_, err := EffectivePolicy(raw(`{}`), raw(`{}`), raw(`{}`))
	require.Error(t, err)
}

func TestEffectivePolicyDefaults(t *testing.T) {
	p, err := EffectivePolicy(raw(`{"base_url":"https://api.example.com/","auth_kind":"none"}`), nil, nil)
	require.NoError(t, err)
	require.Equal(t, "https://api.example.com", p.BaseURL, "trailing slash trimmed")
	require.Equal(t, DefaultMaxRequestBytes, p.MaxRequestBytes)
	require.Equal(t, DefaultMaxResponseBytes, p.MaxResponseBytes)
	require.Empty(t, p.Methods, "unset methods must deny everything")
	require.False(t, p.methodAllowed("GET"))
	require.False(t, p.pathAllowed("/anything"), "unset path_allow must default-deny")
	require.False(t, p.AllowPrivate)
}

func TestEffectivePolicyMethodIntersection(t *testing.T) {
	p, err := EffectivePolicy(
		raw(`{"base_url":"https://x.example"}`),
		raw(`{"methods":["GET","POST"]}`),
		raw(`{"methods":["get"]}`),
	)
	require.NoError(t, err)
	require.Equal(t, []string{"GET"}, p.Methods)
	require.True(t, p.methodAllowed("GET"))
	require.False(t, p.methodAllowed("POST"), "grant tightened methods to GET")
}

func TestEffectivePolicyPathFold(t *testing.T) {
	// Grant path_allow replaces the ceiling's (CP proved it a subset);
	// deny is a union and always wins.
	p, err := EffectivePolicy(
		raw(`{"base_url":"https://x.example"}`),
		raw(`{"path_allow":["/issues/**","/search"],"path_deny":["/issues/private/**"]}`),
		raw(`{"path_allow":["/issues/**"],"path_deny":["/issues/secret*"]}`),
	)
	require.NoError(t, err)
	require.Equal(t, []string{"/issues/**"}, p.PathAllow)
	require.True(t, p.pathAllowed("/issues/123"))
	require.False(t, p.pathAllowed("/search"), "grant narrowed away /search")
	require.False(t, p.pathAllowed("/issues/private/9"), "ceiling deny still wins")
	require.False(t, p.pathAllowed("/issues/secret-1"), "grant-added deny wins")
}

func TestEffectivePolicyNumericMin(t *testing.T) {
	p, err := EffectivePolicy(
		raw(`{"base_url":"https://x.example"}`),
		raw(`{"max_request_bytes":5000,"max_response_bytes":9000,"rate_per_min":60}`),
		raw(`{"max_request_bytes":1000,"max_response_bytes":2000,"rate_per_min":10}`),
	)
	require.NoError(t, err)
	require.Equal(t, 1000, p.MaxRequestBytes)
	require.Equal(t, 2000, p.MaxResponseBytes)
	require.Equal(t, 10, p.RatePerMin)
}

func TestEffectivePolicyEgressRequiresBothInternal(t *testing.T) {
	p, err := EffectivePolicy(raw(`{"base_url":"https://x.example"}`),
		raw(`{"egress_class":"internal"}`), raw(`{"egress_class":"internal"}`))
	require.NoError(t, err)
	require.True(t, p.AllowPrivate)

	p, err = EffectivePolicy(raw(`{"base_url":"https://x.example"}`),
		raw(`{"egress_class":"internal"}`), raw(`{"egress_class":"public"}`))
	require.NoError(t, err)
	require.False(t, p.AllowPrivate, "grant cannot widen egress")

	p, err = EffectivePolicy(raw(`{"base_url":"https://x.example"}`), raw(`{}`), raw(`{"egress_class":"internal"}`))
	require.NoError(t, err)
	require.False(t, p.AllowPrivate, "grant cannot exceed ceiling")
}

// ── Auth injection per kind ───────────────────────────────────────────────

func TestAuthInjectionBearer(t *testing.T) {
	var gotAuth string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer up.Close()

	creds := &fakeCreds{secret: &credentials.Secret{
		Kind:   AuthBearer,
		Fields: map[string]string{"token": "test-token-FAKE-bearer"},
	}}
	d := New(creds)
	g := openGrant(up.URL, AuthBearer)
	res, err := d.Handle(context.Background(), callReq("a-bearer", `{"method":"GET","path":"/x"}`, g))
	require.NoError(t, err)
	require.Equal(t, "Bearer test-token-FAKE-bearer", gotAuth)
	require.Equal(t, int64(1), creds.calls.Load())
	require.Equal(t, http.StatusOK, respOf(t, res).Status)
}

func TestAuthInjectionAPIKeyHeader(t *testing.T) {
	var gotKey, gotAuth string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-Api-Key")
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer up.Close()

	cfg := fmt.Sprintf(`{"base_url":%q,"auth_kind":"api_key_header","header_name":"X-Api-Key"}`, up.URL)
	g := grantFor(cfg,
		`{"methods":["GET"],"path_allow":["/**"],"egress_class":"internal"}`,
		`{"egress_class":"internal"}`)
	creds := &fakeCreds{secret: &credentials.Secret{
		Kind:   AuthAPIKeyHeader,
		Fields: map[string]string{"token": "test-key-FAKE-123"},
	}}
	d := New(creds)
	_, err := d.Handle(context.Background(), callReq("a-apikey", `{"method":"GET","path":"/x"}`, g))
	require.NoError(t, err)
	require.Equal(t, "test-key-FAKE-123", gotKey)
	require.Empty(t, gotAuth, "api_key_header must not touch Authorization")
}

func TestAuthInjectionBasic(t *testing.T) {
	var gotAuth string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer up.Close()

	creds := &fakeCreds{secret: &credentials.Secret{
		Kind:   AuthBasic,
		Fields: map[string]string{"username": "ross", "password": "test-pass-FAKE"},
	}}
	d := New(creds)
	g := openGrant(up.URL, AuthBasic)
	_, err := d.Handle(context.Background(), callReq("a-basic", `{"method":"GET","path":"/x"}`, g))
	require.NoError(t, err)
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("ross:test-pass-FAKE"))
	require.Equal(t, want, gotAuth)
}

func TestAuthNoneSkipsResolver(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Empty(t, r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{}`))
	}))
	defer up.Close()

	creds := &fakeCreds{}
	d := New(creds)
	g := openGrant(up.URL, AuthNone)
	_, err := d.Handle(context.Background(), callReq("a-none", `{"method":"GET","path":"/x"}`, g))
	require.NoError(t, err)
	require.Equal(t, int64(0), creds.calls.Load(), "auth_kind=none must not fetch a secret")
}

// ── OAuth2 client-credentials ─────────────────────────────────────────────

type mockIdP struct {
	srv   *httptest.Server
	hits  atomic.Int64
	token atomic.Value // string
}

func newMockIdP(t *testing.T) *mockIdP {
	m := &mockIdP{}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		n := m.hits.Add(1)
		tok := fmt.Sprintf("test-access-FAKE-%d", n)
		m.token.Store(tok)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fmt.Sprintf(`{"access_token":%q,"expires_in":3600}`, tok)))
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func oauthGrant(t *testing.T, baseURL string) (*policy.Grant, *fakeCreds, *mockIdP) {
	t.Helper()
	idp := newMockIdP(t)
	creds := &fakeCreds{secret: &credentials.Secret{
		Kind: AuthOAuth2ClientCreds,
		Fields: map[string]string{
			"client_id":     "cid-FAKE",
			"client_secret": "csecret-FAKE",
			"token_url":     idp.srv.URL + "/token",
		},
	}}
	return openGrant(baseURL, AuthOAuth2ClientCreds), creds, idp
}

func TestOAuthHappyPathCachesToken(t *testing.T) {
	var gotAuth string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer up.Close()

	g, creds, idp := oauthGrant(t, up.URL)
	d := New(creds)
	for i := 0; i < 3; i++ {
		_, err := d.Handle(context.Background(), callReq("a-oauth", `{"method":"GET","path":"/x"}`, g))
		require.NoError(t, err)
	}
	require.Equal(t, int64(1), idp.hits.Load(), "token must be cached across calls")
	require.Equal(t, "Bearer test-access-FAKE-1", gotAuth)
	require.Equal(t, int64(3), creds.calls.Load(), "secret resolved per call")
}

func TestOAuthRefreshOn401ExactlyOneRetry(t *testing.T) {
	var upHits atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := upHits.Add(1)
		if n == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"fresh":true}`))
	}))
	defer up.Close()

	g, creds, idp := oauthGrant(t, up.URL)
	d := New(creds)
	res, err := d.Handle(context.Background(), callReq("a-oauth401", `{"method":"GET","path":"/x"}`, g))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, respOf(t, res).Status)
	require.Equal(t, int64(2), upHits.Load(), "one original + one retry, no more")
	require.Equal(t, int64(2), idp.hits.Load(), "initial mint + exactly one refresh")
}

func TestOAuthStill401AfterRetryStands(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer up.Close()

	g, creds, idp := oauthGrant(t, up.URL)
	d := New(creds)
	res, err := d.Handle(context.Background(), callReq("a-oauth401b", `{"method":"GET","path":"/x"}`, g))
	require.NoError(t, err, "upstream status passes through after retry budget spent")
	require.Equal(t, http.StatusUnauthorized, respOf(t, res).Status)
	require.Equal(t, int64(2), idp.hits.Load())
}

// ── ACL / headers / caps ──────────────────────────────────────────────────

func TestACLDefaultDenyPreNetwork(t *testing.T) {
	var hits atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer up.Close()
	creds := &fakeCreds{secret: &credentials.Secret{Kind: AuthBearer, Fields: map[string]string{"token": "t-FAKE"}}}
	d := New(creds)

	// Ceiling sets no methods and no path_allow → everything denied.
	g := grantFor(fmt.Sprintf(`{"base_url":%q,"auth_kind":"bearer"}`, up.URL),
		`{"egress_class":"internal"}`, `{"egress_class":"internal"}`)
	_, err := d.Handle(context.Background(), callReq("a-acl", `{"method":"GET","path":"/x"}`, g))
	require.ErrorIs(t, err, drivers.ErrDenied)
	require.Contains(t, err.Error(), "method_denied")
	require.Equal(t, int64(0), hits.Load(), "denied before network")
	require.Equal(t, int64(0), creds.calls.Load(), "denied before secret fetch")
}

func TestACLMethodDeniedPreNetwork(t *testing.T) {
	var hits atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer up.Close()
	d := New(&fakeCreds{})
	g := openGrant(up.URL, AuthNone)
	_, err := d.Handle(context.Background(), callReq("a-meth", `{"method":"DELETE","path":"/x"}`,
		grantFor(fmt.Sprintf(`{"base_url":%q,"auth_kind":"none"}`, up.URL),
			`{"methods":["GET"],"path_allow":["/**"],"egress_class":"internal"}`,
			`{"egress_class":"internal"}`)))
	require.ErrorIs(t, err, drivers.ErrDenied)
	require.Contains(t, err.Error(), "method_denied")
	// sanity: the openGrant above (all methods) would allow DELETE
	_ = g
	require.Equal(t, int64(0), hits.Load())
}

func TestACLPathDenyOverridePreNetwork(t *testing.T) {
	var hits atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer up.Close()
	creds := &fakeCreds{secret: &credentials.Secret{Kind: AuthBearer, Fields: map[string]string{"token": "t-FAKE"}}}
	d := New(creds)
	g := grantFor(fmt.Sprintf(`{"base_url":%q,"auth_kind":"bearer"}`, up.URL),
		`{"methods":["GET"],"path_allow":["/issues/**"],"path_deny":["/issues/private/**"],"egress_class":"internal"}`,
		`{"egress_class":"internal"}`)
	_, err := d.Handle(context.Background(), callReq("a-path", `{"method":"GET","path":"/issues/private/7"}`, g))
	require.ErrorIs(t, err, drivers.ErrDenied)
	require.Contains(t, err.Error(), "path_denied")
	require.Equal(t, int64(0), hits.Load())

	_, err = d.Handle(context.Background(), callReq("a-path", `{"method":"GET","path":"/issues/7"}`, g))
	require.NoError(t, err)
	require.Equal(t, int64(1), hits.Load())
}

func TestHeaderRejection(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer up.Close()
	d := New(&fakeCreds{})
	g := openGrant(up.URL, AuthNone)

	bad := []string{"authorization", "Authorization", "host", "x-forwarded-for", "X-FORWARDED-HOST", "cookie", "user-agent", "proxy-authorization"}
	for _, h := range bad {
		payload, _ := json.Marshal(map[string]any{"method": "GET", "path": "/x", "headers": map[string]string{h: "evil"}})
		_, err := d.Handle(context.Background(), callReq("a-hdr", string(payload), g))
		require.ErrorIs(t, err, drivers.ErrDenied, "header %q must be denied", h)
		require.Contains(t, err.Error(), "header_denied")
	}

	// Allowed headers pass.
	payload := `{"method":"GET","path":"/x","headers":{"content-type":"application/json","accept":"*/*"}}`
	_, err := d.Handle(context.Background(), callReq("a-hdr-ok", payload, g))
	require.NoError(t, err)
}

func TestOversizeRequestDeniedPreSend(t *testing.T) {
	var hits atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer up.Close()
	d := New(&fakeCreds{})
	g := grantFor(fmt.Sprintf(`{"base_url":%q,"auth_kind":"none"}`, up.URL),
		`{"methods":["POST"],"path_allow":["/**"],"max_request_bytes":100,"egress_class":"internal"}`,
		`{"egress_class":"internal"}`)
	body := strings.Repeat("A", 101)
	_, err := d.Handle(context.Background(), callReq("a-big", `{"method":"POST","path":"/x","body":"`+body+`"}`, g))
	require.ErrorIs(t, err, drivers.ErrDenied)
	require.Contains(t, err.Error(), "request_too_large")
	require.Equal(t, int64(0), hits.Load(), "oversize body must be rejected before send")
}

func TestOversizeResponseTruncated(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("B", 2000)))
	}))
	defer up.Close()
	d := New(&fakeCreds{})
	g := grantFor(fmt.Sprintf(`{"base_url":%q,"auth_kind":"none"}`, up.URL),
		`{"methods":["GET"],"path_allow":["/**"],"max_response_bytes":1000,"egress_class":"internal"}`,
		`{"egress_class":"internal"}`)
	res, err := d.Handle(context.Background(), callReq("a-trunc", `{"method":"GET","path":"/x"}`, g))
	require.NoError(t, err)
	out := respOf(t, res)
	require.True(t, out.Truncated)
	body, err := base64.StdEncoding.DecodeString(out.BodyB64)
	require.NoError(t, err)
	require.Len(t, body, 1000)
}

func TestResponseScrubbing(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Set-Cookie", "session=leaky-FAKE")
		w.Header().Add("Set-Authorization", "nope-FAKE")
		w.Header().Add("Authorization", "echo-FAKE")
		w.Header().Add("Www-Authenticate", "Bearer realm=x")
		w.Header().Add("X-Keep", "yes")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer up.Close()
	d := New(&fakeCreds{})
	g := openGrant(up.URL, AuthNone)
	res, err := d.Handle(context.Background(), callReq("a-scrub", `{"method":"GET","path":"/x"}`, g))
	require.NoError(t, err)
	h := respOf(t, res).Headers
	require.Equal(t, "yes", h["X-Keep"])
	for _, k := range []string{"Set-Cookie", "Set-Authorization", "Authorization", "Www-Authenticate"} {
		_, present := h[k]
		require.False(t, present, "%s must be scrubbed from agent-visible headers", k)
	}
}

// ── Rate limit ────────────────────────────────────────────────────────────

func TestRateLimitEnforced(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer up.Close()
	d := New(&fakeCreds{})
	g := grantFor(fmt.Sprintf(`{"base_url":%q,"auth_kind":"none"}`, up.URL),
		`{"methods":["GET"],"path_allow":["/**"],"rate_per_min":1,"egress_class":"internal"}`,
		`{"egress_class":"internal"}`)
	_, err := d.Handle(context.Background(), callReq("a-rl", `{"method":"GET","path":"/x"}`, g))
	require.NoError(t, err)
	_, err = d.Handle(context.Background(), callReq("a-rl", `{"method":"GET","path":"/x"}`, g))
	require.ErrorIs(t, err, drivers.ErrDenied)
	require.Contains(t, err.Error(), "rate_limited")
	// Different agent has its own bucket.
	_, err = d.Handle(context.Background(), callReq("a-rl-other", `{"method":"GET","path":"/x"}`, g))
	require.NoError(t, err)
}

// ── SSRF / redirect / fail-closed ─────────────────────────────────────────

type stubResolver struct{ answers []net.IPAddr }

func (s *stubResolver) LookupIPAddr(_ context.Context, _ string) ([]net.IPAddr, error) {
	return s.answers, nil
}

func TestSSRFBlockedPublicClass(t *testing.T) {
	creds := &fakeCreds{secret: &credentials.Secret{Kind: AuthBearer, Fields: map[string]string{"token": "t-FAKE"}}}
	d := New(creds)
	d.Resolver = &stubResolver{answers: []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}}
	g := grantFor(`{"base_url":"https://api.rebind.test","auth_kind":"bearer"}`,
		`{"methods":["GET"],"path_allow":["/**"]}`, `{}`)
	_, err := d.Handle(context.Background(), callReq("a-ssrf", `{"method":"GET","path":"/x"}`, g))
	require.ErrorIs(t, err, drivers.ErrDenied)
	require.Contains(t, err.Error(), "ssrf_blocked")
}

func TestCrossHostRedirectRefusedForCredentialedCall(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/a" {
			w.Header().Set("Location", "http://elsewhere.invalid/b")
			w.WriteHeader(http.StatusFound)
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer up.Close()
	creds := &fakeCreds{secret: &credentials.Secret{Kind: AuthAPIKeyHeader, Fields: map[string]string{"token": "k-FAKE"}}}
	cfg := fmt.Sprintf(`{"base_url":%q,"auth_kind":"api_key_header","header_name":"X-Api-Key"}`, up.URL)
	g := grantFor(cfg,
		`{"methods":["GET"],"path_allow":["/**"],"egress_class":"internal"}`,
		`{"egress_class":"internal"}`)
	d := New(creds)
	_, err := d.Handle(context.Background(), callReq("a-redir", `{"method":"GET","path":"/a"}`, g))
	require.ErrorIs(t, err, drivers.ErrDenied)
	require.Contains(t, err.Error(), "redirect_denied", "custom auth headers must not ride cross-host redirects")
}

func TestCredentialKindMismatchFailClosed(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("upstream must not be reached on kind mismatch")
	}))
	defer up.Close()
	creds := &fakeCreds{secret: &credentials.Secret{Kind: AuthBasic, Fields: map[string]string{"username": "u", "password": "***"}}}
	d := New(creds)
	g := openGrant(up.URL, AuthBearer) // config says bearer, secret is basic
	_, err := d.Handle(context.Background(), callReq("a-mismatch", `{"method":"GET","path":"/x"}`, g))
	require.ErrorIs(t, err, drivers.ErrDenied)
	require.Contains(t, err.Error(), "credential_kind_mismatch")
}

func TestCredentialsUnavailableFailClosed(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("upstream must not be reached when secret resolution fails")
	}))
	defer up.Close()
	creds := &fakeCreds{err: credentials.ErrUnavailable}
	d := New(creds)
	g := openGrant(up.URL, AuthBearer)
	_, err := d.Handle(context.Background(), callReq("a-nocreds", `{"method":"GET","path":"/x"}`, g))
	require.ErrorIs(t, err, drivers.ErrDenied)
	require.Contains(t, err.Error(), "credentials_unavailable")
}

func TestNoGrantDenied(t *testing.T) {
	d := New(&fakeCreds{})
	_, err := d.Handle(context.Background(), &drivers.Request{
		Agent:     &auth.AgentPrincipal{AgentID: "a-nogrant"},
		Resource:  "rest-x",
		Operation: "call",
		Payload:   []byte(`{"method":"GET","path":"/x"}`),
	})
	require.ErrorIs(t, err, drivers.ErrDenied)
	require.Contains(t, err.Error(), "no_grant")
}

// ── Path normalization ────────────────────────────────────────────────────

func TestPathNormalization(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer up.Close()
	d := New(&fakeCreds{})
	g := openGrant(up.URL, AuthNone)

	bad := []string{"", "issues/1", "//evil.example/x", "http://evil.example/x", "/a/../b", "/a/./b", "/a/..%2fb"}
	for _, p := range bad {
		payload, _ := json.Marshal(map[string]any{"method": "GET", "path": p})
		_, err := d.Handle(context.Background(), callReq("a-pathnorm", string(payload), g))
		require.Error(t, err, "path %q must be rejected", p)
		require.NotErrorIs(t, err, drivers.ErrDenied, "malformed path is bad_request, not a policy denial")
	}
}

// TG-4c (S-259): per-agent credential isolation at the gateway. The
// resolution is keyed by the calling agent, so agent A's credential
// can never be injected for agent B (and vice versa).
func TestPerAgentCredentialIsolation(t *testing.T) {
	var gotAuth atomic.Value
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{}`))
	}))
	defer up.Close()

	creds := &keyedCreds{byAgent: map[string]*credentials.Secret{
		"agent-A": {Kind: AuthBearer, Fields: map[string]string{"token": "fake-token-A-DO-NOT-USE"}},
		"agent-B": {Kind: AuthBearer, Fields: map[string]string{"token": "fake-token-B-DO-NOT-USE"}},
	}}
	d := New(creds)
	g := openGrant(up.URL, AuthBearer)

	_, err := d.Handle(context.Background(), callReq("agent-A", `{"method":"GET","path":"/x"}`, g))
	require.NoError(t, err)
	require.Equal(t, "Bearer fake-token-A-DO-NOT-USE", gotAuth.Load().(string), "agent A must inject its OWN credential")

	_, err = d.Handle(context.Background(), callReq("agent-B", `{"method":"GET","path":"/x"}`, g))
	require.NoError(t, err)
	require.Equal(t, "Bearer fake-token-B-DO-NOT-USE", gotAuth.Load().(string), "agent B must inject its OWN credential")

	// Unknown agent with no resolvable credential: fail closed. Never
	// proceed with another agent's material.
	_, err = d.Handle(context.Background(), callReq("agent-C", `{"method":"GET","path":"/x"}`, g))
	require.Error(t, err)
	require.Contains(t, err.Error(), "credentials_unavailable")
}

// TG-4c: a credentialed kind without a resolved agent identity is
// denied BEFORE resolution — we cannot honour the per-agent guarantee
// without knowing who is calling.
func TestCredentialedCallWithoutAgentIdentityDenied(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("upstream must not be reached when the agent identity is missing")
	}))
	defer up.Close()

	creds := &fakeCreds{secret: &credentials.Secret{Kind: AuthBearer, Fields: map[string]string{"token": "t-FAKE"}}}
	d := New(creds)
	g := openGrant(up.URL, AuthBearer)
	req := &drivers.Request{
		Agent:     nil,
		Resource:  g.ResourceID,
		Operation: "call",
		Payload:   []byte(`{"method":"GET","path":"/x"}`),
		Grant:     g,
	}
	_, err := d.Handle(context.Background(), req)
	require.Error(t, err)
	require.Contains(t, err.Error(), "agent_identity_missing")
	require.Equal(t, int64(0), creds.calls.Load(), "deny must happen BEFORE any secret fetch")
}
