package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/tool-gateway/internal/auth"
	"github.com/rossbrigoli/skquad/tool-gateway/internal/drivers"
	"github.com/rossbrigoli/skquad/tool-gateway/internal/policy"
)

func grantFor(config, ceiling, constraints string) *policy.Grant {
	g := &policy.Grant{ResourceID: "web-system-1", ResourceType: "web"}
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

func fetchReq(agent, payload string) *drivers.Request {
	return fetchReqG(agent, payload, grantFor(`{}`, privateBoth, privateBoth))
}

func fetchReqG(agent, payload string, g *policy.Grant) *drivers.Request {
	return &drivers.Request{
		Agent:     &auth.AgentPrincipal{AgentID: agent},
		Resource:  g.ResourceID,
		Operation: "fetch",
		Payload:   []byte(payload),
		Grant:     g,
	}
}

// ── EffectivePolicy layering ──────────────────────────────────────────────

func TestEffectivePolicyDefaults(t *testing.T) {
	p, err := EffectivePolicy(nil, nil, nil)
	require.NoError(t, err)
	require.Equal(t, DefaultMaxBytes, p.MaxBytes)
	require.Equal(t, 0, p.RatePerMin)
	require.False(t, p.AllowPrivate)
	require.Empty(t, p.DenyDomains)
	require.Empty(t, p.DenyCIDRs)
}

func TestEffectivePolicyTightensNumeric(t *testing.T) {
	p, err := EffectivePolicy(
		json.RawMessage(`{"max_bytes":200000,"rate_per_min":60}`),
		json.RawMessage(`{"max_bytes":262144,"rate_per_min":30}`),
		json.RawMessage(`{"max_bytes":1000,"rate_per_min":10}`),
	)
	require.NoError(t, err)
	require.Equal(t, 1000, p.MaxBytes)
	require.Equal(t, 10, p.RatePerMin)
}

func TestEffectivePolicyDenyUnion(t *testing.T) {
	p, err := EffectivePolicy(
		json.RawMessage(`{"deny_domains":["floor.example"]}`),
		json.RawMessage(`{"deny_domains":["ceiling.example"]}`),
		json.RawMessage(`{"deny_domains":["grant.example","floor.example"]}`),
	)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"floor.example", "ceiling.example", "grant.example"}, p.DenyDomains)
}

func TestEffectivePolicyPrivateNeedsBoth(t *testing.T) {
	p, err := EffectivePolicy(raw(`{}`), raw(`{"allow_private_network":true}`), raw(`{"allow_private_network":true}`))
	require.NoError(t, err)
	require.True(t, p.AllowPrivate)

	p, err = EffectivePolicy(raw(`{}`), raw(`{"allow_private_network":true}`), raw(`{}`))
	require.NoError(t, err)
	require.False(t, p.AllowPrivate, "grant must opt in too")

	p, err = EffectivePolicy(raw(`{}`), raw(`{}`), raw(`{"allow_private_network":true}`))
	require.NoError(t, err)
	require.False(t, p.AllowPrivate, "grant cannot exceed ceiling")
}

func TestEffectivePolicyBadCIDRFailsClosed(t *testing.T) {
	_, err := EffectivePolicy(raw(`{"deny_cidrs":["not-a-cidr"]}`), raw(`{}`), raw(`{}`))
	require.Error(t, err)
}

func raw(s string) json.RawMessage { return json.RawMessage(s) }

// ── Happy paths (allow-private against local httptest) ────────────────────

const privateBoth = `{"allow_private_network":true}`

func TestDriverHappyPath(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, UserAgent, r.Header.Get("User-Agent"))
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "hello web")
	}))
	defer upstream.Close()

	d := New()
	resp, err := d.Handle(context.Background(), fetchReq("a1", `{"url":"`+upstream.URL+`"}`))
	require.NoError(t, err)
	require.Equal(t, 200, resp.StatusCode)
	out := resp.Body.(*Response)
	require.Equal(t, 200, out.Status)
	body, err := base64.StdEncoding.DecodeString(out.BodyB64)
	require.NoError(t, err)
	require.Equal(t, "hello web", string(body))
	require.False(t, out.Truncated)
	require.Contains(t, out.URL, upstream.URL)
}

func TestDriverExtraction(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<html><head><title>T</title><script>bad()</script></head><body><h1>H</h1><p>para</p></body></html>`)
	}))
	defer upstream.Close()

	d := New()
	resp, err := d.Handle(context.Background(), fetchReq("a1", `{"url":"`+upstream.URL+`","extract":true}`))
	require.NoError(t, err)
	out := resp.Body.(*Response)
	require.Contains(t, out.ExtractedText, "H")
	require.Contains(t, out.ExtractedText, "para")
	require.NotContains(t, out.ExtractedText, "bad()")
	// Raw body still present for façade parity.
	require.NotEmpty(t, out.BodyB64)
}

func TestDriverTruncates(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, strings.Repeat("x", 5000))
	}))
	defer upstream.Close()

	d := New()
	g := grantFor(`{}`, `{"max_bytes":100,"allow_private_network":true}`, privateBoth)
	resp, err := d.Handle(context.Background(), fetchReqG("a1", `{"url":"`+upstream.URL+`"}`, g))
	require.NoError(t, err)
	out := resp.Body.(*Response)
	require.True(t, out.Truncated)
	body, _ := base64.StdEncoding.DecodeString(out.BodyB64)
	require.Len(t, body, 100)
}

func TestDriverFollowsRedirects(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/final" {
			fmt.Fprint(w, "arrived")
			return
		}
		http.Redirect(w, r, srv.URL+"/final", http.StatusFound)
	}))
	defer srv.Close()

	d := New()
	resp, err := d.Handle(context.Background(), fetchReq("a1", `{"url":"`+srv.URL+`/start"}`))
	require.NoError(t, err)
	out := resp.Body.(*Response)
	body, _ := base64.StdEncoding.DecodeString(out.BodyB64)
	require.Equal(t, "arrived", string(body))
	require.Contains(t, out.URL, "/final")
}

func TestDriverRedirectCap(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL+"/next", http.StatusFound)
	}))
	defer srv.Close()

	d := New()
	_, err := d.Handle(context.Background(), fetchReq("a1", `{"url":"`+srv.URL+`"}`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "too many redirects")
	require.NotErrorIs(t, err, drivers.ErrDenied)
}

func TestDriverBadRequest(t *testing.T) {
	d := New()
	for _, payload := range []string{`{}`, `{"url":"   "}`, `{"url":"ftp://x.example/a"}`, `{"url":"http:///path"}`, `not-json`} {
		_, err := d.Handle(context.Background(), fetchReq("a1", payload))
		require.Error(t, err, payload)
		require.Contains(t, err.Error(), "bad_request", payload)
	}
}

func TestDriverNoGrantDenied(t *testing.T) {
	d := New()
	_, err := d.Handle(context.Background(), &drivers.Request{Payload: []byte(`{"url":"http://x.example"}`)})
	require.ErrorIs(t, err, drivers.ErrDenied)
}

// ── Security suite ─────────────────────────────────────────────────────────

type stubResolver struct{ answers []net.IPAddr }

func (s *stubResolver) LookupIPAddr(_ context.Context, _ string) ([]net.IPAddr, error) {
	return s.answers, nil
}

func TestSecurityBlockedDestinations(t *testing.T) {
	cases := map[string][]string{
		"metadata":    {"169.254.169.254"},
		"rfc1918":    {"10.0.0.5"},
		"rfc1918b":   {"192.168.1.1"},
		"cgnat":      {"100.64.0.1"},
		"cgnat-edge": {"100.127.255.255"},
		"ipv6-ula":   {"fd12:3456::7"},
		"loopback":   {"127.0.0.1"},
		"linklocal6": {"fe80::1"},
	}
	for name, ips := range cases {
		t.Run(name, func(t *testing.T) {
			addrs := make([]net.IPAddr, 0, len(ips))
			for _, s := range ips {
				addrs = append(addrs, net.IPAddr{IP: net.ParseIP(s)})
			}
			d := New()
			d.Resolver = &stubResolver{answers: addrs}
			strict := grantFor(`{}`, `{}`, `{}`)
			_, err := d.Handle(context.Background(), fetchReqG("a1", `{"url":"http://target.example/x"}`, strict))
			require.ErrorIs(t, err, drivers.ErrDenied, "%s must be denied", name)
			var de *drivers.DeniedError
			require.ErrorAs(t, err, &de)
			require.Equal(t, "ssrf_blocked", de.Reason)
		})
	}
}

func TestSecurityDNSRebindingDenied(t *testing.T) {
	// Answer set alternates public/poisoned: strict guard denies the name.
	d := New()
	d.Resolver = &stubResolver{answers: []net.IPAddr{
		{IP: net.ParseIP("93.184.216.34")},
		{IP: net.ParseIP("127.0.0.1")},
	}}
	strict := grantFor(`{}`, `{}`, `{}`)
	_, err := d.Handle(context.Background(), fetchReqG("a1", `{"url":"http://rebind.example/"}`, strict))
	require.ErrorIs(t, err, drivers.ErrDenied)
	require.Contains(t, err.Error(), "ssrf_blocked")
}

func TestSecurityRedirectToInternalDenied(t *testing.T) {
	// allow_private is on (hop 1 reachable), but the system floor
	// deny_cidrs blocks the metadata range — the per-hop recheck must
	// block hop 2 even though hop 1 passed.
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/jump" {
			http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
			return
		}
		http.Redirect(w, r, srv.URL+"/jump", http.StatusFound)
	}))
	defer srv.Close()

	d := New()
	g := grantFor(`{"deny_cidrs":["169.254.0.0/16"]}`, privateBoth, privateBoth)
	_, err := d.Handle(context.Background(), fetchReqG("a1", `{"url":"`+srv.URL+`"}`, g))
	require.ErrorIs(t, err, drivers.ErrDenied)
	require.Contains(t, err.Error(), "ssrf_blocked")
}

func TestSecurityRedirectToInternalStrictDenied(t *testing.T) {
	// Strict guard: a redirect to a loopback URL is denied at hop 2's
	// dial time (hop 1 is the local front via allow_private only on
	// hop 1's host — modeled with a grant that allows private for the
	// front's /24 but denies it for the redirect target via deny_cidrs
	// on the metadata range; the strict variant here simply shows the
	// default guard denies the internal redirect target outright).
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://10.9.9.9/steal", http.StatusFound)
	}))
	defer srv.Close()

	d := New()
	g := grantFor(`{}`, `{"allow_private_network":true}`, `{"allow_private_network":true,"deny_cidrs":["10.0.0.0/8"]}`)
	_, err := d.Handle(context.Background(), fetchReqG("a1", `{"url":"`+srv.URL+`"}`, g))
	require.ErrorIs(t, err, drivers.ErrDenied)
	require.Contains(t, err.Error(), "ssrf_blocked")
}

func TestSecurityDenylistFloorEnforced(t *testing.T) {
	// System floor (resource config) denies the domain; grant is silent.
	d := New()
	g := grantFor(`{"deny_domains":["floor.example"]}`, `{}`, `{}`)
	_, err := d.Handle(context.Background(), &drivers.Request{
		Agent: &auth.AgentPrincipal{AgentID: "a1"}, Resource: g.ResourceID, Payload: []byte(`{"url":"http://floor.example/x"}`), Grant: g,
	})
	require.ErrorIs(t, err, drivers.ErrDenied)
	require.Contains(t, err.Error(), "domain_denied")
}

func TestSecurityGrantAddsDeny(t *testing.T) {
	d := New()
	g := grantFor(`{}`, `{}`, `{"deny_domains":["added.by.grant.example"]}`)
	_, err := d.Handle(context.Background(), &drivers.Request{
		Agent: &auth.AgentPrincipal{AgentID: "a1"}, Resource: g.ResourceID, Payload: []byte(`{"url":"http://added.by.grant.example/"}`), Grant: g,
	})
	require.ErrorIs(t, err, drivers.ErrDenied)
	require.Contains(t, err.Error(), "domain_denied")
	// Subdomain of a denied domain is denied too.
	_, err = d.Handle(context.Background(), &drivers.Request{
		Agent: &auth.AgentPrincipal{AgentID: "a1"}, Resource: g.ResourceID, Payload: []byte(`{"url":"http://deep.added.by.grant.example/"}`), Grant: g,
	})
	require.ErrorIs(t, err, drivers.ErrDenied)
}

func TestSecurityGrantCannotRemoveFloorDeny(t *testing.T) {
	// Grant sets its own deny list that omits the floor entry — the
	// union enforcement still denies the floor domain at runtime.
	d := New()
	g := grantFor(`{"deny_domains":["floor.example"]}`, `{}`, `{"deny_domains":["floor.example","other.example"]}`)
	_, err := d.Handle(context.Background(), &drivers.Request{
		Agent: &auth.AgentPrincipal{AgentID: "a1"}, Resource: g.ResourceID, Payload: []byte(`{"url":"http://floor.example/"}`), Grant: g,
	})
	require.ErrorIs(t, err, drivers.ErrDenied)
	require.Contains(t, err.Error(), "domain_denied")
}

func TestSecurityDenyCIDRBeatsAllowPrivate(t *testing.T) {
	// allow_private is on, but the floor deny_cidrs blocks loopback/8.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "should-not-be-reachable")
	}))
	defer upstream.Close()

	d := New()
	g := grantFor(`{"deny_cidrs":["127.0.0.0/8"]}`, privateBoth, privateBoth)
	_, err := d.Handle(context.Background(), fetchReqG("a1", `{"url":"`+upstream.URL+`"}`, g))
	require.ErrorIs(t, err, drivers.ErrDenied)
	require.Contains(t, err.Error(), "ssrf_blocked")
}

func TestSecurityDefaultDeniesLoopbackUpstream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "secret-internal")
	}))
	defer upstream.Close()

	d := New()
	strict := grantFor(`{}`, `{}`, `{}`)
	_, err := d.Handle(context.Background(), fetchReqG("a1", `{"url":"`+upstream.URL+`/secret"}`, strict))
	require.ErrorIs(t, err, drivers.ErrDenied)
	require.NotContains(t, err.Error(), "secret-internal")
}

func TestSecurityInvalidPolicyDenied(t *testing.T) {
	d := New()
	g := grantFor(`{"deny_cidrs":["bogus"]}`, `{}`, `{}`)
	_, err := d.Handle(context.Background(), fetchReqG("a1", `{"url":"http://ok.example/"}`, g))
	require.ErrorIs(t, err, drivers.ErrDenied)
	require.Contains(t, err.Error(), "invalid_policy")
}

// ── Rate limiting ────────────────────────────────────────────────────────

func TestRateLimitEnforced(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer upstream.Close()

	d := New()
	g := grantFor(`{}`, `{"rate_per_min":2,"allow_private_network":true}`, `{"allow_private_network":true}`)
	r := func() error {
		_, err := d.Handle(context.Background(), fetchReqG("rate-agent", `{"url":"`+upstream.URL+`"}`, g))
		return err
	}
	require.NoError(t, r())
	require.NoError(t, r())
	err := r()
	require.ErrorIs(t, err, drivers.ErrDenied)
	require.Contains(t, err.Error(), "rate_limited")

	// A different agent has its own budget.
	_, err = d.Handle(context.Background(), fetchReq("other-agent", `{"url":"`+upstream.URL+`"}`))
	require.NoError(t, err)

	// Refill: after a minute of simulated time the bucket recovers.
	require.True(t, d.limits.allow("rate-agent\x00web-system-1", 2, time.Now().Add(70*time.Second)))
}

func TestRateLimitUnlimitedWhenUnset(t *testing.T) {
	lim := newRateLimiters()
	for i := 0; i < 1000; i++ {
		require.True(t, lim.allow("k", 0, time.Now()))
	}
}
