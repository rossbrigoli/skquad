// BT-6: web_fetch control-plane proxy.
//
// The agent pods run under a default-deny egress NetworkPolicy (only DNS +
// skquad-system are reachable), so the runtime CANNOT fetch arbitrary URLs
// itself. web_fetch therefore executes HERE, in the control plane, mirroring
// the web_search proxy pattern (ADR-0012 §3): the runtime sends the URL,
// the control plane performs the guarded fetch and returns the body.
//
// Security (same contract the runtime previously enforced locally):
//   - SSRF guard at DIAL time via the SHARED netguard library
//     (github.com/rossbrigoli/skquad/shared/netguard — TG-3 single
//     source of truth, also consumed by tool-gateway): every resolved
//     destination IP is checked (loopback, RFC1918, CGNAT 100.64/10,
//     link-local incl. cloud metadata, multicast, reserved, IPv6 ULA)
//     unless the policy sets allowPrivateNetwork=true. Dial-time checks
//     also close the DNS-rebinding TOCTOU the old resolve-then-connect
//     runtime guard had.
//   - http/https only; redirects capped at 3 and re-guarded per hop
//     (Control runs for every new connection).
//   - Response body capped at maxBytes (default 256 KiB); truncated flag
//     tells the runtime.
//   - Timeout from policy (default 30s), enforced via context.
//
// TG-3 façade: when cfg.WebFetchViaGateway && cfg.ToolGatewayURL are
// set, the request is forwarded to the gateway's /v1/web/fetch with the
// agent's credential headers passed through and the gateway's response
// re-emitted under the exact same contract. Gateway failures surface as
// the same 502-class tool errors the legacy path produces.

package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/rossbrigoli/skquad/shared/netguard"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

const (
	defaultFetchTimeoutSeconds = 30
	defaultFetchMaxBytes       = 262144
	fetchMaxRedirects          = 3
	fetchUserAgent             = "skquad-webfetch/1.0"
)

// blockedDestAddr reports whether a dial destination address must be
// rejected by the SSRF guard. address is host:port as passed to
// Dialer.Control. TG-3: the table lives in the shared netguard library;
// this is a thin delegation kept for the existing test surface.
func blockedDestAddr(address string) bool {
	return netguard.BlockedHostPort(address)
}

// fetchPolicy is the validated read of the web_fetch policy row.
type fetchPolicy struct {
	timeoutSeconds      int
	maxBytes            int
	allowPrivateNetwork bool
}

func webFetchPolicyOf(cfg *domain.BuiltinToolConfig) fetchPolicy {
	p := fetchPolicy{
		timeoutSeconds: defaultFetchTimeoutSeconds,
		maxBytes:       defaultFetchMaxBytes,
	}
	if len(cfg.Policy) == 0 {
		return p
	}
	var raw struct {
		TimeoutSeconds      int  `json:"timeoutSeconds"`
		MaxBytes            int  `json:"maxBytes"`
		AllowPrivateNetwork bool `json:"allowPrivateNetwork"`
	}
	if err := json.Unmarshal(cfg.Policy, &raw); err == nil {
		if raw.TimeoutSeconds > 0 {
			p.timeoutSeconds = raw.TimeoutSeconds
		}
		if raw.MaxBytes > 0 {
			p.maxBytes = raw.MaxBytes
		}
		p.allowPrivateNetwork = raw.AllowPrivateNetwork
	}
	return p
}

// guardedHTTPClient builds a client whose every outbound connection passes
// the shared netguard SSRF guard (unless allowPrivate) and whose
// redirects are capped and re-validated (Control fires per hop).
func guardedHTTPClient(p fetchPolicy) *http.Client {
	guard := &netguard.Guard{AllowPrivate: p.allowPrivateNetwork}
	dialer := &net.Dialer{Timeout: time.Duration(p.timeoutSeconds) * time.Second}
	// Control runs AFTER DNS resolution, immediately before connect, and
	// sees the actual resolved IP:port — the canonical Go SSRF hook.
	// (Checking in DialContext sees only the pre-resolution hostname.)
	dialer.Control = guard.ControlHook()
	transport := &http.Transport{
		DialContext: dialer.DialContext,
	}
	return &http.Client{
		Transport: transport,
		Timeout:   time.Duration(p.timeoutSeconds) * time.Second,
		CheckRedirect: netguard.RedirectCheck(fetchMaxRedirects, nil),
	}
}

// agentWebFetch — POST /api/v1/tools/web_fetch (agent-credential auth).
// Body: {"url": "https://example.com"}.
// Response: {"url","status","contentType","bodyB64","truncated"}.
func (s *Server) agentWebFetch(w http.ResponseWriter, r *http.Request) {
	principal := currentAgent(r.Context())
	cfg, err := s.store.GetBuiltinTool(r.Context(), domain.BuiltinToolWebFetch)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		writeStorageError(w, err)
		return
	}
	if err != nil || cfg == nil || !cfg.Enabled {
		writeError(w, http.StatusForbidden, "tool_disabled", "web_fetch is disabled by platform policy")
		return
	}

	var req struct {
		URL string `json:"url"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	target := strings.TrimSpace(req.URL)
	if target == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "url is required")
		return
	}
	parsed, err := url.Parse(target)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "url must be a valid http(s) URL")
		return
	}

	p := webFetchPolicyOf(cfg)

	// TG-3 façade: forward to the governed tool gateway when wired.
	// The runtime sees the exact same contract either way.
	if s.cfg != nil && s.cfg.WebFetchViaGateway && s.cfg.ToolGatewayURL != "" {
		s.webFetchViaGateway(w, r, p, target)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(p.timeoutSeconds)*time.Second)
	defer cancel()

	client := guardedHTTPClient(p)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid url")
		return
	}
	httpReq.Header.Set("Accept", "*/*")
	httpReq.Header.Set("User-Agent", fetchUserAgent)

	resp, err := client.Do(httpReq)
	if err != nil {
		// Never echo the raw error back: it can carry internal host/IP
		// detail. Log server-side, return a generic failure.
		log.Printf("web_fetch proxy error agent=%s url=%q: %v", principal.Agent.ID, target, err)
		writeError(w, http.StatusBadGateway, "fetch_failed", "fetch failed (blocked by network policy or unreachable target)")
		return
	}
	defer resp.Body.Close()

	// A 3xx surviving the client means the redirect cap was exhausted
	// (CheckRedirect returned ErrUseLastResponse): fail like the old
	// runtime's "too many redirects" instead of returning an empty body.
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		writeError(w, http.StatusBadGateway, "fetch_failed", "too many redirects (max 3)")
		return
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(p.maxBytes)+1))
	if err != nil {
		log.Printf("web_fetch proxy read error agent=%s url=%q: %v", principal.Agent.ID, target, err)
		writeError(w, http.StatusBadGateway, "fetch_failed", "failed reading upstream response")
		return
	}
	truncated := len(body) > p.maxBytes
	if truncated {
		body = body[:p.maxBytes]
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"url":         resp.Request.URL.String(),
		"status":      resp.StatusCode,
		"contentType": resp.Header.Get("Content-Type"),
		"bodyB64":     base64.StdEncoding.EncodeToString(body),
		"truncated":   truncated,
	})
}

// webFetchViaGateway forwards a validated web_fetch to the tool gateway
// (POST {gateway}/v1/web/fetch), passing through the agent's credential
// headers so the gateway authenticates the same agent and enforces the
// grant-side policy. The gateway's success body is re-emitted under the
// exact legacy contract (url/status/contentType/bodyB64/truncated — any
// additive gateway fields are dropped). Any gateway failure surfaces as
// the same 502 fetch_failed the legacy path produces, so the runtime
// needs zero changes.
func (s *Server) webFetchViaGateway(w http.ResponseWriter, r *http.Request, p fetchPolicy, target string) {
	principal := currentAgent(r.Context())
	body, err := json.Marshal(map[string]string{"url": target})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to encode gateway request")
		return
	}
	gwURL := s.cfg.ToolGatewayURL + "/v1/web/fetch"

	// Gateway gets the fetch timeout plus slack for its own policy/authn
	// round-trips; the CP never waits longer than that.
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(p.timeoutSeconds+15)*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gwURL, bytes.NewReader(body))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to build gateway request")
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if v := r.Header.Get("X-Skquad-Agent-ID"); v != "" {
		req.Header.Set("X-Skquad-Agent-ID", v)
	}
	if v := r.Header.Get("Authorization"); v != "" {
		req.Header.Set("Authorization", v)
	}

	client := &http.Client{Timeout: time.Duration(p.timeoutSeconds+15) * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("web_fetch gateway error agent=%s url=%q: %v", principal.Agent.ID, target, err)
		writeError(w, http.StatusBadGateway, "fetch_failed", "fetch failed (blocked by network policy or unreachable target)")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Surface every gateway failure (401/403/429/5xx) as the CP's
		// own 502-class tool error during rollout; the runtime treats
		// tool errors uniformly.
		log.Printf("web_fetch gateway status=%d agent=%s url=%q", resp.StatusCode, principal.Agent.ID, target)
		writeError(w, http.StatusBadGateway, "fetch_failed", "fetch failed (blocked by network policy or unreachable target)")
		return
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, int64(p.maxBytes)*2+4096))
	if err != nil {
		log.Printf("web_fetch gateway read error agent=%s url=%q: %v", principal.Agent.ID, target, err)
		writeError(w, http.StatusBadGateway, "fetch_failed", "failed reading gateway response")
		return
	}
	var gw struct {
		URL         string `json:"url"`
		Status      int    `json:"status"`
		ContentType string `json:"contentType"`
		BodyB64     string `json:"bodyB64"`
		Truncated   bool   `json:"truncated"`
	}
	if err := json.Unmarshal(raw, &gw); err != nil || gw.BodyB64 == "" && gw.Status == 0 {
		log.Printf("web_fetch gateway bad response agent=%s url=%q: %v", principal.Agent.ID, target, err)
		writeError(w, http.StatusBadGateway, "fetch_failed", "malformed gateway response")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"url":         gw.URL,
		"status":      gw.Status,
		"contentType": gw.ContentType,
		"bodyB64":     gw.BodyB64,
		"truncated":   gw.Truncated,
	})
}
