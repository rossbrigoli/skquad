# WP8 Migration Plan — AIModel-8 / S-113: Backfill agent model bindings & drop the `llm_provider` grant type

Status: 0014 IMPLEMENTED (this branch, WP8 closeout) — legacy code paths removed from control-plane + web-v2, and `0014_drop_legacy_llm_provider.sql` written (delete llm_provider grants → tighten resource_type CHECK → drop agents.default_provider/default_model + providers.default_model/models/pricing; idempotent). Prior: 0013 backfill live since 2026-09-24; all 3 agents bound; gates V1-V3 = 0. 0014 ships on next deploy per the ≥1-release-clean gate (docs §3).
> **Note (2026-09-26):** `web-v2` is now `web` (v1 UI retired 2026-09-26); `web-v2` references in this document mean the current `web/` app.

Date: 2026-09-24
Baseline: `main` @ 3dbb458 (ADR-0010 epic WP1–WP7 merged)

## 0a. LIVE DRY-RUN RESULTS (skquad-system/skquad-postgres-0)

**Live DB is at 0010** — epic migrations (0011/0012) NOT yet applied; deployed api-server predates the merge. Backfill sequence must be: deploy epic control-plane (0011+0012) → 0013 backfill → step-4 cutover → 0014 drops.

Verified shapes:
- `llm_providers.models` = **plain JSON string array** ✓ (`["halogen-qwen3.8-flash-next"]`)
- `llm_providers.pricing` = **`{}` on BOTH providers** → every backfilled `ai_models` row starts zero-priced until admin fills rates (Halogen = own hardware, plausibly intentional; Local LLM likewise)
- DB column is **`agents.default_provider`** (NOT `default_provider_id` — that's only the JSON name). Plan SQL corrected.
- Agents (3): `test agent`, `Enzo`, `Mary` — ALL → Halogen + `halogen-qwen3.8-flash-next`. **100% resolvable** once the ai_models row exists. Unresolvable list: EMPTY.
- `agent_permissions`: exactly **1 `llm_provider` row** (inert; 0014 deletes).
- Provider `Local LLM` (default_model="default", models=[]) has no agents; contributes no backfill rows; candidate for plain deletion by admin.

Implication: backfill is trivial at current scale — one ai_models row (Halogen/halogen-qwen3.8-flash-next), three agent bindings, zero-priced.

## 0. Numbering correction

`0011_ai_models.sql` header says the deferred drops happen in `0012_ai_model_backfill.sql`,
but that slot is taken by `0012_metering_rate_snapshot.sql` (WP5). This plan uses:

- **`0013_ai_model_backfill.sql`** — backfill only (additive, idempotent, reversible)
- **`0014_drop_legacy_llm_provider.sql`** — drops (point-of-no-return, gated on verification)
- Fix the stale comment in 0011 as part of 0013's PR.

## 1. Inventory

### 1a. STAYS — provider registry (credential holders)

| Item | Location | Note |
|---|---|---|
| `providers` table (id, name, kind, base_url, api_key_ref, status, registered_by, created_at) | migrations/0011 | Renamed from `llm_providers`; still the credential holder |
| `/registry/llm-providers` CRUD handlers | server.go:611–1008 | Registry admin stays; only the legacy fields inside payloads go |
| `ResLLMProvider` audit resource type | domain/types.go:379 | Still used for registry audit actions (`registry.llm_provider.*`); rename to `provider` is cosmetic, defer |

### 1b. GOES — grant type

| Item | Location | Note |
|---|---|---|
| `'llm_provider'` in `agent_permissions.resource_type` CHECK | migrations/0001_init.sql:186–187 | Tighten CHECK in 0014 after deleting rows |
| Existing `llm_provider` permission rows | live DB | Inert since S-107 (new grants rejected at server.go:2062–2067); gateway now derives keys from bindings |
| S-107 rejection guard | server.go:2062–2067 | **Keep one release past 0014** — harmless, catches stale clients |
| web v1 grant UI | SquadsSection.tsx:520–522 | Remove with v1 retirement (see §3 step 3) |
| web v1 `ResourceType` union entry | web/src/lib/api.ts:107 | Remove with v1 |
| web-v2 filter for legacy rows | agents/[agentId]/page.tsx:85–88, 232 | Keep until 0014 shipped, then simplify filter |

### 1c. GOES — legacy agent fields (`default_provider_id`, `default_model`)

| Layer | Location | Note |
|---|---|---|
| Domain | domain/types.go:76–77 (`DefaultProvider`, `DefaultModel`) | After backfill + code cutover |
| HTTP create/update | server.go:1393–1419 (POST), 1462–1497 (PATCH) | Remove fields from request structs |
| Storage | postgres.go:605, 2845 | Column refs; drop with 0014 |
| CR writer | kube/cr_writer.go:112–114, 293–301 (`agentDefaultModel` fallback) | Simplify: binding-derived name only |
| Outbox | kube/outbox_worker.go:130 | Falls back to legacy text — remove after cutover |
| Operator API types | operator/internal/api/v1/types.go:96–97 | CRD spec fields |
| Operator env injection | operator/internal/controller/agent_controller.go:105–106 (`SKQUAD_DEFAULT_PROVIDER_ID`, `SKQUAD_DEFAULT_MODEL`) | Remove after runtime no longer reads them |
| CRD schema | charts/skquad/crds/skquad.io_agents.yaml:46–48 (`defaultProviderId`, `defaultModel`) | K8s prunes unknown fields — removal is silent-tolerant for existing CRs |
| agent-runtime | runtime.py:45–46, 314–315, 733, 837, 1085–1087 | `model = self.model or config.default_model or config.default_provider_id` → binding env only |
| web v1 | page.tsx:86, 504–514, 545, 560; SquadsSection.tsx:122–123, 467, 590–591, 994; api.ts:26–27 | **Active writer — see §3** |
| web-v2 | AgentForm.tsx:11–12; agents/page.tsx:56 (display) | Remove in code-removal PR |

### 1d. GOES — provider model columns (backfill source)

| Column | Shape (from 0001) | Note |
|---|---|---|
| `providers.default_model` | `text NOT NULL DEFAULT ''` | Legacy free-text |
| `providers.models` | `jsonb NOT NULL DEFAULT '[]'` | **Verify actual shape with dry-run before INSERT mapping** — array of names vs objects |
| `providers.pricing` | `jsonb NOT NULL DEFAULT '{}'` | **Verify key shape** (per-model vs flat) before copying into `ai_models.pricing` |

## 2. Backfill design (0013 — additive, idempotent)

Verified constraints that make this safe: `ai_models UNIQUE (provider_id, model_name)` (0011:63);
`agents.fallback_nequals_primary` CHECK (0011:87–89).

### Step A — dry-run (SELECT only, run first, capture output)

```sql
-- A1: what exists
SELECT id, name, default_model, models, pricing FROM providers ORDER BY name;

-- A2: agents needing binding (have legacy pair, no ai_model_id)
SELECT a.name, a.default_provider_id, a.default_model, p.name AS provider_name
FROM agents a
LEFT JOIN providers p ON p.id::text = a.default_provider_id
WHERE a.ai_model_id IS NULL
  AND (coalesce(a.default_provider_id,'') <> '' OR coalesce(a.default_model,'') <> '');

-- A3: resolvable pairs (provider+model_name exists in ai_models)
SELECT a.name AS agent, am.id AS ai_model_id
FROM agents a
JOIN ai_models am ON am.provider_id::text = a.default_provider_id
                AND am.model_name = a.default_model
WHERE a.ai_model_id IS NULL;

-- A4: UNRESOLVABLE pairs — the honest list. No silent guesses.
SELECT a.name AS agent, a.default_provider_id, a.default_model, p.name AS provider_name
FROM agents a
LEFT JOIN providers p ON p.id::text = a.default_provider_id
LEFT JOIN ai_models am ON am.provider_id::text = a.default_provider_id
                     AND am.model_name = a.default_model
WHERE a.ai_model_id IS NULL
  AND coalesce(a.default_model,'') <> ''
  AND am.id IS NULL;
```

### Step B — create missing `ai_models` rows from provider metadata

For each (provider, model) in `providers.models` / `providers.default_model` not already in
`ai_models`: INSERT with pricing copied from `providers.pricing` **after mapping the verified
json shape** to the four `*_per_1m` fields. `ON CONFLICT (provider_id, model_name) DO NOTHING`.
New rows start `status='active'`; admin review list captured in migration output.
If `providers.pricing` lacks an entry for a model → insert with zero pricing and emit a
warning list (do NOT invent rates; costs show as zero until admin fixes them).

### Step C — bind agents (single transaction)

```sql
UPDATE agents a
SET ai_model_id = am.id
FROM ai_models am
WHERE a.ai_model_id IS NULL
  AND am.provider_id::text = a.default_provider_id
  AND am.model_name = a.default_model;
```

Idempotency: every statement is `IS NULL`-guarded or `ON CONFLICT DO NOTHING` — safe to re-run.
Unresolvable agents (A4) are **left unbound** and reported; unbound agents keep working via
platform-default path until an admin binds them in the Settings/agent UI.

Fallback bindings: legacy schema has no fallback concept → `fallback_ai_model_id` stays NULL. Expected.

## 3. Ordering vs deploy (critical path)

**The blocker is web v1: it still WRITES `default_provider_id`/`default_model` on agent
create/update (page.tsx:513–514, 560).** Until v1 is retired or rewired, legacy fields keep
receiving new writes and the backfill is a moving target.

1. **Pre**: `pg_dump` of skquad DB (rollback point R1). Run §2 Step A dry-run against live DB.
2. **0013 backfill** (additive). Re-run dry-run A2/A3 to confirm coverage. New agents created
   via v1 after this point still get legacy fields only → re-run 0013 Step C once more just
   before v1 retirement (idempotent).
3. **Retire web v1** (or gate its agent create/edit to read-only). This stops legacy writes.
   ← decision needed from Ross: v1 retirement date/mechanism.
4. **Deploy code cutover PR**: CR writer + outbox stop falling back to legacy text (binding
   only); web-v2 AgentForm legacy fields removed; operator env vars for SKQUAD_DEFAULT_* removed.
   Runtime already prefers binding-derived model (WP5); verify in staging agent.
5. **0014 drops** (after ≥1 release of step-4 code running clean):
   - `DELETE FROM agent_permissions WHERE resource_type='llm_provider';`
   - Tighten CHECK: `resource_type IN ('skill','tool','api','knowledge_base','project_workspace')`
   - `ALTER TABLE agents DROP COLUMN default_provider_id, DROP COLUMN default_model;`
   - `ALTER TABLE providers DROP COLUMN default_model, DROP COLUMN models, DROP COLUMN pricing;`
   - CRD: remove `defaultProviderId`/`defaultModel` (pruned silently from live CRs — harmless)
6. **Cleanup PR**: remove S-107 guard (server.go:2062–2067), web-v2 legacy-row filters,
   domain fields, storage refs.

Rollback: R1 before step 2; after step 2 rollback = restore R1 or leave additive rows (harmless).
After 0014 = no rollback except R1 restore. Do not ship 0014 until step 4 has run ≥1 release clean.

## 4. Drop list (0014)

| Object | Dependency check before drop |
|---|---|
| `agents.default_provider_id`, `agents.default_model` | grep confirms no live reader after step-4 deploy; §6 V2 = 0 |
| `providers.default_model`, `providers.models`, `providers.pricing` | §6 V3 confirms all models represented in ai_models |
| `agent_permissions` CHECK incl. `llm_provider` | DELETE rows first (same txn); no FKs reference resource_type |
| CRD props `defaultProviderId`, `defaultModel` | K8s structural pruning = silent drop; operator no longer reads after step 4 |

## 5. Code removal list (step 4 cutover vs step 6 cleanup)

**Step 4 (cutover, before 0014):** cr_writer.go:293–301 + 112–114 (binding-only),
outbox_worker.go:130 fallback, web-v2 AgentForm.tsx:11–12, agents/page.tsx:56 display,
operator agent_controller.go:105–106 + types.go:96–97, runtime.py:45–46/314–315/733/837/1085–1087.

**Step 6 (cleanup, after 0014):** S-107 guard server.go:2062–2067, web-v2 agent page
legacy filters (page.tsx:85–88, 232), domain Agent.DefaultProvider/DefaultModel (types.go:76–77),
storage postgres.go:605/2845, HTTP request struct fields (server.go:1393–1419, 1462–1497),
web v1 retirement (whole app or route freeze).

## 6. Verification plan (post-0013 and post-0014)

```sql
-- V1 (after 0013): unbound agents with a legacy pair should equal the accepted A4 list
SELECT count(*) FROM agents WHERE ai_model_id IS NULL
  AND coalesce(default_model,'') <> '';

-- V2 (before 0014 drop): no code path reads legacy → assert stable at 0 new writes for N days
SELECT count(*) FROM agents WHERE ai_model_id IS NULL
  AND coalesce(default_provider_id,'') <> '';

-- V3 (before 0014 drop): every provider model represented
SELECT p.name, m.model FROM providers p,
  jsonb_array_elements_text(p.models) AS m(model)
  EXCEPT SELECT am.model_name, '' FROM ai_models am WHERE am.provider_id = p.id;
-- (adjust to verified jsonb shape; goal: empty set)

-- V4 (after 0014): zero legacy grant rows
SELECT count(*) FROM agent_permissions WHERE resource_type='llm_provider';
```

Test coverage: control-plane suites (drop legacy-field cases from create/patch tests), operator
env-injection tests, runtime tests for binding-only model resolution, web-v2 vitest (AgentForm
without legacy fields; agent page filter simplification). Add a migration round-trip test
(fresh 0001→0014 on empty DB + seeded fixture) if the migration runner supports fixtures.

## 7. Risks & open questions

1. **web v1 retirement is the critical path** — until its agent create/update stops writing
   legacy fields, 0014 cannot ship. Ross decision: retire, freeze, or rewire v1?
2. **`providers.models`/`pricing` jsonb shapes are unverified** — Step B mapping must be
   written against the live SELECT (A1), not code assumptions. Budget one dry-run session.
3. **Zero-priced backfilled models** — models with no legacy pricing entry will report $0 cost
   until admin fixes; surface the list in migration output, not silently.
4. **Runtime fallback chain** (runtime.py:733/837) uses `default_provider_id` as a *model
   name* fallback — smells like a latent bug; removal in step 4 eliminates it, but check
   staging agent logs for which value actually served before trusting bindings.
5. **CRD field removal timing** — pruning is silent; if any GitOps-managed Agent CR still sets
   `defaultProviderId`, it will silently stop having effect at step 4, not step 5. Check
   k3s-cluster repo for CR manifests that set these fields before deploying step 4.
6. **Unbound agents** keep platform-default behavior — acceptable, but list must be reviewed
   with Ross before 0014 so nobody's agent silently changes provider semantics.

## Go / No-Go

**GO for 0013 (backfill)** once the A1 dry-run shape check is done — additive, idempotent,
rollback = ignore.
**CONDITIONAL GO for 0014** — gated on: (a) web v1 retired/frozen, (b) step-4 cutover code
run ≥1 release clean, (c) V1–V3 verification empty.
**Recommend**: ship 0013 + step-4 cutover together this cycle; schedule 0014 next cycle.
