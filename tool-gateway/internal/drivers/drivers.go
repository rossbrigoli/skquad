// Package drivers defines the gateway driver contract. Drivers are the only
// code that performs I/O toward external endpoints (design §5.2). TG-1
// ships the echo driver to prove the pipeline; web/rest/mcp/browser arrive
// in TG-3..TG-6.
package drivers

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/rossbrigoli/skquad/tool-gateway/internal/auth"
)

// ErrDenied wraps driver-level policy denials with a client-safe message.
var ErrDenied = errors.New("denied")

// Request is the dispatched call handed to a driver after authn and
// policy lookup succeeded.
type Request struct {
	Agent     *auth.AgentPrincipal
	Resource  string // logical resource name ("echo" for the TG-1 driver)
	Operation string // e.g. "echo"
	Payload   []byte // raw request body
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
