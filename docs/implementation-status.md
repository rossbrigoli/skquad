# Implementation Status

This ledger reconciles the design documents with the current implementation.
The ADRs and design docs remain valid architecture decisions, but a `Draft v1`
or `Accepted` status means "design accepted", not "fully implemented".

## Current Implemented Slice

| Area | Implemented |
| --- | --- |
| Control plane API | Dev auth, OIDC bearer validation with IdP group → `platform_admin` **bootstrap on first login** (`SKQUAD_OIDC_ADMIN_GROUPS`; bootstrap-only — existing rows are app-managed), app-managed role changes via `PATCH /users/:id/role` (last-admin guard, `user.role_changed` audit, fail-closed rollback), LAN/Tailscale-only break-glass admin login (argon2id, CIDR allowlist, rate-limited, fail-closed), squads, agents, boards, tasks, registry, permissions, access grants, audit/metering reads, gateway metering callback, lease-backed/fenced agent task APIs, retry/dead-letter-aware message APIs, agent chat history endpoint, agent work-wait endpoint, context APIs, Kubernetes outbox intents, and the task execution reaper (expires lapsed leases and re-queues stuck tasks; see [`execution-reaper.md`](execution-reaper.md)). |
| Persistence | In-memory dev store and PostgreSQL store selected by `SKQUAD_DATABASE_URL`; current schema includes domain state, task executions/results, messages, agent memory, metering, audit, identities (incl. gateway key token/status, migration `0007`), Kubernetes outbox, and Postgres `LISTEN/NOTIFY` triggers for task/inbox wake-ups. |
| Kubernetes operator | `Squad` and `Agent` CRDs, namespace/base-resource reconciliation, agent Deployments, finalizer cleanup, desired-active wake-up, and idle-timeout scale-down. |
| Helm chart | CRDs, API server, operator, LiteLLM gateway, web app, optional PostgreSQL, ingress toggle, external Secret knobs, image values, runtime config wiring, and RBAC for current controllers. |
| Agent runtime | Bootstrap/readiness, mounted Secret loading, task loop with control-plane work waiting plus fallback polling, task execution fence propagation, in-flight lease heartbeat while a handler runs, inbox draining, LiteLLM task handler, LLM-backed replies to user chat messages (history-aware, agent `system_prompt` persona), importlib plugin loading, per-task context fetch, permission-filtered tool exposure, and bounded memory persistence. |
| LiteLLM gateway | Charted LiteLLM proxy, Postgres-backed virtual-key storage, master-key wiring, callback module, image smoke tests, API-side virtual-key generation for active provider grants, and key lifecycle enforcement: `/key/update` on grant changes, `/key/delete` on revocation/agent+squad deletion/provider deprecation, gateway-sync-before-commit (a gateway failure aborts the permission change), and idempotent drift repair via `POST /api/v1/admin/gateway/keys/reconcile`. |
| Web app | Authenticated Next.js 16 shell (v2 IA; v1 retired 2026-09-26) with dashboard, squad cockpit/board, agent (including system-prompt persona), task, identity, near-real-time polling chat, registry, grant, audit, and metering workflows, plus the admin **Access tab** (platform role management + per-user AI model grants). Current package audit is clean at `moderate` and above. |
| CI/CD | Real validation CI, integration smoke, deployable images, GHCR publishing, optional Docker Hub mirroring, and lab GitOps promotion path. |

## Explicit Boundaries

These items are not implemented yet and must not be implied as production-ready:

| Gap | Tracking |
| --- | --- |
| Automatic task materialization for `delegate`/`handoff` messages and richer consult/reply workflows. | Follow-up after Kanbunny `9a4ec0f0-af8c-4eac-b27f-8fc328ced0a4` - `Add message retry, expiry, and dead-letter handling`. |
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
