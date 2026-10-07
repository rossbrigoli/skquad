# skquad — Resource Registry Design

> **Status:** Draft v1
>
> The **resource registry** is the catalog of things agents can use: **LLM
> providers, skills, tools, APIs, knowledge bases, and project workspaces**.
> The **platform admin registers** resources; the **squad owner grants** agents
> access to them.
>
> Registry CRUD, permission grants, and sanitized runtime resource discovery are
> implemented. Since TG-1…TG-8 the **tool gateway** is the enforcement point
> for `web`/`rest`/`mcp`/`git`/`browser` traffic, and grants carry risk
> tiers, ceilings, confirmation gates and standing grants (see
> [`tool-gateway.md`](tool-gateway.md),
> [`tg8-grant-approvals-spec.md`](tg8-grant-approvals-spec.md)). Deeper
> connector semantics remain follow-up work;
> see [`implementation-status.md`](implementation-status.md).

---

## 1. Purpose

- Give the platform a **single, governed catalog** of reusable resources.
- Keep agents **decoupled** from specific resources — an agent uses whatever it
  is **permitted** to use, resolved through the registry.
- Enable **BYOM** (LLM providers) and **extensibility** (skills, tools,
  connectors) without modifying the core.

---

## 2. Resource Types

| Type | What it is | Used by |
|------|------------|---------|
| **LLM Provider** | A model endpoint + models + optional per-token pricing. | LLM gateway (routing, metering, cost). |
| **Skill** | A packaged, reusable capability (prompt + logic). | Agent runtime (plugin). |
| **Tool** | A callable function the LLM can invoke. | Agent runtime (plugin). |
| **API** | An external HTTP endpoint. | Agent runtime (tool/connector). |
| **Knowledge Base** | A vector database collection. | Agent runtime (RAG connector). |
| **Project Workspace** | git repo / Jira / Confluence. | Agent runtime (workspace connector). |
| **MCP Server** (TG-5) | A registered streamable-HTTP MCP endpoint (snapshot + drift detection). | Tool gateway `mcp` driver. |
| **Browser session** (TG-6) | Managed Chromium, registered as an `mcp`-type resource whose grant selects `driver: "browser"`. | Tool gateway `browser` driver → `browser-service` quarantine zone. |

---

## 3. Registration (platform admin)

- Only the **platform admin** can **register** a resource.
- Registration stores the resource's **definition** (see per-type shape below)
  and any **credential reference** (a secret ref — the raw secret is never
  stored in the registry row).
- A registered resource is **active** and can be **granted** to agents.
- Resources can be **deprecated** (hidden from new grants; existing running
  agents unaffected).

```
Platform admin → "Register resource"
  → define type + fields + credential ref
  → resource is active in the registry
```

---

## 4. Granting (squad owner)

- The **squad owner** grants an **agent** access to specific registered
  resources (part of the agent's **permission set** — see
  [identity-security.md](identity-security.md)).
- Granting is per-agent and per-resource.
- Enforcement happens at the relevant point:
  - **LLM provider/model** → LLM gateway.
  - **Skill / tool / API** → agent runtime plugin loader.
  - **Knowledge base** → RAG connector.
  - **Project workspace** → workspace connector.

```
Squad owner → "Grant agent X access to resource Y"
  → agent X's permission set includes Y
  → enforced at the relevant component
```

### 4a. Ceilings, risk tiers and confirmation (TG-8, shipped 0.1.270)

Grants carry two governance fields beyond "allow":

- **`risk_tier`** — `low | medium | high`, deciding the grant path: BYO
  low/medium self-grants; shared medium needs owner approval; high needs
  owner approval **plus** platform-admin co-sign (grant-request workflow,
  owner Inbox).
- **`policy_ceiling`** — JSONB cap on what the grant may use (hosts,
  methods, MCP tools, numeric caps). Invariant: `effective ⊆ ceiling ∩
  grantor's delegable scope` — grants transfer, never amplify.
- **Confirmation gate** — `require_confirmation` (alias
  `requires_confirmation`) at grant or ceiling level, or per tool via
  `per_tool.<name>.require_confirmation`; sticky-true (either layer true ⇒
  gated). Gated calls pause at the gateway (HTTP 202
  `pending_confirmation`) until the owner decides **deny / approve-once /
  approve-standing**; the retry carries `X-Skquad-Confirmation-Id`. The
  gate is fail-closed: gateway cannot reach the control plane ⇒ 503, the
  tool never runs.
- **Standing grants** — "Approve This and Future" materializes a
  `(resource, agent, tool)` row with expiry (default +90 d). Every
  auto-passing call logs `matched_standing_grant_id` (audit honesty —
  standing approvals are never invisible); revocation is one row and is
  re-validated at consume time, so it is immediately effective.
- **Pre-effect linting** — every grant/ceiling change is machine-checked by
  `grantlint` before it takes effect or auto-approves; `block` findings
  (metadata paths, cluster-internal reach, new credentialed reach,
  widening) force owner review. See
  [`security-threat-model.md`](security-threat-model.md) T13.

---

## 5. Per-Type Shape

### 5.1 LLM Provider
```
llm_provider(
  id, name,
  type,            # openai | anthropic | ollama | ...
  base_url,
  api_key_ref,     # secret ref (credential)
  default_model,   # LiteLLM/gateway model alias used by default
  models,          # list of LiteLLM/gateway model aliases this provider serves
  pricing,         # { input_per_token, output_per_token } (optional)
  status           # active | deprecated
)
```
- Consumed by the **LLM gateway** for routing + metering + cost.
- `id` identifies the registry provider. It is not the model string sent to
  LiteLLM. Agents send `default_model` or an explicit model alias; the gateway
  maps that model to the provider and upstream endpoint.

### 5.2 Skill
```
skill(
  id, name, description,
  package_ref,     # where the skill package lives (image / repo / path)
  version,
  status
)
```
- A **skill** is a packaged capability (prompt + logic) loaded as a plugin by
  the agent runtime.

### 5.3 Tool
```
tool(
  id, name, description,
  schema,          # JSON schema of the tool's parameters
  endpoint_ref,    # how to invoke it (function ref / endpoint)
  status
)
```
- A **tool** is a callable function the LLM can invoke (exposed via the plugin
  interface).

### 5.4 API
```
api(
  id, name, description,
  base_url,
  auth_ref,        # secret ref (credential)
  spec_ref,        # OpenAPI spec (optional)
  status
)
```
- An external **HTTP endpoint** the agent can call (typically wrapped as a tool).

### 5.5 Knowledge Base
```
knowledge_base(
  id, name, description,
  vector_db_ref,   # connection to the vector DB (secret ref)
  collection,      # collection / index name
  embedding_model, # model used for embeddings
  status
)
```
- A **vector database collection** the agent can query via the RAG connector.
- (Distinct from the agent's own long-term memory, which is Postgres +
  pgvector.)

### 5.6 Project Workspace
```
project_workspace(
  id, name, description,
  type,            # git | jira | confluence
  endpoint,        # repo URL / Jira site / Confluence site
  auth_ref,        # secret ref (credential)
  status
)
```
- A **git repo / Jira / Confluence** the agent can read/write via a workspace
  connector.

### 5.7 Browser (via MCP, TG-6)
```
registry_resource(
  type = "mcp",              # browser rides the MCP resource type
  url,                       # browser-service MCP endpoint (:8090/mcp)
  status
)
# grant selects the driver and the session policy:
grant.config.driver = "browser"
grant policy: deny_hosts, max_pages, max_screenshot_bytes,
              idle_timeout_s, max_session_minutes,
              max_sessions_per_agent (default 1)
```
- The gateway `browser` driver creates a session bound immutably to
  `(agent_id, task_id, resource_id)` and injects the session token into
  tool args; agents never see or supply it.
- Egress is layered: quarantine netpol except-list → mandatory Chromium
  `--proxy-server` → netguard L7 floor. See
  [`tg6-browser-protocol.md`](tg6-browser-protocol.md) and threat-model
  T12.

---

## 6. Lifecycle

```
[admin registers] → active → deprecated
```
- **Register:** admin defines the resource + credential ref.
- **Active:** can be granted to agents; usable.
- **Deprecated:** hidden from new grants; existing running agents unaffected.
  (Prevents breaking changes from propagating.)

Current implementation note: the control plane exposes
`GET /api/v1/agents/me/resources` for agent-authenticated runtime discovery.
The response includes only active resources granted to that agent and omits
provider API-key refs and resource auth refs. Dynamic package loading still
belongs to the runtime/plugin-loader slice.

---

## 7. Credentials

- Resources that need credentials (LLM providers, APIs, KBs, workspaces) store a
  **secret reference** (`*_ref`), not the raw secret.
- Secrets live in **K8s** (optionally an external secrets manager).
- The **control plane** resolves the ref when a resource is used; it never
  returns raw secrets to clients.
- **Least privilege:** each resource's credential is scoped to that resource.

---

## 8. Relationship to Other Components

- **LLM gateway** — consumes **LLM provider** entries (routing, metering, cost).
- **Agent runtime** — loads **skills/tools/APIs** as plugins; uses **KB** and
  **workspace** connectors.
- **Identity & AuthZ** — the agent's **permission set** references registry
  resources (granting).
- **Postgres** — stores registry entries + credential refs.
- **Web app** — admin UI to register resources; owner UI to grant access.

---

## 9. Extensibility

- New **resource types** can be added by extending the registry schema + adding
  a connector — without changing the core.
- New **skills/tools** are just registry entries + packages (see
  [plugin-architecture.md](plugin-architecture.md)).

---

## 10. Open Points

- **Versioning** of resources (skills/tools) — multiple versions of the same
  resource (start with a single `version` field; add full versioning later).
- **Scoping** — whether some resources are platform-wide vs. per-tenant (start
  platform-wide).
- **Discovery** — search/filter the registry in the web app (later).

## AI Model binding — drill notes (2026-09-25, S-114)

- Binding an agent's `ai_model_id`/`fallback_ai_model_id` via `PATCH
  /api/v1/agents/{id}` is **owner-only** (`ensureSquadAccess(ownerOnly=true)`);
  `platform_admin` (incl. break-glass) cannot rebind another user's squad agent.
  Provisioning/rotating identity and binding therefore must be done by the squad
  owner (UI) — the break-glass path can grant models and read, but not rebind.
- A user grant (`PUT /api/v1/users/{id}/models`) is required before a model can
  be bound to that owner's agents; the binding allow-list compiled onto the
  virtual key is derived from the owner's grants (ADR-0010).
- Creating an AI Model requires all four pricing fields
  (`input_per_1m`, `cached_input_per_1m`, `cache_write_per_1m`, `output_per_1m`).
- Deleting a granted/bound AI Model returns `409 in_use`; revoke grants and
  unbind agents first (or `?force=true`).
