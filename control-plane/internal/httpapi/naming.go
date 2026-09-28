package httpapi

// S-156: Kubernetes resource naming conventions.
//
// Squad namespace:   skquad-<owner>-<squad>
// Agent deployment:  skquad-<owner>-agent-<agent>
//
// Squad and agent names are IMMUTABLE after creation because the K8s
// namespace/deployment names are derived from them and K8s objects cannot be
// renamed in place. Uniqueness rules (enforced in the stores):
//   - a user may not create two squads with the same name
//   - a user may not create two agents with the same name (across all their squads)
//   - different users MAY reuse the same squad/agent names (owner segment differs)

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

// k8sNameMax is the DNS-label length limit enforced on Namespace names.
const k8sNameMax = 63

// slugify lowercases an arbitrary name and joins alphanumeric runs with "-".
func slugify(raw string) string {
	parts := strings.FieldsFunc(strings.ToLower(raw), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	})
	return strings.Join(parts, "-")
}

// fitK8sName trims a composed name to the DNS-label limit without leaving a
// trailing dash (which would be invalid).
func fitK8sName(name string) string {
	if len(name) <= k8sNameMax {
		return name
	}
	return strings.TrimRight(name[:k8sNameMax], "-")
}

// ownerSlug derives the owner segment of K8s resource names from the squad
// owner's user record: display name, then email local-part, then user-id
// prefix. Never returns "".
func ownerSlug(u *domain.User) string {
	if u == nil {
		return "user"
	}
	if s := slugify(u.Name); s != "" {
		return s
	}
	if at := strings.IndexByte(u.Email, '@'); at > 0 {
		if s := slugify(u.Email[:at]); s != "" {
			return s
		}
	}
	if s := slugify(u.ID); s != "" {
		return fitK8sName(s)
	}
	return "user"
}

// squadNamespaceFor builds the deterministic namespace for a squad.
func squadNamespaceFor(owner, squadName string) string {
	slug := slugify(squadName)
	if slug == "" {
		slug = "squad"
	}
	return fitK8sName("skquad-" + owner + "-" + slug)
}

// agentDeploymentNameFor builds the deterministic Deployment name for an
// agent. The name is unique per owner because agent names are unique per
// owner (S-156 rule); different owners land in different namespaces anyway.
func agentDeploymentNameFor(owner, agentName string) string {
	slug := slugify(agentName)
	if slug == "" {
		slug = "agent"
	}
	return fitK8sName("skquad-" + owner + "-agent-" + slug)
}

// uniqueSquadNamespace de-duplicates a namespace across users who share the
// same display name and squad name by appending -2, -3, ... The per-owner
// squad-name uniqueness check runs BEFORE this, so a hit here always means a
// DIFFERENT user already owns the namespace.
func (s *Server) uniqueSquadNamespace(ctx context.Context, base string) (string, error) {
	candidate := base
	for i := 2; i <= 999; i++ {
		taken, err := s.store.SquadNamespaceExists(ctx, candidate)
		if err != nil {
			return "", err
		}
		if !taken {
			return candidate, nil
		}
		suffix := fmt.Sprintf("-%d", i)
		cut := k8sNameMax - len(suffix)
		if len(candidate) > cut {
			candidate = strings.TrimRight(candidate[:cut], "-")
		}
		candidate = candidate + suffix
	}
	return "", errors.New("squad namespace space exhausted for " + base)
}
