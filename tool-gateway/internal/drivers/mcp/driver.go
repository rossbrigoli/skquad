// Package mcp implements the gateway's governed `mcp` driver
// (docs/tool-gateway.md §6.3, TG-5 slice A): BYO-credential access
// to streamable-HTTP MCP (Model Context Protocol) servers with
// tool-level allowlists, argument-size enforcement and untrusted
// result wrapping.
//
// Invariants enforced here (effective policy = config ∧ ceiling ∧
// grant constraints, see policy.go):
//
//   - Tool ACL: the requested tool must match tools_allow (default-
//     deny when unset) and must NOT match tools_deny (deny wins).
//     Both checks happen BEFORE any network activity or credential
//     resolution.
//   - requires_confirmation tools are DENIED fail-closed
//     ("confirmation_required"): the approval flow is later-slice
//     work, so a confirmation-gated tool must not execute silently.
//   - Argument size: serialized arguments must fit max_args_bytes,
//     else the call is rejected ("args_too_large" → HTTP 413).
//   - Credential injection: the per-(resource,agent) bearer secret
//     is resolved PER CALL from the CP internal credentials API and
//     injected as Authorization. The agent's own Authorization
//     header never reaches the upstream. Secrets are never logged and
//     never appear in audit payloads.
//   - Cross-host redirects are refused for credentialed calls (same
//     credential-leak defense as the rest/git drivers).
//   - Egress: the shared netguard dial-time SSRF guard. Public by
//     default; private ranges only when BOTH ceiling and grant
//     constraints set egress_class=internal.
//   - Error taxonomy: a JSON-RPC error response from the upstream is
//     surfaced as a TOOL error (ok=true carrying rpcError, like a
//     rest 4xx), not a transport failure; only connection/protocol
//     failures map to ok=false (502).
//   - Results are wrapped as untrusted content (S-PROMPT WP3 tag
//     format: <skquad_untrusted source="mcp" ...>body</...>). The
//     canonical wrapper lives in the agent runtime (Python); the
//     gateway cannot import it, so a byte-compatible Go equivalent
//     is applied here (attrs escaped, body verbatim — same posture).
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/rossbrigoli/skquad/shared/netguard"

	"github.com/rossbrigoli/skquad/tool-gateway/internal/credentials"
	"github.com/rossbrigoli/skquad/tool-gateway/internal/drivers"
)

// Timeout bounds each upstream RPC (handshake + list + call phases).
const Timeout = 30 * time.Second

// MaxRedirects mirrors the web/rest drivers' redirect cap.
const MaxRedirects = 3

// UserAgent identifies gateway-originated MCP traffic.
const UserAgent = "skquad-mcp/1.0"

// CallRequest is the agent-facing mcp_call payload:
//
//	{"tool": "name", "arguments": { ... }}
type CallRequest struct {
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// RPCError is a JSON-RPC error surfaced in-band from the upstream.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// CallResult is returned to the caller on success (HTTP 200). A
// JSON-RPC error from the upstream is carried in rpcError with
// IsError=true — the call reached the tool layer, so it is NOT a
// transport failure.
type CallResult struct {
	Tool     string    `json:"tool"`
	Content  string    `json:"content,omitempty"` // wrapped untrusted
	IsError  bool      `json:"isError"`
	RPCError *RPCError `json:"rpcError,omitempty"`
}

// Driver performs governed MCP calls over streamable HTTP. State is
// limited to the per-(agent,resource) rate buckets.
type Driver struct {
	limits *rateLimiters
	creds  credentials.Resolver
	// Resolver overrides DNS for tests (stub resolvers simulate
	// rebinding). nil uses the system resolver.
	Resolver netguard.Resolver
}

// New builds an mcp driver bound to a credential resolver.
func New(creds credentials.Resolver) *Driver {
	return &Driver{limits: newRateLimiters(), creds: creds}
}

func (d *Driver) Name() string { return "mcp" }

// Handle executes one governed tools/call: policy fold → tool ACL →
// args cap → rate limit → credential resolution → initialize
// handshake → tools/call → untrusted wrap. All policy denials happen
// before any upstream connection.
func (d *Driver) Handle(ctx context.Context, req *drivers.Request) (*drivers.Response, error) {
	if req.Grant == nil {
		return nil, drivers.Denied("no_grant")
	}
	var in CallRequest
	if err := json.Unmarshal(req.Payload, &in); err != nil {
		return nil, fmt.Errorf("bad_request: %w", err)
	}
	in.Tool = strings.TrimSpace(in.Tool)
	if in.Tool == "" {
		return nil, fmt.Errorf("bad_request: tool is required")
	}
	if len(in.Arguments) > 0 {
		var obj map[string]any
		if err := json.Unmarshal(in.Arguments, &obj); err != nil {
			return nil, fmt.Errorf("bad_request: arguments must be a JSON object")
		}
	}

	pol, err := EffectivePolicy(req.Grant.Config, req.Grant.Ceiling, req.Grant.Constraints)
	if err != nil {
		// A malformed policy must never widen reach: deny.
		return nil, drivers.Denied("invalid_policy")
	}
	if pol.AuthKind != AuthBearer {
		return nil, drivers.Denied("unsupported_auth_kind")
	}

	// Tool ACL strictly BEFORE any network activity or secret fetch.
	if !pol.toolAllowed(in.Tool) {
		return nil, drivers.Denied("tool_denied")
	}
	if pol.requiresConfirmation(in.Tool) {
		return nil, drivers.Denied("confirmation_required")
	}

	// Args size: measured on the serialized bytes as received
	// (whitespace-trimmed), never on the parsed values.
	argsBytes := len(strings.TrimSpace(string(in.Arguments)))
	if argsBytes > pol.MaxArgsBytes {
		return nil, drivers.Denied("args_too_large")
	}

	agentID := ""
	if req.Agent != nil {
		agentID = req.Agent.AgentID
	}
	if pol.RatePerMin > 0 && !d.limits.allow(agentID+"\x00"+req.Resource, pol.RatePerMin, time.Now()) {
		return nil, drivers.Denied("rate_limited")
	}

	// Per-call secret resolution (fail-closed). A credentialed call
	// without a resolved agent identity is denied: without it we
	// could not honour the per-agent isolation guarantee.
	if d.creds == nil {
		return nil, drivers.Denied("credentials_unavailable")
	}
	if agentID == "" {
		return nil, drivers.Denied("agent_identity_missing")
	}
	secret, err := d.creds.Resolve(ctx, req.Resource, agentID)
	if err != nil {
		return nil, drivers.Denied("credentials_unavailable")
	}
	if secret.Kind != AuthBearer {
		// Config/secret drift: refuse rather than inject the wrong
		// credential shape.
		return nil, drivers.Denied("credential_kind_mismatch")
	}
	token := secret.Fields["token"]
	if token == "" {
		return nil, drivers.Denied("credential_unusable")
	}

	endpoint, err := mcpEndpoint(pol.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("bad_request: invalid mcp endpoint")
	}

	// Egress-guarded transport: dial-time SSRF pin + per-hop redirect
	// re-check, identical posture to the rest driver.
	guard := &netguard.Guard{AllowPrivate: pol.AllowPrivate}
	dialer := netguard.Dialer{Guard: guard, Timeout: Timeout, Resolver: d.Resolver}
	transport := &http.Transport{
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   Timeout,
		ResponseHeaderTimeout: Timeout,
	}
	originalHost := mustHost(endpoint)
	client := &http.Client{Transport: transport, Timeout: Timeout}
	// Credential-bearing call: refuse cross-host redirects (the
	// bearer would otherwise follow the hop to a foreign host).
	client.CheckRedirect = func(httpReq *http.Request, via []*http.Request) error {
		if httpReq.URL.Host != originalHost {
			return fmt.Errorf("denied redirect: cross-host redirect not allowed for credentialed calls")
		}
		if len(via) >= MaxRedirects {
			return fmt.Errorf("denied redirect: too many redirects")
		}
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()

	c := newClient(ctx, endpoint, token, client)

	// MCP requires the initialize handshake before other methods.
	if _, err := c.initialize(); err != nil {
		return nil, err
	}

	resultRaw, rpcErr, err := c.toolsCall(in.Tool, in.Arguments)
	if err != nil {
		return nil, err
	}
	if rpcErr != nil {
		// JSON-RPC error: the tool layer answered — surface as a tool
		// error (ok=true), not a transport failure.
		return &drivers.Response{
			StatusCode: http.StatusOK,
			Body: &CallResult{
				Tool:     in.Tool,
				IsError:  true,
				RPCError: &RPCError{Code: rpcErr.Code, Message: sanitizeRPCMessage(rpcErr.Message)},
			},
		}, nil
	}

	content, err := extractContent(resultRaw)
	if err != nil {
		return nil, err
	}
	wrapped := WrapUntrusted(content, "mcp", map[string]string{
		"resource": req.Resource,
		"tool":     in.Tool,
	})
	return &drivers.Response{
		StatusCode: http.StatusOK,
		Body: &CallResult{
			Tool:    in.Tool,
			Content: wrapped,
		},
	}, nil
}

// extractContent renders a tools/call result's content blocks into
// text. Text blocks pass through verbatim; non-text blocks (image,
// resource, …) are represented by their compact JSON so nothing is
// silently dropped. isError is surfaced via CallResult.IsError by the
// caller.
func extractContent(raw json.RawMessage) (string, error) {
	var res struct {
		Content []json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", fmt.Errorf("mcp_failed: unparseable tools/call result")
	}
	var sb strings.Builder
	for i, blockRaw := range res.Content {
		if i > 0 {
			sb.WriteString("\n")
		}
		var block struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(blockRaw, &block); err != nil {
			return "", fmt.Errorf("mcp_failed: unparseable content block")
		}
		if block.Type == "text" {
			sb.WriteString(block.Text)
			continue
		}
		sb.WriteString("[" + block.Type + " content: " + string(blockRaw) + "]")
	}
	return sb.String(), nil
}

// WrapUntrusted applies the S-PROMPT WP3 tag format (byte-compatible
// with the agent runtime's wrap_untrusted): attributes are XML-
// escaped, the body is left verbatim (it is quoted data; escaping it
// would corrupt legitimate content such as code and diffs).
func WrapUntrusted(content, source string, attrs map[string]string) string {
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	sb.WriteString(`<skquad_untrusted source="`)
	sb.WriteString(xmlEscape(source))
	sb.WriteString(`"`)
	for _, k := range keys {
		sb.WriteString(` `)
		sb.WriteString(k)
		sb.WriteString(`="`)
		sb.WriteString(xmlEscape(attrs[k]))
		sb.WriteString(`"`)
	}
	sb.WriteString(`>`)
	sb.WriteString(content)
	sb.WriteString(`</skquad_untrusted>`)
	return sb.String()
}

// xmlEscape mirrors the runtime's _xml_escape: & " ' < > only.
func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", `"`, "&quot;", "'", "&#39;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

// sanitizeRPCMessage keeps JSON-RPC error messages client-safe: they
// originate from the upstream and may echo internal detail; cap the
// length and strip control characters. (The message itself is the
// tool error explanation the agent needs — e.g. "unknown tool".)
func sanitizeRPCMessage(msg string) string {
	msg = strings.Map(func(r rune) rune {
		if r < 0x20 {
			return ' '
		}
		return r
	}, msg)
	const maxLen = 512
	if len(msg) > maxLen {
		msg = msg[:maxLen] + "…"
	}
	return msg
}

// mcpEndpoint normalizes the registered base URL to the streamable-
// HTTP endpoint: if the path doesn't already end in /mcp, append it.
func mcpEndpoint(base string) (string, error) {
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("invalid mcp base_url")
	}
	p := strings.TrimRight(u.Path, "/")
	if !strings.HasSuffix(p, "/mcp") {
		p = p + "/mcp"
	}
	u.Path = p
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
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
