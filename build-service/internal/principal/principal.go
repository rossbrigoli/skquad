// Package principal models the build-service's caller identities.
//
// A principal is the per-agent identity the build service authenticates
// directly: the agent presents a bearer token (delivered unchanged through
// the tool-gateway `rest` driver's credential custody), and the service
// matches it against a SHA-256 hash recorded in the mounted principals
// file. The plaintext token never exists in this service — only hashes,
// mirroring the CP credential_hash convention (base64 raw-std of
// sha256(token)).
//
// Each principal carries its own governance allowlists:
//   - ReposAllow: git repos the principal may build (glob "org/repo",
//     same semantics as the git driver's repos_allow). This is a FLOOR
//     check only; the live git-grant-scope check against the control
//     plane is separate and stricter.
//   - DestinationsAllow: image destination prefixes the principal may
//     push to. Intersected with the global config allowlist at request
//     time (both must match).
package principal

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Principal is one authenticated caller.
type Principal struct {
	// ID is the stable principal identifier (e.g. "sherlock"). Used in
	// job labels, quota accounting and audit.
	ID string `json:"id"`
	// AgentID is the skquad agent UUID this principal maps to. Used to
	// fetch the agent's live git-grant policy from the control plane.
	// Empty means "no CP policy linkage" (static-only governance).
	AgentID string `json:"agent_id,omitempty"`
	// DisplayName is cosmetic.
	DisplayName string `json:"display_name,omitempty"`
	// TokenSHA256B64 is base64(raw-std) of sha256(token). Same encoding
	// as the CP policy snapshot's credential_hash.
	TokenSHA256B64 string `json:"token_sha256_b64"`
	// ReposAllow are glob patterns ("rossbrigoli/*") over "org/repo".
	ReposAllow []string `json:"repos_allow"`
	// DestinationsAllow are destination prefixes; every entry must end
	// in "/" so prefixes cannot be bypassed by sibling names.
	DestinationsAllow []string `json:"destinations_allow"`
	// Disabled deactivates the principal without deleting it (audit
	// trail stays intact). Disabled principals authenticate as 401.
	Disabled bool `json:"disabled,omitempty"`
}

// Store is the set of known principals plus authentication.
type Store struct {
	byID  map[string]*Principal
	byTok map[string]*Principal // key: token hash b64
}

// fileShape is the on-disk principals document.
type fileShape struct {
	Principals []Principal `json:"principals"`
}

// LoadFile reads and validates the principals JSON file.
func LoadFile(path string) (*Store, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read principals: %w", err)
	}
	return Parse(raw)
}

// Parse validates a principals document and builds the store.
func Parse(raw []byte) (*Store, error) {
	var f fileShape
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("parse principals: %w", err)
	}
	if len(f.Principals) == 0 {
		return nil, fmt.Errorf("principals: empty (default-deny requires at least one principal)")
	}
	s := &Store{byID: map[string]*Principal{}, byTok: map[string]*Principal{}}
	for i := range f.Principals {
		p := f.Principals[i]
		if err := validate(&p); err != nil {
			return nil, fmt.Errorf("principals[%d]: %w", i, err)
		}
		if _, dup := s.byID[p.ID]; dup {
			return nil, fmt.Errorf("principals: duplicate id %q", p.ID)
		}
		if _, dup := s.byTok[p.TokenSHA256B64]; dup {
			return nil, fmt.Errorf("principals: duplicate token hash for id %q", p.ID)
		}
		s.byID[p.ID] = &p
		s.byTok[p.TokenSHA256B64] = &p
	}
	return s, nil
}

func validate(p *Principal) error {
	if strings.TrimSpace(p.ID) == "" {
		return fmt.Errorf("id is required")
	}
	if len(p.TokenSHA256B64) == 0 {
		return fmt.Errorf("token_sha256_b64 is required")
	}
	if _, err := base64.StdEncoding.DecodeString(p.TokenSHA256B64); err != nil {
		return fmt.Errorf("token_sha256_b64 is not valid base64: %w", err)
	}
	if len(p.ReposAllow) == 0 {
		return fmt.Errorf("repos_allow must not be empty (default-deny)")
	}
	for _, r := range p.ReposAllow {
		if strings.TrimSpace(r) == "" || strings.HasPrefix(r, "/") {
			return fmt.Errorf("repos_allow entry %q invalid", r)
		}
	}
	if len(p.DestinationsAllow) == 0 {
		return fmt.Errorf("destinations_allow must not be empty (default-deny)")
	}
	for _, d := range p.DestinationsAllow {
		if strings.TrimSpace(d) == "" || !strings.HasSuffix(d, "/") {
			return fmt.Errorf("destinations_allow entry %q must end in '/'", d)
		}
	}
	return nil
}

// HashToken computes the stored form of a bearer token:
// base64(raw-std sha256(token)).
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// Authenticate matches a bearer token against principal hashes in
// constant time across all entries (no early-exit timing oracle on
// membership). Returns nil when unknown or disabled.
func (s *Store) Authenticate(token string) *Principal {
	if token == "" {
		return nil
	}
	want := HashToken(token)
	var found *Principal
	for h, p := range s.byTok {
		if subtle.ConstantTimeCompare([]byte(h), []byte(want)) == 1 && !p.Disabled {
			found = p
		}
	}
	return found
}

// ByID returns a principal by identifier (nil if unknown).
func (s *Store) ByID(id string) *Principal { return s.byID[id] }

// Count returns the number of principals.
func (s *Store) Count() int { return len(s.byID) }
