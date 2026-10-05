// Package web implements the gateway's governed `web` driver
// (docs/tool-gateway.md §6.1, TG-3): the relocated BT-6 web_fetch.
//
// Invariants enforced here (from the policy grant's ceiling ∧ constraints
// ∧ resource config):
//
//   - GET only; ≤3 redirects with per-hop re-validation (scheme,
//     denylist, and dial-time SSRF via the shared netguard transport).
//   - Dial-time IP pin via shared netguard — defeats DNS rebinding.
//   - deny_domains / deny_cidrs: system floor (resource config) plus
//     additive tightening from ceiling and grant constraints. A grant
//     may ADD deny entries, never remove floor entries (the union is
//     what gets enforced; the CP no-escalation validator already
//     rejected grants that try to shrink denylists).
//   - max_bytes cap with truncation flag; fixed 30s timeout (mirrors
//     the BT-6 default; the web wire shape has no timeout field).
//   - rate_per_min per (agent, resource) leaky bucket.
//   - NO credential injection, ever (design §6.1: a BYO cookie on a
//     broad-web fetch + redirect = credential leak). The driver never
//     attaches auth beyond the agent's own gateway credential, which
//     never leaves the agent→gateway hop.
package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/rossbrigoli/skquad/shared/htmltext"
	"github.com/rossbrigoli/skquad/shared/netguard"

	"github.com/rossbrigoli/skquad/tool-gateway/internal/drivers"
)

const (
	// DefaultMaxBytes mirrors BT-6 defaultFetchMaxBytes (256 KiB).
	DefaultMaxBytes = 262144
	// Timeout mirrors BT-6 defaultFetchTimeoutSeconds. The web policy
	// wire shape (egresspolicy) has no timeout field, so the driver
	// keeps the platform default fixed.
	Timeout = 30 * time.Second
	// MaxRedirects mirrors BT-6 fetchMaxRedirects.
	MaxRedirects = 3
	// UserAgent mirrors BT-6 fetchUserAgent for upstream parity.
	UserAgent = "skquad-webfetch/1.0"
)

// Policy is the effective web policy: ceiling ∧ constraints ∧ config.
type Policy struct {
	RatePerMin   int // 0 = unlimited
	MaxBytes     int
	AllowPrivate bool
	DenyDomains  []string
	DenyCIDRs    []*net.IPNet
}

// webShape is the shared JSON shape of web config / ceiling / constraints
// (control-plane/internal/egresspolicy package doc is the contract).
type webShape struct {
	DenyDomains  []string `json:"deny_domains"`
	DenyCIDRs    []string `json:"deny_cidrs"`
	RatePerMin   int      `json:"rate_per_min"`
	MaxBytes     int      `json:"max_bytes"`
	AllowPrivate bool     `json:"allow_private_network"`
}

func parseShape(raw json.RawMessage) (webShape, error) {
	var s webShape
	if len(raw) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return s, fmt.Errorf("invalid web policy JSON: %w", err)
	}
	return s, nil
}

// EffectivePolicy folds resource config (system floor), the resource
// ceiling and the grant constraints into one enforced policy.
//
// Layering rules (§6.1 + §8):
//   - denylists: UNION of config ∪ ceiling ∪ constraints (additive only)
//   - numeric bounds: MIN of every layer that sets one
//   - allow_private_network: ceiling AND constraints (config mirrors the
//     ceiling; the grant can never exceed the ceiling)
func EffectivePolicy(configRaw, ceilingRaw, constraintsRaw json.RawMessage) (*Policy, error) {
	cfg, err := parseShape(configRaw)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	ce, err := parseShape(ceilingRaw)
	if err != nil {
		return nil, fmt.Errorf("ceiling: %w", err)
	}
	con, err := parseShape(constraintsRaw)
	if err != nil {
		return nil, fmt.Errorf("constraints: %w", err)
	}

	p := &Policy{MaxBytes: DefaultMaxBytes}

	// Denylists: union, deduped, normalized.
	seen := map[string]bool{}
	for _, list := range [][]string{cfg.DenyDomains, ce.DenyDomains, con.DenyDomains} {
		for _, d := range list {
			n := netguard.NormalizeHost(d)
			if n == "" || seen[n] {
				continue
			}
			seen[n] = true
			p.DenyDomains = append(p.DenyDomains, n)
		}
	}
	cidrSeen := map[string]bool{}
	for _, layer := range [][]string{cfg.DenyCIDRs, ce.DenyCIDRs, con.DenyCIDRs} {
		for _, c := range layer {
			_, n, err := net.ParseCIDR(strings.TrimSpace(c))
			if err != nil {
				return nil, fmt.Errorf("invalid deny_cidr %q", c)
			}
			if cidrSeen[n.String()] {
				continue
			}
			cidrSeen[n.String()] = true
			p.DenyCIDRs = append(p.DenyCIDRs, n)
		}
	}

	// Numeric bounds: tighten (min) across layers that set them.
	for _, v := range []int{cfg.MaxBytes, ce.MaxBytes, con.MaxBytes} {
		if v > 0 && v < p.MaxBytes {
			p.MaxBytes = v
		}
	}
	rates := []int{}
	for _, v := range []int{cfg.RatePerMin, ce.RatePerMin, con.RatePerMin} {
		if v > 0 {
			rates = append(rates, v)
		}
	}
	if len(rates) > 0 {
		p.RatePerMin = rates[0]
		for _, r := range rates[1:] {
			if r < p.RatePerMin {
				p.RatePerMin = r
			}
		}
	}

	p.AllowPrivate = ce.AllowPrivate && con.AllowPrivate
	return p, nil
}

// Request is the driver's inbound payload.
type Request struct {
	URL     string `json:"url"`
	Extract bool   `json:"extract,omitempty"` // html → readable text
}

// Response mirrors the CP BT-6 contract so the façade can pass it
// through unchanged; extractedText is additive (omitempty).
type Response struct {
	URL           string `json:"url"`
	Status        int    `json:"status"`
	ContentType   string `json:"contentType"`
	BodyB64       string `json:"bodyB64"`
	Truncated     bool   `json:"truncated"`
	ExtractedText string `json:"extractedText,omitempty"`
}

// Driver performs governed web fetches. It is stateless except for the
// per-(agent,resource) rate buckets.
type Driver struct {
	limits *rateLimiters
	// Resolver overrides DNS for tests (stub resolvers simulate
	// rebinding). nil uses the system resolver.
	Resolver netguard.Resolver
}

// New builds a web driver with a fresh rate-limiter state map.
func New() *Driver {
	return &Driver{limits: newRateLimiters()}
}

func (d *Driver) Name() string { return "web" }

// Handle executes one governed fetch.
func (d *Driver) Handle(ctx context.Context, req *drivers.Request) (*drivers.Response, error) {
	if req.Grant == nil {
		return nil, drivers.Denied("no_grant")
	}
	var in Request
	if err := json.Unmarshal(req.Payload, &in); err != nil {
		return nil, fmt.Errorf("bad_request: %w", err)
	}
	target := strings.TrimSpace(in.URL)
	if target == "" {
		return nil, fmt.Errorf("bad_request: url is required")
	}
	parsed, err := url.Parse(target)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("bad_request: url must be a valid http(s) URL")
	}

	pol, err := EffectivePolicy(req.Grant.Config, req.Grant.Ceiling, req.Grant.Constraints)
	if err != nil {
		// A malformed policy must never widen reach: deny.
		return nil, drivers.Denied("invalid_policy")
	}

	// Domain denylist (system floor + additive tightening).
	if netguard.DomainDenied(parsed.Hostname(), pol.DenyDomains) {
		return nil, drivers.Denied("domain_denied")
	}

	// Rate limit per (agent, resource).
	agentID := ""
	if req.Agent != nil {
		agentID = req.Agent.AgentID
	}
	if pol.RatePerMin > 0 && !d.limits.allow(agentID+"\x00"+req.Resource, pol.RatePerMin, time.Now()) {
		return nil, drivers.Denied("rate_limited")
	}

	guard := &netguard.Guard{AllowPrivate: pol.AllowPrivate, DenyCIDRs: pol.DenyCIDRs}
	dialer := netguard.Dialer{Guard: guard, Timeout: Timeout, Resolver: d.Resolver}
	transport := &http.Transport{
		DialContext:         dialer.DialContext,
		TLSHandshakeTimeout: Timeout,
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   Timeout,
		CheckRedirect: netguard.RedirectCheck(MaxRedirects, func(host string) bool {
			return netguard.DomainDenied(host, pol.DenyDomains)
		}),
	}

	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("bad_request: invalid url")
	}
	httpReq.Header.Set("Accept", "*/*")
	httpReq.Header.Set("User-Agent", UserAgent)

	resp, err := client.Do(httpReq)
	if err != nil {
		// Classify: SSRF/denylist blocks from the guard are policy
		// denials; everything else is an upstream failure. Never echo
		// raw error text (it can carry internal host/IP detail).
		if strings.Contains(err.Error(), "ssrf_guard") || strings.Contains(err.Error(), "denied redirect") {
			return nil, drivers.Denied("ssrf_blocked")
		}
		return nil, fmt.Errorf("fetch_failed: %w", err)
	}
	defer resp.Body.Close()

	// A surviving 3xx means the redirect cap was exhausted.
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return nil, fmt.Errorf("fetch_failed: too many redirects (max %d)", MaxRedirects)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(pol.MaxBytes)+1))
	if err != nil {
		return nil, fmt.Errorf("fetch_failed: reading upstream: %w", err)
	}
	truncated := len(body) > pol.MaxBytes
	if truncated {
		body = body[:pol.MaxBytes]
	}

	out := &Response{
		URL:         resp.Request.URL.String(),
		Status:      resp.StatusCode,
		ContentType: resp.Header.Get("Content-Type"),
		BodyB64:     base64.StdEncoding.EncodeToString(body),
		Truncated:   truncated,
	}
	if in.Extract && strings.Contains(strings.ToLower(out.ContentType), "html") {
		out.ExtractedText = htmltext.Extract(string(body))
	}
	return &drivers.Response{StatusCode: http.StatusOK, Body: out}, nil
}

// compile-time interface check
var _ drivers.Driver = (*Driver)(nil)
