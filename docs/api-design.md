# skquad — Control Plane API Design

> **Status:** Draft v1
>
> The **API server** exposes a **REST API** for the web app and external
> clients. It is the single entry point: it performs **authN** (OIDC JWT),
> **authZ** (user RBAC + access grants), and all domain CRUD. It also queues
> `Squad`/`Agent` CR intents that the Kubernetes outbox worker writes for the
> operator to reconcile.
>
> This document describes the target API shape. Implemented endpoints and
> deferred cross-cutting behavior are reconciled in
> [`implementation-status.md`](implementation-status.md).

---

## 1. Conventions

- **Base URL:** `/api/v1`
- **AuthN:** `Authorization: Bearer <JWT>` (OIDC-issued).
- **AuthZ:** enforced per endpoint (role + ownership + access grants).
- **Format:** JSON (request + response).
- **Errors:** consistent error envelope (see §10).
- **Pagination:** current list endpoints are unpaginated; `?cursor=` +
  `?limit=` is a future compatibility target for high-cardinality lists.
- **Idempotency:** current mutation endpoints do not persist
  `Idempotency-Key`; clients should retry only after checking resource state.
  Durable idempotency keys are tracked as a later hardening slice.
- **Versioning:** URI versioning (`/api/v1`); breaking changes bump the version.

---

## 2. Auth

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/v1/auth/login` | Redirect to the OIDC IdP. |
| `GET` | `/api/v1/auth/callback` | OIDC callback; issues a JWT. |
| `GET` | `/api/v1/auth/me` | Current user (`id`, OIDC issuer/subject when present, email profile, `email_verified`, role). |
| `POST` | `/api/v1/auth/logout` | Invalidate the session. |

---

## 3. Users (platform admin)

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/v1/users` | List users (admin). |
| `GET` | `/api/v1/users/:id` | Get a user (admin). |
| `PATCH` | `/api/v1/users/:id/role` | Set a user's role (admin). |
| `PATCH` | `/api/v1/users/:id/status` | Activate/deactivate (admin). |

---

## 4. Squads

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/api/v1/squads` | Create a squad (owner = caller). Creates the `Squad` CR. |
| `GET` | `/api/v1/squads` | List squads the caller owns / has access to. |
| `GET` | `/api/v1/squads/:id` | Get a squad (owner or granted). |
| `PATCH` | `/api/v1/squads/:id` | Update name / mission / operating model (owner). |
| `DELETE` | `/api/v1/squads/:id` | Delete a squad (owner). Enqueues deletion of the `Squad` CR; operator finalizer removes managed resources. |
| `POST` | `/api/v1/squads/:id/archive` | Archive a squad (owner). |

---

## 5. Agents

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/api/v1/squads/:id/agents` | Create an agent in a squad (owner). Creates the `Agent` CR. |
| `GET` | `/api/v1/squads/:id/agents` | List agents in a squad. |
| `GET` | `/api/v1/agents/:id` | Get an agent (owner or granted). |
| `PATCH` | `/api/v1/agents/:id` | Update role / default model / idle timeout (owner). |
| `DELETE` | `/api/v1/agents/:id` | Delete an agent (owner). Enqueues deletion of the `Agent` CR. |
| `POST` | `/api/v1/agents/:id/identity` | **Create the agent identity** (one-click; owner). |
| `POST` | `/api/v1/agents/:id/identity/rotate` | Rotate the agent credential / virtual key (owner). |
| `GET` | `/api/v1/agents/:id/permissions` | List the agent's resource permissions. |
| `PUT` | `/api/v1/agents/:id/permissions` | Set the agent's resource permissions (owner). |

Identity responses expose credential and virtual-key references only. Raw agent
credential and gateway virtual-key material is written to the configured Secret
backend and is not returned by the API. The control plane stores only the
one-way verifier hash for runtime authentication.

---

## 6. Boards & Tasks

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/v1/squads/:id/board` | Get the squad's board (columns + tasks). |
| `POST` | `/api/v1/squads/:id/board/tasks` | Create a task on the board. |
| `GET` | `/api/v1/tasks/:id` | Get a task. |
| `PATCH` | `/api/v1/tasks/:id` | Update title / description / metadata. |
| `POST` | `/api/v1/tasks/:id/move` | Move a task to a column (`{ status }`). |
| `DELETE` | `/api/v1/tasks/:id` | Delete a task. |

> **Agent-facing task endpoints** (used by the agent runtime, authenticated by
> the agent's identity):
>
> | Method | Path | Description |
> |--------|------|-------------|
> | `GET` | `/api/v1/agents/me/tasks` | Tasks assigned to this agent. |
> | `GET` | `/api/v1/agents/me/resources` | Active registry resources granted to this agent; secret refs are not returned. |
> | `POST` | `/api/v1/agents/me/tasks/claim` | Claim the next assigned `todo` task or an assigned `in-progress` task whose prior execution lease has expired. Returns task fields plus `execution_id`, `worker_id`, `fencing_token`, and `lease_expires_at`. |
> | `GET` | `/api/v1/agents/me/tasks/:id/context` | Return task-scoped context for an assigned task: task metadata, granted active resource descriptors, recent scoped memory, and payload limits. |
> | `POST` | `/api/v1/agents/me/tasks/:id/start` | Mark a task `in-progress` (context reset). |
> | `POST` | `/api/v1/agents/me/tasks/:id/complete` | Mark a task done / in-review. Requires `{ execution_id, fencing_token }`; may include `{ summary, persist_memory }`. The summary is stored on the execution attempt atomically with the terminal task status; optional memory persistence is best-effort after that commit. |
> | `POST` | `/api/v1/agents/me/tasks/:id/block` | Mark a task `blocked`. Requires `{ execution_id, fencing_token }`; may include `{ summary }`. |
> | `POST` | `/api/v1/agents/me/heartbeat` | Report `idle`, `busy`, or `error`. Busy heartbeats can include `{ execution_id, fencing_token }` to extend the active task lease. |

Task status values currently accepted by the API are `todo`, `in-progress`,
`in-review`, `done`, and `blocked`. User-driven move requests reject unknown
statuses with a `bad_request` error. Agent completion is narrower: agents may
complete to `in-review` or `done`; they must use the block endpoint for
`blocked`.

Agent runtimes should send `X-Skquad-Worker-ID` on claim and follow-up calls.
The control plane rejects stale/missing execution fences on terminal updates
with `409 conflict` or `400 bad_request`, preventing duplicate pods from
completing the same task after a lease has moved on.

---

## 7. Chat (user ↔ agent)

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/api/v1/agents/:id/chat` | Send a message to an agent (owner or `talk` grant). |
| `GET` | `/api/v1/agents/:id/chat` | Get the chat history with an agent (owner or `read` grant). |

> Chat is a **lightweight, non-task** interaction. It does not reset the task
> context. Enforced by scoped access grants for non-owners.
>
> Current implementation: `POST` enqueues a durable pending message; `GET`
> returns queued message history visible to the caller.

---

## 8. Messaging (agent ↔ agent)

> Agent-facing (authenticated by the agent's identity). The control plane
> enforces **access grants** for cross-squad messages.

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/api/v1/agents/me/messages` | Send a message (`{ to_agent_id, type, payload, max_attempts?, ttl_seconds? }`). |
| `GET` | `/api/v1/agents/me/messages` | Get this agent's retry-due pending inbox. |
| `GET` | `/api/v1/agents/me/messages/history` | Full chat history for this agent (all statuses, oldest first). Used by the runtime to build LLM chat context. |
| `POST` | `/api/v1/agents/me/messages/:id/ack` | Acknowledge a delivered message. |
| `POST` | `/api/v1/agents/me/messages/:id/fail` | Report handler failure; schedules retry or dead-letters after attempts expire. |
| `GET` | `/api/v1/agents/me/work/wait?timeout_seconds=N` | Long-poll for this agent's assigned task or ready inbox changes. Backed by Postgres `LISTEN/NOTIFY`; returns `{ work_available }` and falls back to timeout. |

- **Cross-squad** messages are rejected (and audited) without an access grant:
  `talk` allows consult/reply, `ping` allows ping, and `add_task` allows
  delegate/handoff.
- **Delegation / handoff** create a task on the target board + a ping.

Current implementation covers durable message enqueue, retry-due inbox list,
acknowledgement, failure reporting, retry scheduling, TTL expiry,
dead-lettering, audit, and pending-message wake signals. Automatic task
creation for `delegate`/`handoff` is tracked as a later workflow slice.

---

## 9. Resource Registry

> **Registration** is platform-admin only. **Granting** is squad-owner only.

### 9.1 LLM Providers
| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/api/v1/registry/llm-providers` | Register a provider (admin). |
| `GET` | `/api/v1/registry/llm-providers` | List providers. |
| `GET` | `/api/v1/registry/llm-providers/:id` | Get a provider. |
| `PATCH` | `/api/v1/registry/llm-providers/:id` | Update (admin). |
| `POST` | `/api/v1/registry/llm-providers/:id/deprecate` | Deprecate (admin). |

Provider payloads distinguish registry identity from model routing:
`default_provider_id`/provider `id` is the registry UUID, while
`default_model` is the LiteLLM/gateway model alias sent by the runtime.

### 9.2 Skills / Tools / APIs / Knowledge Bases / Workspaces
The same CRUD pattern applies to each type:

| Type | Base path |
|------|-----------|
| Skills | `/api/v1/registry/skills` |
| Tools | `/api/v1/registry/tools` |
| APIs | `/api/v1/registry/apis` |
| Knowledge bases | `/api/v1/registry/knowledge-bases` |
| Project workspaces | `/api/v1/registry/project-workspaces` |

Each supports `POST` (register, admin), `GET` (list/get), `PATCH` (admin),
`POST /:id/deprecate` (admin).

Current list endpoints return full result sets. Pagination parameters are not
yet interpreted by the control plane.

---

## 10. Access Grants

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/api/v1/squads/:id/access-grants` | Grant scoped user/agent access (owner). Permissions are comma-separated `read`, `talk`, `ping`, `add_task`, `admin`, or `*`. |
| `GET` | `/api/v1/squads/:id/access-grants` | List grants for a squad. |
| `DELETE` | `/api/v1/access-grants/:id` | Revoke a grant (owner). |

---

## 11. Metering

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/v1/squads/:id/metering` | Squad metering (tokens + cost, time-range). |
| `GET` | `/api/v1/agents/:id/metering` | Agent metering (tokens + cost, time-range). |
| `GET` | `/api/v1/metering/summary` | Platform-wide summary (admin). |
| `POST` | `/api/v1/gateway/metering` | Internal LiteLLM gateway callback ingestion. Authenticated with `SKQUAD_GATEWAY_CALLBACK_TOKEN`, not user/session auth. |

Current read endpoints return aggregate totals. `?from=`, `?to=`, and
`?groupBy=agent|squad|provider|model` are future compatibility targets.

---

## 12. Audit

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/v1/audit` | Query the audit log (admin; filters by actor, squad, action, time). |
| `GET` | `/api/v1/squads/:id/audit` | Audit log for a squad (owner/admin). |

---

## 13. Admin (platform config)

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/v1/admin/config` | Get platform config (admin). |
| `PATCH` | `/api/v1/admin/config` | Update platform config (admin). |
| `GET` | `/api/v1/admin/health` | Platform health (all components). |

---

## 14. Error Model

```json
{
  "error": {
    "code": "forbidden",
    "message": "You do not have access to this squad.",
    "details": { "squad_id": "..." }
  }
}
```

| HTTP | Code | Meaning |
|------|------|---------|
| 400 | `bad_request` | Invalid input. |
| 401 | `unauthorized` | Missing/invalid JWT. |
| 403 | `forbidden` | Authenticated but not allowed (RBAC / access grant). |
| 404 | `not_found` | Resource does not exist. |
| 409 | `conflict` | Version conflict / duplicate. |
| 422 | `unprocessable` | Semantically invalid. |
| 429 | `rate_limited` | Too many requests. |
| 500 | `internal` | Unexpected error. |

---

## 15. AuthZ Matrix (summary)

| Action | platform_admin | squad owner | granted user | agent |
|--------|:--------------:|:-----------:|:------------:|:-----:|
| Manage users / registry | ✅ | — | — | — |
| Create / delete squad | ✅ | ✅ (own) | — | — |
| Manage squad agents | ✅ | ✅ (own) | — | — |
| Create / assign tasks | ✅ | ✅ (own) | — | ✅ (`add_task` grant for delegate/handoff) |
| Chat with agent | ✅ | ✅ (own) | ✅ (`talk` grant) | — |
| Cross-squad message | ✅ | ✅ (per grants) | — | ✅ (per grants) |
| View metering | ✅ (all) | ✅ (own) | — | — |
| View audit | ✅ (all) | ✅ (own) | — | — |

---

## 16. Open Points

- **Webhooks** — notify external systems on task/agent events (later).
- **GraphQL** — whether to add a GraphQL layer (start REST).
- **Pagination/idempotency** — add persisted idempotency-key handling and
  cursor pagination when API traffic justifies the extra storage/indexing.
- **Rate limiting** — per-user and per-agent limits (the gateway handles LLM
  calls; the API server handles API calls).
- **OpenAPI spec** — generate and publish an OpenAPI 3 document (implementation).
