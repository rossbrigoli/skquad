// Package confirmation is the gateway-side client for the control
// plane's TG-8 slice C internal confirmation endpoints:
//
//	POST {CP}/internal/v1/confirmation/check   {resource_id, agent_id, tool, args_hash}
//	POST {CP}/internal/v1/confirmation/consume {id, args_hash}
//
// Same trust class as /internal/v1/policy (netpol-internal, no app-layer
// auth). The client is FAIL-CLOSED: any transport error or unexpected
// status is ErrUnavailable and the caller must not execute the gated
// call. Only a well-formed 200 answer drives a decision.
package confirmation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ErrUnavailable means the CP could not deliver a usable confirmation
// answer. Callers must DENY the gated dispatch (fail-closed).
var ErrUnavailable = errors.New("confirmation_unavailable")

// CheckResult mirrors the CP ConfirmationCheckResult:
// mode "auto" (live standing grant short-circuited) or "pending".
type CheckResult struct {
	Mode                   string `json:"mode"`
	ConfirmationID         string `json:"confirmation_id,omitempty"`
	MatchedStandingGrantID string `json:"matched_standing_grant_id,omitempty"`
}

// ConsumeResult mirrors the CP ConfirmationConsumeResult. Reason is
// agent-visible on denials (stable codes: denied_replayed,
// args_hash_mismatch, approval_expired, standing_grant_not_live,
// denied_by_owner[: detail], confirmation_expired, confirmation_pending).
type ConsumeResult struct {
	Allowed                bool   `json:"allowed"`
	Mode                   string `json:"mode,omitempty"` // "once" | "standing"
	Reason                 string `json:"reason,omitempty"`
	MatchedStandingGrantID string `json:"matched_standing_grant_id,omitempty"`
}

// ArgsHash computes the canonical sha256 hex over
// (resource_id, operation, payload bytes). Length-prefixed so no
// separator-collision is possible: shifting bytes between fields
// changes the hash. Same inputs ⇒ same hash, deterministically.
func ArgsHash(resourceID, operation string, payload []byte) string {
	h := sha256.New()
	writeField := func(b []byte) {
		var l [4]byte
		binary.BigEndian.PutUint32(l[:], uint32(len(b)))
		h.Write(l[:])
		h.Write(b)
	}
	writeField([]byte(resourceID))
	writeField([]byte(operation))
	writeField(payload)
	return hex.EncodeToString(h.Sum(nil))
}

// Client talks to the CP confirmation endpoints. Safe for concurrent use.
type Client struct {
	baseURL string
	http    *http.Client
}

// NewClient builds a confirmation client. timeout bounds each CP call.
func NewClient(cpBaseURL string, timeout time.Duration) *Client {
	return &Client{baseURL: cpBaseURL, http: &http.Client{Timeout: timeout}}
}

// Check asks the CP whether the call needs owner confirmation.
func (c *Client) Check(ctx context.Context, resourceID, agentID, tool, argsHash string) (*CheckResult, error) {
	var out CheckResult
	if err := c.post(ctx, "/internal/v1/confirmation/check", map[string]string{
		"resource_id": resourceID, "agent_id": agentID, "tool": tool, "args_hash": argsHash,
	}, &out); err != nil {
		return nil, err
	}
	if out.Mode != "auto" && out.Mode != "pending" {
		return nil, fmt.Errorf("%w: unexpected check mode %q", ErrUnavailable, out.Mode)
	}
	if out.Mode == "pending" && out.ConfirmationID == "" {
		return nil, fmt.Errorf("%w: pending check without confirmation_id", ErrUnavailable)
	}
	return &out, nil
}

// Consume releases (or refuses) the gated call bound to argsHash.
func (c *Client) Consume(ctx context.Context, id, argsHash string) (*ConsumeResult, error) {
	var out ConsumeResult
	if err := c.post(ctx, "/internal/v1/confirmation/consume", map[string]string{
		"id": id, "args_hash": argsHash,
	}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) post(ctx context.Context, path string, body any, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("%w: encoding request: %v", ErrUnavailable, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: control plane unreachable: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: CP returned status %d", ErrUnavailable, resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("%w: reading response: %v", ErrUnavailable, err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%w: decoding response: %v", ErrUnavailable, err)
	}
	return nil
}
