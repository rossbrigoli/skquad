# skquad — Data Model (Postgres)

> **Status:** Draft v1 · **Decision:** [ADR-0005](adr/0005-persistence.md)
>
> Single **Postgres** store (with **pgvector**) for domain data, agent
> long-term memory, audit log, metering, and the v1 message queue. This is the
> physical schema behind the [domain model](domain-model.md).
>
> The current schema is implemented as embedded numbered SQL migrations.
> Startup migration execution is serialized with a Postgres advisory lock and
> recorded in `schema_migrations` by filename and SHA-256 checksum.

---

## 1. Conventions

- Primary keys: `uuid` (default `gen_random_uuid()`).
- Timestamps: `timestamptz`.
- Soft deletes where noted (`status`); hard delete for cascaded squad resources.
- Credential values are **never** stored here — only **secret references**.
- `jsonb` for flexible/structured documents (operating model, permissions,
  metadata).

---

## 2. Identity & Users

```sql
CREATE TABLE users (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    oidc_issuer   text,
    oidc_subject  text,
    email         text NOT NULL,
    email_verified boolean NOT NULL DEFAULT false,
    name          text NOT NULL DEFAULT '',
    role          text NOT NULL DEFAULT 'user'
                  CHECK (role IN ('platform_admin', 'user')),
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX idx_users_oidc_subject
    ON users(oidc_issuer, oidc_subject)
    WHERE oidc_issuer IS NOT NULL AND oidc_subject IS NOT NULL;
CREATE INDEX idx_users_email ON users(email);

CREATE TABLE schema_migrations (
    version    text PRIMARY KEY,        -- migration filename, e.g. 0001_init.sql
    checksum   text NOT NULL,           -- SHA-256 of embedded SQL bytes
    applied_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE agent_identities (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    agent_id      uuid NOT NULL,        -- set when attached to an agent
    subject       text NOT NULL UNIQUE, -- stable agent principal
    credential_ref text,                -- K8s secret ref
    credential_hash text,               -- one-way runtime auth verifier
    virtual_key_ref text,               -- LLM gateway virtual key ref
    created_by    uuid NOT NULL REFERENCES users(id),
    status        text NOT NULL DEFAULT 'active'
                  CHECK (status IN ('active', 'rotated', 'revoked')),
    created_at    timestamptz NOT NULL DEFAULT now(),
    rotated_at    timestamptz
);
```

---

## 3. Squads, Agents, Boards, Tasks

```sql
CREATE TABLE squads (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name          text NOT NULL,
    mission       text,
    operating_model jsonb,              -- roles + collaboration rules
    owner_id      uuid NOT NULL REFERENCES users(id),
    k8s_namespace text NOT NULL UNIQUE, -- squad-<id>
    status        text NOT NULL DEFAULT 'active'
                  CHECK (status IN ('active', 'archived', 'deleted')),
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE agents (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name          text NOT NULL,
    squad_id      uuid NOT NULL REFERENCES squads(id) ON DELETE CASCADE,
    role          text,                 -- from operating model
    system_prompt text NOT NULL DEFAULT '', -- optional persona for chat/tasks
    identity_id   uuid REFERENCES agent_identities(id),
    credentials_ref text,               -- K8s secret ref
    default_provider_id uuid REFERENCES llm_providers(id),
    default_model text NOT NULL DEFAULT '', -- LiteLLM/gateway model alias
    status        text NOT NULL DEFAULT 'idle'
                  CHECK (status IN ('idle', 'busy', 'error', 'deleted')),
    idle_timeout  interval NOT NULL DEFAULT '300s',
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (squad_id, name)
);

CREATE TABLE kanban_boards (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    squad_id      uuid NOT NULL UNIQUE REFERENCES squads(id) ON DELETE CASCADE,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE tasks (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    board_id      uuid NOT NULL REFERENCES kanban_boards(id) ON DELETE CASCADE,
    title         text NOT NULL,
    description   text NOT NULL DEFAULT '',
    status        text NOT NULL DEFAULT 'todo'
                  CHECK (status IN ('todo','in-progress','in-review','done','blocked')),
    assignee_agent_id uuid REFERENCES agents(id) ON DELETE SET NULL,
    created_by_type text NOT NULL CHECK (created_by_type IN ('user','agent')),
    created_by_id uuid NOT NULL,        -- user id or agent id (polymorphic)
    position      integer NOT NULL DEFAULT 0,
    metadata      jsonb NOT NULL DEFAULT '{}',
    version       integer NOT NULL DEFAULT 0,  -- optimistic concurrency
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_tasks_board_status ON tasks(board_id, status, position);
CREATE INDEX idx_tasks_assignee ON tasks(assignee_agent_id) WHERE status IN ('todo','in-progress');

CREATE TABLE task_executions (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    task_id          uuid NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    agent_id         uuid NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    worker_id        text NOT NULL,          -- runtime pod/process instance
    fencing_token    text NOT NULL UNIQUE DEFAULT gen_random_uuid()::text,
    status           text NOT NULL DEFAULT 'active'
                     CHECK (status IN ('active','completed','blocked','expired')),
    lease_expires_at timestamptz NOT NULL,
    result_status    text CHECK (result_status IN ('in-review','done','blocked')),
    result_summary   text NOT NULL DEFAULT '',
    started_at       timestamptz NOT NULL DEFAULT now(),
    completed_at     timestamptz,
    updated_at       timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_task_executions_task
    ON task_executions(task_id, status, lease_expires_at);
CREATE INDEX idx_task_executions_agent_active
    ON task_executions(agent_id, lease_expires_at)
    WHERE status = 'active';
```

`task_executions` is the durable runtime attempt/result table. The active row's
`fencing_token` must accompany complete/block calls; stale tokens are rejected.
Terminal updates store `result_status` and `result_summary` on the execution in
the same database transaction that moves the task to `in-review`, `done`, or
`blocked`.

---

## 4. Resource Registry

```sql
CREATE TABLE llm_providers (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name          text NOT NULL UNIQUE,
    type          text NOT NULL,        -- openai | anthropic | ollama | ...
    base_url      text NOT NULL,
    api_key_ref   text,                 -- secret ref
    default_model text NOT NULL DEFAULT '', -- default LiteLLM/gateway model alias
    models        jsonb NOT NULL DEFAULT '[]',  -- list of model ids
    pricing       jsonb,                -- { input_per_token, output_per_token, currency }
    status        text NOT NULL DEFAULT 'active'
                  CHECK (status IN ('active', 'deprecated')),
    registered_by uuid NOT NULL REFERENCES users(id),
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE skills (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name          text NOT NULL UNIQUE,
    description   text,
    package_ref   text NOT NULL,        -- image / repo / path
    version       text NOT NULL DEFAULT '1',
    status        text NOT NULL DEFAULT 'active' CHECK (status IN ('active','deprecated')),
    registered_by uuid NOT NULL REFERENCES users(id),
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE tools (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name          text NOT NULL UNIQUE,
    description   text,
    schema        jsonb NOT NULL DEFAULT '{}',  -- parameter JSON schema
    endpoint_ref  text,
    status        text NOT NULL DEFAULT 'active' CHECK (status IN ('active','deprecated')),
    registered_by uuid NOT NULL REFERENCES users(id),
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE apis (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name          text NOT NULL UNIQUE,
    description   text,
    base_url      text NOT NULL,
    auth_ref      text,
    spec_ref      text,                 -- OpenAPI spec ref
    status        text NOT NULL DEFAULT 'active' CHECK (status IN ('active','deprecated')),
    registered_by uuid NOT NULL REFERENCES users(id),
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE knowledge_bases (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name          text NOT NULL UNIQUE,
    description   text,
    vector_db_ref text NOT NULL,        -- connection secret ref
    collection    text NOT NULL,
    embedding_model text,
    status        text NOT NULL DEFAULT 'active' CHECK (status IN ('active','deprecated')),
    registered_by uuid NOT NULL REFERENCES users(id),
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE project_workspaces (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name          text NOT NULL UNIQUE,
    description   text,
    type          text NOT NULL CHECK (type IN ('git','jira','confluence')),
    endpoint      text NOT NULL,
    auth_ref      text,
    status        text NOT NULL DEFAULT 'active' CHECK (status IN ('active','deprecated')),
    registered_by uuid NOT NULL REFERENCES users(id),
    created_at    timestamptz NOT NULL DEFAULT now()
);
```

---

## 5. Permissions & Access Grants

```sql
-- Agent → registry resource grants (Layer 2 RBAC, squad-owner managed)
CREATE TABLE agent_permissions (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    agent_id      uuid NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    resource_type text NOT NULL
                  CHECK (resource_type IN
                    ('llm_provider','skill','tool','api','knowledge_base','project_workspace')),
    resource_id   uuid NOT NULL,
    granted_by    uuid NOT NULL REFERENCES users(id),
    created_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (agent_id, resource_type, resource_id)
);
CREATE INDEX idx_agent_permissions_agent ON agent_permissions(agent_id);

-- Cross-user / cross-squad access grants (owner-issued)
CREATE TABLE access_grants (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    squad_id      uuid NOT NULL REFERENCES squads(id) ON DELETE CASCADE,
    grantee_type  text NOT NULL CHECK (grantee_type IN ('user','agent')),
    grantee_id    uuid NOT NULL,        -- user id or agent id
    permissions   text NOT NULL DEFAULT 'talk',
    granted_by    uuid NOT NULL REFERENCES users(id),
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_access_grants_squad ON access_grants(squad_id);
CREATE INDEX idx_access_grants_grantee ON access_grants(grantee_type, grantee_id);
```

---

## 6. Messaging (v1 queue)

```sql
CREATE TABLE messages (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    from_type     text NOT NULL CHECK (from_type IN ('agent','user')),
    from_id       uuid NOT NULL,
    to_agent_id   uuid NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    squad_id      uuid NOT NULL REFERENCES squads(id) ON DELETE CASCADE,
    type          text NOT NULL
                  CHECK (type IN ('consult','delegate','handoff','ping','reply')),
    payload       jsonb NOT NULL DEFAULT '{}',
    status        text NOT NULL DEFAULT 'pending'
                  CHECK (status IN ('pending','delivered','expired','dead')),
    correlation_id uuid,                -- links a reply to its consult
    attempts      integer NOT NULL DEFAULT 0,
    max_attempts  integer NOT NULL DEFAULT 3,
    next_retry_at timestamptz NOT NULL DEFAULT now(),
    ttl           interval,
    expires_at    timestamptz NOT NULL DEFAULT now() + interval '24 hours',
    terminal_reason text NOT NULL DEFAULT '',
    created_at    timestamptz NOT NULL DEFAULT now(),
    delivered_at  timestamptz
);
CREATE INDEX idx_messages_inbox ON messages(to_agent_id, status, created_at)
    WHERE status = 'pending';
CREATE INDEX idx_messages_retry ON messages(to_agent_id, status, next_retry_at)
    WHERE status = 'pending';
CREATE INDEX idx_messages_squad ON messages(squad_id, created_at);
```

Current implementation note: the embedded migration creates this durable inbox
schema, and the control plane exposes enqueue, retry-due inbox listing,
acknowledgement, failure reporting, expiry, and dead-letter transitions.

---

## 7. Audit Log

```sql
CREATE TABLE audit_log (
    id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    actor_type    text NOT NULL CHECK (actor_type IN ('user','agent','system')),
    actor_id      uuid NOT NULL,
    action        text NOT NULL,        -- e.g. 'squad.create', 'task.assign'
    resource_type text,
    resource_id   uuid,
    squad_id      uuid,
    metadata      jsonb NOT NULL DEFAULT '{}',
    timestamp     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_audit_squad_time ON audit_log(squad_id, timestamp);
CREATE INDEX idx_audit_actor_time ON audit_log(actor_type, actor_id, timestamp);
-- Partition by month for retention (see below).
```

---

## 8. Metering

```sql
CREATE TABLE metering (
    id            bigint GENERATED ALWAYS AS IDENTITY,
    agent_id      uuid NOT NULL,
    squad_id      uuid NOT NULL,
    task_id       uuid REFERENCES tasks(id) ON DELETE SET NULL,
    model         text NOT NULL,
    provider      text NOT NULL,
    input_tokens  bigint NOT NULL DEFAULT 0,
    output_tokens bigint NOT NULL DEFAULT 0,
    cost          numeric(18,8),        -- null if no pricing configured
    currency      text,
    timestamp     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (id, timestamp)
) PARTITION BY RANGE (timestamp);
-- Create monthly partitions; retain raw for N months, then aggregate + drop.
CREATE INDEX idx_metering_agent_time ON metering(agent_id, timestamp);
CREATE INDEX idx_metering_squad_time ON metering(squad_id, timestamp);
CREATE INDEX idx_metering_task ON metering(task_id, timestamp);
```

Current implementation note: LiteLLM success callbacks create metering rows
through the internal gateway callback endpoint. Failure callbacks are recorded
as best-effort system audit entries, not metering rows.

---

## 9. Kubernetes Outbox

```sql
CREATE TABLE kubernetes_outbox (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregate_type  text NOT NULL,  -- squad | agent
    aggregate_id    uuid NOT NULL,
    operation       text NOT NULL,  -- upsert/delete squad/agent
    payload         jsonb NOT NULL DEFAULT '{}',
    status          text NOT NULL DEFAULT 'pending',
    attempts        integer NOT NULL DEFAULT 0,
    last_error      text NOT NULL DEFAULT '',
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    locked_until    timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);
```

- Squad and Agent mutations enqueue Kubernetes CR intents with the domain change
  so Postgres and the Kubernetes mirror request cannot diverge silently.
- Delete events include enough non-secret payload to delete CRs after the domain
  row is gone.
- Workers lease rows with `FOR UPDATE SKIP LOCKED`, apply idempotent Kubernetes
  writes/deletes, and record applied or failed state with retry scheduling.
- Raw credential and virtual-key token values are not stored in the outbox.

---

## 10. Agent Long-Term Memory (pgvector)

```sql
CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE agent_memory (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    agent_id      uuid NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    squad_id      uuid REFERENCES squads(id) ON DELETE CASCADE,
    content       text NOT NULL,        -- bounded summary or durable fact
    raw_content   text NOT NULL DEFAULT '',
    trust_level   text NOT NULL DEFAULT 'raw_model_output',
    provenance    text NOT NULL DEFAULT 'legacy',
    review_status text NOT NULL DEFAULT 'pending_review',
    embedding     vector(1536),         -- dimension matches embedding model
    embedding_model text NOT NULL DEFAULT '',
    source_task_id uuid REFERENCES tasks(id) ON DELETE SET NULL,
    metadata      jsonb NOT NULL DEFAULT '{}',
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_memory_agent ON agent_memory(agent_id);
CREATE INDEX idx_memory_squad ON agent_memory(squad_id) WHERE squad_id IS NOT NULL;
CREATE INDEX idx_memory_embedding ON agent_memory
    USING ivfflat (embedding vector_cosine_ops);  -- tune lists for data size
```

- **Per-agent** memory by default; runtime reads always constrain by `agent_id`.
- `squad_id` scopes memories to a squad/task context. Shared squad memory is a
  later policy decision, not implied by setting `squad_id` alone.
- `trust_level` distinguishes raw model output from distilled or verified
  memory. Runtime completion summaries are stored as `raw_model_output` and
  `pending_review`; they are contextual evidence, not trusted instructions.
- Semantic search uses cosine distance on `embedding` only when a query vector
  is supplied. Without a query vector, retrieval falls back to bounded recency.

Current implementation note: the embedded migration creates the pgvector-backed
memory table and indexes. The control plane now persists trust/provenance/review
metadata, optional embedding vectors, scoped recency retrieval, and
embedding-aware storage ranking. Automatic embedding generation, explicit
approval/distillation workflows, and artifact storage are still follow-up
implementation slices.

---

## 11. Relationships (summary)

```
users 1—* squads (owner)
squads 1—1 kanban_boards
squads 1—* agents
agents *—1 agent_identities
agents *—1 llm_providers (default provider)
agents default_model —> gateway model alias
agents *—* {registry resources} via agent_permissions
squads 1—* access_grants
kanban_boards 1—* tasks
tasks *—1 agents (assignee)
agents 1—* messages (inbox)
{squads,agents} 1—* kubernetes_outbox
agents 1—* agent_memory
agents 1—* metering
squads 1—* metering
```

---

## 12. Partitioning & Retention

| Table | Strategy |
|-------|----------|
| `metering` | Range-partition by `timestamp` (monthly). Retain raw N months; aggregate + drop older. |
| `audit_log` | Range-partition by `timestamp` (monthly). Retain longer (compliance). |
| `messages` | Prune `delivered`/`expired`/`dead` older than a retention window. |
| `kubernetes_outbox` | Retain failed rows until resolved; prune applied rows after an operational window. |
| `agent_memory` | Retain per agent; optional pruning of low-value rows (later). |

---

## 13. Security Notes

- **No raw secrets** in this schema — only `*_ref` (K8s secret references).
- **Kubernetes outbox payloads** include only non-secret CR state and Secret
  references, never raw runtime credentials or gateway keys.
- **Least-privilege DB roles** — the API server, gateway, and operator each get
  a role scoped to the tables they need.
- **Row-level isolation** — squad-scoped queries always filter by `squad_id`
  (enforced in the API layer + RLS optionally).
- **Audit** — all significant writes are mirrored to `audit_log`.

---

## 14. Open Points

- **Embedding dimension** — set to match the chosen embedding model (1536 for
  OpenAI `text-embedding-3-small`; adjust as needed).
- **RLS** — whether to enable Postgres Row-Level Security for squad isolation
  (defense-in-depth; start with app-level enforcement).
- **Metering aggregation** — materialized views / rollups for fast dashboards
  (later).
- **Soft vs hard delete** — confirm per table (squads cascade hard; users/agents
  soft where noted).
