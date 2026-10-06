package mcp

// Streamable-HTTP MCP client (docs/tool-gateway.md §6.3, TG-5 slice A).
//
// JSON-RPC 2.0 over HTTP POST to the resource's single `/mcp`
// endpoint. Request headers follow the streamable-HTTP transport spec:
//
//	Content-Type: application/json
//	Accept: application/json, text/event-stream
//	Authorization: Bearer <resolved credential>
//	Mcp-Session-Id: <captured from initialize> (when the server sends one)
//
// A server may answer with either plain JSON or a (single-event) SSE
// stream; both shapes are handled: for `text/event-stream` bodies the
// client extracts the `data:` lines and picks the JSON-RPC response
// whose id matches the request.
//
// Error taxonomy: transport/protocol-level failures (connection,
// non-2xx HTTP, unparseable body) return a Go error — the driver maps
// those to ok=false (502). A well-formed JSON-RPC *error* response is
// returned in-band (resp.Error) so the caller can surface it as a tool
// error rather than a transport failure.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
)

// MaxResponseBytes caps a single upstream RPC body (4 MiB). Larger
// responses are a protocol error, never buffered unbounded.
const MaxResponseBytes = 4 << 20

// ProtocolVersion is the MCP protocol version advertised on initialize.
const ProtocolVersion = "2025-03-26"

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.Number     `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// ToolInfo is one entry of tools/list.
type ToolInfo struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
}

// ServerInfo is the initialize handshake's server metadata.
type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// InitializeResult is the parsed initialize handshake.
type InitializeResult struct {
	ProtocolVersion string
	ServerInfo      ServerInfo
	SessionID       string
}

// client is a single-endpoint JSON-RPC client. Not shared across
// resources: one client per governed call.
type client struct {
	endpoint string
	token    string
	http     *http.Client
	ctx      context.Context

	sessionID string
	nextID    atomic.Int64
}

func newClient(ctx context.Context, endpoint, token string, hc *http.Client) *client {
	return &client{ctx: ctx, endpoint: endpoint, token: token, http: hc}
}

// call performs one JSON-RPC exchange. Returns the parsed response
// (which may carry a JSON-RPC Error in-band) or a transport error.
func (c *client) call(method string, params any) (*rpcResponse, error) {
	id := c.nextID.Add(1)
	body, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if err != nil {
		return nil, fmt.Errorf("mcp_failed: encoding request")
	}

	httpReq, err := http.NewRequestWithContext(c.ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("mcp_failed: building request")
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	if c.token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.token)
	}
	if c.sessionID != "" {
		httpReq.Header.Set("Mcp-Session-Id", c.sessionID)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		// Cause wrapped (%w) so callers can classify redirect/SSRF
		// refusals (TG-5 B1 enumerate taxonomy); behavior for the
		// call path is unchanged (still a transport error → 502).
		return nil, fmt.Errorf("mcp_failed: upstream request failed: %w", err)
	}
	defer resp.Body.Close()

	// Streamable-HTTP session pinning: capture the session id from any
	// response that carries one (initialize in practice) so subsequent
	// requests on this client echo it back.
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		c.sessionID = sid
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("mcp_failed: reading upstream")
	}
	if len(raw) > MaxResponseBytes {
		return nil, fmt.Errorf("mcp_failed: upstream response too large")
	}

	// 202 Accepted with no body is a valid streamable-HTTP ack but
	// carries no RPC response — treat as protocol failure here since
	// the driver only issues request/response methods.
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, fmt.Errorf("mcp_failed: empty upstream response (status %d)", resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("mcp_failed: upstream status %d", resp.StatusCode)
	}

	ctype := resp.Header.Get("Content-Type")
	if strings.Contains(ctype, "text/event-stream") {
		return parseSSEResponse(raw, id)
	}
	var rpc rpcResponse
	if err := json.Unmarshal(raw, &rpc); err != nil {
		return nil, fmt.Errorf("mcp_failed: unparseable upstream response")
	}
	return &rpc, nil
}

// parseSSEResponse extracts the JSON-RPC response matching wantID
// from an SSE body. Handles the single-event case the spec allows for
// request/response exchanges (event: message\ndata: {...}) as well as
// bare data-only events; non-matching or unparsable events are skipped
// so a leading comment/keepalive line cannot break the exchange.
func parseSSEResponse(raw []byte, wantID int64) (*rpcResponse, error) {
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64*1024), MaxResponseBytes)
	var dataLines []string
	var found *rpcResponse
	handleEvent := func() {
		if found != nil || len(dataLines) == 0 {
			dataLines = dataLines[:0]
			return
		}
		payload := strings.Join(dataLines, "\n")
		dataLines = dataLines[:0]
		var rpc rpcResponse
		if err := json.Unmarshal([]byte(payload), &rpc); err != nil {
			return
		}
		if rpc.ID.String() == fmt.Sprint(wantID) || rpc.ID.String() == "" {
			found = &rpc
		}
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			handleEvent()
		case strings.HasPrefix(line, "data:"):
			dataLines = append(dataLines, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		default:
			// event:, id:, retry:, comment lines — ignored for
			// response extraction.
		}
	}
	handleEvent()
	if found == nil {
		return nil, fmt.Errorf("mcp_failed: no matching response in SSE stream")
	}
	return found, nil
}

// initialize performs the MCP handshake and captures the protocol
// version, server info, and (if the server sends one) the session id.
func (c *client) initialize() (*InitializeResult, error) {
	params := map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]string{"name": "skquad-tool-gateway", "version": "1.0"},
	}
	resp, err := c.call("initialize", params)
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("mcp_failed: initialize rejected: %s", resp.Error.Message)
	}
	var res struct {
		ProtocolVersion string     `json:"protocolVersion"`
		ServerInfo      ServerInfo `json:"serverInfo"`
	}
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		return nil, fmt.Errorf("mcp_failed: unparseable initialize result")
	}
	return &InitializeResult{
		ProtocolVersion: res.ProtocolVersion,
		ServerInfo:      res.ServerInfo,
		SessionID:       c.sessionID,
	}, nil
}

// toolsList enumerates the upstream tools.
func (c *client) toolsList() ([]ToolInfo, error) {
	resp, err := c.call("tools/list", map[string]any{})
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("mcp_failed: tools/list rejected: %s", resp.Error.Message)
	}
	var res struct {
		Tools []ToolInfo `json:"tools"`
	}
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		return nil, fmt.Errorf("mcp_failed: unparseable tools/list result")
	}
	return res.Tools, nil
}

// toolsCall invokes a tool and returns the raw result payload (or the
// in-band JSON-RPC error). Transport failures return err.
func (c *client) toolsCall(name string, arguments json.RawMessage) (json.RawMessage, *rpcError, error) {
	args := arguments
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	resp, err := c.call("tools/call", map[string]any{"name": name, "arguments": args})
	if err != nil {
		return nil, nil, err
	}
	if resp.Error != nil {
		return nil, resp.Error, nil
	}
	return resp.Result, nil, nil
}
