package browser

// Minimal internal client for the browser-service (docs/tg6-browser-protocol.md).
// The mcp package's JSON-RPC client is unexported, so this is a small
// (~100 line) re-implementation of the same patterns: JSON-RPC 2.0 over
// HTTP POST with initialize + tools/call, plus the non-MCP session
// control calls (POST /v1/sessions, DELETE /v1/sessions/{id}).
//
// Auth planes (protocol §1): control calls carry the gateway's internal
// bearer token; MCP tool calls carry the SESSION token. Error taxonomy:
// a well-formed JSON-RPC error is returned in-band (rpcError) so the
// driver can map codes -32001/-32002/-32003; transport/HTTP failures
// return a Go error.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// MaxResponseBytes caps a single upstream RPC body (4 MiB), matching
// the mcp client's bound.
const MaxResponseBytes = 4 << 20

// ProtocolVersion is the MCP protocol version advertised on initialize.
const ProtocolVersion = "2025-03-26"

// JSON-RPC error codes pinned by the browser protocol (§3).
const (
	ErrCodeSessionInvalid  = -32001
	ErrCodeCeilingExceeded = -32002
	ErrCodeBrowserBusy     = -32003
)

// ErrBrowserBusy signals a 503 {"error":"browser_busy"} from the
// session control plane (pool exhausted past queue timeout).
var ErrBrowserBusy = errors.New("browser_busy")

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

// sessionResponse is the POST /v1/sessions 201 body.
type sessionResponse struct {
	SessionID string    `json:"session_id"`
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// serviceClient talks to ONE browser service instance. Not safe for
// concurrent use across agents; the driver builds one per exchange.
type serviceClient struct {
	baseURL string // control plane base, e.g. http://browser:8090
	token   string // internal token (control) or session token (MCP)
	http    *http.Client
	ctx     context.Context

	nextID atomic.Int64
}

func newServiceClient(ctx context.Context, baseURL, token string, hc *http.Client) *serviceClient {
	return &serviceClient{baseURL: strings.TrimRight(baseURL, "/"), token: token, http: hc, ctx: ctx}
}

// doJSON performs one HTTP exchange with JSON bodies and the bearer
// auth header. Returns status code and raw body.
func (c *serviceClient) doJSON(method, url string, body any) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, fmt.Errorf("browser_failed: encoding request")
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(c.ctx, method, url, rdr)
	if err != nil {
		return 0, nil, fmt.Errorf("browser_failed: building request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("browser_failed: upstream request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	if err != nil {
		return 0, nil, fmt.Errorf("browser_failed: reading upstream")
	}
	if len(raw) > MaxResponseBytes {
		return 0, nil, fmt.Errorf("browser_failed: upstream response too large")
	}
	return resp.StatusCode, raw, nil
}

// createSession performs POST /v1/sessions (internal-token auth).
// 503 browser_busy maps to ErrBrowserBusy; anything other than 201 is
// a transport-level failure.
func (c *serviceClient) createSession(binding map[string]any) (*sessionResponse, error) {
	status, raw, err := c.doJSON(http.MethodPost, c.baseURL+"/v1/sessions", binding)
	if err != nil {
		return nil, err
	}
	if status == http.StatusServiceUnavailable {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Error == "browser_busy" {
			return nil, ErrBrowserBusy
		}
		return nil, fmt.Errorf("browser_failed: session create status %d", status)
	}
	if status != http.StatusCreated {
		return nil, fmt.Errorf("browser_failed: session create status %d", status)
	}
	var sr sessionResponse
	if err := json.Unmarshal(raw, &sr); err != nil || sr.SessionID == "" || sr.Token == "" {
		return nil, fmt.Errorf("browser_failed: unparseable session response")
	}
	return &sr, nil
}

// closeSession performs DELETE /v1/sessions/{id} (idempotent 204).
// Best-effort: errors are returned but callers may ignore them.
func (c *serviceClient) closeSession(sessionID string) error {
	status, _, err := c.doJSON(http.MethodDelete, c.baseURL+"/v1/sessions/"+sessionID, nil)
	if err != nil {
		return err
	}
	if status != http.StatusNoContent && status != http.StatusOK {
		return fmt.Errorf("browser_failed: session close status %d", status)
	}
	return nil
}

// rpcCall performs one JSON-RPC 2.0 exchange against the MCP endpoint.
func (c *serviceClient) rpcCall(endpoint, method string, params any) (*rpcResponse, error) {
	id := c.nextID.Add(1)
	status, raw, err := c.doJSON(http.MethodPost, endpoint, rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, fmt.Errorf("browser_failed: empty upstream response (status %d)", status)
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("browser_failed: upstream status %d", status)
	}
	var rpc rpcResponse
	if err := json.Unmarshal(raw, &rpc); err != nil {
		return nil, fmt.Errorf("browser_failed: unparseable upstream response")
	}
	return &rpc, nil
}

// initialize performs the MCP handshake (protocol §3).
func (c *serviceClient) initialize(endpoint string) error {
	params := map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]string{"name": "skquad-tool-gateway", "version": "1.0"},
	}
	resp, err := c.rpcCall(endpoint, "initialize", params)
	if err != nil {
		return err
	}
	if resp.Error != nil {
		return fmt.Errorf("browser_failed: initialize rejected: %s", resp.Error.Message)
	}
	return nil
}

// toolsCall invokes a tool; a JSON-RPC error is returned in-band
// (second value), transport failures in the third.
func (c *serviceClient) toolsCall(endpoint, name string, arguments json.RawMessage) (json.RawMessage, *rpcError, error) {
	args := arguments
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	resp, err := c.rpcCall(endpoint, "tools/call", map[string]any{"name": name, "arguments": args})
	if err != nil {
		return nil, nil, err
	}
	if resp.Error != nil {
		return nil, resp.Error, nil
	}
	return resp.Result, nil, nil
}
