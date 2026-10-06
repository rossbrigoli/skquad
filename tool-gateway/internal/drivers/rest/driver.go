// Package rest implements the gateway's governed `rest` driver
// (docs/tool-gateway.md §6.2, TG-4): BYO-credential REST API access
// with per-call secret resolution and zero credential exposure.
//
// Invariants enforced here (effective policy = config ∧ ceiling ∧
// grant constraints, see policy.go):
//
//   - Method/path ACL: default-deny path_allow globs, path_deny wins,
//     method allow-set. Everything is rejected BEFORE any network call.
//   - Agent-controlled headers: only content-type and accept. Anything
//     else (host, authorization, x-forwarded-*, cookie, user-agent, …)
//     is a policy denial — the agent can never steer auth or routing.
//   - Secrets are resolved PER CALL from the CP internal credentials
//     API and never persisted: the only retention is the process-memory
//     OAuth access-token cache (bounded by token expiry). Secrets are
//     never logged and never appear in audit payloads.
//   - OAuth2 client-credentials: cached token + refresh-on-401 with a
//     single retry (no retry storms).
//   - Cross-host redirects are rejected whenever the resource injects
//     credentials: Go strips Authorization on cross-host redirects but
//     NOT custom api_key_header headers, so the driver refuses the hop
//     entirely (credential-leak defense, mirrors §6.1's BYO-cookie
//     reasoning).
//   - Caps: max_request_bytes rejects an oversize body pre-send;
//     max_response_bytes truncates with a flag (mirrors web driver).
//   - Rate limit per (agent, resource) leaky bucket (same pattern as
//     web).
//   - Response scrubbing: set-cookie, set-authorization, authorization
//     and www-authenticate never pass through to the agent.
//   - Egress class: public = netguard default (no private IPs);
//     internal = allow-private only when ceiling AND grant constraints
//     say internal (mirrors web allow_private_network semantics).
package rest

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/rossbrigoli/skquad/shared/netguard"

	"github.com/rossbrigoli/skquad/tool-gateway/internal/credentials"
	"github.com/rossbrigoli/skquad/tool-gateway/internal/drivers"
)

// Timeout is the fixed per-call upstream timeout (the rest ceiling shape
// has no timeout field; 30s mirrors the platform web default).
const Timeout = 30 * time.Second

// MaxRedirects mirrors the web driver's redirect cap.
const MaxRedirects = 3

// UserAgent identifies gateway-originated REST calls.
const UserAgent = "skquad-rest/1.0"

// Request is the agent-facing rest_call payload.
type Request struct {
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`
}

// Response is returned to the caller: upstream status, scrubbed
// headers, base64 body and a truncation flag.
type Response struct {
	Status    int               `json:"status"`
	Headers   map[string]string `json:"headers"`
	BodyB64   string            `json:"bodyB64"`
	Truncated bool              `json:"truncated"`
}

// Driver performs governed REST calls. State is limited to the
// per-(agent,resource) rate buckets and the process-memory OAuth token
// cache.
type Driver struct {
	limits *rateLimiters
	oauth  *oauthCache
	creds  credentials.Resolver
	// Resolver overrides DNS for tests (stub resolvers simulate
	// rebinding). nil uses the system resolver.
	Resolver netguard.Resolver
}

// New builds a rest driver bound to a credential resolver.
func New(creds credentials.Resolver) *Driver {
	return &Driver{limits: newRateLimiters(), oauth: newOAuthCache(), creds: creds}
}

func (d *Driver) Name() string { return "rest" }

// allowedAgentHeaders is the ONLY set of agent-controlled request
// headers that may reach the upstream (§6.2).
var allowedAgentHeaders = map[string]bool{
	"content-type": true,
	"accept":       true,
}

// scrubbedResponseHeaders never pass from upstream to the agent.
var scrubbedResponseHeaders = map[string]bool{
	"Set-Cookie":        true,
	"Set-Authorization": true,
	"Authorization":     true,
	"Www-Authenticate":  true,
}

// Handle executes one governed REST call.
func (d *Driver) Handle(ctx context.Context, req *drivers.Request) (*drivers.Response, error) {
	if req.Grant == nil {
		return nil, drivers.Denied("no_grant")
	}
	var in Request
	if err := json.Unmarshal(req.Payload, &in); err != nil {
		return nil, fmt.Errorf("bad_request: %w", err)
	}

	method := strings.ToUpper(strings.TrimSpace(in.Method))
	if method == "" {
		return nil, fmt.Errorf("bad_request: method is required")
	}
	path, query, err := normalizePath(in.Path)
	if err != nil {
		return nil, fmt.Errorf("bad_request: %v", err)
	}

	pol, err := EffectivePolicy(req.Grant.Config, req.Grant.Ceiling, req.Grant.Constraints)
	if err != nil {
		// A malformed policy must never widen reach: deny.
		return nil, drivers.Denied("invalid_policy")
	}

	// ACL checks happen strictly before any network activity.
	if !pol.methodAllowed(method) {
		return nil, drivers.Denied("method_denied")
	}
	if !pol.pathAllowed(path) {
		return nil, drivers.Denied("path_denied")
	}
	if err := checkAgentHeaders(in.Headers); err != nil {
		return nil, err
	}
	if len(in.Body) > pol.MaxRequestBytes {
		return nil, drivers.Denied("request_too_large")
	}

	agentID := ""
	if req.Agent != nil {
		agentID = req.Agent.AgentID
	}
	if pol.RatePerMin > 0 && !d.limits.allow(agentID+"\x00"+req.Resource, pol.RatePerMin, time.Now()) {
		return nil, drivers.Denied("rate_limited")
	}

	// Per-call secret resolution (fail-closed). auth_kind=none needs
	// no secret and skips the CP round-trip. TG-4c (S-259): the
	// calling agent's identity is part of the resolution key — the CP
	// injects that agent's OWN credential when it has one, else the
	// resource default. A credentialed call without a resolved agent
	// identity is denied: without it we could not honour the
	// per-agent isolation guarantee.
	var secret *credentials.Secret
	if pol.AuthKind != AuthNone && pol.AuthKind != "" {
		if d.creds == nil {
			return nil, drivers.Denied("credentials_unavailable")
		}
		if agentID == "" {
			return nil, drivers.Denied("agent_identity_missing")
		}
		s, err := d.creds.Resolve(ctx, req.Resource, agentID)
		if err != nil {
			return nil, drivers.Denied("credentials_unavailable")
		}
		if s.Kind != pol.AuthKind {
			// Config/secret drift: refuse rather than inject the wrong
			// credential shape.
			return nil, drivers.Denied("credential_kind_mismatch")
		}
		secret = s
	}

	target, err := joinURL(pol.BaseURL, path, query)
	if err != nil {
		return nil, fmt.Errorf("bad_request: invalid target url")
	}

	// Egress-guarded transport: dial-time SSRF pin + per-hop redirect
	// re-check, identical posture to the web driver.
	guard := &netguard.Guard{AllowPrivate: pol.AllowPrivate}
	dialer := netguard.Dialer{Guard: guard, Timeout: Timeout, Resolver: d.Resolver}
	transport := &http.Transport{
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   Timeout,
		ResponseHeaderTimeout: Timeout,
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   Timeout,
	}

	originalHost := mustHost(target)
	checkRedirect := netguard.RedirectCheck(MaxRedirects, nil)
	if secret != nil {
		// Credential-bearing call: refuse cross-host redirects (a custom
		// api-key header would otherwise follow the redirect to a
		// foreign host). Same-host hops remain allowed and re-guarded.
		client.CheckRedirect = func(httpReq *http.Request, via []*http.Request) error {
			if httpReq.URL.Host != originalHost {
				return fmt.Errorf("denied redirect: cross-host redirect not allowed for credentialed calls")
			}
			return checkRedirect(httpReq, via)
		}
	} else {
		client.CheckRedirect = checkRedirect
	}

	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()

	doCall := func(tokenOverride string) (*http.Response, error) {
		var bodyReader io.Reader
		if in.Body != "" {
			bodyReader = strings.NewReader(in.Body)
		}
		httpReq, err := http.NewRequestWithContext(ctx, method, target, bodyReader)
		if err != nil {
			return nil, fmt.Errorf("bad_request: invalid request")
		}
		httpReq.Header.Set("User-Agent", UserAgent)
		for k, v := range in.Headers {
			lk := strings.ToLower(strings.TrimSpace(k))
			if !allowedAgentHeaders[lk] {
				// Re-checked defensively (already rejected earlier).
				return nil, drivers.Denied("header_denied")
			}
			httpReq.Header.Set(k, v)
		}
		if err := injectAuth(httpReq, pol, secret, tokenOverride); err != nil {
			return nil, err
		}
		return client.Do(httpReq)
	}

	var resp *http.Response
	if secret != nil && secret.Kind == AuthOAuth2ClientCreds {
		guardedFetch := newDefaultTokenFetcher(client)
		token, err := d.oauth.token(ctx, req.Resource, secret, false, guardedFetch)
		if err != nil {
			return nil, drivers.Denied("credentials_unavailable")
		}
		resp, err = doCall(token)
		if err != nil {
			return classifyDoError(err)
		}
		if resp.StatusCode == http.StatusUnauthorized {
			// Refresh-on-401: exactly one retry with a freshly minted
			// token, then the upstream response stands.
			resp.Body.Close()
			d.oauth.invalidate(req.Resource)
			token, err = d.oauth.token(ctx, req.Resource, secret, true, guardedFetch)
			if err != nil {
				return nil, drivers.Denied("credentials_unavailable")
			}
			resp, err = doCall(token)
			if err != nil {
				return classifyDoError(err)
			}
		}
	} else {
		resp, err = doCall("")
		if err != nil {
			return classifyDoError(err)
		}
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(pol.MaxResponseBytes)+1))
	if err != nil {
		return nil, fmt.Errorf("rest_failed: reading upstream")
	}
	truncated := len(body) > pol.MaxResponseBytes
	if truncated {
		body = body[:pol.MaxResponseBytes]
	}

	out := &Response{
		Status:    resp.StatusCode,
		Headers:   scrubHeaders(resp.Header),
		BodyB64:   base64.StdEncoding.EncodeToString(body),
		Truncated: truncated,
	}
	return &drivers.Response{StatusCode: http.StatusOK, Body: out}, nil
}

// injectAuth attaches the resolved credential to the outbound request.
// tokenOverride carries the OAuth access token for oauth2 resources;
// it is ignored for other kinds.
func injectAuth(httpReq *http.Request, pol *Policy, secret *credentials.Secret, tokenOverride string) error {
	if secret == nil || pol.AuthKind == AuthNone {
		return nil
	}
	switch pol.AuthKind {
	case AuthBearer:
		tok := secret.Fields["token"]
		if tok == "" {
			return drivers.Denied("credential_unusable")
		}
		httpReq.Header.Set("Authorization", "Bearer "+tok)
	case AuthAPIKeyHeader:
		tok := secret.Fields["token"]
		if tok == "" {
			return drivers.Denied("credential_unusable")
		}
		name := http.CanonicalHeaderKey(pol.HeaderName)
		// Defensive: never let the config steer auth onto a forbidden
		// header (registration validation already forbids this).
		if name == "" || name == "Authorization" || strings.HasPrefix(strings.ToLower(name), "x-forwarded-") {
			return drivers.Denied("credential_unusable")
		}
		httpReq.Header.Set(name, tok)
	case AuthBasic:
		user := secret.Fields["username"]
		pass := secret.Fields["password"]
		if user == "" || pass == "" {
			return drivers.Denied("credential_unusable")
		}
		httpReq.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(user+":"+pass)))
	case AuthOAuth2ClientCreds:
		if tokenOverride == "" {
			return drivers.Denied("credential_unusable")
		}
		httpReq.Header.Set("Authorization", "Bearer "+tokenOverride)
	default:
		return drivers.Denied("unsupported_auth_kind")
	}
	return nil
}

// checkAgentHeaders enforces the agent header allowlist.
func checkAgentHeaders(headers map[string]string) error {
	for k := range headers {
		lk := strings.ToLower(strings.TrimSpace(k))
		if !allowedAgentHeaders[lk] {
			return drivers.Denied("header_denied")
		}
	}
	return nil
}

// normalizePath validates the agent-supplied path: must be a relative
// path (no scheme/host, no protocol-relative "//", no "." / ".."
// traversal). Returns (path, rawQuery).
func normalizePath(p string) (string, string, error) {
	p = strings.TrimSpace(p)
	if p == "" || !strings.HasPrefix(p, "/") {
		return "", "", fmt.Errorf("path must start with /")
	}
	if strings.HasPrefix(p, "//") {
		return "", "", fmt.Errorf("path must not be protocol-relative")
	}
	u, err := url.Parse(p)
	if err != nil {
		return "", "", fmt.Errorf("invalid path")
	}
	if u.Scheme != "" || u.Host != "" {
		return "", "", fmt.Errorf("path must be relative (no scheme or host)")
	}
	for _, seg := range strings.Split(u.Path, "/") {
		if seg == "." || seg == ".." {
			return "", "", fmt.Errorf("path must not contain . or .. segments")
		}
	}
	return u.Path, u.RawQuery, nil
}

// joinURL appends the validated path/query to the registered base_url.
func joinURL(base, path, query string) (string, error) {
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("invalid base_url")
	}
	basePath := strings.TrimRight(u.Path, "/")
	u.Path = basePath + path
	if query != "" {
		u.RawQuery = query
	}
	return u.String(), nil
}

// scrubHeaders removes credential-bearing headers from the upstream
// response before it reaches the agent.
func scrubHeaders(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, vals := range h {
		if scrubbedResponseHeaders[k] {
			continue
		}
		out[k] = strings.Join(vals, ", ")
	}
	return out
}

// classifyDoError maps transport errors to client-safe classes: SSRF
// guard trips are policy denials; everything else is a generic upstream
// failure. Raw error text (which can carry internal host/IP detail) is
// never echoed.
func classifyDoError(err error) (*drivers.Response, error) {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "ssrf_guard"):
		return nil, drivers.Denied("ssrf_blocked")
	case strings.Contains(msg, "denied redirect"):
		return nil, drivers.Denied("redirect_denied")
	default:
		return nil, fmt.Errorf("rest_failed: upstream request failed")
	}
}

func mustHost(u string) string {
	parsed, err := url.Parse(u)
	if err != nil {
		return ""
	}
	return parsed.Host
}

// compile-time interface check
var _ drivers.Driver = (*Driver)(nil)
