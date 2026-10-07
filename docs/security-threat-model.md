# skquad — Security & Threat Model

> **Status:** Draft v1
>
> skquad v1 is an **enterprise, single-tenant Kubernetes platform** with
> load-bearing squad/agent isolation inside one operator-managed install. This
> document identifies the **assets**, **trust boundaries**, **threats**, and **mitigations**. It
> complements [identity-security.md](identity-security.md) (the controls) and
> [deployment-operator.md](deployment-operator.md) (isolation).
>
> This is the target threat model, not a production certification. Open
> implementation gaps are tracked in
> [`implementation-status.md`](implementation-status.md).
>
> **Refreshed 2026-10-07** to reflect what TG-6 (browser service, 0.1.269),
> TG-8 (risk tiers / approvals / standing grants / grant linter, 0.1.270) and
> TG-9 (drills, netpol-guard init-container, audit dashboard — in review on
> `feat/tg9-audit-drills`) actually shipped, per live-cluster drills and the
> WORKLOG. New: T11–T13 and three refreshed residual risks in §5.

---

## 1. Assets

| Asset | Why it matters |
|-------|----------------|
| **User data** (squads, tasks, chat) | Confidential business content. |
| **Agent credentials** (identity, virtual keys) | Grant access to LLMs + resources. |
| **LLM provider keys** (BYOM) | Cost + access to external models. |
| **Squad isolation** | One squad must not read/control another. |
| **Metering integrity** | Cost accounting must be accurate. |
| **Audit log** | Compliance / forensics; must be tamper-evident. |
| **Registry** | Governed catalog; must not be tampered with. |

---

## 2. Trust Boundaries

```
┌─────────────────────────────────────────────────────────────┐
│  Untrusted: Browser / external clients                       │
├─────────────────────────────────────────────────────────────┤
│  Control plane (skquad-system): API, gateway, operator       │  ← enforcement
│  (authN, RBAC, access grants, metering, audit)              │
├─────────────────────────────────────────────────────────────┤
│  Data plane: squad namespaces (agent pods)                   │  ← isolated
│  (each squad isolated; agents have scoped credentials)       │
├─────────────────────────────────────────────────────────────┤
│  External: LLM providers, KBs, git/Jira/Confluence           │  ← untrusted I/O
└─────────────────────────────────────────────────────────────┘
```

- **Control plane** is the **enforcement point** (authN, RBAC, access grants,
  metering, audit).
- **Squad namespaces** are **isolated** (network policies; no direct
  pod-to-pod between squads).
- **External systems** (LLM providers, KBs, workspaces) are **untrusted I/O** —
  their content can be adversarial (prompt injection).

---

## 3. Threats & Mitigations

### T1. Multi-tenant isolation bypass
- **Threat:** Squad A reads or controls squad B's agents/tasks/data.
- **Mitigations:**
  - Squad = **namespace**; **network policies** default-deny between squads.
  - Squad namespace egress is limited to DNS plus API server / LLM gateway pods
    selected in the control-plane namespace; direct Postgres egress is not part
    of the default agent policy.
  - All cross-squad interaction goes through the **control plane**, which
    enforces **access grants**.
  - API layer always filters by `squad_id` + ownership (optionally Postgres
    RLS).
  - Agents have **scoped credentials** (only their squad's resources).

### T2. Agent credential theft / leak
- **Threat:** An agent's credential or virtual key is stolen and abused.
- **Mitigations:**
  - Credentials stored as **K8s secrets** (scoped per agent/squad); never
    returned to clients.
  - The API server gets generated Secret write/delete authority through
    operator-created RoleBindings in each managed squad namespace, not through a
    chart-level cluster-wide Secret writer role.
  - **Virtual keys** are per-agent, revocable, and rate-limited.
  - **Rotation** without re-creating the agent; old keys revoked on rotation.
  - **Least privilege** — a key only grants the agent's permitted models.
  - **Audit** all credential use; alert on anomalies.

### T3. Prompt injection (via untrusted content)
- **Threat:** Adversarial content in a **task description**, **knowledge base**,
  **workspace**, or **message** manipulates an agent into harmful actions
  (exfiltration, unauthorized calls).
- **Mitigations:**
  - **Least privilege** — an agent can only use **permitted** resources; a
    prompt cannot grant new access.
  - **Egress controls** — network policies restrict where agents can send data
    (only permitted endpoints).
  - **Tool allow-lists** — agents can only invoke permitted tools/skills.
  - **Audit** all agent actions (LLM calls, tool calls, messages) for review.
  - **Content provenance** — task-context memory is labeled with trust level,
    provenance, review status, and source task. Raw completion summaries remain
    `raw_model_output` / `pending_review`; runtime prompts tell the model that
    memory is contextual evidence, not executable instruction.
  - Injection detection / guardrails at the gateway remain later work.
  - **Human review** — shipped (TG-8, 0.1.270): confirmation-gated tool
    calls land in the owner Inbox with deny / approve-once / approve-standing
    actions; risky grant changes are linted before taking effect (T13).

### T4. LLM gateway abuse
- **Threat:** An agent (or compromised agent) makes excessive or unauthorized
  LLM calls (cost abuse, using models it shouldn't).
- **Mitigations:**
  - **Permission enforcement** at the gateway (agent can only use permitted
    providers/models).
  - **Rate limiting** + **budgets** per virtual key (per agent/squad).
  - **Metering** — all calls recorded; cost visible; alert on spend anomalies.
  - **Virtual keys** are revocable.

### T5. RBAC bypass (user)
- **Threat:** A user accesses a squad they don't own / aren't granted.
- **Mitigations:**
  - **Central authZ** in the API server on every request (role + ownership +
    access grants).
  - **JWT** carries identity + role; validated on every call.
  - **Deny by default** — no access without an explicit grant.
  - **Audit** all access attempts (including denials).

### T6. Access-grant abuse (cross-squad)
- **Threat:** A granted agent/user abuses cross-squad access.
- **Mitigations:**
  - Grants are **explicit, scoped** (specific permissions), and **revocable**.
  - Cross-squad messages/task creation are **enforced + audited**.
  - Owners can **revoke** grants; revocation takes effect on the next check
    (gateway policy cache TTL ≤ 30 s, fail-closed on CP unreachable).
  - **Audit honesty for standing grants (TG-8):** every confirmation-gated
    call that passes on a standing grant logs `matched_standing_grant_id`
    and `gate:"standing"` on its audit event — a standing approval can
    never act as an invisible bypass. The standing grant is **re-validated
    at consume time**, so a revoke between approval and execution is
    immediately effective.
  - **Least privilege** — grant only what's needed (e.g. `read`, `talk`,
    `ping`, or `add_task`).

### T7. Supply chain (malicious plugin)
- **Threat:** A malicious or compromised **plugin** (skill/tool/connector)
  exfiltrates data or misbehaves.
- **Mitigations:**
  - Plugins run **inside the agent pod** (isolated per squad namespace).
  - **Sandboxing** for untrusted/community plugins (restricted sidecar).
  - **Manifest-declared permissions** — a plugin only gets what the agent is
    permitted.
  - **Provenance** — plugin packages signed / checksummed (later); registry
    tracks source.
  - **Audit** plugin invocations.

### T8. Data exfiltration (agent → external)
- **Threat:** An agent sends confidential data to an external endpoint.
- **Mitigations:**
  - **Egress network policies** — agents can only reach **permitted** endpoints
    (LLM gateway, queue, granted resources).
  - **Tool allow-lists** — only permitted tools can make external calls.
  - **Audit** all external calls.
  - (Later) **DLP** / content inspection at the gateway for sensitive data.

### T9. Audit log tampering
- **Threat:** An attacker modifies/deletes audit records to hide actions.
- **Mitigations:**
  - Audit log is **append-only** (no update/delete for normal roles).
  - **DB roles** — only the control plane can write; admins can read.
  - (Later) **hash chaining** / external logging for tamper-evidence.
  - **Retention** — audit retained even after squad/agent deletion.

### T10. Denial of service
- **Threat:** Excessive tasks/agents/calls degrade the platform.
- **Mitigations:**
  - **Resource quotas** per squad namespace.
  - **Rate limiting** (API + gateway).
  - **Scale-to-zero** — idle agents consume no resources.
  - **Backpressure** — message queue + task backlog bounds concurrent work.
  - **Alerts** on resource saturation.

### T11. Network-policy startup race (exfiltration window at pod birth)
- **Threat:** A freshly scheduled pod in a squad namespace runs for ~1–3 s
  before the CNI finishes enforcing the namespace NetworkPolicies. A pod
  born malicious can egress to the internet inside that window. Found live
  by the TG-9 bypass drill (2026-10-07, reproduced twice).
- **Mitigation (built in TG-9 slice B; pending merge + operator deploy):**
  a blocking **`netpol-guard` init-container** on agent pods. It probes a
  public canary every `probeIntervalMs` (default 250 ms); a **non-zero curl
  exit** (refused/timeout/unreachable) counts as *connection blocked*.
  **`requiredBlockedProbes` consecutive blocked probes** (default 3)
  confirm enforcement and release the workload. If enforcement is not
  confirmed within `maxWaitSeconds` (default 30) the init-container exits 1
  — **fail-closed**: the pod stays `Init:Error` and the agent workload never
  starts. Chart values: `agentNetpolGuard.*` (enabled by default; canary,
  interval, wait, confirmation count, image). Guarded pod kind: the agent
  runtime pod only — browser pods are covered by the proxy/netguard story
  (T12) instead. The drill's `agent-guard-birth` check asserts the first
  app-container probe is already blocked before the workload starts.

### T12. Browser quarantine escape
- **Threat:** A compromised browser session (drive-by / Chromium exploit) or
  the browser container itself reaches internal services or bypasses the
  egress denylist.
- **Mitigations (defense-in-depth, three layers, TG-6 0.1.269):**
  1. **NetworkPolicy except-list** — quarantine namespace
     (`skquad-browser`) is default-deny all ingress+egress; browser pod
     egress is public `80/443` with RFC1918, CGNAT, link-local and cluster
     pod/service CIDRs excluded
     (`browserService.deniedEgressCIDRs` + `toolGateway.clusterCIDRs`).
  2. **Mandatory Chromium `--proxy-server`** — browser-service always
     launches Chromium with `--proxy-server=http://127.0.0.1:8888` into
     the browser-proxy sidecar; there is no supported launch path without
     it, and the in-service navigation ceiling re-checks every subrequest
     (host denylist, private-IP check, scheme/limits) fail-closed.
  3. **netguard L7 floor** — the browser-proxy sidecar dials with
     resolve→pin→blocklist (loopback/RFC1918/CGNAT/link-local/metadata/
     IPv6 ULA denied), ports 80/443 only.
- **Residual:** layers 2–3 are process-level; a raw socket from the pod
  bypasses both (see §5, first bullet).

### T13. Risky grant change (silent widening)
- **Threat:** A grant/ceiling edit quietly gives an agent new credentialed
  reach, new HTTP methods / MCP tools, or a path to cloud-metadata or
  cluster-internal space.
- **Mitigation (shipped TG-8, 0.1.270):** the **grant-change linter**
  (`control-plane/internal/grantlint`) runs as a **pre-effect policy
  check** on every grant/ceiling change — before persistence and before any
  auto-approval. It is a deterministic before/after diff
  (`LintChange(before, after)`) producing findings: `metadata_path`
  (absolute block, fires regardless of baseline), `cluster_internal_path`,
  `new_credentialed_reach`, `new_http_method`, `new_mcp_tool`,
  `ceiling_widened` — severity `block` or `warn`. **Block findings
  override auto-approval** and force the change through the owner Inbox
  grant-request workflow with the findings shown; warns ride along without
  blocking (v1).

---

## 4. Defense in Depth

| Layer | Control |
|-------|---------|
| **Network** | Namespace isolation, egress policies, default-deny, `netpol-guard` init-container on agent pods (T11), browser quarantine except-list + mandatory proxy + netguard floor (T12). |
| **Identity** | OIDC (users), owner-created agent identities, virtual keys. |
| **Authorization** | Two-layer RBAC (user + agent), access grants, deny-by-default, risk tiers + pre-effect grant-change linting (T13), confirmation gates / standing grants. |
| **Data** | Scoped credentials, no raw secrets in DB, RLS (optional). |
| **Runtime** | Plugin sandboxing, tool allow-lists, least privilege. |
| **Accountability** | Append-only audit, metering, alerts. |
| **Resilience** | Quotas, rate limits, scale-to-zero, backpressure. |

---

## 5. Residual Risks

- **Raw non-proxy egress from the browser pod (public hosts only).** The
  quarantine netpol must allow public `80/443` at **pod** level because the
  netguard proxy shares the browser pod's network namespace. A raw `curl`
  from the browser container therefore reaches any **public** host directly,
  bypassing the proxy's SSRF host-denylist — the proxy denylist is an L7
  control and is **not network-enforced** for public destinations. Private
  space is excluded at L3 (post-hotfix; see next bullet). Accepted for v1;
  pod-per-session isolation is the eventual answer.
- **Deployed browser/gateway except-list bug (hotfix pending merge).** The
  shipped chart emitted one `except:` key per loop iteration; duplicate YAML
  keys collapsed to the **last CIDR only** (10.43.0.0/16), so on the live
  cluster (0.1.270) RFC1918/CGNAT/link-local are still reachable from the
  quarantine pod. Fixed by a single-`except:` rendering fix, **open as PR
  #203** — treat as live until merged and deployed. (TG-9 drill finding 2.)
- **Gateway agent-existence oracle (502-vs-401).** With a valid internal
  token, an unknown agent id returns `502 policy_unavailable` (a
  fail-closed deny) while a bad credential returns `401` — letting a
  caller distinguish "agent exists" from "credential invalid". Low
  severity (requires cluster-internal reachability + token class);
  normalize to a single 401 shape later. (TG-9 drill finding, INFO.)
- **Browser pod holds `BROWSER_INTERNAL_TOKEN`.** The browser-service
  container carries the platform-internal gateway↔browser service token in
  its env (secretRef). Strict gateway-custody says data-plane pods hold no
  tokens; the browser zone is currently classified as trusted infra.
  **Custody ruling pending** (TG-9 drill finding 3, MEDIUM).
- **Prompt injection** cannot be fully eliminated with LLMs; mitigations reduce
  blast radius (least privilege, egress controls, audit) but a determined
  attacker with a permitted tool could still cause harm within that scope.
  **Mitigation:** keep scopes narrow; confirmation gates + owner Inbox
  approvals shipped in TG-8 (0.1.270).
- **Compromised control plane** — if the API server is compromised, enforcement
  is bypassed. **Mitigation:** harden the control plane, least-privilege DB
  roles, audit, and (later) external secrets manager + mTLS.
- **Plugin supply chain** — untrusted plugins are a risk; **mitigation:**
  sandboxing + provenance + signing (later).

---

## 6. Security Requirements Traceability

| Requirement | Covered by |
|-------------|------------|
| OIDC for humans | [identity-security.md](identity-security.md) §2 |
| Two-layer RBAC | [identity-security.md](identity-security.md) §3, §5 |
| Audit logging | [identity-security.md](identity-security.md) §8 |
| Multi-tenant isolation | [deployment-operator.md](deployment-operator.md) §8 |
| Agent identity/credentials | [identity-security.md](identity-security.md) §4, §7 |
| Metering integrity | [observability-metering.md](observability-metering.md) §2 |
| BYOM credential safety | [resource-registry.md](resource-registry.md) §7 |

---

## 7. Open Points

- **External secrets manager** (Vault / ExternalSecrets) for agent credentials
  (later).
- **mTLS** between control plane and agent pods (hardening).
- **Prompt-injection guardrails** at the gateway (later).
- **Human-approval gates** for sensitive actions — **shipped** (TG-8:
  risk tiers, grant requests, confirmation gates, standing grants).
- **Netpol-guard rollout:** the guard is effective only once the operator
  image carrying it is deployed; until then real agent pods remain
  exposed to the birth race (T11).
- **Penetration test** before GA (implementation).
