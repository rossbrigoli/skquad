// Package git implements the gateway's governed `git` driver
// (docs/tool-gateway.md §6.5, TG-4b): a Git smart-HTTP proxy so an
// agent's git remote points at the gateway and the PAT never enters
// the agent pod.
//
// Invariants enforced here (effective policy = config ∧ ceiling ∧
// grant constraints, see policy.go):
//
//   - Service gating: git-upload-pack = read (clone/fetch),
//     git-receive-pack = write (push). A grant without push is
//     rejected with 403 BEFORE any credential resolution or upstream
//     connection.
//   - Repo allowlist: ceiling repos_allow (e.g. "org/repo1",
//     "org/*"), default-deny; a grant may narrow but never widen.
//     The repo is parsed from the git path; a trailing ".git" is
//     normalized away and matching is case-sensitive (GitHub-style
//     case-folding is NOT applied — allowlists stay exact).
//   - Credential injection: the per-(resource,agent) secret is
//     resolved PER CALL from the CP internal credentials API and
//     injected as Authorization (bearer PAT or basic). The agent's
//     own Authorization header is never forwarded. Secrets are never
//     logged and never appear in audit payloads.
//   - Streaming: packfile bodies pass through without full buffering
//     — io.Copy with http.Flusher chunked passthrough on both
//     directions. No io.ReadAll on request or response bodies.
//   - Cross-host redirects are refused (same credential-leak defense
//     as the rest driver).
//   - Push audit: receive-pack report-status is tee-parsed
//     (streaming, reportstatus.go) so the audit names the refs the
//     push advanced/rejected.
//   - Egress: the shared netguard dial-time SSRF guard. Public by
//     default; private ranges only when the resource's grant-level
//     egress_class is "internal" (the git ceiling JSON has no egress
//     option — deviation noted in WORKLOG).
package git

import (
	"context"
	"encoding/base64"
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

// Git smart-HTTP service names.
const (
	ServiceUploadPack  = "git-upload-pack"  // fetch / clone
	ServiceReceivePack = "git-receive-pack" // push
)

// Timeout bounds each upstream connection. Git transfers can be long;
// this is an idle/establish bound (the transport's per-phase
// timeouts), not a whole-stream cap — streaming a large packfile
// legitimately exceeds it between phases.
const Timeout = 120 * time.Second

// UserAgent identifies gateway-originated git traffic.
const UserAgent = "skquad-git/1.0"

// Driver performs governed git smart-HTTP proxying. State is limited
// to the per-(agent,resource) rate buckets.
type Driver struct {
	limits *rateLimiters
	creds  credentials.Resolver
	// Resolver overrides DNS for tests (stub resolvers simulate
	// rebinding). nil uses the system resolver.
	Resolver netguard.Resolver
}

// New builds a git driver bound to a credential resolver.
func New(creds credentials.Resolver) *Driver {
	return &Driver{limits: newRateLimiters(), creds: creds}
}

func (d *Driver) Name() string { return "git" }

// Handle is not used: the git driver is streaming-only. The pipeline
// dispatches via ServeStream (drivers.StreamingDriver).
func (d *Driver) Handle(context.Context, *drivers.Request) (*drivers.Response, error) {
	return nil, fmt.Errorf("git driver is streaming-only: use ServeStream")
}

// ServeStream proxies one git smart-HTTP exchange. req.Path carries
// the trailing path routed by the gateway
// ("<org>/<repo>.git/<service>" or "<org>/<repo>.git/info/refs"),
// and the service for info/refs comes from the query string. Nothing
// is written to w before an error is returned.
func (d *Driver) ServeStream(w http.ResponseWriter, r *http.Request, req *drivers.Request) (*drivers.StreamResult, error) {
	if req.Grant == nil {
		return nil, drivers.Denied("no_grant")
	}
	pol, err := EffectivePolicy(req.Grant.Config, req.Grant.Ceiling, req.Grant.Constraints)
	if err != nil {
		// A malformed policy must never widen reach: deny.
		return nil, drivers.Denied("invalid_policy")
	}

	repo, service, err := ParseGitPath(req.Path, r.URL.RawQuery)
	if err != nil {
		return nil, fmt.Errorf("bad_request: %v", err)
	}

	// Egress class: public by default (no private ranges). The git
	// ceiling shape has no egress option, so the resource's
	// grant-level egress_class column is the only lever — an
	// "internal" git resource (e.g. a self-hosted Gitea on RFC1918)
	// needs it explicitly set by the admin. Deviation note vs the
	// rest driver (which folds egress from ceiling ∧ constraints
	// JSON): recorded in the WORKLOG.
	allowPrivate := strings.EqualFold(strings.TrimSpace(req.Grant.EgressClass), "internal")

	// Repo allowlist (default-deny) BEFORE any credential or network.
	if !pol.repoAllowed(repo) {
		return nil, drivers.Denied("repo_denied")
	}
	// Service gating: push requires an explicit push grant, enforced
	// before any upstream connection (read-only grants are
	// structurally incapable of pushing).
	if service == ServiceReceivePack && !pol.AllowPush {
		return nil, drivers.Denied("push_not_allowed")
	}

	agentID := ""
	if req.Agent != nil {
		agentID = req.Agent.AgentID
	}
	if pol.RatePerMin > 0 && !d.limits.allow(agentID+"\x00"+req.Resource, pol.RatePerMin, time.Now()) {
		return nil, drivers.Denied("rate_limited")
	}

	// Per-call credential resolution (fail-closed). A git resource
	// is always credentialed: the whole point is that the PAT lives
	// at the gateway, not in the agent.
	if d.creds == nil || agentID == "" {
		return nil, drivers.Denied("credentials_unavailable")
	}
	secret, err := d.creds.Resolve(r.Context(), req.Resource, agentID)
	if err != nil {
		return nil, drivers.Denied("credentials_unavailable")
	}

	target, err := joinURL(pol.BaseURL, req.Path, r.URL.RawQuery)
	if err != nil {
		return nil, fmt.Errorf("bad_request: invalid target url")
	}

	// Egress-guarded transport: dial-time SSRF pin + per-hop redirect
	// re-check, identical posture to the rest/web drivers.
	guard := &netguard.Guard{AllowPrivate: allowPrivate}
	dialer := netguard.Dialer{Guard: guard, Timeout: Timeout, Resolver: d.Resolver}
	transport := &http.Transport{
		DialContext:         dialer.DialContext,
		TLSHandshakeTimeout: Timeout,
		// No ResponseHeaderTimeout: git-upload-pack can hold a
		// connection while the server negotiates a large pack.
	}
	client := &http.Client{Transport: transport}
	originalHost := mustHost(target)
	// Credentials are always injected: refuse cross-host redirects
	// outright (same rule as the rest driver).
	client.CheckRedirect = func(httpReq *http.Request, via []*http.Request) error {
		if httpReq.URL.Host != originalHost {
			return fmt.Errorf("denied redirect: cross-host redirect not allowed for credentialed calls")
		}
		if len(via) >= 3 {
			return fmt.Errorf("denied redirect: too many redirects")
		}
		return nil
	}

	ctx, cancel := context.WithTimeout(r.Context(), Timeout)
	defer cancel()

	outReq, err := http.NewRequestWithContext(ctx, r.Method, target, streamingBody(r))
	if err != nil {
		return nil, fmt.Errorf("bad_request: invalid upstream request")
	}
	outReq.Header.Set("User-Agent", UserAgent)
	// Forward only the headers git needs from the client; the agent's
	// own Authorization/Cookie never rides along.
	for _, h := range []string{"Content-Type", "Accept"} {
		if v := r.Header.Get(h); v != "" {
			outReq.Header.Set(h, v)
		}
	}
	if err := injectAuth(outReq, secret); err != nil {
		return nil, err
	}

	resp, err := client.Do(outReq)
	if err != nil {
		return classifyDoError(err)
	}
	defer resp.Body.Close()

	// Copy the upstream response through, header by header, minus
	// credential-bearing headers.
	copyResponseHeaders(w, resp.Header)
	w.WriteHeader(resp.StatusCode)

	result := &drivers.StreamResult{Repo: repo, Service: service, StatusCode: resp.StatusCode}
	flusher, _ := w.(http.Flusher)
	if service == ServiceReceivePack && resp.StatusCode == http.StatusOK {
		rs, terr := teeReportStatus(resp.Body, w, flusher)
		result.RefsAdvanced = rs.Advanced
		result.RefsRejected = rs.Rejected
		if terr != nil {
			// Response bytes already streamed; report the truncation
			// as an error AFTER the audit fields are captured.
			return result, fmt.Errorf("git_stream: report-status parse: %w", terr)
		}
		return result, nil
	}
	if _, cerr := copyStream(w, resp.Body, flusher); cerr != nil {
		return result, fmt.Errorf("git_stream: %w", cerr)
	}
	return result, nil
}

// streamingBody adapts the inbound request body for the upstream
// call. GET/HEAD carry none; POST streams the raw body straight —
// never buffered whole (packfile uploads).
func streamingBody(r *http.Request) io.Reader {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return nil
	}
	if r.Body == nil {
		return nil
	}
	return r.Body
}

// copyStream pipes src to dst in fixed chunks, flushing after each
// so chunked passthrough reaches the client incrementally (no
// whole-body buffering, no head-of-line stall behind the Go writer).
func copyStream(w io.Writer, src io.Reader, flusher http.Flusher) (int64, error) {
	buf := make([]byte, 32*1024)
	var written int64
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return written, werr
			}
			flush(flusher)
			written += int64(n)
		}
		if rerr == io.EOF {
			return written, nil
		}
		if rerr != nil {
			return written, rerr
		}
	}
}

// injectAuth attaches the resolved credential. Supported kinds:
// bearer (GitHub fine-grained PAT / App installation token) and
// basic (username + token, e.g. GitHub HTTPS with any username).
func injectAuth(outReq *http.Request, secret *credentials.Secret) error {
	switch secret.Kind {
	case "bearer":
		tok := secret.Fields["token"]
		if tok == "" {
			return drivers.Denied("credential_unusable")
		}
		outReq.Header.Set("Authorization", "Bearer "+tok)
	case "basic":
		user := secret.Fields["username"]
		pass := secret.Fields["password"]
		if user == "" || pass == "" {
			return drivers.Denied("credential_unusable")
		}
		outReq.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(user+":"+pass)))
	default:
		return drivers.Denied("unsupported_auth_kind")
	}
	return nil
}

// ParseGitPath parses the routed trailing path of a git smart-HTTP
// request into (repo, service). Accepted shapes:
//
//	<org>/<repo>.git/info/refs?service=git-upload-pack
//	<org>/<repo>.git/info/refs?service=git-receive-pack
//	<org>/<repo>.git/git-upload-pack
//	<org>/<repo>.git/git-receive-pack
//
// The ".git" suffix is normalized away; matching downstream is
// case-sensitive. Nested groups (gitlab-style org/sub/repo) are
// supported: the repo is everything before the ".git" segment.
func ParseGitPath(trailing, rawQuery string) (string, string, error) {
	path := strings.Trim(trailing, "/")
	if path == "" {
		return "", "", fmt.Errorf("missing git path")
	}
	segs := strings.Split(path, "/")
	for _, s := range segs {
		if s == "" || s == "." || s == ".." {
			return "", "", fmt.Errorf("invalid git path")
		}
	}

	var repoPath, service string
	switch {
	case len(segs) >= 4 && segs[len(segs)-2] == "info" && segs[len(segs)-1] == "refs":
		q, err := url.ParseQuery(rawQuery)
		if err != nil {
			return "", "", fmt.Errorf("invalid query")
		}
		service = q.Get("service")
		if service != ServiceUploadPack && service != ServiceReceivePack {
			return "", "", fmt.Errorf("info/refs requires service=git-upload-pack or git-receive-pack")
		}
		repoPath = strings.Join(segs[:len(segs)-2], "/")
	case len(segs) >= 3:
		service = segs[len(segs)-1]
		if service != ServiceUploadPack && service != ServiceReceivePack {
			return "", "", fmt.Errorf("unknown git service %q", service)
		}
		repoPath = strings.Join(segs[:len(segs)-1], "/")
	default:
		return "", "", fmt.Errorf("git path must be <org>/<repo>.git/<service>")
	}
	if !strings.HasSuffix(repoPath, ".git") {
		return "", "", fmt.Errorf("git path must carry the .git suffix")
	}
	repo := strings.TrimSuffix(repoPath, ".git")
	if repo == "" || strings.HasSuffix(repo, "/") || !strings.Contains(repo, "/") {
		return "", "", fmt.Errorf("git path must include org and repo")
	}
	return repo, service, nil
}

// joinURL appends the validated git path/query to the registered
// base_url.
func joinURL(base, path, query string) (string, error) {
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("invalid base_url")
	}
	basePath := strings.TrimRight(u.Path, "/")
	u.Path = basePath + "/" + strings.TrimLeft(path, "/")
	u.RawQuery = query
	return u.String(), nil
}

// copyResponseHeaders passes upstream headers through minus
// credential-bearing headers (same scrub list as the rest driver).
func copyResponseHeaders(w http.ResponseWriter, h http.Header) {
	scrub := map[string]bool{
		"Set-Cookie":        true,
		"Set-Authorization": true,
		"Authorization":     true,
		"Www-Authenticate":  true,
	}
	for k, vals := range h {
		if scrub[k] || k == "Connection" {
			continue
		}
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
}

// classifyDoError maps transport errors to client-safe classes: SSRF
// guard trips are policy denials; everything else is a generic
// upstream failure. Raw error text (which can carry internal host/IP
// detail) is never echoed.
func classifyDoError(err error) (*drivers.StreamResult, error) {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "ssrf_guard"):
		return nil, drivers.Denied("ssrf_blocked")
	case strings.Contains(msg, "denied redirect"):
		return nil, drivers.Denied("redirect_denied")
	default:
		return nil, fmt.Errorf("git_failed: upstream request failed")
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
var _ drivers.StreamingDriver = (*Driver)(nil)
