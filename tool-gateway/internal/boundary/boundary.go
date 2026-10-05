// Package boundary implements the fail-closed launch verification hook
// (design §5.2, OpenShell-adopted). The gateway readiness gate requires a
// positive fence-state confirmation before it serves traffic.
//
// TG-1 ships the interface plus two stubs:
//   - StaticVerifier: reports the fence as applied (placeholder so the
//     gateway is deployable; the REAL cluster verification — operator
//     proving the netpol is applied for the agent generation — lands with
//     TG-2/TG-9 wiring and should replace this via configuration).
//   - NoneVerifier: always reports not-applied (used to prove the gate
//     actually blocks).
package boundary

import (
	"context"
	"time"
)

// FenceState describes the verification result.
type FenceState struct {
	Applied   bool   `json:"applied"`
	Verifier  string `json:"verifier"`
	CheckedAt string `json:"checked_at"`
	Note      string `json:"note,omitempty"`
}

// Verifier checks whether the network boundary (netpol fence + gateway
// path) is confirmed for this gateway instance. Implementations must be
// safe for concurrent use.
type Verifier interface {
	Verify(ctx context.Context) FenceState
}

// StaticVerifier always reports applied — an explicit, configurable
// placeholder. TODO(TG-2/TG-9): replace with a real hook that reads the
// applied NetworkPolicy state (operator-written status configmap or the
// K8s API) and fails closed when the fence is missing.
type StaticVerifier struct{}

func (StaticVerifier) Verify(context.Context) FenceState {
	return FenceState{
		Applied:   true,
		Verifier:  "static",
		CheckedAt: time.Now().UTC().Format(time.RFC3339),
		Note:      "TG-1 placeholder: static confirmation (real netpol-applied hook is TODO)",
	}
}

// NoneVerifier never confirms the fence.
type NoneVerifier struct{}

func (NoneVerifier) Verify(context.Context) FenceState {
	return FenceState{
		Applied:   false,
		Verifier:  "none",
		CheckedAt: time.Now().UTC().Format(time.RFC3339),
		Note:      "boundary verification disabled; gateway will never become ready",
	}
}

// NewVerifier maps the config value to an implementation.
func NewVerifier(kind string) Verifier {
	switch kind {
	case "none":
		return NoneVerifier{}
	default:
		return StaticVerifier{}
	}
}
