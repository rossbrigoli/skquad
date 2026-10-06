// MCP tool enumeration for control-plane registration
// (docs/tool-gateway.md §6.3, TG-5 slice B1).
//
// The control-plane has no direct egress to arbitrary MCP servers, so
// at registration it asks the GATEWAY to connect upstream and
// enumerate the real tool set. The CP stores the snapshot (names +
// input schemas + hash) and lets the admin pick the allowlist from
// the enumerated set; the hash drives drift detection.
//
// The transport posture is IDENTICAL to the governed call path
// (driver.go): netguard dial-time SSRF pin (private ranges only when
// allowPrivate is set explicitly), cross-host redirect refusal for
// the credentialed session, initialize-before-methods, and the same
// client. The upstream bearer token is used only on the wire — it is
// never embedded in the result, the hash, or any log line.
package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/rossbrigoli/skquad/shared/netguard"
)

// EnumerateResult is the outcome of a successful enumeration.
type EnumerateResult struct {
	Tools           []ToolInfo `json:"tools"`
	Hash            string     `json:"hash"`
	ServerName      string     `json:"server_name,omitempty"`
	ProtocolVersion string     `json:"protocol_version,omitempty"`
}

// CanonicalToolSetHash computes the drift-detection hash over a tool
// set. Slice B2 (control-plane) MUST reproduce this algorithm exactly
// to compare stored snapshots against re-enumerated sets.
//
// Algorithm (normative):
//
//  1. Sort the tools by `name` ascending, byte-wise (Go string <).
//     Duplicate names are kept (stable order among equal names).
//  2. For each tool, build one canonical JSON object with EXACTLY
//     these fields in this order:
//     {"name":<name>,"description":<description>,"inputSchema":<schema>}
//     - `description` is the empty string when absent.
//     - `inputSchema` is canonicalized: parsed with json.Number
//     preservation (numeric literals kept verbatim), then
//     re-serialized with object keys sorted lexicographically at
//     every nesting level; array order is preserved. A missing or
//     blank inputSchema canonicalizes to the empty object {}.
//     - JSON string escaping is Go's standard json.Marshal escaping
//     (", \, control chars, and \u003c/\u003e/\u0026 for < > &).
//  3. Join the per-tool canonical JSON strings with a single "\n"
//     (U+000A). No trailing newline.
//  4. hash = lowercase hex (128 chars) of the SHA-256 over those
//     UTF-8 bytes.
//
// Any tool addition, removal, rename, description change, or schema
// change (at any nesting depth) flips the hash; reordering the same
// set does not.
func CanonicalToolSetHash(tools []ToolInfo) (string, error) {
	sorted := make([]ToolInfo, len(tools))
	copy(sorted, tools)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	parts := make([]string, 0, len(sorted))
	for _, tl := range sorted {
		schema := json.RawMessage("{}")
		if len(bytes.TrimSpace(tl.InputSchema)) > 0 {
			dec := json.NewDecoder(bytes.NewReader(tl.InputSchema))
			dec.UseNumber()
			var v any
			if err := dec.Decode(&v); err != nil {
				return "", fmt.Errorf("mcp_failed: uncanonical inputSchema for tool %q", tl.Name)
			}
			b, err := json.Marshal(v) // Go map marshaling sorts keys at every level
			if err != nil {
				return "", fmt.Errorf("mcp_failed: uncanonical inputSchema for tool %q", tl.Name)
			}
			schema = b
		}
		entry := struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"inputSchema"`
		}{Name: tl.Name, Description: tl.Description, InputSchema: schema}
		b, err := json.Marshal(entry)
		if err != nil {
			return "", fmt.Errorf("mcp_failed: canonicalizing tool %q", tl.Name)
		}
		parts = append(parts, string(b))
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return hex.EncodeToString(sum[:]), nil
}

// Enumerate connects to an upstream streamable-HTTP MCP server and
// performs initialize + tools/list, returning the tool set with its
// canonical hash. baseURL is normalized like the call path (a path
// not already ending in /mcp gets /mcp appended). allowPrivate
// mirrors the driver's internal-egress class: private/loopback
// destinations are refused unless the caller (CP, from the resource's
// egress class) sets it explicitly. resolver is optional (tests).
//
// Failures are plain Go errors ("mcp_failed: ...") — the HTTP layer
// maps them to a structured 502 the CP turns into a registration
// failure. The token never appears in any error string.
func Enumerate(ctx context.Context, baseURL, token string, allowPrivate bool, resolver netguard.Resolver) (*EnumerateResult, error) {
	endpoint, err := mcpEndpoint(baseURL)
	if err != nil {
		return nil, fmt.Errorf("bad_request: invalid mcp endpoint")
	}
	if token == "" {
		return nil, fmt.Errorf("bad_request: credential token is required")
	}

	// Same guarded transport as the governed call path: dial-time
	// SSRF pin + cross-host redirect refusal for the credentialed
	// session.
	guard := &netguard.Guard{AllowPrivate: allowPrivate}
	dialer := netguard.Dialer{Guard: guard, Timeout: Timeout, Resolver: resolver}
	transport := &http.Transport{
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   Timeout,
		ResponseHeaderTimeout: Timeout,
	}
	originalHost := mustHost(endpoint)
	client := &http.Client{Transport: transport, Timeout: Timeout}
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
	init, err := c.initialize()
	if err != nil {
		return nil, err
	}
	tools, err := c.toolsList()
	if err != nil {
		return nil, err
	}
	hash, err := CanonicalToolSetHash(tools)
	if err != nil {
		return nil, err
	}
	return &EnumerateResult{
		Tools:           tools,
		Hash:            hash,
		ServerName:      init.ServerInfo.Name,
		ProtocolVersion: init.ProtocolVersion,
	}, nil
}
