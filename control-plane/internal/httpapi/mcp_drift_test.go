// TG-5 slice B2b (S-244-series): MCP drift detection tests.
//
// Covers the re-enumerate endpoint (unchanged / added / removed /
// schema-changed), the wildcard newly-matches deny mechanism
// (drift_pending + wildcard expansion), approve-tools (valid clears
// drift; unknown tool rejected with the registration validation), the
// surfaced drift view on GET, and the periodic scan function invoked
// directly (never via the timer).
package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
	"github.com/stretchr/testify/require"
)

// driftHandler builds a handler with an explicit store + mutable stub
// so tests can flip the upstream tool set between calls.
func driftHandler(t *testing.T) (http.Handler, *storage.MemoryStore, *stubMCPEnumerate, *fakeResourceSecretStore) {
	t.Helper()
	store := storage.NewMemoryStore()
	secrets := newFakeResourceSecretStore()
	stub := &stubMCPEnumerate{tools: defaultMCPTools(), hash: fakeMCPHash}
	handler := newServer(testConfig(), store, serverDeps{
		resourceSecrets: secrets,
		mcpEnumerate:    stub,
	})
	return handler, store, stub, secrets
}

func reEnumPath(id string) string { return registryBase + "mcp/" + id + "/re-enumerate" }
func approvePath(id string) string {
	return registryBase + "mcp/" + id + "/approve-tools"
}

func getMCPResource(t *testing.T, handler http.Handler, id string) map[string]any {
	t.Helper()
	var got map[string]any
	doJSON(t, handler, http.MethodGet, registryBase+"mcp/"+id, nil, http.StatusOK, &got)
	return got
}

func auditByAction(t *testing.T, store *storage.MemoryStore, action string) []*domain.AuditEntry {
	t.Helper()
	entries, err := store.ListAudit(context.Background(), "", 500)
	require.NoError(t, err)
	var out []*domain.AuditEntry
	for _, e := range entries {
		if e.Action == action {
			out = append(out, e)
		}
	}
	return out
}

func ceilingAllow(t *testing.T, res map[string]any) []string {
	t.Helper()
	ce, ok := res["policy_ceiling"].(map[string]any)
	require.True(t, ok, "policy_ceiling present")
	allow, _ := ce["tools_allow"].([]any)
	out := make([]string, 0, len(allow))
	for _, a := range allow {
		out = append(out, a.(string))
	}
	return out
}

// (1) re-enumerate unchanged → changed:false, enumerated_at moves, no
// drift event, no pending.
func TestMCPDriftUnchanged(t *testing.T) {
	handler, store, stub, _ := driftHandler(t)
	res := createTypedResource(t, handler, "mcp", mcpBody("drift-unchanged", map[string]any{"tools_allow": []string{"a", "b"}}))
	before := getMCPResource(t, handler, res.ID)
	beforeEnum, err := time.Parse(time.RFC3339Nano, before["tools_enumerated_at"].(string))
	require.NoError(t, err)
	time.Sleep(2 * time.Millisecond)

	var out mcpDriftOutcome
	doJSON(t, handler, http.MethodPost, reEnumPath(res.ID), nil, http.StatusOK, &out)
	require.False(t, out.Changed)
	require.Equal(t, fakeMCPHash, out.Hash)
	require.Empty(t, out.DriftPending)
	require.Equal(t, 2, stub.calls, "re-enumerate hits the gateway once more")

	after := getMCPResource(t, handler, res.ID)
	afterEnum, err := time.Parse(time.RFC3339Nano, after["tools_enumerated_at"].(string))
	require.NoError(t, err)
	require.True(t, afterEnum.After(beforeEnum), "enumerated_at updated on unchanged re-check")

	drift := after["drift"].(map[string]any)
	require.Equal(t, false, drift["drifted"])
	require.Empty(t, drift["pending"])
	require.NotEmpty(t, drift["last_checked_at"])

	require.Empty(t, auditByAction(t, store, mcpAuditDriftDetected), "unchanged must not emit a review event")
}

// (2) upstream ADDS a tool with an EXPLICIT allowlist → changed:true,
// added reported, review event emitted; the new tool is not in the
// allowlist so it stays denied (no pending override needed).
func TestMCPDriftAddedExplicitAllowDenies(t *testing.T) {
	handler, store, stub, _ := driftHandler(t)
	res := createTypedResource(t, handler, "mcp", mcpBody("drift-add", map[string]any{"tools_allow": []string{"a", "b"}}))

	tools := append(defaultMCPTools(), MCPToolInfo{Name: "newtool", Description: "fresh", InputSchema: json.RawMessage(`{"type":"object"}`)})
	stub.tools = tools
	stub.hash = "2222222222222222222222222222222222222222222222222222222222222222"

	var out mcpDriftOutcome
	doJSON(t, handler, http.MethodPost, reEnumPath(res.ID), nil, http.StatusOK, &out)
	require.True(t, out.Changed)
	require.Equal(t, []string{"newtool"}, out.Added)
	require.Empty(t, out.Removed)
	require.Empty(t, out.ChangedTools)
	require.Empty(t, out.DriftPending, "explicit allowlist never matched the added tool — no pending override")
	require.False(t, out.AllowRewritten)

	// The stored allowlist still doesn't match newtool → gateway denies.
	allow := ceilingAllow(t, getMCPResource(t, handler, res.ID))
	require.Equal(t, []string{"a", "b"}, allow)
	require.False(t, mcpAllowedByEntries(allow, "newtool"))

	evts := auditByAction(t, store, mcpAuditDriftDetected)
	require.Len(t, evts, 1, "drift must emit exactly one admin review event")
	var meta map[string]any
	require.NoError(t, json.Unmarshal(evts[0].Metadata, &meta))
	require.Equal(t, []any{"newtool"}, meta["added"])
	require.Equal(t, false, meta["review_required"])
	require.Equal(t, "admin_re_enumerate", meta["trigger"])
	require.Equal(t, string(domain.ResMCP), evts[0].ResourceType)
	require.Equal(t, res.ID, evts[0].ResourceID)
}

// (3) WILDCARD allowlist newly matches an added tool → drift_pending
// includes it, the wildcard is expanded so the tool is DENIED at the
// gateway, and approve-tools clears the pending state.
func TestMCPDriftWildcardNewlyMatchedDeniedUntilApproved(t *testing.T) {
	handler, store, stub, _ := driftHandler(t)
	res := createTypedResource(t, handler, "mcp", mcpBody("drift-wild", map[string]any{"tools_allow": []string{"*"}}))

	tools := append(defaultMCPTools(), MCPToolInfo{Name: "dangerous_tool", Description: "rm -rf", InputSchema: json.RawMessage(`{"type":"object"}`)})
	stub.tools = tools
	stub.hash = "3333333333333333333333333333333333333333333333333333333333333333"

	var out mcpDriftOutcome
	doJSON(t, handler, http.MethodPost, reEnumPath(res.ID), nil, http.StatusOK, &out)
	require.True(t, out.Changed)
	require.Equal(t, []string{"dangerous_tool"}, out.Added)
	require.Equal(t, []string{"dangerous_tool"}, out.DriftPending)
	require.True(t, out.AllowRewritten)

	// Deny mechanism: the wildcard was expanded into the concrete OLD
	// snapshot names — dangerous_tool is NOT allowed by the effective
	// ceiling anymore even though the admin originally said "*".
	allow := ceilingAllow(t, getMCPResource(t, handler, res.ID))
	require.NotContains(t, allow, "*")
	require.ElementsMatch(t, []string{"a", "b", "list_pulls", "get_issue", "merge_pull"}, allow)
	require.False(t, mcpAllowedByEntries(allow, "dangerous_tool"), "newly-matched tool MUST be denied after drift")

	// GET surfaces the pending review state.
	drift := getMCPResource(t, handler, res.ID)["drift"].(map[string]any)
	require.Equal(t, true, drift["drifted"])
	require.Equal(t, []any{"dangerous_tool"}, drift["pending"])

	evts := auditByAction(t, store, mcpAuditDriftDetected)
	require.Len(t, evts, 1)
	var meta map[string]any
	require.NoError(t, json.Unmarshal(evts[0].Metadata, &meta))
	require.Equal(t, true, meta["review_required"])
	require.Equal(t, true, meta["allow_rewritten"])

	// Admin accepts the drifted tool set: approve-tools with "*" again
	// (validated against the CURRENT snapshot, which now includes
	// dangerous_tool) replaces the allowlist and clears the pending set.
	var updated domain.RegistryResource
	doJSON(t, handler, http.MethodPost, approvePath(res.ID), map[string]any{"tools_allow": []string{"*"}}, http.StatusOK, &updated)
	updatedGet := getMCPResource(t, handler, res.ID)
	require.Equal(t, []string{"*"}, ceilingAllow(t, updatedGet))
	updatedDrift := updatedGet["drift"].(map[string]any)
	require.Equal(t, false, updatedDrift["drifted"])
	require.Empty(t, updatedDrift["pending"])

	aprEvts := auditByAction(t, store, mcpAuditToolsApproved)
	require.Len(t, aprEvts, 1)
	var ameta map[string]any
	require.NoError(t, json.Unmarshal(aprEvts[0].Metadata, &ameta))
	require.Equal(t, []any{"dangerous_tool"}, ameta["cleared_pending"])
}

// (4) upstream REMOVES a tool → removed reported; a pending marker
// for a tool that vanished upstream is dropped from pending.
func TestMCPDriftRemoved(t *testing.T) {
	handler, _, stub, _ := driftHandler(t)
	createTypedResource(t, handler, "mcp", mcpBody("drift-rm", map[string]any{"tools_allow": []string{"a"}}))
	resID := lastMCPResourceID(t, handler, "drift-rm")

	// Upstream drops merge_pull.
	tools := []MCPToolInfo{
		{Name: "a", Description: "tool a", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "b", Description: "tool b", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "list_pulls", Description: "list pulls", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "get_issue", Description: "get issue", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}
	stub.tools = tools
	stub.hash = "4444444444444444444444444444444444444444444444444444444444444444"

	var out mcpDriftOutcome
	doJSON(t, handler, http.MethodPost, reEnumPath(resID), nil, http.StatusOK, &out)
	require.True(t, out.Changed)
	require.Equal(t, []string{"merge_pull"}, out.Removed)
	require.Empty(t, out.Added)
}

// lastMCPResourceID looks up a single mcp resource by exact name via
// the list endpoint.
func lastMCPResourceID(t *testing.T, handler http.Handler, name string) string {
	t.Helper()
	var list []map[string]any
	doJSON(t, handler, http.MethodGet, registryBase+"mcp", nil, http.StatusOK, &list)
	for _, r := range list {
		if r["name"] == name {
			return r["id"].(string)
		}
	}
	t.Fatalf("mcp resource %q not found", name)
	return ""
}

// (5) schema-only change → changed_tools reported, added/removed empty.
func TestMCPDriftSchemaOnlyChange(t *testing.T) {
	handler, _, stub, _ := driftHandler(t)
	res := createTypedResource(t, handler, "mcp", mcpBody("drift-schema", map[string]any{"tools_allow": []string{"get_issue"}}))

	tools := defaultMCPTools()
	for i := range tools {
		if tools[i].Name == "get_issue" {
			// Same fields, reordered + whitespace — must NOT count as a
			// change; add a genuinely new property so only this tool is
			// changed.
			tools[i].InputSchema = json.RawMessage(`{"type":"object","properties":{"number":{"type":"integer"},"extra":{"type":"string"}}}`)
		}
	}
	stub.tools = tools
	stub.hash = "6666666666666666666666666666666666666666666666666666666666666666"

	var out mcpDriftOutcome
	doJSON(t, handler, http.MethodPost, reEnumPath(res.ID), nil, http.StatusOK, &out)
	require.True(t, out.Changed)
	require.Empty(t, out.Added)
	require.Empty(t, out.Removed)
	require.Equal(t, []string{"get_issue"}, out.ChangedTools)
	require.Empty(t, out.DriftPending, "schema change on an allowed tool is reported, not pending-denied")
}

// (6)+(7) approve-tools validation: unknown tool rejected with the
// same unknown_tool rule as registration; valid list accepted.
func TestMCPApproveToolsValidation(t *testing.T) {
	handler, _, _, _ := driftHandler(t)
	res := createTypedResource(t, handler, "mcp", mcpBody("approve-valid", map[string]any{"tools_allow": []string{"a"}}))

	rec := doRawJSON(t, handler, http.MethodPost, approvePath(res.ID), `{"tools_allow":["a","drop_table"]}`)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "unknown_tool")
	require.Contains(t, rec.Body.String(), "drop_table")

	rec = doRawJSON(t, handler, http.MethodPost, approvePath(res.ID), `{"tools_allow":["zzz_*_nothing"]}`)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "unmatched_wildcard")

	rec = doRawJSON(t, handler, http.MethodPost, approvePath(res.ID), `{"tools_allow":["a","b","merge_pull"]}`)
	require.Equal(t, http.StatusOK, rec.Code)
}

// (8) periodic scan function: sweeps multiple mcp resources, emits a
// review event per drifted resource, skips non-active resources, and
// is repeatable (second sweep with a stable upstream drifts nothing).
func TestMCPDriftScanOnce(t *testing.T) {
	handler, store, stub, secrets := driftHandler(t)
	r1 := createTypedResource(t, handler, "mcp", mcpBody("scan-1", map[string]any{"tools_allow": []string{"a"}}))
	r2 := createTypedResource(t, handler, "mcp", mcpBody("scan-2", map[string]any{"tools_allow": []string{"*"}}))
	r3 := createTypedResource(t, handler, "mcp", mcpBody("scan-dead", map[string]any{"tools_allow": []string{"a"}}))
	// Deprecate r3 — the scan must skip it.
	doJSONNoBody(t, handler, http.MethodPost, registryBase+"mcp/"+r3.ID+"/deprecate", nil, http.StatusNoContent)

	// Stable upstream: nothing drifted.
	drifted, err := ScanMCPDriftOnce(context.Background(), store, stub, secrets)
	require.NoError(t, err)
	require.Equal(t, 0, drifted)
	require.Empty(t, auditByAction(t, store, mcpAuditDriftDetected))

	// Upstream changes: both active resources drift.
	tools := append(defaultMCPTools(), MCPToolInfo{Name: "sneaky_tool", Description: "sneaky", InputSchema: json.RawMessage(`{"type":"object"}`)})
	stub.tools = tools
	stub.hash = "7777777777777777777777777777777777777777777777777777777777777777"

	drifted, err = ScanMCPDriftOnce(context.Background(), store, stub, secrets)
	require.NoError(t, err)
	require.Equal(t, 2, drifted, "both ACTIVE resources drifted")

	evts := auditByAction(t, store, mcpAuditDriftDetected)
	require.Len(t, evts, 2)
	ids := map[string]bool{}
	for _, e := range evts {
		ids[e.ResourceID] = true
		var meta map[string]any
		require.NoError(t, json.Unmarshal(e.Metadata, &meta))
		require.Equal(t, "periodic_scan", meta["trigger"])
		require.Equal(t, "system", e.ActorType)
	}
	require.True(t, ids[r1.ID])
	require.True(t, ids[r2.ID])
	require.False(t, ids[r3.ID], "deprecated resource must not be scanned")

	// r2 had the wildcard → pending + deny; r1 explicit → no pending.
	r2Get := getMCPResource(t, handler, r2.ID)
	require.Equal(t, true, r2Get["drift"].(map[string]any)["drifted"])
	require.Equal(t, []any{"sneaky_tool"}, r2Get["drift"].(map[string]any)["pending"])
	require.False(t, mcpAllowedByEntries(ceilingAllow(t, r2Get), "sneaky_tool"))
	r1Get := getMCPResource(t, handler, r1.ID)
	require.Equal(t, false, r1Get["drift"].(map[string]any)["drifted"])

	// Repeat: snapshots are current, nothing drifts again.
	drifted, err = ScanMCPDriftOnce(context.Background(), store, stub, secrets)
	require.NoError(t, err)
	require.Equal(t, 0, drifted)
}

// re-enumerate on a non-mcp resource is 404; without the enumerate
// client the endpoint is 503.
func TestMCPDriftEndpointGating(t *testing.T) {
	handler, _, _, _ := driftHandler(t)
	web := createTypedResource(t, handler, "web", map[string]any{
		"name":            "not-mcp",
		"endpoint_config": map[string]any{"deny_domains": []string{"x.example"}, "rate_per_min": 60, "max_bytes": 1048576},
		"policy_ceiling":  map[string]any{"rate_per_min": 120, "max_bytes": 2097152},
	})
	rec := doRawJSON(t, handler, http.MethodPost, registryBase+"web/"+web.ID+"/re-enumerate", "")
	require.Equal(t, http.StatusNotFound, rec.Code)

	noGW := newServer(testConfig(), storage.NewMemoryStore(), serverDeps{resourceSecrets: newFakeResourceSecretStore()})
	rec = doRawJSON(t, noGW, http.MethodPost, reEnumPath("whatever"), "")
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

// StartMCPDriftScanner stays off without the enumerate path configured
// (the unit-test / unconfigured case) — no goroutine, no timer.
func TestMCPDriftScannerGating(t *testing.T) {
	require.False(t, StartMCPDriftScanner(context.Background(), storage.NewMemoryStore(), testConfig()))
}
