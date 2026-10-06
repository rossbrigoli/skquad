// Package auth mirrors the control-plane agent-credential verification
// (control-plane/internal/httpapi authenticateAgent + matchesAgentCredential)
// for the tool gateway. There is no shared cross-module verifier package yet,
// so the logic is duplicated here deliberately — keep the two in sync
// (TG-2 may extract a shared module).
//
// Contract:
//   - Agent presents: X-Skquad-Agent-ID: <agent_id> and Authorization: Bearer <token>
//   - The gateway verifies the token against the CredentialHash delivered by
//     the CP policy API: base64(sha256(token)) with RawStdEncoding,
//     compared in constant time.
//   - Unlike the CP, the gateway NEVER falls back to plaintext CredentialRef
//     comparison: it must not depend on secret material beyond the hash.
package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"

	"github.com/rossbrigoli/skquad/tool-gateway/internal/policy"
)

// AgentPrincipal is the resolved caller identity attached to the request context.
type AgentPrincipal struct {
	AgentID  string
	Snapshot *policy.Snapshot
}

type principalKey struct{}

// ErrUnauthenticated variants (mapped to 401 by the HTTP layer).
var (
	ErrMissingAgentID = errors.New("missing agent id")
	ErrMissingToken   = errors.New("missing bearer token")
	ErrBadCredential  = errors.New("missing or invalid agent credential")
)

// HashCredential mirrors control-plane hashCredential: sha256, base64
// (standard, unpadded) encoding. Keep byte-identical with the CP.
func HashCredential(token string) string {
	sum := sha256.Sum256([]byte(token))
	return base64.RawStdEncoding.EncodeToString(sum[:])
}

// MatchesCredential verifies token against the snapshot's CredentialHash in
// constant time. An empty hash never matches (fail-closed: a policy snapshot
// without credential material cannot authorize anything).
func MatchesCredential(token string, snap *policy.Snapshot) bool {
	if token == "" || snap == nil || snap.CredentialHash == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(HashCredential(token)), []byte(snap.CredentialHash)) == 1
}

// BearerToken extracts the bearer token from an Authorization header value.
func BearerToken(authorization string) string {
	const prefix = "Bearer "
	if len(authorization) <= len(prefix) || !strings.EqualFold(authorization[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(authorization[len(prefix):])
}

// Principal returns the authenticated agent principal from the context, if any.
func Principal(ctx context.Context) *AgentPrincipal {
	p, _ := ctx.Value(principalKey{}).(*AgentPrincipal)
	return p
}

// WithPrincipal stores the principal in a context (used by tests and the
// middleware).
func WithPrincipal(ctx context.Context, p *AgentPrincipal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// Middleware authenticates the agent using the policy lookup for credential
// material. It returns typed errors so the HTTP layer can map them:
// auth errors -> 401, policy errors -> 502 policy_unavailable.
func Middleware(ctx context.Context, lookup policy.Lookup, agentID, authorization string) (*AgentPrincipal, error) {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return nil, ErrMissingAgentID
	}
	token := BearerToken(authorization)
	if token == "" {
		return nil, ErrMissingToken
	}
	snap, err := lookup.Get(ctx, agentID)
	if err != nil {
		// Propagate policy errors (fail-closed handled by caller mapping).
		return nil, err
	}
	if snap.AgentID != agentID {
		// CP returned a snapshot for a different agent — treat as unavailable.
		return nil, policy.ErrPolicyUnavailable
	}
	if !MatchesCredential(token, snap) {
		return nil, ErrBadCredential
	}
	return &AgentPrincipal{AgentID: agentID, Snapshot: snap}, nil
}

// MiddlewareHandler is an http middleware variant kept for future routes;
// current pipeline calls Middleware directly so audit can classify errors.
func MiddlewareHandler(lookup policy.Lookup, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := Middleware(r.Context(), lookup, r.Header.Get("X-Skquad-Agent-ID"), r.Header.Get("Authorization"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
	})
}
