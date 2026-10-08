# skquad — Tool Gateway & Governed Egress Design

> **Status:** Draft v1 — FOR REVIEW (not built)
> **Author:** Sherlock, 2026-10-06
> **Supersedes/extends:** [resource-registry.md](resource-registry.md) (connector semantics),
> [ADR-0012](adr/0012-builtin-platform-tools.md) BT-6 (control-plane fetch proxy)
> **ADRs:** [0013 Tool Gateway](adr/0013-tool-gateway.md),
> [0014 Browser-as-a-Service](adr/0014-browser-as-a-service.md),
> [0015 Three-Party Grant Model](adr/0015-three-party-grant-model.md)

---

## 1. Context & current state

Today:

- Agent pods run **default-deny egress NetworkPolicy** (DNS + `skquad-system` only).
  Agents have **no web path**.
- `web_fetch` / `web_search` are proxied **through the control plane**
  (ADR-0012 BT-6) with a dial-time SSRF guard. Provider keys never reach the runtime.
- The **resource registry** exists: admin registers resources, squad owners grant
  them to agents, agents discover via `GET /api/v1/agents/me/resources`.
  Types today: `llm_provider`, `skill`, `tool`, `api`, `knowledge_base`,
  `project_workspace`. Connector semantics are immature ("generated network
  egress … remain follow-up work").

What's missing is a **general, governed egress plane**: arbitrary REST APIs, MCP
servers, and browser automation, each grantable per-agent with policy ceilings,
credential custody, and semantic audit.

## 2. Goals

1. Agents gain access to **web (GET), REST APIs, MCP servers, and a headless
   browser** — all as *registered, grantable resources*.
2. **Zero credentials in the sandbox.** No agent-visible tokens, keys, or CAs.
3. **No privilege escalation**: an owner can only grant what they can delegate,
   within the admin-set ceiling.
4. **Semantic audit**: every call logged as
   `(agent, task, resource, operation, args-digest, decision, latency, bytes)`.
5. **Fast revocation**: grant/resource deletion effective ≤ 30 s; global kill switch.
6. Reuse, not replace: extend the existing registry, audit, metering, and the
   BT-6 SSRF guard.

## 3. Non-goals (v1)

- Raw TCP/tunnel egress, SSH, git-over-ssh passthrough.
- TLS MITM / DLP inspection of arbitrary TLS.
- Authenticated browser sessions (cookie/login broker) — deferred to Phase 3.5.
- Replacing the LLM gateway (separate concern, unchanged).

## 4. Architecture overview

```
┌────────────────────────── agent sandbox (default-deny netpol) ──────────────────┐
│  agent pod: pinned image, NO secrets, NO direct egress except ▼                 │
└──────────────┬──────────────────────────────────────────────────────────────────┘
               │ only allowed: DNS, control-plane, tool-gateway (cluster-internal)
               ▼
┌───────────────────────────── skquad-system ────────────────────────────────────┐
│  Control Plane (brain)          Tool Gateway (muscle)                           │
│  - resource registry (extend)   - stateless, horizontally scalable              │
│  - grant engine + ceilings      - authn (agent credential)                      │
│  - credential vault (K8s        - drivers: web │ rest │ mcp │ browser-session   │
│    secret refs, never handed    - SSRF guard (shared lib, dial-time)            │
│    out)                         - per-request policy + rate limit               │
│  - approval workflows           - audit + metering sink                         │
│  - audit store                  - routes ONLY to registered+granted endpoints   │
│                                 └──────────────┬───────────────────────────────┘
└────────────────────────────────────────────────┼───────────────────────────────┘
                                                 ▼  (gateway = only component with general internet egress)
                       External: REST APIs · MCP servers · web sites
                                                 ▲
                       ┌───────────────────────────┴──────────┐
                       │ skquad-browser ns (quarantine zone)  │
                       │ Browser Service (Playwright MCP)     │
                       │ ephemeral contexts; egress: web only │
                       │ (no internal ranges, guard at proxy) │
                       └──────────────────────────────────────┘
```

**Capability inversion:** agents hold no network capability primitive. Their
entire network world is the set of resources granted to them, each addressed by
`resource_id` through the gateway. "Broad web read" is an explicitly registered,
explicitly granted object — never an implicit hole.

## 5. Components

### 5.1 Control Plane (extensions)

1. **Typed resource definitions.** Extend registry rows with a typed config +
   **policy ceiling** (see §7). New types: `web`, `rest`, `mcp`
   (`browser` is an `mcp` resource bound to the managed Browser Service).
   The existing `api` type migrates to `rest` (alias kept for compatibility).
2. **Grant engine** with the no-escalation rule (§8) and risk-tier routing (§9).
3. **Credential vault.** Unchanged pattern: `auth_ref` → K8s Secret (or external
   secrets manager). Resolved only by the gateway at call time via internal API;
   raw secrets never cross the CP boundary to clients (already enforced today).
4. **Approval workflows** (Phase 4): grant request → resource-owner approval →
   admin co-sign (tier-dependent). Modeled as audit-event-backed state machine.
5. **Policy read API** for the gateway:
   `GET /internal/v1/policy?agent=<id>` → grants + ceilings + resource defs,
   ETag-cached. Gateway never reads the vault directly; it fetches
   *credential material* via a dedicated, mTLS/internal-only
   `POST /internal/v1/credentials/{ref}` with its own service identity.

### 5.2 Tool Gateway (new component)

- **Deployment:** separate Deployment + Service in `skquad-system`; separate
  binary in the skquad repo (`tool-gateway/`, Go). Stateless; HPA on RPS.
- **Authn:** existing agent credentials (ADR-0007). Same verifier as CP.
  No anonymous access.
- **Route table:** cached view of registry+grants (TTL 30 s, ETag). Cache miss
  → fail-closed (deny), never fail-open.
- **Drivers** (§6) are the only code that performs I/O to external endpoints.
- **Egress posture:** the *only* component with general internet egress.
  Hardened: minimal image, no shell/exec tools, read-only rootfs, its own
  NetworkPolicy that **blocks all cluster-internal ranges** except CP, DNS,
  and the Browser Service. Registered `internal` endpoints require
  `egressClass: internal` (admin-only registration) and are routed via a
  separate, logged path.
- **Rate limiting:** per (agent, resource) token bucket from grant constraints;
  global per-resource cap to protect shared upstreams.
- **Audit/metering:** every request emits an audit event (append-only store,
  existing audit pipeline) and metering sample (existing metering pipeline).
- **Fail-closed launch verification (adopted from OpenShell, 2026-10-06):**
  an agent workload must not start until the boundary is *proven*: the
  operator verifies the netpol fence is applied and the gateway path is
  live before the agent container starts; the gateway refuses traffic for
  unconfirmed generations. (OpenShell holds launch until the supervisor
  confirms the boundary — same property, adapted to our netpol topology.)
- **Generation-bound tokens (adopted from OpenShell):** each agent run is a
  *generation*; agent credentials/JWTs are bound to
  `(agent_id, generation)` and are minted fresh on every restart — old
  generation tokens stop working. Combined with short TTL, revocation is:
  kill the generation → everything issued in it dies at once.

**Why a separate component (full rationale in ADR-0013):** the gateway is the
blast-radius boundary — it terminates connections to hostile/arbitrary external
endpoints on behalf of semi-trusted agents. The CP holds identity, grants, and
vault pointers and must not share a process boundary with that traffic. A
compromised gateway holds *no* long-lived secrets (it fetches per-call, and its
own vault-read scope is auditable); a compromised CP cannot emit network traffic
to external endpoints at all.

### 5.3 Browser Service (new, quarantine zone)

Full rationale in ADR-0014. Summary:

- Namespace `skquad-browser`; Playwright-based **MCP server** exposing
  `browser.navigate`, `browser.click`, `browser.type`, `browser.screenshot`,
  `browser.extract`, `browser.close_session`.
- **Ephemeral contexts:** one browser context per (agent, task), destroyed on
  session end or idle timeout (10 min). No persistent profiles in v1.
- **Egress quarantine:** its own default-deny netpol; egress only to public web
  via a small forward-proxy sidecar applying the same SSRF/IP-pin guard. No
  route to any cluster-internal address, including `skquad-system`.
- **Session brokering:** the gateway's `browser` driver creates/binds sessions:
  `session ↔ (agent_id, task_id, resource_id)`; all browser MCP calls are
  checked against the binding. Orphan sweeper reaps leaked contexts.
- v1 = **unauthenticated browsing only**. Authenticated flows (BYO login,
  cookie injection from vault) = Phase 3.5 (TG-7), because a browser holding
  a logged-in session to a personal account is a materially higher risk tier.
- **Multi-tenant concurrency (Ross Q1, 2026-10-06):** agents never share a
  browser *process*. The service maintains a **warm pool of browser
  instances**; each session binds one instance exclusively (one context per
  instance) → process-level isolation between agents. A crash/exploit kills
  only that agent's session (retryable error), not co-residents. Capacity:
  headless Chromium ≈ 150–300 MB/instance → ~20 concurrent sessions per
  8 GB worker. Enforcement: `max_sessions_per_agent: 1`, global instance cap,
  queue-with-timeout ("browser busy") instead of OOM. Pod-per-session
  (gVisor-grade) isolation is a future option if the threat model escalates.

### 5.4 NetworkPolicy topology (enforcement of *topology*, not semantics)

| Workload | Egress allow |
|---|---|
| Agent pods | DNS, control-plane, tool-gateway |
| Tool gateway | DNS, CP (internal API), Browser Service, public `*:80,443` (minus blocked ranges at app layer) |
| Browser Service | DNS, public `*:80,443` only |
| CP | DNS, Postgres, K8s API — **no internet egress** |

NetworkPolicy cannot express per-URL/per-tool policy — that is the gateway's
job. The netpol exists solely to make the gateway unbypassable and to keep the
CP and browser zone mutually non-reachable beyond defined paths.

## 6. Drivers

### 6.1 `web` driver

Semantics = today's BT-6 `web_fetch`, relocated:

- GET only; ≤ 3 redirects, SSRF re-check each hop; dial-time IP pin
  (defeats DNS rebinding); block loopback/RFC1918/CGNAT/link-local/metadata/
  IPv6 ULA unless `allowPrivateNetwork` (which requires an `internal`-class
  registration).
- html → readable text, `maxBytes` truncation, timeout.
- Migration: CP keeps `POST /api/v1/tools/web_fetch` as a thin façade that
  forwards to the gateway during rollout; runtime unchanged. After parity,
  façade becomes a redirect/deprecation notice.
- Registration: platform admin registers the system `web` endpoint with a
  ceiling (rate, maxBytes, allowPrivateNetwork=false). Granting it to an
  agent reproduces today's behavior — explicitly, with an audit trail.
- **Denylist model (Ross Q2, 2026-10-06):** because `web_search` results can
  point at any URL, URL-level control inside the broad `web` resource is a
  **denylist** (domains + IPs), not a whitelist — the open internet cannot be
  pre-whitelisted. Layering: capability-level whitelist (registered/granted
  resource) → URL-level denylist within it. Policy fields:
  `deny_domains: [...]`, `deny_cidrs: [...]`, set at the system floor (admin)
  and per-grant (additive only — a grant can tighten, never loosen, the floor).
  Design note: the denylist is a **content-policy knob, not a security
  boundary** (denylists always lag; bad actors sit on innocent domains/CDNs).
  The actual safety of broad web GET comes from its properties: GET-only,
  size/time caps, the non-negotiable dial-time SSRF floor, and **no credential
  injection ever** on the web driver (a BYO cookie attached to a broad-web
  fetch + redirect = credential leak to arbitrary sites).

### 6.2 `rest` driver

Registration (admin):

```yaml
type: rest
base_url: https://api.example.com/v2
auth:
  kind: bearer | api_key_header | basic | oauth2_client_credentials
  secret_ref: skquad-secret://example-api-auth
ceiling:
  methods: [GET, POST]
  path_allow: ["/issues/**", "/search"]        # glob; default-deny
  path_deny: ["/admin/**"]
  max_request_bytes: 65536
  max_response_bytes: 262144
  rate_per_min: 60
  egress_class: public
optional: openapi_spec_ref                     # enables typed tool surface
```

Agent-facing tool: `rest_call(resource_id, method, path, headers?, body?)`

- Gateway validates method/path against ceiling ∧ grant, injects auth at call
  time (OAuth2 client-credentials with cached token + refresh-on-401),
  forwards, caps response, logs.
- Agent-controlled headers: allowlist only (`content-type`, `accept`);
  everything else (esp. `host`, `authorization`, `x-forwarded-*`) rejected.
- If an OpenAPI spec is registered, the gateway publishes a **typed tool list**
  (`operationId`-level) so grants can be per-operation, not per-glob.

### 6.3 `mcp` driver

Registration (admin):

```yaml
type: mcp
url: https://mcp.example.com/mcp        # streamable HTTP (SSE fallback)
auth: { kind: bearer, secret_ref: ... }
ceiling:
  tools_allow: ["list_pulls", "get_issue"]     # enumerated at registration
  tools_deny: ["merge_pull", "*_delete"]
  per_tool: { "create_issue": { requires_confirmation: true } }
  rate_per_min: 30
  max_args_bytes: 32768
```

- **At registration:** gateway connects, enumerates tools, stores a **snapshot**
  (names + input schemas + hash). Registration fails if unreachable.
- **Drift detection:** periodic re-enumeration (hourly + on-demand). New/changed
  tools upstream are **denied by default** and raise an admin review event.
  Rationale: an upstream adding a `drop_table` tool must not silently gain
  reach through an existing grant.
- Agent-facing tools: `mcp_list(resource_id)`, `mcp_call(resource_id, tool, args)`.
- Enforcement: tool ∈ allowlist ∧ args validate against snapshot schema ∧
  size limits; `requires_confirmation` tools return `pending_confirmation` and
  only execute after the owning human approves via the web UI (approval event
  audited).
- Results wrapped as untrusted content (existing S-PROMPT WP3 wrapping).
- **Transport scope (resolved, 2026-10-06):** v1 speaks **streamable-HTTP
  only** (single `/mcp` endpoint; legacy SSE fallback optional). Confirmed
  network-capable for the first BYO targets: GitHub MCP supports streamable
  HTTP for remote use ([github/github-mcp-server](https://github.com/github/github-mcp-server));
  SonarQube MCP supports streamable HTTP via `SONARQUBE_TRANSPORT=http` at
  `/mcp` with `Authorization: Bearer`
  ([docs.sonarsource.com](https://docs.sonarsource.com/sonarqube-mcp-server/setup/self-hosted)).
  **stdio-only MCP servers cannot be reached over the network**; supporting
  them later requires an "MCP host adapter" (a pod running the stdio server
  behind an HTTP bridge) — explicitly deferred, not in v1.

### 6.4 `browser` driver (browser-as-a-service)

- Registration is an `mcp` resource whose `url` points at the in-cluster
  Browser Service; ceiling lists browser tools. The `browser` driver adds:
  session lifecycle (create/bind/close), per-session context isolation, and
  screenshot/extract size caps.
- Grant constraint extras: `max_sessions_per_agent: 1`, `max_session_minutes: 30`,
  `navigation_ceiling: { deny_hosts: [...], max_pages: 50 }` (enforced by the
  browser service via gateway-supplied policy at session create).
- All browser actions are MCP calls → same audit path; screenshots stored in
  object storage with task-scoped refs, never inlined into agent context
  beyond a preview.

### 6.5 `git` driver (folded into the gateway — Ross Q6, 2026-10-06)

- **Git smart-HTTP proxy:** the gateway proxies `git-upload-pack`
  (fetch/clone; GET + POST) and `git-receive-pack` (push; POST) with
  streaming support; packfiles pass through without buffering into agent
  memory.
- **Credential injection:** per-resource token (GitHub fine-grained PAT or
  App installation token) injected at the gateway; the agent's git remote is
  `https://gateway/...` with no embedded secret.
- **Repo allowlist (ceiling):** `repos_allow: ["org/repo1", "org/*"]`;
  default-deny. Read-only grants may omit `receive-pack` entirely (GET +
  upload-pack only → structurally read-only).
- **Write governance:** push permission = grant includes `git:push` for the
  repo. Durable PR-only enforcement lives in **GitHub branch protection**
  (server-side, unbypassable by any client); the gateway adds a second layer
  by denying `receive-pack` for repos not explicitly push-granted.
- **Audit:** clone/fetch/push events with repo, refs advanced (parsed from
  receive-pack report-status), agent/task identity. Commit-level attribution
  remains via the existing `skquad/<agent-id>` author-stamp convention.
- Registration type: `git` (added alongside `web`/`rest`/`mcp` in the §7
  type widening).

### 6.6 `ssh` driver — Terminal-as-a-Service (Ross, 2026-10-06)

**Context:** SRE-type agents that maintain production apps. A raw SSH tunnel is
an opaque encrypted channel — after the handshake the gateway sees nothing,
so per-command governance and audit are impossible. Same answer as the
browser: put the privileged protocol behind a service.

**Path split (shrinks the risk envelope):**

- **Write path stays GitOps** (§6.5 + rest/mcp). An SRE agent proposes;
  review + CI applies. Direct prod mutation is the exception path, gated.
- **SSH is the diagnostic path**: logs, status, inspection — read-mostly,
  time-boxed.

**Terminal Service (quarantine namespace `skquad-terminal`, mirrors §5.3):**

- Holds **all** SSH credentials; agent holds none (invariant preserved —
  even ephemeral certs never enter the sandbox).
- **SSH CA model:** short-lived, host-principal certificates per session
  (Vault SSH engine or `step-ca`), TTL 15–60 min. Prod hosts trust the CA;
  no static keys, no per-host key sprawl.
- **Per-command tool surface (not per-connection):**
  - `ssh_exec(resource, host, command, timeout)` → stdout/stderr/exit —
    one audit event per command, recorded BEFORE execution
  - `ssh_session_open / session_send / session_events / session_close` for
    genuine interactive-TTY needs, streamed via the gateway
  - **No scp / port-forward / raw tunnels in v1** — code moves via git,
    artifacts via APIs.
- **Command policy:** per-host allow/deny patterns from the grant ceiling.
  Deny-pattern hits (`rm -rf`, `dd of=`, `mkfs`, `shutdown`,
  `systemctl stop *`, writes to `/etc/`) route to the owner Inbox
  confirmation gate (§9); "Approve This and Future" = scoped standing grant
  for that pattern+host.
- **Session recording:** full ttyrec/asciinema-style capture to object
  storage, replayable; live-view + kill switch for high-tier sessions.
- **Host allowlist:** `hosts_allow: ["prod-web-*"]` in the ceiling;
  default-deny. Egress netpol: terminal service → registered host SSH ports
  only; no cluster-internal routes.
- **Risk tier: high.** Grant = prod resource-owner approval + admin co-sign.

**M1 implementation notes (S-256, 2026-10-08):**

- **CA: `step-ca`** chosen over Vault (not deployed here; step-ca is the
  lighter fit). Deployed in `skquad-terminal` as an islander: ingress
  only from terminal-service, zero egress.
- **Deny-pattern gate identity:** the gateway drives the TG-8
  confirmation client with tool identity
  `ssh_exec#<host>#deny:<pattern>` and
  `args_hash = ArgsHash(resource, "ssh_exec", canonical{host,command})`.
  CP standing grants match on (resource, agent, tool) — so "Approve
  This and Future" lands as a **pattern+host-scoped** standing grant
  with no schema change, while "approve once" pins the exact command
  via args_hash on Consume.
- **Deny globs match ANYWHERE** in the normalized command (chained
  payloads like `ls /; rm -rf /` still trip `rm -rf *`); **allow globs
  are anchored** (prefix semantics). Over-matching deny costs extra
  confirmations; under-matching would be a bypass.
- **endpoint_config shape:** `{ssh_user, port?, known_hosts, auth_mode:
  "ca"|"static_key"}`. `known_hosts` is REQUIRED — there is no code
  path that skips host-key verification. Static keys live in credential
  custody (kind `ssh_key`, per-agent or resource-level); CA mode
  carries no secret at all.
- **Recording format:** framed JSONL
  `{v:1, recording_id, t_ms, stream: meta|out|in, data_b64}`; meta
  frame first. Object storage has no append, so parts
  `recordings/<date>/<id>.partN.jsonl` + final `<id>.index.json`.
  Recording failure never kills the session (`recording_error` surfaced
  in the response).
- **Gateway fail-closed wiring:** the ssh driver registers only when
  `SKQUAD_TERMINAL_SERVICE_URL` + `SKQUAD_TERMINAL_INTERNAL_TOKEN`
  are both set; every terminal call carries the bearer token; terminal
  errors map to stable client-safe codes.
- **Secret wiring (no plaintext in git):** all terminal secrets are
  SealedSecrets referenced by the chart via `existingSecret` values —
  `skquad-terminal-service-internal-token` (both ns, key `token`),
  `skquad-step-ca-passwords` (`ca-password.txt` /
  `provisioner-password.txt`), `skquad-terminal-service-recording`
  (`access-key` / `secret-key`). The recordings MinIO service account
  is registered by a one-shot idempotent job
  (`k3s-cluster/manifests/minio/09-terminal-recording-setup-job.yaml`)
  with a bucket-scoped policy `skquad-terminal-rw`.
- **Deferred (M2):** recording replay UI + CP read API, live step-ca
  provisioner (JWS/OTT) integration validation, bypass drill.

**Alternatives rejected:**

1. Gateway TCP-tunnel to :22 — the "VPN effect": opaque post-handshake,
   credentials in the sandbox, prod becomes a launch pad.
2. CP-minted ephemeral certs placed in the agent pod — breaks the
   zero-credentials invariant; still no per-command visibility.
3. Teleport as a whole-platform adoption — provides JIT/recording/approval
   out of the box but is a heavyweight auth/UI universe; viable substitute
   backend behind the same tool surface (build-vs-buy call for Ross).

## 7. Data model (Postgres, embedded migrations)

Extends the existing registry rather than forking it:

```sql
-- 1. widen resource type enum/check to include: 'web','rest','mcp'
--    (existing 'api' rows migrated to 'rest' with a view alias)

-- 2. typed config + ceiling (JSONB, validated server-side per type)
ALTER TABLE resources ADD COLUMN endpoint_config JSONB NOT NULL DEFAULT '{}';
ALTER TABLE resources ADD COLUMN policy_ceiling JSONB NOT NULL DEFAULT '{}';
ALTER TABLE resources ADD COLUMN risk_tier     TEXT   NOT NULL DEFAULT 'low'
  CHECK (risk_tier IN ('low','medium','high'));
ALTER TABLE resources ADD COLUMN egress_class TEXT   NOT NULL DEFAULT 'public'
  CHECK (egress_class IN ('public','internal'));

-- 3. MCP tool snapshot (drift detection)
CREATE TABLE mcp_tool_snapshots (
  resource_id   UUID REFERENCES resources(id),
  snapshot_hash TEXT NOT NULL,
  tools         JSONB NOT NULL,          -- [{name, input_schema}]
  captured_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  review_state  TEXT NOT NULL DEFAULT 'approved'
    CHECK (review_state IN ('approved','drift_detected','suspended'))
);

-- 4. grant constraints (⊆ ceiling enforced in application layer)
ALTER TABLE grants ADD COLUMN constraints JSONB NOT NULL DEFAULT '{}';

-- 5. approval workflow (phase 4)
CREATE TABLE grant_requests (
  id UUID PK, resource_id, requested_by, grantee_type, grantee_id,
  constraints JSONB, status TEXT   -- pending_owner|pending_admin|approved|denied
    CHECK (...), owner_decision_at, admin_decision_at, notes
);
```

Validation rule (the **no-escalation invariant**, enforced in CP code, tested
exhaustively): a grant's `constraints` must be a subset of the resource's
`policy_ceiling` (methods ⊆, tools ⊆, rates ≤, sizes ≤,
`egress_class` equal). Reject with structured error listing violations.

## 8. Grant model (full rationale: ADR-0015)

Three parties (**"resource owner" is an attribute, not a role** — Ross Q3,
2026-10-06; skquad adds **no new RBAC role or IdP group**):

| Party | Owns | Realized as |
|---|---|---|
| Platform admin | Catalog existence, ceilings, risk tiers, `internal` egress class, kill switch | Existing `platform_admin` role |
| Resource owner | The credential/identity behind the resource | `resources.owner_user_id` (+ optional co-owners). BYO: the user who connected the credential. Admin-registered shared resource: registering admin, or their assignee |
| Agent/squad owner | Which of their agents use which resources, within constraints | Existing user / squad-ownership model |

Approval authority derives from the **attribute**
(`resources.owner_user_id`), not a role lookup. In practice the three-party
model collapses onto today's two roles: `platform_admin` and users wearing
different hats per resource.

**Credential provenance decides the path:**

- **BYO** (user connects their own GitHub/Linear/browser login): user is the
  resource owner → self-grant, zero friction. Expected majority of volume.
- **Shared org resource**: grant requires **resource-owner approval**
  (grant_requests workflow).
- **High tier**: resource-owner approval **+ platform-admin co-sign**.

**Invariants:**

1. `effective ⊆ ceiling ∩ grantor's delegable scope` — grants transfer, never
   amplify.
2. Grants are separate objects from resources → revocation is one row,
   effective ≤ 30 s (gateway cache TTL).
3. Agents act under **their own identity** end-to-end; the owner link is
   recorded, never impersonated.
4. **Grant-change linting (prover-style, adopted from OpenShell):** every
   grant/ceiling change is machine-checked *before* it takes effect (and
   before any auto-approval) for risky deltas: **new credentialed reach**
   (a credential becomes usable against a host it couldn't reach before),
   new HTTP methods, new MCP tools, any path to cloud metadata or
   cluster-internal ranges, ceiling widening. Findings block auto-approval
   and force the change through the owner Inbox with the diff + findings
   shown. This is OpenShell's policy-prover pattern (formally verified policy
   changes, [docs](https://docs.nvidia.com/openshell/latest/about/architecture))
   implemented as a deterministic policy-diff linter — full formal
   verification optional later.

## 9. Risk tiers

| Tier | Examples | Grant path | Extras |
|---|---|---|---|
| low | system `web` (GET), public read APIs | self-grant | rate limits |
| medium | BYO REST write, MCP with write tools | self-grant (BYO) / owner approval (shared) | confirmation-gate available |
| high | shared org credentials, `internal` egress, authenticated browser (Phase 3.5), **prod SSH (diagnostic)** | owner approval + admin co-sign | confirmation gates default-on for writes; tighter retention |

**Confirmation-gate UX (Ross Q5, 2026-10-06):** approvals land in the
**owner Inbox** as a message with three actions:

1. **Deny** — request refused, audited, agent notified.
2. **Approve** — one-time execution of the single pending call.
3. **Approve This and Future Requests** — materializes as a **standing grant**
   object scoped to `(resource, tool/operation)`, visible in the grants UI,
   revocable, with optional expiry. Standing approvals must never be invisible
   bypasses: every auto-approved future call logs against the standing grant
   it matched, so the audit trail stays honest.

## 10. Security & threat model (delta)

| Threat | Mitigation |
|---|---|
| SSRF via registered endpoint (metadata, internal admin) | Shared dial-time guard: resolve→pin→blocklist; per-redirect recheck; `internal` class opt-in only |
| DNS rebinding | IP pinning at connect time (BT-6 pattern, reused) |
| Credential exfil to agent | Secrets only in vault; gateway injects at call time; responses scrubbed of `authorization`/`set-cookie` headers before returning to agent |
| Prompt injection via fetched/MCP/browser content | Existing untrusted-wrapping (WP3); browser extracts marked untrusted; no auto-execution of fetched content |
| MCP upstream tool drift (new malicious tool) | Snapshot + deny-by-default + admin review event |
| Browser exploit / drive-by | Quarantine namespace, ephemeral contexts, no internal routes, no persistent state, seccomp/read-only rootfs |
| Gateway compromise | Gateway holds no long-lived secrets; per-call vault reads are scoped+audited; minimal image; netpol limits lateral movement (cannot reach CP DB, Postgres, etc.) |
| Agent bypasses gateway | Netpol default-deny; bypass tests in CI (direct connect attempts must fail) |
| Grant escalation | CP-side subset validation + property-based tests; UI cannot express what API rejects |
| Quota abuse / cost bomb | Per-(agent,resource) + per-resource rate limits; metering per call; task budget integration |

## 11. Resolved decisions (Ross, 2026-10-06)

| # | Question | Decision |
|---|---|---|
| 1 | Gateway↔CP policy transport | **CP internal API** (`GET /internal/v1/policy?agent=<id>`, ETag-cached). No shared-DB coupling. |
| 2 | MCP transport | **Streamable-HTTP only in v1** (SSE fallback optional). GitHub + SonarQube confirmed network-capable. stdio-only servers → deferred "MCP host adapter" (out of v1). |
| 3 | Browser engine | **Playwright / Chromium** confirmed. Pinned digest image, patch cadence required. |
| 4 | First BYO integrations | **GitHub** and **SonarQube** (both as `rest` and/or `mcp` resources; SonarQube MCP via streamable HTTP per §6.3). |
| 5 | Confirmation-gate UX | **Owner Inbox** message with 3 actions: Deny / Approve / Approve-This-and-Future (standing grant object — visible, scoped, revocable, expiring; see §9). |
| 6 | `project_workspace` (git) | **Folded into the gateway** as the `git` driver (§6.5). |

Remaining open items (non-blocking, refine during build):

- Standing-grant default expiry (suggest 90 days) and renewal UX — TG-8 detail.
- Whether SonarQube is registered primarily as `mcp` (rich tool surface) or
  `rest` (raw API) or both — recommend **mcp** first since the official
  server exists; `rest` only if agents need endpoints the MCP doesn't cover.
- MCP host adapter (stdio bridging) — revisit when a concrete stdio-only
  target appears.

## 12. Related work: NVIDIA OpenShell (reviewed 2026-10-06)

OpenShell (Apache-2.0, v0.1.x) is "a safe, private runtime for fleets of
autonomous agents" — same philosophy as this design (no real credentials in
the sandbox, every connection policy-checked), enforced at the **runtime
layer** instead of a central gateway
([architecture](https://docs.nvidia.com/openshell/latest/about/architecture)).

**Their model:** per-sandbox **supervisor** (trusted side of the boundary)
receives every TCP/DNS open intercepted at syscall level (seccomp
user-notify; Landlock for filesystem) from an in-sandbox **sandbox** shim
that makes zero policy decisions and identifies the *actual calling
executable* via trusted `/proc`. Gateway delivers policy/providers and runs a
**policy prover** — formal verification of proposed policy changes that
flags new credentialed reach, new HTTP methods, metadata access, and forces
human review. Fail-closed launch (agent can't start until boundary
confirmed; freezes if the supervisor channel drops). Generation-bound JWTs:
restart mints fresh tokens/certs; the sandbox side holds nothing stealable
(only the gateway public key).

**Mapping:** supervisor ≈ our Tool Gateway chokepoint; providers ≈ our vault +
per-call credential injection (identical invariant); policy ≈ our registry +
ceilings; prover ≈ our grant-change linting (§8 inv. 4); outer fence ≈ our
default-deny netpol; generation JWTs ≈ our short-TTL agent credentials,
sharpened.

**Where they are stronger:** syscall-level mediation lets agents run **real
CLIs** (`git`, `kubectl`, `pip install`) with every connection still
policy-checked — our semantic-tool model requires agents to use gateway tools
and cannot support arbitrary networked programs. Program-identity policy
("only `git` may reach github.com") has no equivalent in our design.
Fail-closed launch/freeze is actively proven, not statically assumed.

**Where our design still stands:** multi-tenant governance (users/squads/
three-party grants/BYO workflows/Inbox) — their gateway is not a
multi-tenant grant system; semantic MCP governance (tool allowlists, drift,
per-tool confirmation); REST method/path ACLs; SSH credential custody
(credential injection is HTTP-shaped; opaque SSH needs the terminal
service).

**Decision (Ross agreed, 2026-10-06): Hybrid — Option C.**

1. **Adopt now** into this architecture: prover-style grant-change linting
   (§8 inv. 4 → TG-8), fail-closed launch verification (§5.2 → TG-1),
   generation-bound tokens (§5.2 → TG-2).
2. **Do not adopt** OpenShell as the runtime layer mid-flight: v0.1.x API
   churn would convert our build risk into their upgrade risk while our
   gateway is still unwritten.
3. **Re-evaluate at their 1.0:** their isolation-backend interface is
   pluggable and their K8s topology (supervisor pod + workload pod + mTLS +
   CNI netpol) matches ours; by then our tool-surface abstraction will show
   whether their policy model maps onto our grants. Trigger: OpenShell 1.0
   release, or a hard requirement for arbitrary-networked-CLI workloads that
   our semantic-tool model can't serve.
