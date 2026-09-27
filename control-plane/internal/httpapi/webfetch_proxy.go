// BT-6: web_fetch control-plane proxy.
//
// The agent pods run under a default-deny egress NetworkPolicy (only DNS +
// skquad-system are reachable), so the runtime CANNOT fetch arbitrary URLs
// itself. web_fetch therefore executes HERE, in the control plane, mirroring
// the web_search proxy pattern (ADR-0012 §3): the runtime sends the URL,
// the control plane performs the guarded fetch and returns the body.
//
// Security (same contract the runtime previously enforced locally):
//   - SSRF guard at DIAL time via net.Dialer.Control: every resolved
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

package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

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
// Dialer.Control.
func blockedDestAddr(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return true // malformed address: fail closed
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return true // Control should see resolved IPs; fail closed
	}
	switch {
	case ip.IsLoopback(), ip.IsPrivate(), ip.IsLinkLocalUnicast(),
		ip.IsLinkLocalMulticast(), ip.IsInterfaceLocalMulticast(),
		ip.IsMulticast(), ip.IsUnspecified():
		return true
	}
	if !ip.IsGlobalUnicast() {
		return true
	}
	if ip.To4() != nil {
		if _, cgnat, err := net.ParseCIDR("100.64.0.0/10"); err == nil && cgnat.Contains(ip) {
			return true
		}
	} else {
		if _, ula, err := net.ParseCIDR("fc00::/7"); err == nil && ula.Contains(ip) {
			return true
		}
	}
	return false
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
// the SSRF dial guard (unless allowPrivate) and whose redirects are capped
// and re-validated (Control fires per hop).
func guardedHTTPClient(p fetchPolicy) *http.Client {
	dialer := &net.Dialer{Timeout: time.Duration(p.timeoutSeconds) * time.Second}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			if !p.allowPrivateNetwork && blockedDestAddr(address) {
				return nil, fmt.Errorf("ssrf_guard: blocked dial to %s", address)
			}
			return dialer.DialContext(ctx, network, address)
		},
	}
	return &http.Client{
		Transport: transport,
		Timeout:   time.Duration(p.timeoutSeconds) * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > fetchMaxRedirects {
				return http.ErrUseLastResponse
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return fmt.Errorf("redirect to unsupported scheme %q", req.URL.Scheme)
			}
			return nil
		},
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
