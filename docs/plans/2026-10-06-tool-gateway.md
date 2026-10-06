# Plan — Tool Gateway & Governed Egress (web / rest / mcp / browser)

> **Status:** Draft v1 — FOR ROSS'S REVIEW. Nothing built until approved.
> Design: [`../tool-gateway.md`](../tool-gateway.md) ·
> ADRs: [0013](../adr/0013-tool-gateway.md)
> [0014](../adr/0014-browser-as-a-service.md)
> [0015](../adr/0015-three-party-grant-model.md)
> Estimated total: ~8–10 working days of focused build across phases
> (Phases 0–2 ≈ 6 days incl. TG-4b; Phase 3 ≈ 2–3 days; Phase 4 ≈ 2 days).
> Work packages within a phase parallelize where dirs are disjoint.

---

## Phase 0 — Foundations (TG-1, TG-2)

### TG-1 · Gateway skeleton + network topology
**Dir:** `tool-gateway/` (new Go module), `charts/`, `operator/`
**Build:**
- New `tool-gateway` binary: healthz, config, agent-credential auth
  middleware (reuse CP verifier lib), request pipeline (authn → policy lookup
  → driver dispatch → audit), echo driver for tests.
- Policy cache client: `GET /internal/v1/policy?agent=<id>` (CP) with ETag,
  30 s TTL, **fail-closed** on error.
- Audit emitter wired to existing audit pipeline; metering sample per request.
- Helm: Deployment + Service + HPA (RPS) in `skquad-system`; NetworkPolicies:
  - agent pods: +allow tool-gateway (extend existing default-deny)
  - gateway: deny all cluster-internal except CP/DNS/browser-svc; allow
    public `80,443`
  - CP: assert **no internet egress** (add explicit deny, verify)
- Kill switch: `SKQUAD_TOOL_GATEWAY_ENABLED` operator-level override.
- **Fail-closed launch verification (OpenShell-adopted):** operator proves
  the boundary before agent start — netpol applied + gateway path live;
  gateway refuses traffic for unconfirmed generations.

**Accept:**
- `curl` from agent pod to gateway echo works; to any other external host
  still blocked (bypass test).
- Gateway cannot reach Postgres/CP-DB ranges.
- Cache fail-closed: CP down > TTL → requests denied, not error-open.
- Fail-closed launch test: agent container does not start until boundary
  proof passes; injected failure (netpol missing) blocks start and alerts.
- Helm deploys via GitOps to k3s; rollback tested.

### TG-2 · Data model + registry extension + admin API
**Dir:** `control-plane/`
**Build:**
- Migrations per design §7: `endpoint_config`, `policy_ceiling`, `risk_tier`,
  `egress_class` on resources (+ `owner_user_id` — resource-owner attribute,
  **no new RBAC role**); `constraints` on grants;
  `mcp_tool_snapshots`; `grant_requests` (schema only, workflow in TG-8).
- Type widening: add `web`, `rest`, `mcp`, `git`; migrate `api` → `rest`
  (alias view).
- **Generation-bound credentials (OpenShell-adopted):** agent credential /
  JWT bound to `(agent_id, generation)`; every agent restart mints a new
  generation; prior-generation tokens rejected at gateway and CP.
- Per-type JSON schema validation for config/ceiling (unknown keys → 400).
- **No-escalation validator** (shared lib): `validate(grant.constraints ⊆
  resource.ceiling)` with structured violation output.
- Admin API: register/update typed endpoints (extends existing registry CRUD),
  `PATCH` audit-logged as today.
- Agent discovery: `GET /api/v1/agents/me/resources` includes typed
  resources (config sans secrets, ceiling-derived *effective* constraints only).
- Unit + property-based tests for the subset validator (fuzz constraints vs
  ceilings).

**Accept:**
- Register `web`/`rest`/`mcp` resources via admin API; ceiling validation
  proven by tests; escalation attempts rejected (property tests green).
- Existing registry consumers unaffected (alias works).

---

## Phase 1 — web + rest drivers (TG-3, TG-4)

### TG-3 · `web` driver (port of BT-6) + CP façade
**Dirs:** `tool-gateway/internal/drivers/web/`, `control-plane/` (façade only)
**Build:**
- Port the existing dial-time SSRF guard into a **shared Go lib**
  (`tool-gateway` + CP both consume; single source of truth).
- Web driver: GET, ≤3 redirects w/ per-hop recheck, maxBytes, timeout,
  html→text extraction (reuse runtime's extractor logic or CP's).
- Retarget CP façade `POST /api/v1/tools/web_fetch` → gateway; runtime
  unchanged (zero agent-visible change).
- System `web` resource seeded (enabled = mirrors current BT-6 config) +
  grant migration: agents with builtin web_fetch today get grant to system
  `web` resource automatically (migration script).
- Security test suite: metadata IP, RFC1918, CGNAT, IPv6 ULA, DNS-rebind
  (test server with rotating DNS), redirect-to-internal — all denied.

**Accept:** parity test — same fetch through old path vs gateway returns same
content; security suite green; existing S-122 chat tool loop unaffected.

### TG-4 · `rest` driver + BYO credential custody
**Dirs:** `tool-gateway/internal/drivers/rest/`, `control-plane/`, `web/`
**Build:**
- Auth kinds: bearer, api_key_header (custom header name), basic,
  oauth2_client_credentials (token cache + refresh-on-401). Secrets resolved
  per-call via CP internal credentials API (mTLS/internal-only).
- Method/path glob ACL (ceiling ∧ grant), header allowlist, request/response
  size caps, rate limit.
- Optional OpenAPI spec import at registration → typed operation list
  (`operationId`-level grant granularity). v1: store + expose; enforcement
  via globs still primary.
- Agent tool: `rest_call(resource_id, method, path, headers?, body?)` —
  registered in runtime builtin tool list *only when* agent holds a rest
  grant (fetch-at-wake pattern).
- Response scrubbing: strip `set-cookie`, `authorization` before returning.
- Web UI: BYO credential form (user pastes key / does OAuth dance), grant
  to own agents, view effective constraints.
- Tests: auth injection per kind, ACL enforcement, scrubbing, OAuth refresh
  (mock IdP), oversized payload rejection.

**Accept:** live demo — register GitHub API (BYO fine-grained PAT), agent
calls `GET /repos/x/y/pulls` through gateway; PAT never visible in pod env
(`kubectl exec` proof); audit event shows full semantic record.

### TG-4b · `git` driver (folded in — Ross Q6)
**Dirs:** `tool-gateway/internal/drivers/git/`, `control-plane/` (registration), `web/` (BYO form reuse)
**Build:**
- Git smart-HTTP proxy: `upload-pack` (clone/fetch) + optional `receive-pack`
  (push), streaming passthrough (no full-pack buffering), chunked-encoding safe.
- Token injection (fine-grained PAT / GitHub App installation token); agent
  remote = `https://gateway/git/<resource_id>/<repo>`.
- Repo allowlist from ceiling (`repos_allow`); read-only grants structurally
  deny `receive-pack`.
- Audit: clone/fetch/push + refs advanced (receive-pack report-status parse).
- Tests: clone allowed repo OK; denied repo refused; push without grant 403;
  large-pack streaming (memory-bounded); GitHub branch-protection interop
  demo (push to protected branch rejected server-side → PR flow).

**Accept:** agent clones skquad repo through gateway with zero visible token;
`git push` to a protected branch fails → PR opened via TG-4 rest/GitHub MCP.

---

## Phase 2 — MCP driver (TG-5)

**Dirs:** `tool-gateway/internal/drivers/mcp/`, `control-plane/`, `web/`
**Build:**
- MCP client: streamable-HTTP transport (SSE fallback optional); connect,
  initialize, `tools/list`, `tools/call`. stdio-only servers out of v1 scope
  (would need deferred "MCP host adapter"). First targets confirmed
  network-capable: GitHub MCP, SonarQube MCP
  ([docs.sonarsource.com](https://docs.sonarsource.com/sonarqube-mcp-server/setup/self-hosted)).
- Registration flow: connect → enumerate → store snapshot (hash + schemas) →
  admin picks `tools_allow` from enumerated list (can't allowlist what doesn't
  exist — prevents blind grants).
- Drift detection: hourly + on-demand re-enumerate; diff vs snapshot →
  `drift_detected` → new/changed tools **denied by default** + admin review
  event (audit + web UI badge).
- Enforcement: tool ∈ allowlist ∧ args validate vs snapshot schema ∧
  `max_args_bytes`; `requires_confirmation` → `pending_confirmation` result
  (execution gated on TG-8 approval; v2 stub: admin-only manual approve via
  API).
- Agent tools: `mcp_list(resource_id)`, `mcp_call(resource_id, tool, args)`;
  results wrapped untrusted (WP3).
- Test harness: run a reference MCP server (e.g., official test server) in
  kind/podman for integration tests incl. drift scenario.

**Accept:** register reference MCP server; allowlist 2 of 5 tools; agent
calls allowed tool OK, denied tool refused; mutate test server's tool list →
drift event → new tool denied until re-approved.

---

## Phase 3 — Browser-as-a-Service (TG-6)

**Dirs:** `browser-service/` (new), `tool-gateway/internal/drivers/browser/`,
`charts/`
**Build:**
- `browser-service`: Playwright (Chromium, pinned+digest image) exposing MCP
  server: `navigate/click/type/screenshot/extract/close_session`.
  - Session API: create (returns session token bound to
    agent/task/resource), destroy; context per session; idle timeout 10 min;
    orphan sweeper.
  - Navigation ceiling enforced in-service (deny-hosts, max pages) from
    gateway-supplied policy at create.
  - Hardening: read-only rootfs, seccomp, no secrets env.
  - Forward-proxy sidecar (shared SSRF guard lib) = its only egress path.
- Namespace `skquad-browser` + netpols per design §5.4; verified no path to
  `skquad-system` internals.
- Gateway `browser` driver: session lifecycle + MCP passthrough with caps
  (screenshot size, max session minutes, `max_sessions_per_agent: 1`).
- Screenshots → object storage (minio) task-scoped refs; agent gets preview
  + ref, not base64 dump into context.
- Tests: session binding (wrong-agent call rejected), isolation (browser
  cannot reach internal URLs incl. via JS fetch/WebSocket — verified in
  netpol + proxy), sweeper reaps orphans, extract/ screenshot caps.

**Accept (demo = mini-Maria):** agent granted browser resource browses a
JS-rendered site, screenshots a job listing, extracts text; session dies on
close; `kubectl exec` into browser pod shows no internal connectivity.

### TG-7 · Authenticated browser sessions (Phase 3.5 — optional, separate approval)
BYO login: vault-backed cookie/session injection into browser contexts;
risk tier high; confirmation gates default-on for submit actions. **Not in
v1 estimate** — design doc §5.3 placeholder; schedule after TG-8 so the
approval workflow exists first.

---

## Phase 4 — Governance & polish (TG-8, TG-9)

### TG-8 · Risk tiers + approval workflows + UI
**Dirs:** `control-plane/`, `web/`
**Build:**
- Grant-request state machine (`grant_requests`): pending_owner →
  pending_admin → approved/denied; notifications (existing messaging/notify).
- Tier routing per design §9; BYO self-grant path stays instant.
- Confirmation-gate execution: `pending_confirmation` (from TG-4/4b/5)
  surfaces in the **owner Inbox** as a message with three actions —
  **Deny** / **Approve** (one-shot) / **Approve This and Future** (creates a
  standing-grant object scoped to resource+tool, visible in grants UI,
  revocable, default expiry 90d pending final call). Approve → gateway
  executes (idempotency-keyed); standing matches log against the standing
  grant.
- Delegation ACL on shared resources (`owner_user_id` + co-owners on
  registry row — attribute-based, no new RBAC role).
- **Grant-change linter (prover-style, OpenShell-adopted; design §8 inv. 4):**
  deterministic pre-effect check of every grant/ceiling change — flags new
  credentialed reach, new HTTP methods, new MCP tools, any cloud-metadata /
  cluster-internal path, ceiling widening. Findings block auto-approval and
  surface the diff + findings in the owner Inbox. Unit-tested against a
  findings corpus (incl. known-bad grant shapes). Full formal verification
  (prover-grade) deferred as a possible later upgrade.
- UI: approval inbox, grant requests, drift review badges, tier display.

### TG-9 · Audit/metering dashboards, drills, docs, security pass
**Dirs:** `web/`, `docs/`, `tests/`
**Build:**
- Audit dashboard: per-agent/task/resource call views, decisions, tiers.
- Metering: per-resource cost/usage rollups (existing pipeline).
- **Revocation drill:** grant delete → measure effect ≤ 30 s live; resource
  disable → immediate.
- **Bypass test suite in CI:** agent pod direct-connect attempts (external,
  internal, metadata) all fail; gateway unauthenticated calls fail.
- Update `security-threat-model.md`, `implementation-status.md`,
  `resource-registry.md` (mark connector semantics delivered), READMEs.

---

## Phase 5 — SSH / SRE capability (TG-10) — after v1 core

### TG-10 · Terminal-as-a-Service (design §6.6)
**Dirs:** `terminal-service/` (new), `tool-gateway/internal/drivers/ssh/`, `charts/`, `web/`
**Depends on:** TG-8 (approval gates must exist before command-gated SSH ships —
no gates, no loaded gun) and TG-5 (driver framework).
**Build:**
- `terminal-service` in quarantine ns `skquad-terminal`: holds all SSH
  credentials; SSH CA integration (Vault SSH engine or `step-ca` — pick one,
  decision below) issuing host-principal certs TTL 15–60 min.
- Tools: `ssh_exec` (per-command audit-before-execute),
  `ssh_session_open/send/events/close` (TTY streaming).
  No scp/port-forward/raw tunnel in v1.
- Ceiling: `hosts_allow` globs; per-host command allow/deny patterns;
  deny-pattern → owner Inbox gate (§9) with standing-grant option.
- Session recording (ttyrec) → object storage; replay UI in web; live-view +
  kill for high-tier sessions.
- Netpol: terminal-service → registered hosts :22 only; no cluster-internal.
- Risk tier high: resource-owner + admin co-sign grant flow (reuses TG-8).
- Tests: exec allow/deny, gate round-trip with standing grant, cert TTL,
  recording replay fidelity, netpol isolation (terminal svc can't reach
  cluster internals), session kill.

**Accept:** SRE agent diagnoses a staging host via `ssh_exec` (read-only
commands flow; `rm -rf` triggers Inbox gate; session recording replayable);
agent pod env contains zero SSH material (`kubectl exec` proof).

**Build-vs-buy (Ross):** Teleport provides JIT access, recording, approval,
live-join out of the box behind the same tool-surface abstraction. Thin
self-build (Vault/step-ca + Go service) ≈ 2–3 days and stays in skquad's
grain; Teleport adds ops weight but buys recording/approval UX maturity.
Recommendation: **self-build thin first**, keep the tool surface neutral so
Teleport can replace the backend later if recording/approval demands grow.

---

## Dependency graph

```
TG-1 ──┬─→ TG-3 ─→ TG-4 ─┬─→ TG-5 ─→ TG-6 ─→ (TG-7 optional)
TG-2 ──┘                 └─→ TG-4b ──────────↗
TG-8 needs TG-4/5 gates ─→ TG-9 last
TG-10 (ssh) needs TG-8 + TG-5 ─ Phase 5, after v1 core
```
TG-1 ∥ TG-2 (disjoint dirs). TG-3 after both. TG-4 then TG-4b ∥ TG-5
(git driver reuses TG-4's credential custody; disjoint driver dirs).
TG-6 after TG-5 (browser = MCP). TG-8 can start during TG-6 (UI/workflow
independent of browser build).

## Sequencing rationale

- **web first** = zero-visible-risk port of what already works; proves the
  gateway spine + topology with no new capability risk.
- **rest second** = biggest new capability, BYO self-service, exercises
  credential custody fully before MCP's added complexity.
- **mcp third** = depends on mature driver framework; snapshot/drift is the
  subtle piece.
- **browser last** in v1 = biggest infra delta (Chromium image, quarantine
  zone), rides the proven MCP driver.
- **TG-7 (logged-in browsing) deliberately after the approval workflow
  exists** — never ship credential-bearing browsing without gates.

## Risks & mitigations (build-time)

| Risk | Mitigation |
|---|---|
| Shared SSRF-guard lib drift (CP vs gateway copies) | Single module imported by both; no fork |
| MCP spec churn (streamable HTTP evolving) | Pin spec rev per driver version; conformance test suite |
| Chromium image patch cadence | Pinned digest + scheduled bump job; CVE watch on Playwright |
| Gateway becomes SPOF | Stateless + HPA + PDB; CP façade degrades gracefully (tool returns retryable error, agent retries) |
| Cache TTL vs fail-closed tension (CP blips deny live traffic) | Stale-while-revalidate with 2× TTL grace on *existing* grants only; new grants hard-fail |
| Scope creep (git connector, more drivers) | Explicit non-goals list; new types = new plan |

## Definition of done (v1)

1. All Phase 0–3 acceptance demos pass on k3s via GitOps.
2. Bypass + SSRF CI suites green.
3. No-escalation property tests green.
4. Revocation drill ≤ 30 s.
5. Docs updated; ADRs flipped Proposed → Accepted with Ross's sign-off.
6. Zero secrets in any agent / browser / terminal pod env (verified by probe script).

## Resolved decisions (Ross, 2026-10-06 — see design §11)

1. CP internal API for gateway policy reads ✅
2. Streamable-HTTP MCP only in v1 (GitHub + SonarQube confirmed network-capable;
   stdio bridging deferred)
3. Playwright/Chromium ✅
4. First BYO: **GitHub, SonarQube**
5. Approval: **owner Inbox**, 3 buttons (Deny / Approve / Approve-This-and-Future
   → standing grant object, visible + revocable + expiring)
6. **git folded into gateway** (TG-4b)

Remaining non-blocking refinements: standing-grant default expiry (propose
90d); SonarQube registered as `mcp` (recommended) vs `rest` vs both;
MCP host adapter revisit trigger.

**OpenShell (NVIDIA) review — hybrid decision (Ross agreed, 2026-10-06):**
adopt prover-style grant linting (TG-8), fail-closed launch (TG-1),
generation-bound tokens (TG-2). Do **not** adopt OpenShell as runtime layer
at v0.1.x; re-evaluate at their 1.0 or when arbitrary-networked-CLI
workloads exceed our semantic-tool model. Full analysis: design §12.
