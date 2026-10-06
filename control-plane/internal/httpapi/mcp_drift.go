// TG-5 slice B2b (S-244-series): MCP upstream drift detection.
//
// The gateway computes a canonical hash of the upstream tool set at
// registration (slice B2a) and the CP stores it verbatim. Drift = a
// fresh enumerate through the gateway returns a DIFFERENT hash. This
// file implements the three drift surfaces:
//
//   - POST /api/v1/registry/mcp/{id}/re-enumerate  (admin, on-demand)
//   - hourly background scan (RunMCPDriftScanner, started from main.go
//     only when the CP→gateway enumerate path is configured)
//   - POST /api/v1/registry/mcp/{id}/approve-tools (admin re-approval)
//
// THE DRIFT-DENY MECHANISM (how a newly-added tool cannot silently
// widen reach):
//
//   - Added tools that the current tools_allow does NOT match are
//     already denied by the allowlist itself (default-deny at call
//     time in the gateway driver) — nothing to do beyond reporting them.
//   - The dangerous case is a WILDCARD entry (e.g. "*") that newly
//     matches an added tool. On drift, every tools_allow wildcard that
//     newly matches an added tool is EXPANDED into the concrete names
//     from the OLD snapshot it matched. The resource's effective
//     allowlist therefore becomes explicit and EXCLUDES the added tool
//     — the gateway denies it at call time with no gateway-side change.
//   - The added-but-newly-matched names are recorded in the per-resource
//     mcp_drift_pending set (migration 0048): the review marker that
//     keeps them denied-and-flagged until an admin calls approve-tools,
//     which validates a brand-new tools_allow against the CURRENT
//     snapshot (same unknown_tool/unmatched_wildcard rules as
//     registration), replaces the ceiling wholesale, and clears the
//     pending set. An admin restores a wildcard deliberately there.
//   - Pending markers MERGE across scans: a pending tool that is still
//     present upstream stays pending; one that disappeared upstream is
//     dropped from pending (nothing left to approve).
//
// Every drift detection emits an admin review audit event
// (registry.mcp.drift_detected) committed in the same transaction as
// the snapshot update via the S-86 pending-audit context.
//
// SECURITY: the resource bearer token is read from the managed Secret
// store and never logged, never echoed. The gateway hash is never
// recomputed here — comparison is against the stored verbatim hash.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rossbrigoli/skquad/control-plane/internal/config"
	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

// Audit actions for the drift surface (admin review events).
const (
	mcpAuditDriftDetected = "registry.mcp.drift_detected"
	mcpAuditToolsApproved = "registry.mcp.tools_approved"
)

// mcpToolNameset maps tool name → tool info.
func mcpToolIndex(tools []MCPToolInfo) map[string]MCPToolInfo {
	idx := make(map[string]MCPToolInfo, len(tools))
	for _, t := range tools {
		idx[t.Name] = t
	}
	return idx
}

// normalizeMCPJSONSchema canonicalizes a JSON-Schema blob for
// comparison: unmarshal + re-marshal sorts object keys, so whitespace
// and key-order differences never count as a schema change.
func normalizeMCPJSONSchema(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return string(raw)
	}
	return string(b)
}

// diffMCPTools compares the old and new enumerated tool sets. added =
// names only in new; removed = names only in old; changed = names in
// both whose description or (canonicalized) inputSchema differs. All
// lists are sorted for deterministic output.
func diffMCPTools(oldTools, newTools []MCPToolInfo) (added, removed, changed []string) {
	oldIdx := mcpToolIndex(oldTools)
	newIdx := mcpToolIndex(newTools)
	for name := range newIdx {
		if _, ok := oldIdx[name]; !ok {
			added = append(added, name)
		}
	}
	for name := range oldIdx {
		if _, ok := newIdx[name]; !ok {
			removed = append(removed, name)
		}
	}
	for name, ot := range oldIdx {
		nt, ok := newIdx[name]
		if !ok {
			continue
		}
		if ot.Description != nt.Description || normalizeMCPJSONSchema(ot.InputSchema) != normalizeMCPJSONSchema(nt.InputSchema) {
			changed = append(changed, name)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	sort.Strings(changed)
	return added, removed, changed
}

// mcpCeilingToolsAllow extracts the current tools_allow entries from a
// resource policy ceiling (nil when absent).
func mcpCeilingToolsAllow(ceiling json.RawMessage) []string {
	if isEmptyJSONObject(ceiling) {
		return nil
	}
	var ce struct {
		ToolsAllow []string `json:"tools_allow"`
	}
	if err := json.Unmarshal(ceiling, &ce); err != nil {
		return nil
	}
	return ce.ToolsAllow
}

// mcpAllowedByEntries reports whether tool name matches any allow
// entry (exact or wildcard) using the same semantics the gateway
// enforces at call time (mcpWildcardMatch mirrors drivers/mcp).
func mcpAllowedByEntries(entries []string, name string) bool {
	for _, e := range entries {
		if mcpWildcardMatch(e, name) {
			return true
		}
	}
	return false
}

// mcpExpandWildcardsForDrift rewrites the allow entries so no wildcard
// newly matches a pending tool: every wildcard entry that matches any
// pending name is replaced by the concrete oldNames it matched (sorted,
// deduped). Non-matching entries (exact names, wildcards that match no
// pending tool) pass through unchanged. Returns the new entries and
// whether any expansion happened.
func mcpExpandWildcardsForDrift(entries []string, oldNames []string, pending map[string]bool) ([]string, bool) {
	if len(pending) == 0 {
		return entries, false
	}
	out := make([]string, 0, len(entries))
	expanded := false
	for _, e := range entries {
		if !strings.Contains(e, "*") || !anyPendingMatch(e, pending) {
			out = append(out, e)
			continue
		}
		var matched []string
		for _, n := range oldNames {
			if mcpWildcardMatch(e, n) {
				matched = append(matched, n)
			}
		}
		sort.Strings(matched)
		out = append(out, matched...)
		expanded = true
	}
	return out, expanded
}

func anyPendingMatch(pattern string, pending map[string]bool) bool {
	for name := range pending {
		if mcpWildcardMatch(pattern, name) {
			return true
		}
	}
	return false
}

// mcpSetCeilingToolsAllow returns the ceiling JSON with tools_allow
// replaced and every other key preserved.
func mcpSetCeilingToolsAllow(ceiling json.RawMessage, allow []string) (json.RawMessage, error) {
	var m map[string]any
	if len(ceiling) == 0 || string(ceiling) == "null" {
		m = map[string]any{}
	} else if err := json.Unmarshal(ceiling, &m); err != nil {
		return nil, err
	}
	m["tools_allow"] = allow
	return json.Marshal(m)
}

// mcpDriftOutcome is the re-enumerate/drift-check result. Serialized
// directly as the re-enumerate endpoint response.
type mcpDriftOutcome struct {
	Changed        bool     `json:"changed"`
	Added          []string `json:"added"`
	Removed        []string `json:"removed"`
	ChangedTools   []string `json:"changed_tools"`
	Hash           string   `json:"hash"`
	DriftPending   []string `json:"drift_pending"`
	OldHash        string   `json:"old_hash,omitempty"`
	AllowRewritten bool     `json:"allow_rewritten,omitempty"`
}

// mcpStoredTokenFor is the package-level core of (*Server).mcpStoredToken
// so the background scanner (no *Server) can resolve the resource-level
// bearer from the managed Secret behind auth_ref. Never logs the token.
func mcpStoredTokenFor(ctx context.Context, secrets ResourceSecretStore, resource *domain.RegistryResource) (string, error) {
	if secrets == nil || resource.AuthRef == "" {
		return "", errors.New("no managed MCP credential")
	}
	name := resource.AuthRef[strings.LastIndex(resource.AuthRef, "/")+1:]
	fields, err := secrets.GetResourceSecret(ctx, name)
	if err != nil {
		return "", err
	}
	tok := strings.TrimSpace(fields["token"])
	if tok == "" {
		return "", errors.New("stored MCP credential has no token field")
	}
	return tok, nil
}

// checkMCPResourceDrift runs one fresh gateway enumeration for the
// resource, compares against the stored snapshot, persists the new
// snapshot + drift state (with the wildcard-expansion deny mechanism),
// and stages the admin review audit entry so it commits atomically with
// the update. actorType/actorID identify who triggered the check
// ("user"/admin id for the endpoint, "system"/"" for the scan).
//
// On unchanged hash only ToolsEnumeratedAt/DriftCheckedAt move; no
// audit event is staged (no review needed). The caller persists nothing
// itself — this function owns the UpdateResource call.
func checkMCPResourceDrift(
	ctx context.Context,
	store Store,
	enum MCPEnumerateClient,
	secrets ResourceSecretStore,
	resource *domain.RegistryResource,
	actorType, actorID string,
) (*mcpDriftOutcome, error) {
	if enum == nil {
		return nil, errors.New("mcp drift check: enumerate client not configured")
	}
	if resource == nil || resource.Type != domain.ResMCP {
		return nil, errors.New("mcp drift check: not an mcp resource")
	}
	token, err := mcpStoredTokenFor(ctx, secrets, resource)
	if err != nil {
		return nil, fmt.Errorf("mcp drift check: %w", err)
	}
	upstream := mcpURLFromConfig(resource.EndpointConfig)
	if upstream == "" {
		return nil, errors.New("mcp drift check: resource has no upstream url")
	}
	fresh, err := enum.Enumerate(ctx, resource.ID, upstream, token, mcpAllowPrivateFor(resource.EgressClass, resource.PolicyCeiling))
	if err != nil {
		return nil, fmt.Errorf("mcp drift check: %w", err)
	}
	now := time.Now().UTC()
	outcome := &mcpDriftOutcome{
		Hash:         fresh.Hash,
		OldHash:      resource.ToolsHash,
		Added:        []string{},
		Removed:      []string{},
		ChangedTools: []string{},
		DriftPending: []string{},
	}
	if fresh.Hash == resource.ToolsHash {
		resource.ToolsEnumeratedAt = &now
		resource.MCPDriftCheckedAt = &now
		if _, err := store.UpdateResource(ctx, resource); err != nil {
			return nil, fmt.Errorf("mcp drift check: persist: %w", err)
		}
		return outcome, nil
	}

	oldTools, _ := mcpSnapshotTools(resource.ToolsSnapshot)
	added, removed, changedTools := diffMCPTools(oldTools, fresh.Tools)
	outcome.Changed = true
	outcome.Added, outcome.Removed, outcome.ChangedTools = added, removed, changedTools

	// Pending = added tools the CURRENT allowlist would newly allow
	// (wildcard-matched), merged with prior pending names that are
	// still present upstream.
	allowEntries := mcpCeilingToolsAllow(resource.PolicyCeiling)
	newIdx := mcpToolIndex(fresh.Tools)
	pending := map[string]bool{}
	newlyMatched := map[string]bool{}
	for _, name := range added {
		if mcpAllowedByEntries(allowEntries, name) {
			pending[name] = true
			newlyMatched[name] = true
		}
	}
	for _, p := range decodeMCPDriftPending(resource.MCPDriftPending) {
		if _, stillThere := newIdx[p]; stillThere {
			pending[p] = true
		}
	}

	// Deny mechanism: expand every newly-matching wildcard into the
	// concrete OLD-snapshot names it matched, so the gateway's
	// effective allowlist excludes the added tools immediately.
	if len(newlyMatched) > 0 {
		oldNames := make([]string, 0, len(oldTools))
		for _, t := range oldTools {
			oldNames = append(oldNames, t.Name)
		}
		rewritten, didExpand := mcpExpandWildcardsForDrift(allowEntries, oldNames, newlyMatched)
		if didExpand {
			ceiling, err := mcpSetCeilingToolsAllow(resource.PolicyCeiling, rewritten)
			if err != nil {
				return nil, fmt.Errorf("mcp drift check: rewrite ceiling: %w", err)
			}
			resource.PolicyCeiling = ceiling
			outcome.AllowRewritten = true
		}
	}

	snapshot, err := json.Marshal(fresh.Tools)
	if err != nil {
		return nil, fmt.Errorf("mcp drift check: snapshot encode: %w", err)
	}
	resource.ToolsSnapshot = snapshot
	resource.ToolsHash = fresh.Hash
	resource.ToolsEnumeratedAt = &now
	resource.MCPDriftCheckedAt = &now
	pendingList := sortedKeys(pending)
	outcome.DriftPending = pendingList
	if len(pendingList) > 0 {
		pj, err := json.Marshal(pendingList)
		if err != nil {
			return nil, fmt.Errorf("mcp drift check: pending encode: %w", err)
		}
		resource.MCPDriftPending = pj
	} else {
		resource.MCPDriftPending = nil
	}

	metadata, _ := json.Marshal(map[string]any{
		"added":           outcome.Added,
		"removed":         outcome.Removed,
		"changed_tools":   outcome.ChangedTools,
		"drift_pending":   outcome.DriftPending,
		"old_hash":        outcome.OldHash,
		"new_hash":        outcome.Hash,
		"allow_rewritten": outcome.AllowRewritten,
		"resource_name":   resource.Name,
		"review_required": len(outcome.DriftPending) > 0,
		"trigger":         triggerFor(actorType),
	})
	auditCtx := storage.WithPendingAudit(ctx, storage.NewAuditEntry(
		actorType, actorID, mcpAuditDriftDetected, string(domain.ResMCP), resource.ID, "", metadata,
	))
	if _, err := store.UpdateResource(auditCtx, resource); err != nil {
		return nil, fmt.Errorf("mcp drift check: persist drift: %w", err)
	}
	return outcome, nil
}

func triggerFor(actorType string) string {
	if actorType == "system" {
		return "periodic_scan"
	}
	return "admin_re_enumerate"
}

func decodeMCPDriftPending(raw json.RawMessage) []string {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// attachMCPDrift computes the surfaced drift view on an mcp resource
// (no-op for other types / already-attached resources).
func attachMCPDrift(resource *domain.RegistryResource) {
	if resource == nil || resource.Type != domain.ResMCP {
		return
	}
	pending := decodeMCPDriftPending(resource.MCPDriftPending)
	if pending == nil {
		pending = []string{}
	}
	resource.Drift = &domain.MCPDriftState{
		Pending:       pending,
		LastCheckedAt: resource.MCPDriftCheckedAt,
		Drifted:       len(pending) > 0,
	}
}

// --- HTTP handlers ---------------------------------------------------------

// mcpDriftResource loads the target mcp resource for the drift
// endpoints, writing the error response and returning ok=false on any
// problem (non-mcp type, missing resource, unconfigured enumerate
// client).
func (s *Server) mcpDriftResource(w http.ResponseWriter, r *http.Request) (*domain.RegistryResource, bool) {
	typ, ok := registryTypeFromRequest(w, r)
	if !ok {
		return nil, false
	}
	if typ != domain.ResMCP {
		writeError(w, http.StatusNotFound, "not_found", "re-enumerate and approve-tools are only valid for mcp resources")
		return nil, false
	}
	if s.mcpEnumerate == nil {
		writeError(w, http.StatusServiceUnavailable, "gateway_unavailable",
			"MCP drift checks require the tool gateway (set SKQUAD_TOOL_GATEWAY_URL and SKQUAD_GATEWAY_INTERNAL_TOKEN)")
		return nil, false
	}
	resource, err := s.store.GetResource(r.Context(), typ, chi.URLParam(r, "resourceID"))
	if err != nil {
		writeStorageError(w, err)
		return nil, false
	}
	return resource, true
}

// reEnumerateMCP handles POST /registry/{registryType}/{resourceID}/re-enumerate
// (platform admin). Re-runs the gateway enumerate with the stored
// resource-level bearer and compares the fresh hash against the stored
// snapshot. Response: {changed, added, removed, changed_tools, hash,
// drift_pending}. A changed result has ALREADY been persisted with the
// deny mechanism applied (see package comment).
func (s *Server) reEnumerateMCP(w http.ResponseWriter, r *http.Request) {
	if !s.requirePlatformAdmin(w, r) {
		return
	}
	resource, ok := s.mcpDriftResource(w, r)
	if !ok {
		return
	}
	u := currentUser(r.Context())
	actorID := ""
	if u != nil {
		actorID = u.ID
	}
	outcome, err := checkMCPResourceDrift(r.Context(), s.store, s.mcpEnumerate, s.resourceSecrets, resource, "user", actorID)
	if err != nil {
		s.writeMCPDriftError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, outcome)
}

// writeMCPDriftError maps drift-check failures to a stable, secret-free
// taxonomy: gateway-level unavailability passes through as 503, other
// enumerate failures are 502 (upstream problem, not a bad admin
// request), credential/store problems are 503.
func (s *Server) writeMCPDriftError(w http.ResponseWriter, err error) {
	var ee *mcpEnumerateError
	if errors.As(err, &ee) {
		if ee.Status == http.StatusServiceUnavailable {
			writeError(w, http.StatusServiceUnavailable, ee.Code, "tool gateway unavailable for drift check")
			return
		}
		writeError(w, http.StatusBadGateway, ee.Code, "could not re-enumerate MCP upstream ("+ee.Code+")")
		return
	}
	if errors.Is(err, storage.ErrNotFound) {
		writeStorageError(w, err)
		return
	}
	writeError(w, http.StatusServiceUnavailable, "mcp_drift_check_unavailable", "could not run the MCP drift check")
}

// approveMCPTools handles POST /registry/{registryType}/{resourceID}/approve-tools
// (platform admin). Body: {"tools_allow": [...]}. The new allowlist is
// validated against the CURRENT snapshot with exactly the registration
// rules (unknown_tool / unmatched_wildcard rejected), replaces the
// ceiling's tools_allow wholesale (other ceiling keys preserved), and
// CLEARS the drift_pending set — this is how an admin accepts drifted
// tools and lifts the deny markers.
func (s *Server) approveMCPTools(w http.ResponseWriter, r *http.Request) {
	if !s.requirePlatformAdmin(w, r) {
		return
	}
	resource, ok := s.mcpDriftResource(w, r)
	if !ok {
		return
	}
	var req struct {
		ToolsAllow []string `json:"tools_allow"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	tools, ok := mcpSnapshotTools(resource.ToolsSnapshot)
	if !ok {
		writeError(w, http.StatusConflict, "no_snapshot", "resource has no MCP tool snapshot; run re-enumerate first")
		return
	}
	// Same validation as registration: build a temp ceiling carrying the
	// proposed list and run the shared checker against the live snapshot.
	probe, err := json.Marshal(map[string]any{"tools_allow": req.ToolsAllow})
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid tools_allow payload")
		return
	}
	if v := mcpAllowlistViolations(probe, tools); len(v) > 0 {
		writeViolations(w, "invalid_tool_allowlist", "tools_allow entries must exist in the enumerated MCP tool set", v)
		return
	}
	cleared := decodeMCPDriftPending(resource.MCPDriftPending)
	ceiling, err := mcpSetCeilingToolsAllow(resource.PolicyCeiling, req.ToolsAllow)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ceiling_encode_failed", "could not encode the updated policy ceiling")
		return
	}
	resource.PolicyCeiling = ceiling
	resource.MCPDriftPending = nil
	metadata, _ := json.Marshal(map[string]any{
		"tools_allow":     req.ToolsAllow,
		"cleared_pending": cleared,
		"resource_name":   resource.Name,
	})
	auditCtx := s.pendingUserAuditCtx(r, mcpAuditToolsApproved, string(domain.ResMCP), resource.ID, "", metadata)
	updated, err := s.store.UpdateResource(auditCtx, resource)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	attachMCPDrift(updated)
	writeJSON(w, http.StatusOK, updated)
}

// --- Periodic drift scan ---------------------------------------------------

// StartMCPDriftScanner wires and starts the periodic MCP drift scan
// from config (called once from main.go). It is gated on the full
// CP→gateway enumerate path being configured (gateway URL + internal
// token) AND a resource secret store being available — without either,
// drift checks cannot re-run enumeration and the scanner stays off.
// Returns true when the scanner goroutine was started. Unit tests never
// call this; they invoke ScanMCPDriftOnce directly, so `go test` spawns
// no background timers.
func StartMCPDriftScanner(ctx context.Context, store Store, cfg *config.Config) bool {
	if cfg == nil || strings.TrimSpace(cfg.ToolGatewayURL) == "" || strings.TrimSpace(cfg.GatewayInternalToken) == "" {
		log.Printf("mcp drift scanner not started: gateway enumerate path not configured")
		return false
	}
	enum, err := newGatewayMCPEnumerateClient(cfg.ToolGatewayURL, cfg.GatewayInternalToken)
	if err != nil {
		log.Printf("mcp drift scanner not started: %v", err)
		return false
	}
	secrets := resolveResourceSecrets(cfg, nil)
	if secrets == nil {
		log.Printf("mcp drift scanner not started: resource secret store unavailable")
		return false
	}
	go RunMCPDriftScanner(ctx, store, enum, secrets, cfg.MCPDriftScanInterval)
	slog.Info("started mcp drift scanner", "interval", cfg.MCPDriftScanInterval)
	return true
}

// RunMCPDriftScanner sweeps every interval until ctx is cancelled.
// Like the stuck-task scanner, the sweep is safe on every replica (each
// resource update is a full-row write; concurrent identical sweeps
// converge). Failures are logged and never fatal — the loop must
// survive a bad sweep.
func RunMCPDriftScanner(ctx context.Context, store Store, enum MCPEnumerateClient, secrets ResourceSecretStore, interval time.Duration) {
	if interval <= 0 {
		interval = time.Hour
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			drifted, err := ScanMCPDriftOnce(ctx, store, enum, secrets)
			if err != nil {
				slog.Warn("mcp drift scan failed", "error", err)
				continue
			}
			if drifted > 0 {
				slog.Info("mcp drift scan: review events emitted", "drifted_resources", drifted)
			}
		}
	}
}

// ScanMCPDriftOnce runs a single drift sweep over every ACTIVE mcp
// resource and returns how many drifted (review events emitted).
// Per-resource failures are logged and skipped so one broken upstream
// cannot stall the sweep. Exported for deterministic tests.
func ScanMCPDriftOnce(ctx context.Context, store Store, enum MCPEnumerateClient, secrets ResourceSecretStore) (int, error) {
	resources, err := store.ListResources(ctx, domain.ResMCP)
	if err != nil {
		return 0, err
	}
	drifted := 0
	for _, res := range resources {
		if res.Status != domain.ResourceActive {
			continue
		}
		outcome, err := checkMCPResourceDrift(ctx, store, enum, secrets, res, "system", "")
		if err != nil {
			slog.Warn("mcp drift check failed", "resource_id", res.ID, "error", err)
			continue
		}
		if outcome.Changed {
			drifted++
		}
	}
	return drifted, nil
}
