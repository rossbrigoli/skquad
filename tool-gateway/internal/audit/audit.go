// Package audit emits one structured audit event per gateway request
// (design §2.4: agent, task, resource, operation, decision, latency).
// TG-1 ships a stdout JSON sink; the CP audit pipeline sink can be added
// behind the Emitter interface without touching call sites.
package audit

import (
	"encoding/json"
	"io"
	"sync"
	"time"
)

// Decision values for Event.Decision.
const (
	DecisionAllow = "allow"
	DecisionDeny  = "deny"
	DecisionError = "error"
)

// Event is the semantic audit record for one gateway request.
type Event struct {
	Timestamp  time.Time `json:"ts"`
	RequestID  string    `json:"request_id"`
	AgentID    string    `json:"agent_id,omitempty"`
	Resource   string    `json:"resource"`
	Operation  string    `json:"operation"`
	Decision   string    `json:"decision"`
	StatusCode int       `json:"status_code"`
	LatencyMS  int64     `json:"latency_ms"`
	Detail     string    `json:"detail,omitempty"`
}

// Emitter sinks audit events. Implementations must be safe for concurrent
// use and must never block the request path on I/O errors.
type Emitter interface {
	Emit(Event)
}

// StdoutEmitter writes one JSON object per event to w (os.Stdout in prod,
// a buffer in tests).
type StdoutEmitter struct {
	mu sync.Mutex
	w  io.Writer
}

// NewStdoutEmitter returns an emitter writing newline-delimited JSON.
func NewStdoutEmitter(w io.Writer) *StdoutEmitter {
	return &StdoutEmitter{w: w}
}

// Emit writes the event as a single JSON line. Errors are swallowed by
// design: audit sink failures must not fail the request (the CP-side audit
// pipeline in later TGs adds durable writes + alerting on sink failure).
func (e *StdoutEmitter) Emit(ev Event) {
	if ev.Timestamp.IsZero() {
		ev.Timestamp = time.Now()
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	_, _ = e.w.Write(append(b, '\n'))
}

// NopEmitter discards events (tests that don't care about audit).
type NopEmitter struct{}

func (NopEmitter) Emit(Event) {}
