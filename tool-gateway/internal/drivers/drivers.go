// Package drivers defines the gateway driver contract. Drivers are the only
// code that performs I/O toward external endpoints (design §5.2). TG-1
// ships the echo driver to prove the pipeline; web/rest/mcp/browser arrive
// in TG-3..TG-6.
package drivers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/rossbrigoli/skquad/tool-gateway/internal/auth"
	"github.com/rossbrigoli/skquad/tool-gateway/internal/policy"
)

// ErrDenied wraps driver-level policy denials with a client-safe message.
var ErrDenied = errors.New("denied")

// DeniedError is a policy denial carrying a stable reason code
// (e.g. "domain_denied", "rate_limited", "ssrf_blocked"). The reason
// must never contain internal host/IP detail — it is surfaced to the
// calling agent. errors.Is(err, ErrDenied) matches.
type DeniedError struct {
	Reason string
}

func (e *DeniedError) Error() string { return "denied: " + e.Reason }
func (e *DeniedError) Is(target error) bool {
	if target == ErrDenied {
		return true
	}
	var d *DeniedError
	if errors.As(target, &d) {
		return d.Reason == e.Reason
	}
	return false
}

// Denied builds a client-safe policy denial.
func Denied(reason string) *DeniedError { return &DeniedError{Reason: reason} }

// Request is the dispatched call handed to a driver after authn and
// policy lookup succeeded.
type Request struct {
	Agent     *auth.AgentPrincipal
	Resource  string // logical resource name ("echo" for TG-1; resource id for typed drivers)
	Operation string // e.g. "echo", "fetch"
	Payload   []byte // raw request body
	// Grant is the effective policy grant the dispatch was authorized
	// against (typed drivers enforce ceiling/constraints/config from it).
	// Echo and other pipeline-proving drivers ignore it.
	Grant *policy.Grant
	// Path is the driver-routed trailing path for streaming proxy
	// drivers (TG-4b git: "<org>/<repo>.git/<service>"). Empty for
	// buffered drivers, which carry everything in Payload.
	Path string
}

// Response is what a driver returns on success.
type Response struct {
	StatusCode int
	Body       any
}

// Driver executes one class of governed operation.
type Driver interface {
	Name() string
	Handle(ctx context.Context, req *Request) (*Response, error)
}

// StreamResult is the audit metadata a streaming proxy driver returns
// after it has written the upstream response through the
// ResponseWriter (TG-4b git). It carries no secret material — refs
// and identity only.
type StreamResult struct {
	Repo         string
	Service      string // "git-upload-pack" | "git-receive-pack"
	StatusCode   int    // upstream status passed through to the client
	RefsAdvanced []string // push: refs reported "ok" by the upstream
	RefsRejected []string // push: refs reported "ng" by the upstream
}

// StreamingDriver is implemented by drivers that proxy raw byte
// streams end-to-end without buffering (git smart HTTP, TG-4b, docs
// docs/tool-gateway.md §6.5). The gateway pipeline still owns kill
// switch, authn and grant lookup; the driver then owns the wire: it
// MUST stream request and response bodies with chunked passthrough
// (io.Copy + http.Flusher, never io.ReadAll) and MUST NOT write to w
// before returning an error, so the pipeline can render the error
// envelope itself.
type StreamingDriver interface {
	Name() string
	ServeStream(w http.ResponseWriter, r *http.Request, req *Request) (*StreamResult, error)
}

// Echo is the TG-1 pipeline-proving driver: it returns the request payload
// plus the resolved agent identity. It performs no external I/O.
type Echo struct{}

func (Echo) Name() string { return "echo" }

func (Echo) Handle(_ context.Context, req *Request) (*Response, error) {
	agentID := ""
	if req.Agent != nil {
		agentID = req.Agent.AgentID
	}
	return &Response{
		StatusCode: 200,
		Body: map[string]any{
			"gateway": "tool-gateway",
			"agent":   map[string]any{"id": agentID},
			"payload": jsonOrString(req.Payload),
		},
	}, nil
}

// jsonOrString embeds the payload as JSON when it parses, else as a string,
// so echo round-trips arbitrary bodies losslessly.
func jsonOrString(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(b, &v); err == nil {
		return v
	}
	return string(b)
}
