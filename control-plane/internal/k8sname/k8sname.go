// Package k8sname builds RFC 1123 subdomain names for Kubernetes objects
// from human-readable skquad entities (S-261). The control plane is the
// only place that composes durable resource names; the operator consumes
// them verbatim from the CR spec.
package k8sname

import (
	"strings"
)

// MaxNameLen is the RFC 1123 subdomain length limit for Kubernetes object
// names (PVCs included).
const MaxNameLen = 253

// workspaceInfix joins the entity prefix with the trailing GUID component
// of a workspace PVC name.
const workspaceInfix = "-workspace-"

// SanitizePart lowercases raw and maps every rune outside [a-z0-9-] to
// '-', collapses runs of '-' into a single '-', and trims leading and
// trailing dashes. Unicode letters, spaces, underscores, dots-as-junk etc. all
// become separators. Returns "" when nothing alphanumeric survives.
func SanitizePart(raw string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(raw) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevDash = false
		case r == '-':
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		default:
			// Map anything else to a dash candidate; only emitted when
			// sandwiched between kept runes (trim handles the edges).
			if b.Len() > 0 && !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// OwnerSlug derives the owner segment from the owner's display name, then
// the email local-part, then the user id. Mirrors the S-156 ownerSlug
// precedence so PVC names line up with existing namespaces/deployments.
// Never returns "" when any input is non-empty.
func OwnerSlug(displayName, email, userID string) string {
	if s := SanitizePart(displayName); s != "" {
		return s
	}
	if at := strings.IndexByte(email, '@'); at > 0 {
		if s := SanitizePart(email[:at]); s != "" {
			return s
		}
	}
	if s := SanitizePart(userID); s != "" {
		return s
	}
	return "user"
}

// WorkspacePVCName composes the S-261 durable workspace PVC name:
//
//	<owner>-<squad>-<agent>-workspace-<guid>
//
// Every component is sanitized with SanitizePart; components that sanitize
// to "" are skipped (never producing "--"). The "-workspace-<guid>" tail
// is NEVER truncated: if the composed name exceeds MaxNameLen the entity
// prefix (owner/squad/agent) is cut instead, trimmed so no trailing dash
// remains. A missing/invalid GUID yields "" so callers skip the field
// rather than mint a non-unique name.
func WorkspacePVCName(owner, squad, agent, guid string) string {
	guid = SanitizePart(guid)
	if guid == "" {
		return ""
	}
	parts := make([]string, 0, 3)
	for _, p := range []string{SanitizePart(owner), SanitizePart(squad), SanitizePart(agent)} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	prefix := strings.Join(parts, "-")
	tail := workspaceInfix + guid
	if len(prefix)+len(tail) > MaxNameLen {
		budget := MaxNameLen - len(tail)
		if budget <= 0 {
			// GUID alone would blow the limit; refuse rather than
			// truncate the uniqueness-carrying tail.
			return ""
		}
		prefix = strings.TrimRight(prefix[:budget], "-")
	}
	if prefix == "" {
		// All entity components empty: keep the tail valid by dropping
		// its leading dash.
		return strings.TrimPrefix(tail, "-")
	}
	return prefix + tail
}
