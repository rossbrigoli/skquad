// TG-5 slice B2a (S-244-series): CP→gateway MCP enumerate client.
//
// This is the REVERSE of the gateway→CP callback pattern
// (requireGatewayCallback / X-Skquad-Callback-Token). The control-plane
// holds the MCP resource config and the admin-supplied bearer token but
// has no egress to arbitrary MCP servers, so registration delegates
// `initialize` + `tools/list` to the tool gateway:
//
//	POST {SKQUAD_TOOL_GATEWAY_URL}/internal/mcp/enumerate
//	X-Skquad-Internal-Token: <SKQUAD_GATEWAY_INTERNAL_TOKEN>
//	{"resource_id","url","auth":{"kind":"bearer","token"},"allow_private"}
//
// Contract: tool-gateway/internal/httpapi/mcpenumerate.go (slice B1).
// The gateway returns the enumerated tools plus a canonical sha256 hash
// of the tool set; the CP stores the hash VERBATIM (it is never
// recomputed here — the canonicalization lives normatively in
// drivers/mcp/enumerate.go::CanonicalToolSetHash).
//
// SECURITY: the bearer token and the internal token are never logged,
// never echoed in error messages, and never persisted outside the K8s
// Secret custody path.
package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

// mcpInternalTokenHeader mirrors the gateway's InternalTokenHeader
// (tool-gateway/internal/httpapi/mcpenumerate.go). Kept as a local
// literal because the control-plane must not import the gateway module.
const mcpInternalTokenHeader = "X-Skquad-Internal-Token"

// MCPToolInfo is one enumerated MCP tool as returned by the gateway
// (name + description + JSON-Schema inputSchema).
type MCPToolInfo struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// MCPEnumerateResult is the gateway's successful enumeration payload.
type MCPEnumerateResult struct {
	Tools []MCPToolInfo
	Hash  string
}

// MCPEnumerateClient performs CP→gateway MCP tool enumeration. The
// production implementation talks to the tool gateway over the internal
// cluster network; tests point it at an httptest fake.
type MCPEnumerateClient interface {
	Enumerate(ctx context.Context, resourceID, upstreamURL, bearerToken string, allowPrivate bool) (*MCPEnumerateResult, error)
}

// mcpEnumerateError carries the gateway's stable, secret-free error
// taxonomy (HTTP status + error code) so the registration handler can
// branch on the failure class without echoing upstream content.
// Status 0 means the gateway itself was unreachable (connection error).
type mcpEnumerateError struct {
	Status int
	Code   string
}

func (e *mcpEnumerateError) Error() string { return "mcp enumerate: " + e.Code }

// gatewayMCPEnumerateClient is the MCPEnumerateClient backed by the
// real tool gateway.
type gatewayMCPEnumerateClient struct {
	baseURL       string
	internalToken string
	client        *http.Client
}

// newGatewayMCPEnumerateClient builds the client. Both the gateway URL
// and the internal token are required — a partial configuration is a
// construction error (the caller degrades MCP registration to 503
// rather than half-wiring the trust path).
func newGatewayMCPEnumerateClient(baseURL, internalToken string) (*gatewayMCPEnumerateClient, error) {
	if strings.TrimSpace(baseURL) == "" || strings.TrimSpace(internalToken) == "" {
		return nil, errors.New("mcp enumerate client: gateway url and internal token are required")
	}
	return &gatewayMCPEnumerateClient{
		baseURL:       strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		internalToken: internalToken,
		// Enumeration opens an upstream handshake through the gateway;
		// 45s bounds the registration request generously without hanging it.
		client: &http.Client{Timeout: 45 * time.Second},
	}, nil
}

func (c *gatewayMCPEnumerateClient) Enumerate(ctx context.Context, resourceID, upstreamURL, bearerToken string, allowPrivate bool) (*MCPEnumerateResult, error) {
	payload, err := json.Marshal(map[string]any{
		"resource_id":   resourceID,
		"url":           upstreamURL,
		"auth":          map[string]string{"kind": "bearer", "token": bearerToken},
		"allow_private": allowPrivate,
	})
	if err != nil {
		return nil, &mcpEnumerateError{Code: "bad_request"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/internal/mcp/enumerate", bytes.NewReader(payload))
	if err != nil {
		return nil, &mcpEnumerateError{Code: "bad_request"}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(mcpInternalTokenHeader, c.internalToken)
	resp, err := c.client.Do(req)
	if err != nil {
		// Gateway unreachable / DNS / connection refused. Never surface
		// the raw error (it can embed URLs); the code is the taxonomy.
		return nil, &mcpEnumerateError{Status: 0, Code: "gateway_unreachable"}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, &mcpEnumerateError{Status: resp.StatusCode, Code: "bad_enumerate_response"}
	}
	if resp.StatusCode != http.StatusOK {
		code := "mcp_enumerate_failed"
		var e struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Error.Code != "" {
			code = e.Error.Code
		}
		return nil, &mcpEnumerateError{Status: resp.StatusCode, Code: code}
	}
	var out struct {
		ResourceID string        `json:"resource_id"`
		Tools      []MCPToolInfo `json:"tools"`
		Hash       string        `json:"hash"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, &mcpEnumerateError{Status: resp.StatusCode, Code: "bad_enumerate_response"}
	}
	if !isSHA256Hex(out.Hash) {
		return nil, &mcpEnumerateError{Status: resp.StatusCode, Code: "bad_enumerate_response"}
	}
	if out.Tools == nil {
		out.Tools = []MCPToolInfo{}
	}
	return &MCPEnumerateResult{Tools: out.Tools, Hash: out.Hash}, nil
}

// isSHA256Hex checks the canonical 64-hex shape of the gateway hash.
func isSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		default:
			return false
		}
	}
	return true
}
