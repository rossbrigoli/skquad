// Package mcpserver implements the streamable-HTTP MCP endpoint for
// k8s-mcp (MIG-7 / S-313), compatible with the skquad tool-gateway MCP
// driver (tool-gateway/internal/drivers/mcp/client.go):
//
//   - POST /mcp — JSON-RPC 2.0: initialize, tools/list, tools/call.
//     Responses are plain application/json (no SSE needed for
//     request/response exchanges).
//   - Bearer auth on /mcp (constant-time compare). Fits the skquad mcp
//     resource auth_kind=bearer + per-agent credential custody.
//   - notifications/* get a 202 with no body (valid streamable-HTTP ack).
//   - GET /healthz — unauthenticated liveness probe.
//
// The server is stateless: no Mcp-Session-Id is issued, which the
// gateway client handles (session capture is optional).
package mcpserver

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"

	"github.com/rossbrigoli/skquad/k8s-mcp/internal/tools"
)

// ProtocolVersion matches what the skquad gateway advertises on
// initialize (2025-03-26 streamable-HTTP).
const ProtocolVersion = "2025-03-26"

// MaxRequestBytes caps an incoming RPC body (1 MiB).
const MaxRequestBytes = 1 << 20

const (
	jsonRPCInvalidRequest = -32600
	jsonRPCMethodNotFound = -32601
	jsonRPCInvalidParams  = -32602
	jsonRPCInternalError  = -32603
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.Number     `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      json.Number `json:"id"`
	Result  any         `json:"result,omitempty"`
	Error   *rpcError   `json:"error,omitempty"`
}

// Server serves the MCP endpoint backed by a tool registry.
type Server struct {
	authToken  string
	registry   *tools.Registry
	serverInfo map[string]string
}

// New builds the server.
func New(authToken string, reg *tools.Registry) *Server {
	return &Server{
		authToken:  authToken,
		registry:   reg,
		serverInfo: map[string]string{"name": "skquad-k8s-mcp", "version": "0.1.0"},
	}
}

// Handler returns the routed HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/mcp", s.handleMCP)
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeRPCError(w http.ResponseWriter, id json.Number, code int, msg string) {
	writeJSON(w, http.StatusOK, rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}})
}

func (s *Server) authorized(r *http.Request) bool {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) <= len(prefix) || h[:len(prefix)] != prefix {
		return false
	}
	got := h[len(prefix):]
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.authToken)) == 1
}

func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	// Auth first: unauthenticated callers learn nothing about the catalog.
	if !s.authorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, MaxRequestBytes+1))
	if err != nil || len(body) > MaxRequestBytes {
		writeRPCError(w, json.Number(""), jsonRPCInvalidRequest, "invalid or oversized request body")
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil || req.JSONRPC != "2.0" || req.Method == "" {
		writeRPCError(w, json.Number(""), jsonRPCInvalidRequest, "invalid JSON-RPC 2.0 request")
		return
	}

	// Notifications: acknowledge, no response body (202).
	if len(req.ID.String()) == 0 || req.Method == "notifications/initialized" || req.Method == "notifications/cancelled" {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	switch req.Method {
	case "initialize":
		writeJSON(w, http.StatusOK, rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
			"protocolVersion": ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      s.serverInfo,
		}})
	case "tools/list":
		list := make([]map[string]any, 0, len(s.registry.Tools()))
		for _, t := range s.registry.Tools() {
			list = append(list, map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"inputSchema": t.InputSchema,
			})
		}
		writeJSON(w, http.StatusOK, rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{"tools": list}})
	case "tools/call":
		s.handleToolCall(w, r, req)
	default:
		writeRPCError(w, req.ID, jsonRPCMethodNotFound, fmt.Sprintf("method %q not found", req.Method))
	}
}

func (s *Server) handleToolCall(w http.ResponseWriter, r *http.Request, req rpcRequest) {
	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil || params.Name == "" {
		writeRPCError(w, req.ID, jsonRPCInvalidParams, "tools/call requires {name, arguments}")
		return
	}
	result, known := s.registry.Call(r.Context(), params.Name, params.Arguments)
	if !known {
		writeRPCError(w, req.ID, jsonRPCInvalidParams, fmt.Sprintf("unknown tool %q", params.Name))
		return
	}
	// Audit line: who called what and the outcome class — never args
	// values (may contain identifiers) and never any credential.
	log.Printf("tool_call name=%q error=%v", params.Name, result.IsError)
	writeJSON(w, http.StatusOK, rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
		"content": []map[string]any{{"type": "text", "text": result.Text}},
		"isError": result.IsError,
	}})
}
