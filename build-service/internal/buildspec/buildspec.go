// Package buildspec parses and validates agent-submitted build requests.
//
// Everything that can be checked WITHOUT network access lives here, as
// pure functions over plain strings, so the deny paths are exhaustively
// unit-testable:
//
//   - repo: "host/org/repo" (or "host/org/repo.git"). Host must be on
//     the configured git_hosts_allow floor; the org/repo part is what
//     allowlist globs match against.
//   - revision: a git-safe named ref (branch/tag) or a full 40-hex
//     commit SHA. Ambiguous or unsafe ref forms are rejected before we
//     ever hand them to git or kaniko.
//   - dockerfile: a clean relative path inside the repo.
//   - destination: "registry/namespace/name:tag". Digest-pinned
//     destinations are rejected (the digest is an OUTPUT of the build,
//     never agent-chosen input). The prefix checks run against BOTH the
//     global config allowlist and the principal's allowlist at service
//     level; syntax is enforced here.
package buildspec

import (
	"fmt"
	"regexp"
	"strings"
)

// Request is the agent-facing build request payload.
type Request struct {
	Repo        string `json:"repo"`
	Revision    string `json:"revision"`
	Dockerfile  string `json:"dockerfile"`
	Destination string `json:"destination"`
}

// Violation is one validation failure (field + machine code + message).
type Violation struct {
	Field   string `json:"field"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ValidationError carries all violations for a request.
type ValidationError struct {
	Violations []Violation
}

func (e *ValidationError) Error() string {
	parts := make([]string, 0, len(e.Violations))
	for _, v := range e.Violations {
		parts = append(parts, v.Field+":"+v.Code)
	}
	return "invalid build request: " + strings.Join(parts, ", ")
}

var (
	fullSHARe   = regexp.MustCompile(`^[0-9a-f]{40}$`)
	gitShaRe    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	tagRe       = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.\-]{0,127}$`)
	nameSegRe   = regexp.MustCompile(`^[a-z0-9]+(?:[._\-][a-z0-9]+)*$`)
	badRefChars = regexp.MustCompile(`[\s~^:?*\[\\]|\x00-\x1f]`)
)

// Parse normalizes and validates a request. Returns the normalized spec
// plus a *ValidationError listing every problem found (never partial:
// all fields are checked even if an early one fails).
func Parse(r Request) (*Request, *ValidationError) {
	var v []Violation

	repo, rv := normalizeRepo(r.Repo)
	if rv != "" {
		v = append(v, Violation{Field: "repo", Code: rv, Message: fmt.Sprintf("repo %q is not a valid host/org/repo", r.Repo)})
	}

	rev, rv := normalizeRevision(r.Revision)
	if rv != "" {
		v = append(v, Violation{Field: "revision", Code: rv, Message: fmt.Sprintf("revision %q is not a safe git ref or full SHA", r.Revision)})
	}

	df, rv := normalizeDockerfile(r.Dockerfile)
	if rv != "" {
		v = append(v, Violation{Field: "dockerfile", Code: rv, Message: fmt.Sprintf("dockerfile %q is not a clean relative path", r.Dockerfile)})
	}

	dest, tag, rv := parseDestination(r.Destination)
	if rv != "" {
		v = append(v, Violation{Field: "destination", Code: rv, Message: fmt.Sprintf("destination %q is not registry/name:tag", r.Destination)})
	}

	if len(v) > 0 {
		return nil, &ValidationError{Violations: v}
	}
	return &Request{Repo: repo, Revision: rev, Dockerfile: df, Destination: dest + ":" + tag}, nil
}

// normalizeRepo strips scheme/.git/trailing slash and enforces
// host/org/repo shape. Returns "host/org/repo" (lowercased host).
func normalizeRepo(in string) (string, string) {
	s := strings.TrimSpace(in)
	if s == "" {
		return "", "empty"
	}
	for _, pref := range []string{"https://", "http://", "ssh://", "git://"} {
		s = strings.TrimPrefix(s, pref)
	}
	// Reject any residual userinfo (user:pass@host) — never tolerated.
	if i := strings.Index(s, "@"); i >= 0 {
		return "", "userinfo_forbidden"
	}
	s = strings.TrimSuffix(s, "/")
	s = strings.TrimSuffix(s, ".git")
	parts := strings.Split(s, "/")
	if len(parts) != 3 {
		return "", "not_host_org_repo"
	}
	host := strings.ToLower(parts[0])
	if host == "" || parts[1] == "" || parts[2] == "" {
		return "", "empty_segment"
	}
	if strings.ContainsAny(host, ":") {
		return "", "port_forbidden"
	}
	if strings.ContainsAny(host, " \t") || strings.ContainsAny(parts[1], " \t") || strings.ContainsAny(parts[2], " \t") {
		return "", "whitespace"
	}
	return host + "/" + parts[1] + "/" + parts[2], ""
}

// normalizeRevision validates a branch/tag name or full commit SHA.
func normalizeRevision(in string) (string, string) {
	s := strings.TrimSpace(in)
	if s == "" {
		return "", "empty"
	}
	if fullSHARe.MatchString(s) || gitShaRe.MatchString(s) {
		return s, ""
	}
	// Named ref rules (git-check-ref-format subset, fail-closed):
	if strings.ContainsAny(s, " \t") || badRefChars.MatchString(s) {
		return "", "unsafe_chars"
	}
	if strings.HasPrefix(s, "-") || strings.HasPrefix(s, "/") || strings.HasSuffix(s, "/") {
		return "", "bad_edges"
	}
	if strings.HasSuffix(s, ".") || strings.HasPrefix(s, ".") {
		return "", "bad_dot_edges"
	}
	if strings.Contains(s, "..") {
		return "", "double_dot"
	}
	if strings.Contains(s, "//") || strings.Contains(s, "@{") {
		return "", "bad_sequence"
	}
	// Reject refs that look like an abbreviated SHA (7-39 hex): too easy
	// to drift; require full SHA or a named ref.
	if len(s) >= 7 && len(s) <= 39 && isAllHex(s) {
		return "", "abbreviated_sha"
	}
	return s, ""
}

func isAllHex(s string) bool {
	for _, c := range strings.ToLower(s) {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// normalizeDockerfile enforces a clean relative path.
func normalizeDockerfile(in string) (string, string) {
	s := strings.TrimSpace(in)
	if s == "" {
		return "", "empty"
	}
	if strings.HasPrefix(s, "/") {
		return "", "absolute_path"
	}
	if strings.Contains(s, "\\") {
		return "", "backslash"
	}
	for _, seg := range strings.Split(s, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", "dot_segment"
		}
	}
	return s, ""
}

// parseDestination splits "registry/path...:tag" and validates each
// part. Returns (destWithoutTag, tag, code).
func parseDestination(in string) (string, string, string) {
	s := strings.TrimSpace(in)
	if s == "" {
		return "", "", "empty"
	}
	if strings.Contains(s, "@") {
		// Digest-pinned destination: the digest is build output, never
		// agent input. Reject.
		return "", "", "digest_forbidden"
	}
	colon := strings.LastIndex(s, ":")
	if colon < 0 {
		return "", "", "missing_tag"
	}
	dest := s[:colon]
	tag := s[colon+1:]
	// Guard against a port-looking colon: "reg:5000/x" has no tag;
	// LastIndex(":") after the last "/" is the tag separator.
	if strings.Contains(dest, "/") == false {
		return "", "", "missing_repository_path"
	}
	lastSlash := strings.LastIndex(dest, "/")
	if colon < lastSlash {
		// The colon was before the last slash → registry port, no tag.
		return "", "", "missing_tag"
	}
	if !tagRe.MatchString(tag) {
		return "", "", "bad_tag"
	}
	// Every path segment after the registry host must be a lowercase
	// repository-name component (Docker distribution rules, relaxed to
	// what registries actually accept for namespaced repos).
	segs := strings.Split(dest, "/")
	if len(segs) < 2 {
		return "", "", "missing_namespace"
	}
	for _, seg := range segs[1:] {
		if !nameSegRe.MatchString(seg) {
			return "", "", "bad_name_segment"
		}
	}
	return dest, tag, ""
}

// RepoAllowed reports whether "org/repo" matches any glob in patterns
// (same semantics as the git driver: '*' matches within a path segment
// only, so "org/*" never matches "otherorg/repo").
func RepoAllowed(patterns []string, orgRepo string) bool {
	for _, p := range patterns {
		if globMatch(p, orgRepo) {
			return true
		}
	}
	return false
}

// globMatch: '*' matches any run of characters EXCEPT '/'.
func globMatch(pattern, candidate string) bool {
	// Split pattern on '*' and match segments greedily with the
	// no-cross-slash rule.
	segs := strings.Split(pattern, "*")
	rest := candidate
	// First segment must be a prefix.
	if !strings.HasPrefix(rest, segs[0]) {
		return false
	}
	rest = rest[len(segs[0]):]
	for i := 1; i < len(segs); i++ {
		seg := segs[i]
		if seg == "" {
			// Trailing '*' (or '**' collapse): matches the remainder
			// only if no further constraints; '*' does not cross '/'.
			if i == len(segs)-1 {
				if strings.Contains(rest, "/") {
					return false
				}
				return true
			}
			continue
		}
		idx := strings.Index(rest, seg)
		if idx < 0 {
			return false
		}
		if strings.Contains(rest[:idx], "/") {
			return false
		}
		rest = rest[idx+len(seg):]
	}
	return rest == ""
}

// DestinationAllowed reports whether dest (without tag) starts with the
// given prefix. Prefixes are required (by config/principal validation)
// to end in '/', which makes prefix matching boundary-safe.
func DestinationAllowed(prefixes []string, destWithoutTag string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(destWithoutTag, p) {
			return true
		}
	}
	return false
}
