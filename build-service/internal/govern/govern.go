// Package govern is the build-service validation core: every build
// request passes through the full allow/deny chain before a kaniko Job
// is created. All checks are default-deny and ordered cheapest-first:
//
//  1. repo URL parse + canonicalize (host/org/repo)
//  2. git host floor (SSRF guard for the ls-remote probe)
//  3. principal repo allowlist (glob, same semantics as the git driver)
//  4. live git-grant-scope check via the CP policy API (when wired)
//  5. revision format + existence (ls-remote for named refs)
//  6. dockerfile path safety (relative, no traversal)
//  7. destination parse + global ∧ principal prefix allowlist + tag rules
//  8. build-arg hygiene
//  9. quotas (concurrent via the Jobs API, hourly in-process)
//
// Denials are typed (Code) so the HTTP layer can map them without
// string-matching reasons.
package govern

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/rossbrigoli/skquad/build-service/internal/authn"
	"github.com/rossbrigoli/skquad/build-service/internal/cppolicy"
	"github.com/rossbrigoli/skquad/build-service/internal/gitcheck"
)

// Request is the validated-input shape of a build submission.
type Request struct {
	Repo        string            `json:"repo"`
	Revision    string            `json:"revision"`
	Dockerfile  string            `json:"dockerfile"`
	Destination string            `json:"destination"`
	BuildArgs   map[string]string `json:"build_args,omitempty"`
}

// Decision is the outcome of validation.
type Decision struct {
	Allowed  bool
	Code     string // machine-readable deny code (see codes below)
	Reason   string // human-readable, never contains secrets
	Repo     string // canonicalized repo (host/org/repo)
	RefMatch string // matched ref when revision was probed
}

// Deny codes.
const (
	CodeInvalidRepo         = "invalid_repo"
	CodeGitHostNotAllowed   = "git_host_not_allowed"
	CodeRepoNotAllowed      = "repo_not_allowed"
	CodeOutsideGrantScope   = "outside_grant_scope"
	CodePolicyUnavailable   = "policy_unavailable"
	CodeInvalidRevision     = "invalid_revision"
	CodeRevisionNotFound    = "revision_not_found"
	CodeRevisionCheckFailed = "revision_check_failed"
	CodeInvalidDockerfile   = "invalid_dockerfile"
	CodeInvalidDestination  = "invalid_destination"
	CodeTagForbidden        = "tag_forbidden"
	CodeDestinationDenied   = "destination_not_allowed"
	CodeInvalidBuildArgs    = "invalid_build_args"
	CodeQuotaConcurrent     = "quota_concurrent"
	CodeQuotaRate           = "quota_rate"
)

var (
	// revisionRefRE: branch/tag-ish names (kaniko must be able to check
	// them out; we additionally require existence via ls-remote).
	revisionRefRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,127}$`)
	// fullSHA40: a full commit hash (format-checked only; see Validate).
	fullSHA40 = regexp.MustCompile(`^[0-9a-f]{40}$`)
	// tagRE: docker tag rules, lowercase-only for predictability.
	tagRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)
	// destRepoRE: lowercase docker repository path segments.
	destRepoRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._/-]*$`)
	// buildArgKeyRE: docker/kaniko --build-arg key rules.
	buildArgKeyRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// ActiveCounter reports a principal's currently active (queued/running)
// build count. Implemented by the runner against the Jobs API so the
// quota is authoritative cluster-side.
type ActiveCounter interface {
	CountActive(ctx context.Context, principal string) (int, error)
}

// Deps wires the validator's collaborators. Policy and Prober may be nil
// only in dev mode; with nil Prober the revision-existence probe is
// skipped for named refs (format checks still apply) — the service logs
// a degraded-governance warning at startup in that case.
type Deps struct {
	Policy cppolicy.Lookup
	Prober gitcheck.Prober
	Active ActiveCounter

	GitHostsAllow             []string
	DestinationsAllow         []string
	MaxConcurrentPerPrincipal int
	MaxPerHourPerPrincipal    int
}

// Validator holds the deps plus the in-process hourly rate window.
type Validator struct {
	deps Deps

	mu      sync.Mutex
	submits map[string][]time.Time // principal -> submit times (hour window)
	nowFn   func() time.Time
}

// NewValidator builds a validator.
func NewValidator(deps Deps) *Validator {
	return &Validator{deps: deps, submits: map[string][]time.Time{}, nowFn: time.Now}
}

// Validate runs the full governance chain. principal must be non-nil
// (authentication happens in the HTTP layer).
func (v *Validator) Validate(ctx context.Context, principal *authn.Principal, req Request) Decision {
	if principal == nil {
		return deny(CodeRepoNotAllowed, "no principal")
	}

	// 1. Repo canonicalization.
	canon, err := CanonicalRepo(req.Repo)
	if err != nil {
		return deny(CodeInvalidRepo, err.Error())
	}
	d := Decision{Repo: canon}

	// 2. Git host floor (also bounds where the prober may dial).
	host := canon[:strings.Index(canon, "/")]
	if !hostAllowed(host, v.deps.GitHostsAllow) {
		return deny(CodeGitHostNotAllowed, fmt.Sprintf("git host %q is not in git_hosts_allow", host))
	}

	// 3. Principal repo allowlist (default-deny on empty).
	if len(principal.ReposAllow) == 0 {
		return deny(CodeRepoNotAllowed, "principal has no repos_allow")
	}
	if !anyGlob(principal.ReposAllow, canon) {
		return deny(CodeRepoNotAllowed, fmt.Sprintf("repo %q not in principal allowlist", canon))
	}

	// 4. Live git-grant-scope check (CP policy). Fail-closed.
	if v.deps.Policy != nil {
		if principal.AgentID == "" {
			return deny(CodeOutsideGrantScope, "principal has no agent_id for grant-scope check")
		}
		globs, perr := v.deps.Policy.GitReposAllow(ctx, principal.AgentID)
		if perr != nil {
			if errors.Is(perr, cppolicy.ErrPolicyUnavailable) {
				return deny(CodePolicyUnavailable, "control-plane policy unavailable (fail-closed)")
			}
			return deny(CodePolicyUnavailable, "policy check failed")
		}
		if !anyGlob(globs, canon) {
			return deny(CodeOutsideGrantScope, fmt.Sprintf("repo %q outside agent git grant scope", canon))
		}
	}

	// 5. Revision.
	if req.Revision == "" || !(revisionRefRE.MatchString(req.Revision) || fullSHA40.MatchString(req.Revision)) {
		return deny(CodeInvalidRevision, "revision must be a branch/tag name or a full 40-hex commit sha")
	}
	if v.deps.Prober != nil && !fullSHA40.MatchString(req.Revision) {
		httpsURL := "https://" + canon + ".git"
		refs, perr := v.deps.Prober.ListRefs(ctx, httpsURL)
		if perr != nil {
			return deny(CodeRevisionCheckFailed, "revision existence probe failed")
		}
		ok, ref := gitcheck.MatchRevision(refs, req.Revision)
		if !ok {
			return deny(CodeRevisionNotFound, fmt.Sprintf("revision %q not found in %s", req.Revision, canon))
		}
		d.RefMatch = ref
	}

	// 6. Dockerfile path: relative, no traversal, no leading slash.
	if req.Dockerfile == "" || strings.HasPrefix(req.Dockerfile, "/") || containsDotDot(req.Dockerfile) || strings.ContainsRune(req.Dockerfile, 0) {
		return deny(CodeInvalidDockerfile, "dockerfile must be a relative path without '..' segments")
	}

	// 7. Destination.
	destRepo, tag, err := splitDestination(req.Destination)
	if err != nil {
		return deny(CodeInvalidDestination, err.Error())
	}
	if tag == "latest" {
		return deny(CodeTagForbidden, "'latest' tag is forbidden for agent builds")
	}
	if !tagRE.MatchString(tag) {
		return deny(CodeTagForbidden, "tag must match ^[a-z0-9][a-z0-9._-]{0,127}$")
	}
	if !prefixAllowed(destRepo, v.deps.DestinationsAllow) {
		return deny(CodeDestinationDenied, "destination outside global allowlist")
	}
	if len(principal.DestinationsAllow) == 0 || !prefixAllowed(destRepo, principal.DestinationsAllow) {
		return deny(CodeDestinationDenied, "destination outside principal allowlist")
	}

	// 8. Build args hygiene.
	if err := validateBuildArgs(req.BuildArgs); err != nil {
		return deny(CodeInvalidBuildArgs, err.Error())
	}

	// 9. Quotas.
	if v.deps.Active != nil {
		n, qerr := v.deps.Active.CountActive(ctx, principal.ID)
		if qerr != nil {
			return deny(CodeQuotaConcurrent, "could not count active builds (fail-closed)")
		}
		if n >= v.deps.MaxConcurrentPerPrincipal {
			return deny(CodeQuotaConcurrent, fmt.Sprintf("%d active builds (max %d)", n, v.deps.MaxConcurrentPerPrincipal))
		}
	}
	if !v.allowRate(principal.ID) {
		return deny(CodeQuotaRate, fmt.Sprintf("hourly build quota exceeded (max %d)", v.deps.MaxPerHourPerPrincipal))
	}

	d.Allowed = true
	return d
}

// GitHosts exposes the configured host floor.
func (v *Validator) GitHosts() []string { return v.deps.GitHostsAllow }

// allowRate implements the sliding one-hour window per principal.
func (v *Validator) allowRate(principal string) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	now := v.nowFn()
	kept := v.submits[principal][:0]
	for _, t := range v.submits[principal] {
		if now.Sub(t) < time.Hour {
			kept = append(kept, t)
		}
	}
	if len(kept) >= v.deps.MaxPerHourPerPrincipal {
		v.submits[principal] = kept
		return false
	}
	v.submits[principal] = append(kept, now)
	return true
}

func deny(code, reason string) Decision {
	return Decision{Allowed: false, Code: code, Reason: reason}
}

// CanonicalRepo normalizes accepted git URL forms to "host/org/repo"
// (lowercase host, .git stripped). Accepted:
//
//	https://host/org/repo(.git)
//	http://host/org/repo(.git)          (allowed but discouraged)
//	ssh://git@host/org/repo(.git)
//	git@host:org/repo(.git)             (scp-style)
//
// Anything else (file://, weird schemes, missing path) is rejected.
func CanonicalRepo(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("empty repo")
	}
	// scp-style: git@host:path
	if !strings.Contains(raw, "://") {
		if at := strings.Index(raw, "@"); at >= 0 {
			rest := raw[at+1:]
			colon := strings.Index(rest, ":")
			if colon <= 0 {
				return "", fmt.Errorf("unrecognized repo form")
			}
			host := strings.ToLower(rest[:colon])
			path := strings.Trim(strings.TrimSuffix(rest[colon+1:], "/"), "/")
			return finish(host, path)
		}
		return "", fmt.Errorf("unrecognized repo form")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("unparseable repo url")
	}
	if u.Scheme != "https" && u.Scheme != "http" && u.Scheme != "ssh" {
		return "", fmt.Errorf("scheme %q not allowed", u.Scheme)
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "", fmt.Errorf("missing host")
	}
	if u.User != nil && u.Scheme != "ssh" {
		return "", fmt.Errorf("credentials in URL are not allowed")
	}
	path := strings.Trim(u.Path, "/")
	return finish(host, path)
}

func finish(host, path string) (string, error) {
	path = strings.TrimSuffix(path, ".git")
	if path == "" || strings.Count(path, "/") < 1 {
		return "", fmt.Errorf("repo path must be at least org/repo")
	}
	if strings.Contains(path, "..") {
		return "", fmt.Errorf("repo path must not contain '..'")
	}
	return host + "/" + path, nil
}

// splitDestination splits "registry/path/name:tag" into repo and tag.
// Requires an explicit tag. The registry is the first segment (must look
// like a host: contains '.' or ':' or is localhost).
func splitDestination(dest string) (string, string, error) {
	if dest == "" || strings.ContainsAny(dest, " \t") {
		return "", "", fmt.Errorf("destination must be non-empty without whitespace")
	}
	// Find the tag: last ':' that is AFTER the first '/' (a ':' in the
	// registry[:port] position is not a tag separator).
	firstSlash := strings.Index(dest, "/")
	if firstSlash < 0 {
		return "", "", fmt.Errorf("destination must include registry and repository path")
	}
	registry := dest[:firstSlash]
	if !strings.ContainsAny(registry, ".:") && registry != "localhost" {
		return "", "", fmt.Errorf("destination must start with a registry host")
	}
	lastColon := strings.LastIndex(dest, ":")
	if lastColon <= firstSlash {
		return "", "", fmt.Errorf("destination must include an explicit :tag")
	}
	repo := dest[:lastColon]
	tag := dest[lastColon+1:]
	if !destRepoRE.MatchString(repo) {
		return "", "", fmt.Errorf("destination repository path must be lowercase [a-z0-9._/-]")
	}
	return repo, tag, nil
}

func validateBuildArgs(args map[string]string) error {
	if len(args) > 20 {
		return fmt.Errorf("at most 20 build args")
	}
	for k, val := range args {
		if !buildArgKeyRE.MatchString(k) {
			return fmt.Errorf("invalid build arg key %q", k)
		}
		if len(val) > 512 || strings.ContainsAny(val, "\n\r\x00") {
			return fmt.Errorf("build arg %q value too long or contains newlines", k)
		}
	}
	return nil
}

func containsDotDot(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}

func hostAllowed(host string, allow []string) bool {
	for _, a := range allow {
		if strings.EqualFold(strings.TrimSpace(a), host) {
			return true
		}
	}
	return false
}

func prefixAllowed(destRepo string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(destRepo, p) {
			return true
		}
	}
	return false
}

// anyGlob applies the platform glob semantics (exact, trailing "**" =
// one-or-more trailing chars, "*" within a segment = any chars) — kept
// in sync with control-plane egresspolicy.globCovers and the tool-gateway
// git driver.
func anyGlob(patterns []string, candidate string) bool {
	for _, p := range patterns {
		if globMatch(p, candidate) {
			return true
		}
	}
	return false
}

func globMatch(pattern, candidate string) bool {
	pattern = strings.TrimSpace(pattern)
	if pattern == candidate {
		return true
	}
	if strings.HasSuffix(pattern, "**") {
		prefix := strings.TrimSuffix(pattern, "**")
		if !strings.HasPrefix(candidate, prefix) {
			return false
		}
		return strings.TrimPrefix(candidate, prefix) != ""
	}
	if strings.Contains(pattern, "*") {
		psegs := strings.Split(pattern, "/")
		csegs := strings.Split(candidate, "/")
		if len(psegs) != len(csegs) {
			return false
		}
		for i := range psegs {
			if !starSegmentMatch(psegs[i], csegs[i]) {
				return false
			}
		}
		return true
	}
	return false
}

func starSegmentMatch(pattern, seg string) bool {
	if !strings.Contains(pattern, "*") {
		return pattern == seg
	}
	parts := strings.SplitN(pattern, "*", 2)
	if !strings.HasPrefix(seg, parts[0]) || !strings.HasSuffix(seg, parts[1]) {
		return false
	}
	return len(seg) >= len(parts[0])+len(parts[1])
}
