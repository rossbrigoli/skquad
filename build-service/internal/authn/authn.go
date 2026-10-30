// Package authn implements the build-service caller identity model.
//
// Callers are build PRINCIPALS — one per agent (or human operator) that
// is allowed to submit builds. The gateway's `rest` driver injects a
// per-agent bearer credential for the registered build-service resource
// (TG-4c per-agent credential resolution), so the token itself carries
// the caller identity: the service maps sha256(token) to a principal.
//
// Token hashes use the same encoding as the control plane
// (base64 RawStdEncoding of sha256) so provisioning tooling can reuse
// hashCredential semantics. Raw tokens are NEVER stored or logged.
package authn

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
)

// Principal is one authorized build submitter.
type Principal struct {
	// ID is the stable principal name used in audit records and Job labels.
	ID string `json:"id"`
	// AgentID is the skquad agent this principal maps to (used for the
	// live git-grant-scope check against the CP policy API). Optional
	// only when the CP check is disabled.
	AgentID string `json:"agent_id"`
	// TokenSHA256 is base64(RawStd, sha256(bearer token)).
	TokenSHA256 string `json:"token_b64sha256"`
	// ReposAllow are canonical git repo globs ("github.com/org/*").
	// Empty = default-deny.
	ReposAllow []string `json:"repos_allow"`
	// DestinationsAllow are destination prefixes this principal may push
	// to. Empty = default-deny. Must be within the global allowlist at
	// request time (global ∧ principal).
	DestinationsAllow []string `json:"destinations_allow"`
	// Active gates the principal without deleting it.
	Active bool `json:"active"`
}

// Store is an immutable in-memory principal store.
type Store struct {
	principals []Principal
}

// LoadStore reads the principals JSON file ({"principals":[...]}).
func LoadStore(path string) (*Store, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read principals: %w", err)
	}
	var doc struct {
		Principals []Principal `json:"principals"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse principals: %w", err)
	}
	if len(doc.Principals) == 0 {
		return nil, fmt.Errorf("principals file contains no principals")
	}
	seen := map[string]bool{}
	for _, p := range doc.Principals {
		if p.ID == "" || p.TokenSHA256 == "" {
			return nil, fmt.Errorf("principals: every entry needs id and token_b64sha256")
		}
		if seen[p.ID] {
			return nil, fmt.Errorf("principals: duplicate id %q", p.ID)
		}
		seen[p.ID] = true
	}
	return &Store{principals: doc.Principals}, nil
}

// HashToken mirrors the control-plane credential hash: base64 RawStd
// sha256. Keep byte-identical with tool-gateway auth.HashCredential.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return base64.RawStdEncoding.EncodeToString(sum[:])
}

// Authenticate resolves a bearer token to an active principal, or nil.
// The comparison loop runs over ALL entries with constant-time compares
// so timing does not reveal which entry (if any) matched.
func (s *Store) Authenticate(token string) *Principal {
	if token == "" {
		return nil
	}
	h := HashToken(token)
	var match *Principal
	for i := range s.principals {
		p := &s.principals[i]
		if subtle.ConstantTimeCompare([]byte(h), []byte(p.TokenSHA256)) == 1 && p.Active {
			match = p
		}
	}
	return match
}

// Principals returns a copy of all configured principals (read-only view).
func (s *Store) Principals() []Principal {
	out := make([]Principal, len(s.principals))
	copy(out, s.principals)
	return out
}
