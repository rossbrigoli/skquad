# skquad — Central LLM Gateway Design

> **Status:** Draft v1 · **Decision:** [ADR-0002](adr/0002-llm-gateway.md)
>
> The LLM gateway is the **single, model-agnostic endpoint** that **all agent
> LLM calls** flow through. It is intended to be the enforcement point for
> **metering, cost, BYOM routing, and agent permissions**. It is implemented
> with the **LiteLLM proxy**.
>
> The LiteLLM proxy, virtual-key provisioning, the metering callback path, and
> key lifecycle enforcement (update/revoke on permission change, drift
> reconciliation) are implemented. Full budget hardening remains follow-up
> work; see [`implementation-status.md`](implementation-status.md).

---

## 1. Why a Central Gateway

If every agent called LLM providers directly, we would:

- Duplicate **metering** logic in every agent.
- Scatter **credential** handling across agents.
- Make agent code **model-specific** (breaking BYOM).

A central gateway solves all three in one place: agents call **one endpoint**
with **one virtual key**, and the gateway handles routing, metering, cost, and
permissions.

---

## 2. Responsibilities

| Responsibility | Detail |
|----------------|--------|
| **Model-agnostic routing** | Agents request a *model*; the gateway routes to the correct upstream provider (OpenAI, Anthropic, Ollama, …). |
| **BYOM** | Routes to the right provider with the right credentials (from the registry / secrets). Users can bring their own provider + key. |
| **Metering** | Counts **input + output tokens** per call and attributes them to the **agent** and **squad**. |
| **Cost** | Computes **monetary cost** when the provider has **per-token pricing** configured. |
| **Permission enforcement** | An agent can only use **providers/models it is permitted** to use (checked against the agent's permission set). |
| **Virtual keys** | Issues a **virtual key per agent** so calls are attributable, rate-limitable, and budgetable. |
| **Rate limiting / budgets** | Per-agent (and per-squad) rate limits and optional spend budgets. |

---

## 3. Architecture

```mermaid
flowchart LR
    A1[Agent Pod 1] -->|virtual key| GW
    A2[Agent Pod 2] -->|virtual key| GW
    B1[Agent Pod 3] -->|virtual key| GW
    subgraph GW["LLM Gateway (LiteLLM proxy) — control plane"]
        R[Router] --> P[Permission check]
        P --> M[Metering + cost]
        M --> U[Upstream provider]
    end
    U --> O1[OpenAI]
    U --> O2[Anthropic]
    U --> O3[Ollama / self-hosted]
    M --> DB[("Postgres<br/>metering")]
    REG[Resource Registry<br/>providers + pricing] --> GW
```

- The gateway runs as a **highly-available Deployment** in the control plane
  (`skquad-system`).
- It is **stateless** — all state (metering, keys, budgets) lives in Postgres.
- It reads **provider definitions** (base URL, models, pricing) from the
  **resource registry** and **credentials** from secrets.

---

## 4. Virtual Keys

- Each **agent** is issued a **virtual key** by the gateway (via the control
  plane) when the agent identity is created or rotated and the LiteLLM admin
  URL/master key are configured.
- The agent uses **only its virtual key** to call the gateway — it never holds
  upstream provider credentials.
- The virtual key encodes the **agent identity** (and squad), so every call is
  attributable for metering, permissions, and rate limiting.
- Keys can be **rotated** and **revoked** (e.g. when an agent is deleted or its
  permissions change).

Current implementation note: the control plane calls LiteLLM `/key/generate`
with the model aliases from the agent's active `llm_provider` grants, writes
the returned raw key into the generated Kubernetes Secret, and stores the
Secret ref plus the returned key **token** (LiteLLM's hash of the key, never
the raw key) on the agent identity as `gateway_key_token` /
`gateway_key_status`. If LiteLLM admin settings are absent, local development
falls back to a generated opaque token.

### Key lifecycle enforcement (implemented)

- **Permission change (`PUT /agents/{id}/permissions`)**: the gateway key is
  synced **before** the permission commit. Models added → `/key/update` with
  the new allow-list; last LLM grant removed → `/key/delete` (revoked).
  A gateway failure aborts the permission change (502) so a revoked grant
  never keeps a live key.
- **Re-grant after revocation**: a fresh key is provisioned against the
  existing identity and written to the agent's virtual-key Secret.
- **Agent/squad deletion**: active keys are revoked at the gateway before the
  rows are deleted, so no untracked key survives.
- **Provider deprecation**: all agents granted the deprecated provider are
  re-converged (update, or revoke if no active models remain).
- **Drift reconciliation**: `POST /api/v1/admin/gateway/keys/reconcile`
  (platform admin) re-converges every agent's key against current grants
  and is idempotent; use it after partial failures. The identity-record
  write after a commit failure is audited as
  `*.gateway_key_record_stale` for exactly this repair path.
- **Runtime behavior on revoked keys**: LiteLLM rejects calls with revoked
  keys (auth error), which fails the task — fail-closed by design.

---

## 5. Metering & Cost

- For every completed call, the gateway records:
  - **agent id**, **squad id**, **model**, **provider**
  - **input tokens**, **output tokens**
  - **timestamp**
  - **cost** (if the provider has per-token pricing)
- **Cost calculation:** `cost = input_tokens × price_per_input_token +
  output_tokens × price_per_output_token` (prices from the provider's registry
  entry).
- Records are written to the **metering table** in Postgres (see
  [data-model.md](data-model.md)).
- The **web app** aggregates metering per agent and per squad (and shows cost
  where pricing is configured).
- Current implementation uses a LiteLLM custom callback
  (`skquad_litellm_callbacks.proxy_handler_instance`) loaded by the proxy. The
  runtime attaches agent, squad, and task metadata to every LiteLLM call; the
  callback posts successful usage to the control plane's internal
  `/api/v1/gateway/metering` endpoint. Gateway failures post an audit-only
  event. Callback delivery and audit writes are best-effort; the metering row
  is the durable success record.

```
POST /v1/chat/completions  (agent → gateway)
  → route to provider
  → on response: record {agent, squad, model, in_tokens, out_tokens, cost, ts}
  → return response to agent
```

---

## 6. BYOM Routing

- The **resource registry** holds **LLM provider** entries: `type`, `base_url`,
  `api_key_ref` (secret), `default_model`, `models`, `pricing`.
- The provider `id` is registry identity only. Agents send a concrete
  LiteLLM/gateway **model alias** such as `openai/gpt-4o-mini` or
  `ollama/llama3.2`.
- The gateway maps the requested model alias to its **provider** and routes the
  call to the provider's `base_url` using the provider's credentials.
- **Predefined providers** (admin-registered) power the fast onboarding path.
- **BYOM providers** (user-registered, with the user's key) are routed the same
  way — the agent just requests a model.

---

## 7. Permission Enforcement

- Before routing a call, the gateway checks the **agent's permission set**
  (from the control plane): is this agent permitted to use this **provider /
  model**?
- If not → **reject** the call (403) and log the attempt (audit).
- This means an agent can only spend on / use models its **squad owner** allowed.

---

## 8. API Surface (agent-facing)

The gateway exposes an **OpenAI-compatible** API (so LiteLLM and agent code stay
simple):

- `POST /v1/chat/completions`
- `POST /v1/completions`
- `GET /v1/models` (models the calling agent is permitted to use)

Control-plane management endpoints (for the API server, not agents):

- `POST /key/generate` (issue a virtual key for an agent)
- `POST /key/info`, `POST /key/update`, `POST /key/delete`
- `GET /global/spend`, `GET /spend` (metering queries)

Skquad uses `/key/generate` at identity create/rotate and on re-grant,
`/key/update` when a permission change alters the model allow-list, and
`/key/delete` on revocation, agent deletion, squad deletion, and full
provider deprecation. Budget enforcement remains follow-up work.

---

## 9. High Availability & Operations

- Run as a **Deployment** with ≥2 replicas; it is stateless.
- **Health checks** (`/health/liveliness`, `/health/readiness`).
- **Metrics** (Prometheus): requests, tokens, cost, latency, errors, per
  provider/model.
- **Alerts** on error rate, latency, and spend anomalies.
- **Backpressure / rate limiting** per virtual key to protect upstream providers.

---

## 10. Relationship to Other Components

- **Agent runtime** → calls the gateway (the only LLM path).
- **Resource registry** → provides provider definitions + pricing.
- **Identity & AuthZ** → provides the agent's permission set (for enforcement).
- **Postgres** → stores metering, keys, budgets.
- **Web app** → reads metering/cost for display.

---

## 11. Open Points

- **Caching** — semantic/exact caching of repeated calls to cut cost (later).
- **Fallbacks** — automatic failover to a secondary model on provider errors
  (later).
- **Budgets** — hard spend caps per agent/squad with automatic cutoff (later).
- **Streaming** — support streaming responses end-to-end (confirm in
  implementation).
