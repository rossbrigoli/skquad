# Implementation Plan — AI Model binding (ADR-0010)

> **Note (2026-09-26):** `web-v2` is now `web` (v1 UI retired 2026-09-26); `web-v2` references in this document mean the current `web/` app.

**Goal:** replace `llm_provider`-as-agent-grant with admin-registered **AI Models** granted to
**users**, bound 1:1 to an agent as primary + optional fallback.

**Branch:** `feat/ai-model-binding` off `main`.
**Not in scope:** S-104 operator Ready/credential fixes (already shipped separately).

---

## WP1 — Schema + domain types

**Objective:** new tables and the agent-binding columns, with memory + postgres stores.

- Migration `0011_ai_models.sql`:
  - `ALTER TABLE llm_providers RENAME TO providers;` (internal credential holder:
    `id, name, kind, base_url, api_key_ref, status, registered_by, created_at`).
    Drop `models`, `default_model`, `pricing` from it (they move to `ai_models`).
  - `CREATE TABLE ai_models (id, provider_id FK→providers ON DELETE RESTRICT, display_name,
     model_name, context_window int, supports_tools bool, pricing jsonb,
     long_context_threshold_tokens int, status IN ('active','deprecated'), registered_by,
     created_at, updated_at, UNIQUE(provider_id, model_name))`
  - `CREATE TABLE user_model_grants (id, grantee_user_id FK→users ON DELETE CASCADE,
     ai_model_id FK→ai_models ON DELETE CASCADE, granted_by FK→users, created_at,
     UNIQUE(grantee_user_id, ai_model_id))`
  - `ALTER TABLE agents ADD COLUMN ai_model_id uuid REFERENCES ai_models(id)`,
    `ADD COLUMN fallback_ai_model_id uuid REFERENCES ai_models(id)`
    + `CHECK (fallback_ai_model_id IS NULL OR fallback_ai_model_id <> ai_model_id)`
  - Backfill placeholders so `NOT NULL` can be added **later** (WP8), not now — avoids
    blocking on unmapped data.
- `domain/types.go`: `AIModel`, `UserModelGrant`; `Agent.AIModelID`, `Agent.FallbackAIModelID`.
- `storage/` — Store interface + memory + postgres CRUD:
  `CreateAIModel/GetAIModel/ListAIModels/UpdateAIModel/DeleteAIModel`,
  `GrantModelToUser/RevokeModelFromUser/ListUserModelGrants/ListUsersGrantedModel`,
  agent read/write for the two new columns.
- Tests: store round-trip, unique constraints, FK RESTRICT on provider delete while models exist.

**DoD:** `go test ./...` green in `control-plane/`.

---

## WP2 — Control-plane API: AI Model CRUD + user grants

**Objective:** admin-facing HTTP surface.

- `GET/POST /api/v1/ai-models`, `GET/PATCH/DELETE /api/v1/ai-models/{id}` (platform_admin).
  - Pricing validated: 4 numeric rates (`input_per_1m`, `cached_input_per_1m`,
    `cache_write_per_1m`, `output_per_1m`) + optional `long_context_threshold_tokens`.
  - `DELETE` → `409 in_use` listing affected users + agents unless `?force=true`.
- `GET /api/v1/users/{id}/models`, `PUT /api/v1/users/{id}/models` (platform_admin) —
  set grant set idempotently.
- `GET /api/v1/models/me` — the calling user's granted, active models (drives the agent UI).
- Remove `llm_provider` from the grantable-type whitelist (`server.go:474`) and from
  `ensureRegistryResourceExists`; return `400 provider_not_grantable` with a pointer to
  Settings → AI Models.
- Tests: authz (admin vs user), validation, 409/force, `/models/me` filtering by status.

**DoD:** handler tests green; OpenAPI/docs table updated.

---

## WP3 — Key provisioning: allow-list `{primary, fallback}` + router fallback

**Objective:** the compile-time chain from ADR-0010 D5/D6.

- Replace `allowedGatewayModels` (grant-union) with `boundModelAllowList(agent)`:
  resolves `ai_model_id` (+ `fallback_ai_model_id`) to model names; empty ⇒ precise error.
- `provisionAgentVirtualKey`:
  - no primary bound → `409 model_not_bound` (**replaces the misleading `no_llm_models_granted`**)
  - primary bound but not granted to the agent's owner → `403 model_not_granted`
  - key `models` = `[primary, fallback]` — **fallback included deliberately** so it is
    reachable during an outage.
- Pass fallback to the gateway: `GatewayKeyRequest{Models, FallbackModel}` → LiteLLM key/router
  config so the router performs the failover (no runtime change).
- Failure-class policy per D7 configured at the router: fall back on transport/5xx/timeout/429-after-retry;
  **not** on 400; upstream 401/403 → fall back **and** emit an alert event.
- Update `syncAgentGatewayKey` so binding changes converge the key in both directions.
- Tests: allow-list contains fallback; unbound → `model_not_bound`; ungranted → 403;
  sync updates when fallback added/removed.

**DoD:** provisioning + reconcile paths covered by tests.

---

## WP4 — Revoke cascade (D9)

**Objective:** revoking a model from a user deterministically converges affected agents.

- `RevokeModelFromUser` → enumerate agents owned by that user bound to the model
  (primary or fallback slot).
- If any and no `force` → `409 in_use {agents:[{id,name,squad_id,slot}]}`.
- With `force`: clear the slot, re-provision or revoke each key via `syncAgentGatewayKey`,
  write audit, enqueue owner inbox notification.
- Same cascade on AI Model **deprecation** (deprecate = stop granting, converge keys).
- Tests: warn → force → keys converged; deprecate path; audit recorded.

**DoD:** no path leaves a live key able to call a revoked model.

---

## WP5 — Operator CR + runtime wiring + `model_used` metering

**Objective:** make the binding visible downstream and make fallback usage observable.

- `cr_writer.go`: emit `aiModelId`, `fallbackAiModelId` on the Agent CR.
- `operator/api/v1/types.go` + `agent_controller.go`: env
  `SKQUAD_AI_MODEL_ID`, `SKQUAD_FALLBACK_MODEL_ID` (keep `SKQUAD_DEFAULT_MODEL` populated
  from the AI Model's `model_name` for runtime compatibility).
- `agent-runtime`: resolve model from the AI Model id/name; **record `model_used`** on every
  turn (the model that actually answered, not the requested one) and report it in metering.
- `metering` row: store `model_used`, tokens, and the **rate snapshot** used (D8).
- Tests: CR fields present; runtime records `model_used`; metering stores rate snapshot.

**DoD:** a turn served by the fallback is distinguishable in metering.

---

## WP6 — web-v2 Settings: AI Models + per-user grants

**Objective:** admin screen.

- Settings → **AI Models**: list/create/edit/deprecate. Form fields: provider (internal,
  pick-or-create), model name, context window, supports tools, 4 pricing rates,
  long-context threshold.
- Settings → **Access**: user list → per-user model grant editor (checkbox grid).
- Delete/deprecate shows the WP4 `409 in_use` dialog (affected users + agents, "force").
- Tests: render + grant/revoke interactions + 409 flow.

**DoD:** an admin can register a model and grant it to a user end-to-end.

---

## WP7 — web-v2 agent LLM tab: primary + fallback pickers

**Objective:** the agent-side half of the UX.

- Replace provider/model inputs with:
  - **Model** dropdown → `/models/me` (granted + active only), required.
  - **Fallback model** dropdown → same list, optional, primary excluded.
- Bind-time **soft warnings** (non-blocking, per ADR-0010 Risks 1–2):
  - fallback input rate > primary input rate → "fallback is more expensive"
  - fallback `supports_tools=false` or `context_window < primary` → "may break tool calling"
  - fallback on the same provider as primary → "no availability gain if that provider is down"
- Remove the old Permissions-tab LLM-provider grant UI entirely.
- Tests: filtered list, warning conditions, save payload.

**DoD:** agent page can set primary + fallback and shows the three warnings correctly.

---

## WP8 — Data migration + drop the old grant type

**Objective:** move live data onto the new model, then remove the old door.

> **Sequencing correction (caught in WP1 review, 2026-09-24):** WP1 originally dropped
> `providers.models` / `default_model` / `pricing`. That would have destroyed the only
> backfill source **against the live database** before any mapping existed. The drops are now
> deferred to the END of WP8. WP1 leaves those columns present but deprecated/read-only.

- **Pre-flight, mandatory:** `pg_dump` of `providers`, `agents`, `agent_permissions` retained
  before running anything.
- Script/migration `0012_ai_model_backfill.sql` (+ Go migration helper if logic needed):
  1. Read the **retained legacy** `providers.models[]` / `default_model` / `pricing` →
     `ai_models` rows inheriting the provider credential.
  2. Each agent's `llm_provider` grants → grant those models to the **agent owner** (union).
  3. `agents(default_provider_id, default_model)` → `agents.ai_model_id` where the pair resolves.
  4. Unresolved agents → report to an admin "needs binding" list; do **not** guess.
  5. `NOT NULL` on `agents.ai_model_id` only after step 3 reaches 100%.
  6. Drop `'llm_provider'` from the `agent_permissions` CHECK constraint.
  7. Drop legacy `providers.models` / `default_model` / `pricing` and
     `agents.default_provider` / `default_model` — **last**, never before step 3 is verified.
- Dry-run mode that prints the mapping table and unresolved rows **before** mutating.
- Tests: fixture → expected mapping; unresolved rows surfaced.

**DoD:** staging dry-run clean; no agent silently reassigned.

---

## WP9 — Verification & rollout

1. Full `go test ./control-plane/...`, `agent-runtime` tests, `web-v2` vitest + `next build`.
2. CI green on the branch; image publish; confirm image exists.
3. GitOps: bump tags in `k3s-cluster`, trigger ArgoCD sync, confirm rollout.
4. **S-104 unblock:** grant the Halogen AI Model to Ross, bind it to Enzo, click
   *Provision Identity* → expect 201, key allow-list includes fallback if set.
5. Fallback drill: make the primary unreachable, confirm the agent transparently fails over,
   `model_used` shows the fallback, and a 400-class error does **not** trigger failover.
6. Update `docs/llm-gateway.md`, `docs/resource-registry.md`, `docs/data-model.md`,
   `docs/adr/README.md`, `WORKLOG.md`.

---

## Sequencing & dependencies

```
WP1 ──► WP2 ──► WP3 ──► WP4 ──┐
                └─► WP5 ────────┼──► WP8 ──► WP9
      WP6, WP7 (after WP2 API) ┘
```

WP6/WP7 can proceed once WP2's API shape is fixed. WP8 must come after WP1–WP4 are deployed,
and its `NOT NULL` + constraint-drop steps only after the dry-run is clean.

## Standing rules for each WP

- One focused change per sub-agent task; verify with the narrowest test set before moving on.
- Commit per WP, reference the Kanbunny card ref in the commit message.
- Never run `kubectl apply` directly — GitOps only.
- Stop and escalate rather than retry-loop on any 401/auth or migration ambiguity.
