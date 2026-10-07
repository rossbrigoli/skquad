# TG-8 — Risk Tiers, Approval Workflows, Owner Inbox, Grant-Change Linter (v1 spec)

Implements docs/tool-gateway.md §8–9 (ADR-0015 patterns). Card S-254 (TG-8).
This pins component interfaces so slices build in parallel.

## A. Grant linter — `control-plane/internal/grantlint/` (Go, standalone)

```go
type Finding struct {
    Code     string // "new_credentialed_reach" | "new_http_method" | "new_mcp_tool"
                  // | "metadata_path" | "cluster_internal_path" | "ceiling_widened"
    Severity string // "block" | "warn"
    Detail   string // human-readable, safe to show in Inbox UI
}

// LintChange compares before/after grant+ceiling snapshots. All determinism,
// no I/O. before==nil means a brand-new grant (treat empty baseline).
func LintChange(before, after *ChangeSnapshot) []Finding

type ChangeSnapshot struct {
    ResourceID   string
    RiskTier     string            // low|medium|high
    Hosts        []string          // reachable hosts (from config/ceiling)
    HTTPMethods  []string
    MCPTools     []string
    HasCredential bool            // credential attached/usable after this change
    NumericCaps  map[string]int  // e.g. max_pages, max_request_bytes, rate
    EgressClass  string          // public|internal
}
```

Rules (all `block` severity unless noted):
- **Absolute rule (fires regardless of before):** `metadata_path` — any host/IP
  matching 169.254.0.0/16, metadata.google.internal, *.metadata.goog,
  100.100.100.200 (Alibaba) in `after`.
- **Gained rules (need a baseline; for before==nil, gained = all of after):**
  `cluster_internal_path` — gained hosts in RFC1918/CGNAT/IPv6-ULA,
  *.svc(.cluster.local), *.cluster.local, *.internal; or EgressClass newly
  "internal". (Brand-new grants into private space ARE caught.)
- **Widening rules (only meaningful when before != nil; a brand-new grant has
  nothing to widen from — its review path is tier routing):**
  `new_credentialed_reach` (HasCredential && hosts gained), `new_http_method`
  (methods gained), `new_mcp_tool` (tools gained), `ceiling_widened` (any
  NumericCaps increased; added-cap-on-existing = `warn`).
- Pure shrink/narrow or identical snapshots → no findings. Metadata hosts are
  not double-reported under cluster_internal_path.

> Clarification (post-slice-A): the original draft listed the three
> cred/method/tool codes as plain gained rules, which would flag EVERY brand-new
> credentialed grant and break the BYO instant path. They are widening-only.
> Implemented + corpus-tested in internal/grantlint.
- No findings for identical snapshots.

Corpus tests: known-bad shapes (each code triggered), known-good (narrowing,
BYO self-grant of same shape), edge: rename host but same public reach (warn).

## B. Grant-request state machine + tier routing — CP

New table `grant_requests` (migration): id, resource_id, agent_id (nullable for
"grant to squad" requests), requester_user_id, tier, state, findings_json,
approved_by_owner_at, approved_by_admin_at, denied_reason, expiry, created/updated.

States: `pending_owner` → `pending_admin` → `approved` | `denied`
(BYO low/medium: requester == owner → immediate `approved` self-grant, no request row
unless linter findings; findings ALWAYS create a request row even for BYO).

Routing:
- tier low + BYO + no findings → auto-approve (materialize grant directly).
- tier medium + BYO + no findings → same self-grant path.
- tier medium shared → pending_owner.
- tier high (any) → pending_owner then pending_admin (admin co-sign = existing
  platform_admin role; owner attribute = resources.owner_user_id).
- Linter findings (block) on ANY path → force pending_owner with findings_json
  attached; auto-approval disabled for that request.

Notifications: on state entry, create Inbox message for the authority
(owner_user_id / platform_admins) via the EXISTING inbox/notification service
(grep inbox in control-plane — reuse, do not fork).

Endpoints (admin/owner-scoped):
- POST /api/v1/grant-requests  {resource_id, agent_id, requested_scope} → creates + routes
- POST /api/v1/grant-requests/{id}/approve-owner
- POST /api/v1/grant-requests/{id}/approve-admin  (platform_admin only)
- POST /api/v1/grant-requests/{id}/deny {reason}
- GET /api/v1/grant-requests?state=&mine=owner|admin

Approval materializes the actual grant row (existing grant objects) — requests
are workflow, grants remain the effective artifact (invariant 2).

## C. Confirmation gates + standing grants — CP + gateway touchpoints

- Gated operations (grant/ceiling flag `require_confirmation` for writes;
  default-on for high tier writes) produce a `pending_confirmation` Inbox
  message for the resource owner with EXACTLY 3 actions:
  - Deny → audit + agent notified, call fails `denied_by_owner`.
  - Approve (one-shot) → releases the single pending call (approval token
    bound to the exact call hash; TTL 15 min).
  - Approve This and Future → upsert **standing_grants** row:
    (resource_id, tool/operation, agent_scope, expires_at default +90d,
    created_by owner). Gateway/CP auto-approve path checks standing_grants
    FIRST and logs `matched_standing_grant=<id>` in the audit event.
- Standing grants visible via GET /api/v1/standing-grants (owner sees own,
  admin sees all) + DELETE to revoke (effective ≤30s via gateway cache TTL).

## D. Web UI (slice after B/C land)

- Inbox message card for confirmations: 3 buttons wired to the endpoints above,
  findings list rendered from findings_json (block=red, warn=amber).
- Standing grants panel in Grants UI: table (resource, tool, scope, expiry,
  created_by), revoke button.
- Grant-change diff view: before/after summary + findings when reviewing a
  linter-flagged request.

## Slice order & deps
- A (linter) — independent, spawn now.
- B (state machine) — spawn now against A's pinned LintChange signature.
- C (confirmation/standing) — after B merges in-branch.
- D (web) — after C.

## Non-goals (v1)
Formal verification (OpenShell-style prover) — linter is deterministic diff-based.
New RBAC roles — owner is attribute only.
