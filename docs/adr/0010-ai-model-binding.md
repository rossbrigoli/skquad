# ADR-0010: "AI Model" as the grantable unit, with per-agent primary/fallback binding

- **Status:** Accepted
- **Date:** 2026-09-24
- **Deciders:** Ross (owner), Sherlock (architecture)
- **Supersedes:** the `llm_provider`-as-agent-grant model in ADR-0002 / ADR-0007 for LLM access control.

## Context

Two parallel mechanisms decided which LLM models an agent could call:

1. `agents.default_provider_id` + `agents.default_model` — plumbed through the operator to
   `SKQUAD_DEFAULT_PROVIDER_ID` / `SKQUAD_DEFAULT_MODEL` in the runtime, but **never validated**
   against the provider registry and **not authoritative** for what the LiteLLM virtual key allowed.
2. `agent_permissions` rows of type `llm_provider` — the **load-bearing** mechanism. The virtual
   key's model allow-list was the union of `default_model` + `models[]` across every active
   granted provider (`httpapi/server.go:allowedGatewayModels`).

Result: a decorative attribute and an authoritative grant describing the same thing. Identity
provisioning failed with `409 no_llm_models_granted` when an agent had a "default provider"
showing in the UI but no `llm_provider` grant. This is the direct cause of the S-104
provisioning error.

## Decision

### D1 — Introduce `ai_model` as the first-class unit

An **AI Model** is a single, admin-registered, selectable model (e.g. `gpt-6-sol`,
`halogen/qwen3.8-flash-next`). This is the unit shown in the UI and the unit that gets granted.

### D2 — Do NOT flatten the provider credential into the AI Model

`ai_model` references an internal `provider` row that holds `base_url` + `api_key_ref`.
The provider has **no UI**; it exists so N models from one account share **one** credential.

> If the API key were stored per model pair, rotating a leaked key would require N edits and
> create N leak surfaces.

Existing `llm_providers` is retained and demoted to this internal credential-holder role; its
`models[]` JSON blob becomes real `ai_models` rows.

### D3 — Grants target **users**, not squads or agents

Admin grants AI Models to a `user` (an existing OIDC-authenticated principal with a role).
A user creating an agent may bind only models granted to them.

Rationale: the logged-in human is the natural authorisation and cost-attribution boundary.
This removes the need for a synthetic "squad provider pool" object.

### D4 — Agent binds exactly one primary + optional one fallback

```
agents.ai_model_id          uuid NOT NULL   -- must be granted to the creating user
agents.fallback_ai_model_id uuid NULL       -- optional, must differ from primary,
                                           -- must be granted to the creating user
```

The two nullable, half-decorative columns (`default_provider_id`, `default_model`) collapse
into one required FK. **Depth is exactly 1** — no fallback chains (they mask outages, can loop,
and make cost unpredictable).

### D5 — The grant compiles into the virtual key; the runtime never re-checks grants

```
user grant → agent binding → LiteLLM key.models = [primary, fallback] → gateway enforces
```

The agent runtime holds a key, not a user identity, so it *cannot* re-check grants. Any change
to a grant or a binding MUST trigger key re-provisioning. Never rely on the DB row alone.

> **Trap avoided:** if the key is provisioned with only `[primary]`, the fallback fails
> authorisation at the exact moment it is needed — during an outage.

### D6 — Fallback executes in the gateway router, not the runtime

LiteLLM's router natively provides cooldowns, fallbacks, timeouts and retries across
deployments/providers. Implementing fallback again in `agent-runtime` would create two
competing health models. The runtime stays single-path.

### D7 — Fallback triggers only on the correct failure class

| Failure | Fall back | Note |
|---|---|---|
| Connection refused / DNS / unreachable | yes | the case being solved |
| 5xx, gateway timeout | yes | transient |
| 429 after retries + backoff | yes | upstream quota exhausted |
| 401/403 from **upstream** provider | yes + **loud alert** | otherwise a dead key runs silently on fallback indefinitely |
| 400 bad request / context-length exceeded | **no** | a different model cannot fix the prompt; a smaller-context fallback is strictly worse |
| Our control-plane auth failure | **no**, never | not a model problem |

### D8 — Pricing is four rates, snapshotted at metering time

Per published vendor pricing, one model has **input / cached-input / cache-write / output**
rates per 1M tokens, plus a short-context vs long-context tier with a threshold.

`ai_models.pricing` stores all four rates + `long_context_threshold_tokens`.

Computed cost is written into the `metering` row **at event time**, including the rate used.
Historical cost MUST NOT be re-derived by joining to live pricing — vendors change prices and
the history would silently rewrite itself.

### D9 — Revoke cascades, guarded by `force`

Revoking an AI Model from a user affects every agent that user owns that is bound to it:

1. Enumerate bound agents.
2. If none → revoke cleanly.
3. If some → require `?force=true`; otherwise `409 in_use` with the affected agent list.
4. On force: clear/unbind the slot and re-provision (or revoke) each affected virtual key.
5. Notify affected agent owners.

Reuses the S-103 delete-with-in-use-warning pattern and the existing `syncAgentGatewayKey`
machinery.

### D10 — Budgets stay on squads

Grants follow **people** (who may use a model). Cost centres follow **squads** (K8s namespace +
`ResourceQuota` already exist). These are orthogonal; budgets are not moved to users.

### D11 — Agents outlive their creator

Deleting a user MUST NOT cascade-delete their agents or revoke running keys. Agents get an
owner-reassign path instead.

## Consequences

**Positive**
- One authoritative concept replaces a decorative attribute + a hidden authoritative grant.
- Authorisation reuses the existing authenticated `users` principal — no new scoping object.
- Agent → model is 1:1 and mandatory; provisioning errors become precise.
- Fallback is native to the gateway, not bespoke runtime code.
- Cost attribution is per-person at the grant boundary and per-turn at the metering boundary.

**Negative / costs**
- Schema migration across `agents`, `agent_permissions`, plus two new tables.
- `llm_provider` must be removed from the `agent_permissions` CHECK constraint.
- Grant/binding changes now have a mandatory side effect (key re-provision) — more moving parts
  on the write path.
- Fallback introduces cost and capability hazards requiring UI guardrails (see Risks).

## Risks & required guardrails

1. **Cost spike during outage.** If the fallback's rates exceed the primary's, an availability
   feature becomes a billing incident. → Soft **warn** at bind time when
   `fallback.input > primary.input`.
2. **Capability mismatch.** Agents use tool calling. A fallback without tool-calling support, or
   with a smaller context window, breaks the tool loop mid-task rather than degrading gracefully.
   → Soft **warn** at bind time; registry should carry `supports_tools` and `context_window`.
3. **Invisible fallback usage.** Without per-turn `model_used`, cost attribution lies and
   "we've been on fallback for three days" is undetectable. → Record `model_used` per turn.
4. **Mid-conversation model switch.** Router cooldown auto-return means the model can change
   mid-thread, altering voice and context window. Accepted; must be visible in the turn record.

## Known limitations (verified against the LiteLLM API during WP3, 2026-09-24)

These are upstream constraints, not implementation shortcuts. Confirmed against
`litellm.types.router.UpdateRouterConfig` and the callback API rather than assumed.

### L1 — D7's "do NOT fall back on 400-class" is not expressible in LiteLLM

Generic `fallbacks` fire on **every** exception class; there is no per-class exclusion. We set
`BadRequestErrorRetries: 0` so an oversized/bad prompt is not retried, but **the fallback
deployment is still attempted once** on a 400 / context-length error.

`context_window_fallbacks` exists but does the *inverse* of what we want (it deliberately
falls back to a smaller context), so it is not a remedy.

**Residual risk: low.** The fallback will almost certainly fail on the same request, so the
caller still sees the error — we do not silently truncate. The cost is one wasted upstream
call per 400. The dangerous failure mode D7 was written to prevent (silent truncation) does
not occur, because nothing truncates: both models simply reject the request.

**Do not claim D7 is fully enforced.** If a future LiteLLM release adds per-class fallback
exclusion, tighten this.

### L2 — The upstream 401/403 alert is best-effort

`async_log_failure_event` fires when the request **ultimately** fails. If the fallback
succeeds, the upstream auth failure may never reach the callback and the alert is missed.
What is implemented: whenever a failure event does arrive with 401/403, it alerts loudly
(`LOGGER.error` + `alert: "upstream_auth_failure"` in the metering payload → control-plane
`llm.upstream_auth_alert` audit event).

**Consequence:** a dead primary credential can persist unnoticed if the fallback always
carries the load. **Mitigation to schedule:** an active health probe on each provider's
primary deployment, independent of the request path.

### Implemented faithfully

Transport / 5xx / timeout fallback; 429-after-retries fallback via the retry budget;
upstream 401/403 falls back; per-key precedence (key > team > global).
Recommended belt-and-braces: enable `general_settings.enforce_fallback_model_access: true`
gateway-side, though our keys already carry the fallback in `models`.

## References

- LiteLLM Router — cooldowns, fallbacks, retries: https://docs.litellm.ai/docs/routing#fallbacks
- LiteLLM virtual keys — key-level `models` allow-list: https://docs.litellm.ai/docs/proxy/virtual_keys
- OpenAI published pricing (per 1M tokens, cached input / cache write / output, long-context tiers): https://developers.openai.com/api/docs/pricing
