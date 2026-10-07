# Implementation Status

This ledger reconciles the design documents with the current implementation.
The ADRs and design docs remain valid architecture decisions, but a `Draft v1`
or `Accepted` status means "design accepted", not "fully implemented".

## Current Implemented Slice

| Area | Implemented |
| --- | --- |
| Control plane API | Dev auth, OIDC bearer validation with IdP group → `platform_admin` **bootstrap on first login** (`SKQUAD_OIDC_ADMIN_GROUPS`; bootstrap-only — existing rows are app-managed), app-managed role changes via `PATCH /users/:id/role` (last-admin guard, `user.role_changed` audit, fail-closed rollback), LAN/Tailscale-only break-glass admin login (argon2id, CIDR allowlist, rate-limited, fail-closed), squads, agents, boards, tasks, registry, permissions, access grants, audit/metering reads, gateway metering callback, lease-backed/fenced agent task APIs, retry/dead-letter-aware message APIs, agent chat history endpoint, S-164 squad-peer roster (`GET /agents/me/peers`) and per-correlation-chain message budget (12, `chain_exceeded` 409 + audit), agent work-wait endpoint, context APIs, Kubernetes outbox intents, and the task execution reaper (expires lapsed leases and re-queues stuck tasks; see [`execution-reaper.md`](execution-reaper.md)). |
| Persistence | In-memory dev store and PostgreSQL store selected by `SKQUAD_DATABASE_URL`; current schema includes domain state, task executions/results, messages, agent memory, metering, audit, identities (incl. gateway key token/status, migration `0007`), Kubernetes outbox, and Postgres `LISTEN/NOTIFY` triggers for task/inbox wake-ups. |
| Kubernetes operator | `Squad` and `Agent` CRDs, namespace/base-resource reconciliation, agent Deployments, finalizer cleanup, desired-active wake-up, and idle-timeout scale-down. |
| Helm chart | CRDs, API server, operator, LiteLLM gateway, web app, optional PostgreSQL, ingress toggle, external Secret knobs, image values, runtime config wiring, and RBAC for current controllers. |
| Agent runtime | Bootstrap/readiness, mounted Secret loading, task loop with control-plane work waiting plus fallback polling, task execution fence propagation, in-flight lease heartbeat while a handler runs, inbox draining, LiteLLM task handler, LLM-backed replies to user chat messages (history-aware, agent `system_prompt` persona), S-164 agent-to-agent messaging (`send_message` builtin with squad-roster name resolution; peer `consult`s answered back to the sender as correlated `reply`; peer `reply`/`ping` processed with self-echo guard; squad roster injected into chat context), importlib plugin loading, per-task context fetch, permission-filtered tool exposure, and bounded memory persistence. |
| LiteLLM gateway | Charted LiteLLM proxy, Postgres-backed virtual-key storage, master-key wiring, callback module, image smoke tests, API-side virtual-key generation for active provider grants, and key lifecycle enforcement: `/key/update` on grant changes, `/key/delete` on revocation/agent+squad deletion/provider deprecation, gateway-sync-before-commit (a gateway failure aborts the permission change), and idempotent drift repair via `POST /api/v1/admin/gateway/keys/reconcile`. |
| Tool gateway + browser service (TG-1…TG-6) | **Deployed** (browser first shipped 0.1.269; gateway train through 0.1.270, live on lab). Gateway drivers: `web`, `rest`, `mcp`, `git`, and `browser` (browser-as-a-service: Playwright-MCP `browser-service` + `browser-proxy` netguard egress sidecar in the `skquad-browser` quarantine namespace, warm pool, session bind to agent/task/resource, grant-derived policy). See [`tg6-browser-protocol.md`](tg6-browser-protocol.md). |
| Risk tiers, approvals & standing grants (TG-8) | **Deployed 0.1.270.** Risk tiers low/medium/high; grant-request state machine (owner approval → admin co-sign for high tier); owner Inbox approvals; confirmation gates at the gateway (deny / approve-once / approve-standing, `X-Skquad-Confirmation-Id` retry flow, fail-closed when CP unreachable); standing grants (default +90 d expiry, soft revoke, re-validated at consume, `matched_standing_grant_id` on every audit event); pre-effect grant-change linter (`grantlint` — block findings disable auto-approval); web panels for grant requests, confirmations, and standing grants. See [`tg8-grant-approvals-spec.md`](tg8-grant-approvals-spec.md). |
| Security drills & audit dashboard (TG-9) | **In review** on `feat/tg9-audit-drills` (not yet deployed): live bypass suite + secret-custody probe + revocation-latency harness (`scripts/security-drills/`), blocking `netpol-guard` init-container for agent pods (fail-closed, 3-consecutive-blocked-probe confirmation, chart value `agentNetpolGuard.*`), and the admin Audit & Metering dashboard (`/audit`, human-readable decision codes, audit-derived per-resource call rollups). The browser/gateway egress except-list rendering hotfix is **open as PR #203** — until merged, the deployed quarantine netpol excludes only 10.43.0.0/16. |
| Web app | Authenticated Next.js 16 shell (v2 IA; v1 retired 2026-09-26) with dashboard, squad cockpit/board, agent (including system-prompt persona), task, identity, near-real-time polling chat, registry, grant, audit, and metering workflows, plus the admin **Access tab** (platform role management + per-user AI model grants). Current package audit is clean at `moderate` and above. |
| CI/CD | Real validation CI, integration smoke, deployable images, GHCR publishing, optional Docker Hub mirroring, and lab GitOps promotion path. |

## Explicit Boundaries

These items are not implemented yet and must not be implied as production-ready:

| Gap | Tracking |
| --- | --- |
| TG-9 residual findings: raw non-proxy egress from the browser pod bypasses the proxy denylist for **public** hosts (private space is L3-blocked only after PR #203 deploys); gateway 502-vs-401 agent-existence asymmetry; `BROWSER_INTERNAL_TOKEN` custody classification for the browser pod pending. Details in [`security-threat-model.md`](security-threat-model.md) §5. | TG-9 drill findings 2–4; PR #203 open. |
| Netpol-guard not yet effective on the cluster: the guard ships in the operator image; until the `feat/tg9-audit-drills` branch is merged and the operator redeployed, agent pods remain exposed to the ~1–3 s birth-race window. | TG-9 slice B, this branch. |
| Richer consult/reply **UI** — inbox views, thread grouping by correlation_id, and consultation SLAs in the web app (the messaging loop itself, including S-164 `send_message` and delegate/handoff task materialization, is implemented). | Kanbunny S-164 follow-ups. |
| Broader transactional audit guarantees across all significant mutation paths. Current implementation fail-closes access-grant and agent-permission mutations after recording a required pre-mutation audit event, while many other audit writes remain best-effort. | Follow-up hardening after Kanbunny `23ea081f-37d8-46cd-a379-ec4147826179` - `Harden OIDC identity, grant scopes, and audit guarantees`. |
| Reduced API Secret RBAC and more precise agent egress/network policy generation. | Kanbunny `a4fc5852-3e86-43bd-ba3c-73a8a2f74132` - `Reduce API Secret RBAC and agent network-policy blast radius`. |
| Migration rollback/down-migration automation and operator-led database maintenance jobs. Current startup migrations are versioned, checksum-recorded, and serialized by a Postgres advisory lock, but rollback remains a manual restore/forward-fix operation. | Follow-up after Kanbunny `c8f6bb7d-d8a5-43c5-b814-52e8da5890d1` - `Version database migrations with ledger and lock`. |
| Automatic embedding generation and explicit memory approval/distillation workflow. Current memory rows carry trust/provenance/review labels and optional embedding vectors; retrieval ranks by vector similarity only when an embedding query is supplied. | Follow-up after Kanbunny `da19b194-d738-4d95-84dc-a8287d8e2e3f` - `Harden semantic memory with embeddings and trust boundaries`. |
| A first-class squad LLM. The squad's LLM is a web-app convention stored in `operating_model.llm`, and only the web app keeps it coherent: the control plane does not validate it, and an agent created directly through the API does not inherit it. Target: a squad LLM field the gateway falls back to, which the current UI can adopt without changing shape. | Review follow-up from rossbrigoli/skquad#2; not yet filed on Kanbunny. |
| Grant changes taking effect without an identity rotation. Agent permission changes reach the LLM gateway only when the agent's identity is created or rotated, so the web app asks the owner to rotate after Apply squad LLM. Target: re-provision the gateway key when an agent's grants change, or have the gateway read grants live. | Review follow-up from rossbrigoli/skquad#2; not yet filed on Kanbunny. |
| Richer browser UI polish and public product website. | Kanbunny `6947922e-903c-4843-8f59-828db1a6e481` - `Create the skquad product website`. |

## Reading the Docs

- Requirements and ADRs describe intended product and architectural direction.
- Component design docs describe the target architecture and now include status
  notes where current behavior differs from the target.
- Component READMEs describe how to run or operate the current implementation.
- The operations runbook describes current Kubernetes install and day-two
  procedures without assuming future hardening is complete.

When in doubt, prefer the code, tests, this ledger, and Kanbunny follow-up
cards over older aspirational language in the design docs.
