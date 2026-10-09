package httpapi

// TG-11 slice D (docs/tool-gateway.md §6.7): artifact drift ingest +
// batched owner digest.
//
// The terminal-service drift CronJob runs `ansible-playbook --check`
// (check mode, NO mutation) for every artifact-enabled ssh resource ×
// host_group at the pinned default-branch tip rev and POSTs one report
// per (resource, host_group) here. Hosts whose check run reported
// changed/failed/unreachable are OFF the approved state.
//
// Unlike the /internal/v1 policy/credentials routes (no app-layer auth
// by contract, NetworkPolicy-only), the drift surface is bearer-token
// authenticated with SKQUAD_DRIFT_INGEST_TOKEN: the drift runner is a
// scheduled batch job crossing the namespace boundary, and a token keeps
// the ingest honest even if the L3 fence is widened later. Unconfigured
// token → 503 (fail closed); wrong/missing token → 401.
//
// OWNER DIGEST (batched, never per-host spam): the first drifted report
// for a resource within a UTC calendar day files ONE owner inbox
// message; further drift reports for the same resource that day are
// recorded as rows but do not re-notify. in_sync reports never notify.
// The batch window is probed via CountDriftReports(resource, dayStart,
// driftedOnly) before insert — prior==0 means this report opens the day.
// The inbox has no append/update model, so "one message per resource per
// day" is implemented as "notify on the day's first drift only"; the
// full per-check history lives in drift_reports for the UI.

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

// driftGitRevRE accepts full 40-char lowercase SHAs (same contract as
// the apply lane; drift checks always run at the resolved tip).
var driftGitRevRE = regexp.MustCompile(`^[0-9a-f]{40}$`)

// requireDriftIngestToken guards the drift surface with the direction-
// scoped internal bearer token. Constant-time compare; never log the
// token, never echo it back.
func (s *Server) requireDriftIngestToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		configured := ""
		if s.cfg != nil {
			configured = s.cfg.DriftIngestToken
		}
		if configured == "" {
			writeError(w, http.StatusServiceUnavailable, "drift_ingest_unconfigured", "drift surface has no ingest token configured")
			return
		}
		provided := strings.TrimPrefix(strings.TrimSpace(r.Header.Get("Authorization")), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(provided), []byte(configured)) != 1 {
			writeError(w, http.StatusUnauthorized, "unauthorized", "invalid drift ingest token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// driftReportRequest is the wire shape POSTed by the drift runner.
// in_sync is deliberately NOT accepted from the client: it is derived
// from drifted_hosts so a buggy runner cannot mark drift as in-sync.
type driftReportRequest struct {
	ResourceID   string   `json:"resource_id"`
	HostGroup    string   `json:"host_group"`
	Playbook     string   `json:"playbook"`
	GitRev       string   `json:"git_rev"`
	DriftedHosts []string `json:"drifted_hosts"`
}

// handleDriftIngest stores one drift report and files the batched
// owner digest when this is the resource's first drift of the day.
func (s *Server) handleDriftIngest(w http.ResponseWriter, r *http.Request) {
	var req driftReportRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "request body must be valid JSON")
		return
	}
	req.ResourceID = strings.TrimSpace(req.ResourceID)
	req.HostGroup = strings.TrimSpace(req.HostGroup)
	req.Playbook = strings.TrimSpace(req.Playbook)
	req.GitRev = strings.TrimSpace(req.GitRev)
	if req.ResourceID == "" || req.HostGroup == "" || req.Playbook == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "resource_id, host_group and playbook are required")
		return
	}
	if !driftGitRevRE.MatchString(req.GitRev) {
		writeError(w, http.StatusBadRequest, "bad_request", "git_rev must be a full 40-char lowercase SHA")
		return
	}
	drifted := normalizeDriftHosts(req.DriftedHosts)

	res, err := s.store.GetResourceByID(r.Context(), req.ResourceID)
	if err != nil {
		writeStorageError(w, err)
		return
	}

	inSync := len(drifted) == 0
	digestDue := false
	if !inSync {
		prior, err := s.store.CountDriftReports(r.Context(), req.ResourceID, utcDayStart(time.Now()), true)
		if err != nil {
			writeStorageError(w, err)
			return
		}
		digestDue = prior == 0
	}

	report, err := s.store.CreateDriftReport(r.Context(), &domain.DriftReport{
		ResourceID:   req.ResourceID,
		HostGroup:    req.HostGroup,
		Playbook:     req.Playbook,
		GitRev:       req.GitRev,
		DriftedHosts: drifted,
		InSync:       inSync,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", "failed to store drift report")
		return
	}

	digestCreated := false
	if digestDue {
		if res.OwnerUserID == "" {
			log.Printf("drift ingest: resource %s has no owner_user_id; drift recorded without digest", req.ResourceID)
		} else if _, err := s.store.CreateInboxMessage(r.Context(), driftDigestMessage(res, req.HostGroup, req.Playbook, req.GitRev, drifted)); err != nil {
			// The report row is already committed — a digest failure must
			// never lose the drift record. Surface it in the response and
			// the log; the next day's window retries the notification.
			log.Printf("drift ingest: digest inbox message failed for resource %s: %v", req.ResourceID, err)
		} else {
			digestCreated = true
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":             report.ID,
		"in_sync":        report.InSync,
		"digest_created": digestCreated,
	})
}

// driftDigestMessage builds the one-per-resource-per-day owner inbox
// message. Subject is the one-liner; Body carries the host detail and
// the batching contract so the owner knows not to expect more today.
func driftDigestMessage(res *domain.RegistryResource, hostGroup, playbook, gitRev string, drifted []string) *domain.InboxMessage {
	subject := fmt.Sprintf("Drift detected: %s (%s) — %d host(s) off approved state", res.Name, hostGroup, len(drifted))
	body := fmt.Sprintf(
		"Drift check for resource %q found hosts off the approved state.\n\n"+
			"Host group: %s\nPlaybook: %s\nGit rev: %s\nDrifted hosts: %s\n\n"+
			"Detected by the periodic `ansible-playbook --check` lane (no mutation performed). "+
			"Re-apply the approved playbook revision to converge, or investigate the diff. "+
			"This is the daily digest for this resource — further drift today is recorded but will not re-notify.",
		res.Name, hostGroup, playbook, gitRev, strings.Join(drifted, ", "))
	return &domain.InboxMessage{
		UserID:  res.OwnerUserID,
		Kind:    domain.InboxActionRequired,
		Message: subject,
		Subject: subject,
		Body:    body,
	}
}

// utcDayStart returns 00:00 UTC of the given instant's calendar day —
// the drift digest batch window boundary.
func utcDayStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// normalizeDriftHosts trims, drops empties, dedupes and sorts the
// drifted host list for deterministic storage and digests.
func normalizeDriftHosts(hosts []string) []string {
	out := make([]string, 0, len(hosts))
	for _, h := range hosts {
		h = strings.TrimSpace(h)
		if h == "" || slices.Contains(out, h) {
			continue
		}
		out = append(out, h)
	}
	slices.Sort(out)
	return out
}

// --- artifact-resources read surface for the drift runner ---

// driftHostGroup is one admin-declared inventory group (ceiling
// host_groups entry) with at least one host.
type driftHostGroup struct {
	Name  string   `json:"name"`
	Hosts []string `json:"hosts"`
}

// driftArtifactResource is the runner-facing projection of an
// artifact-enabled ssh resource: everything needed to run a check-mode
// apply, and nothing else. EndpointConfig carries no secret material by
// the TG-2/TG-4 contract (credentials live behind AuthRef / the CA).
type driftArtifactResource struct {
	ResourceID    string           `json:"resource_id"`
	Name          string           `json:"name"`
	GitURL        string           `json:"git_url"`
	DefaultBranch string           `json:"default_branch"`
	PlaybooksPath string           `json:"playbooks_path"`
	DriftPlaybook string           `json:"drift_playbook"`
	SSHUser       string           `json:"ssh_user"`
	KnownHosts    string           `json:"known_hosts"`
	HostGroups    []driftHostGroup `json:"host_groups"`
}

// listDriftArtifactResources answers GET /internal/v1/artifact-resources:
// every ssh resource whose endpoint_config carries an `artifact` section
// AND whose ceiling declares at least one non-empty host_group. A
// resource with no checkable group is omitted — there is nothing to run.
func (s *Server) listDriftArtifactResources(w http.ResponseWriter, r *http.Request) {
	resources, err := s.store.ListResources(r.Context(), domain.ResSSH)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	out := make([]driftArtifactResource, 0, len(resources))
	for _, res := range resources {
		if entry, ok := driftArtifactEntry(res); ok {
			out = append(out, entry)
		}
	}
	slices.SortFunc(out, func(a, b driftArtifactResource) int {
		return strings.Compare(a.ResourceID, b.ResourceID)
	})
	writeJSON(w, http.StatusOK, map[string]any{"resources": out})
}

// driftArtifactEntry projects one registry resource onto the runner
// view, reporting ok=false when the resource is not drift-checkable.
func driftArtifactEntry(res *domain.RegistryResource) (driftArtifactResource, bool) {
	var entry driftArtifactResource
	if len(res.EndpointConfig) == 0 {
		return entry, false
	}
	var ec map[string]json.RawMessage
	if err := json.Unmarshal(res.EndpointConfig, &ec); err != nil {
		return entry, false
	}
	rawArtifact, ok := ec["artifact"]
	if !ok {
		return entry, false
	}
	var artifact struct {
		GitURL        string `json:"git_url"`
		DefaultBranch string `json:"default_branch"`
		PlaybooksPath string `json:"playbooks_path"`
		DriftPlaybook string `json:"drift_playbook"`
	}
	if err := json.Unmarshal(rawArtifact, &artifact); err != nil || strings.TrimSpace(artifact.GitURL) == "" {
		return entry, false
	}
	entry.ResourceID = res.ID
	entry.Name = res.Name
	entry.GitURL = artifact.GitURL
	entry.DefaultBranch = strings.TrimSpace(artifact.DefaultBranch)
	if entry.DefaultBranch == "" {
		entry.DefaultBranch = "main"
	}
	entry.PlaybooksPath = strings.TrimSpace(artifact.PlaybooksPath)
	if entry.PlaybooksPath == "" {
		entry.PlaybooksPath = "playbooks"
	}
	entry.DriftPlaybook = strings.TrimSpace(artifact.DriftPlaybook)
	entry.SSHUser = jsonString(ec["ssh_user"])
	entry.KnownHosts = jsonString(ec["known_hosts"])

	groups, ok := driftHostGroupsFromCeiling(res.PolicyCeiling)
	if !ok {
		return entry, false
	}
	entry.HostGroups = groups
	return entry, true
}

// driftHostGroupsFromCeiling extracts non-empty host_groups from an ssh
// policy ceiling, sorted by group name.
func driftHostGroupsFromCeiling(ceilingRaw json.RawMessage) ([]driftHostGroup, bool) {
	if len(ceilingRaw) == 0 {
		return nil, false
	}
	var ceiling map[string]json.RawMessage
	if err := json.Unmarshal(ceilingRaw, &ceiling); err != nil {
		return nil, false
	}
	raw, ok := ceiling["host_groups"]
	if !ok {
		return nil, false
	}
	var parsed map[string]struct {
		Hosts []string `json:"hosts"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, false
	}
	groups := make([]driftHostGroup, 0, len(parsed))
	for name, g := range parsed {
		if strings.TrimSpace(name) == "" || len(g.Hosts) == 0 {
			continue
		}
		groups = append(groups, driftHostGroup{Name: name, Hosts: g.Hosts})
	}
	if len(groups) == 0 {
		return nil, false
	}
	slices.SortFunc(groups, func(a, b driftHostGroup) int {
		return strings.Compare(a.Name, b.Name)
	})
	return groups, true
}

// jsonString decodes a JSON string value, returning "" when absent or
// not a string.
func jsonString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}
