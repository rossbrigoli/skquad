# ADR-0017: Budget Enforcement Is Admission Control, Not Interruption

- **Status:** Accepted
- **Date:** 2026-11-18
- **Deciders:** Ross, Sherlock
- **Implements:** S-203 WP3 (R1–R3), plan `docs/plans/2026-11-17-s203-wp3-budget-enforcement-remediation-v2.md` (WI-8). PRs: #294 (fail-closed + never-interrupt), #295 (typed send rejection), S-321 (this ADR + metrics + overrun reporting).
- **Numbering note:** the remediation plan referred to this as "ADR 0016"; that number was already taken by ADR-0016 (Agent-Driven Board Transitions), so this record uses the next free number.

## Context

skquad enforces monthly USD budgets per user (and a platform-wide limit).
The binding product intent: when a budget limit is reached, the agent
**finishes the current turn normally**, and the **next** turn must not
start until the budget is raised or the period resets.

There are two symmetric failure modes, and both are unacceptable:

1. **Under-enforcement** — the next turn starts despite the cap being
   crossed (money leak).
2. **Over-enforcement** — an admitted turn is interrupted, truncated,
   or cancelled (correctness/UX regression).

This ADR records the deliberate trade: we enforce at *admission* only,
accept a bounded overrun, and make both observable.

## Decision

### 1. Turn atomicity with respect to budget

A turn is atomic w.r.t. budget enforcement. Once a turn is **admitted**
(the idle→busy transition passed the chokepoint
`setAgentStatusAndMirror` in `control-plane/internal/httpapi/server.go`),
it runs to completion **regardless of any mid-turn budget state
change**. The whole turn — every tool call, every LLM iteration, and
every sub-agent delegation spawned from it — is inside the admitted
envelope. No subdivision, no mid-turn breaker, no gating of
delegations from an admitted turn. The block applies to the **next
inbound message only** (chat send, task pickup, wake).

### 2. Accepted overrun

Final spend for a period = **cap + the cost of the one turn that
crossed it**. This overrun is a designed, bounded trade:

- It is **reported, never prevented or clamped**. Storage month-to-date
  figures are actuals; no API or UI surface clamps spend to the cap.
- The API surfaces it explicitly: `overrun_usd` on the user budget
  view and `platform_overrun_usd` on the platform view
  (`control-plane/internal/httpapi/budgets.go`), rendered as
  "Budget $50.00 · Spent $73.40 · Overrun $23.40 (final turn)".
- The `budget_overrun_dollars` histogram (by source) makes the
  aggregate trade visible to operators.

### 3. Fail-closed posture + the never-interrupt guardrail

Budget checks fail **closed** at both sites:

- **Admission** (`checkAgentBudget`, all start paths): a check error
  yields `BudgetUnverifiable`, which behaves like a block for
  idle→busy, wake, and send — no new turn starts on an unverifiable
  budget. The client gets a typed `503 budget_verification_failed`
  (distinct from `402 budget_blocked`: "could not verify, retry" ≠
  "exhausted").
- **CR sync** (`upsertAgentFromOutbox`,
  `control-plane/internal/kube/outbox_worker.go`): a check error
  zeroes the agent's idle timeout, exactly like a confirmed block.

The guardrail that makes fail-closed safe: **fail-closed may only
prevent a turn from *starting*, or keep an idle pod scaled down. It
may never cancel, truncate, or disturb an admitted turn.** This is
structurally guaranteed:

- The chokepoint passes through already-busy agents regardless of
  verdict (asserted by the never-interrupt tests shipped in #294).
- The operator's `desiredReplicas()` returns 1 whenever
  `Spec.DesiredActive` is true, **before** any idle-timeout logic
  (`operator/internal/controller/agent_controller.go`), so a zeroed
  idle timeout cannot kill a mid-turn pod. Worst case of a spurious
  zero: an idle pod stays scaled down and cold-starts later.

See ADR-0003 (Scale-to-Zero) for how the `idleTimeout=0` budget-block
teardown path interacts with the idle-timeout machinery.

### 4. Owner rule (delegation and handoff)

**The executing agent's squad owner governs.** Enforcement resolves
agent → squad → `owner_id` in a single join
(`IsAgentBudgetBlocked`, `control-plane/internal/storage/budget_enforcement.go`),
and metering attributes cost along the same chain. This holds for
delegated and handed-off work: the delegate's squad owner pays.
Cross-squad delegation requires the delegate owner's access-control
consent, so cost placement is consented, not an escape hatch.
Originator-pays was rejected (large plumbing through every delegation
edge, no current benefit).

### 5. Accepted races (documented, not fixed)

- **Check→transition TOCTOU:** between the budget check and the
  busy-transition write, a block may be created concurrently. The
  overslip is bounded to **one turn**, which is inside the
  already-accepted overrun envelope (§2). No distributed locking.
- **Wake-vs-block interleavings:** a wake admitted just before a block
  lands pre-warms a pod that then refuses to start work or runs at
  most one admitted turn; the next check sees the block. Same bound.
- **Block-row flip between verdict and detail read** on the send path:
  treated as not-blocked (send admitted); the chokepoint still guards
  the actual start.

### 6. Dollars only; hard block at 100%, no grace turn

- Budgets are USD end-to-end. Token caps, if ever added, are separate
  budget rows with their own block reasons — not a flag matrix.
- Hard block at **spend ≥ 100%** of the effective budget (with
  spend > 0). No grace turn: the overrun is already structurally one
  turn (the crossing turn finishes); an explicit "one more turn"
  doubles it, is gameable, and turns a bounded trade into a creeping
  one. The 80%/90% warning notifications are the user's runway.

## Metrics & alerting (WI-9)

Emitted from `control-plane/internal/metrics` (registry served at
`GET /metrics`, internal trust class):

| Series | Labels | Meaning |
|---|---|---|
| `budget_blocks_created_total` | `source` | New blocks (transition into blocked). |
| `turns_rejected_budget_total` | `source`, `site=send\|wake\|chokepoint` | Confirmed-block refusals of a turn start. |
| `budget_check_errors_total` | `site=admission\|crsync` | Fail-closed check failures. Sustained non-zero = users blocked by an outage, not their budget. |
| `budget_overrun_dollars` | `source` (histogram) | Accepted overrun at block creation. |
| `budget_fanout_remirror_duration_seconds` | — (histogram) | Block/resume CR fan-out drain (`remirrorUserAgents`). |

Verification-failure rejections are counted in
`budget_check_errors_total`, not `turns_rejected_budget_total` — a
turn refused by an outage is an error signal, not a budget signal.

**Required alert rule (lives in the platform monitoring stack, not this
repo — no alert-rule files exist here):**

```yaml
- alert: BudgetCheckErrorsSustained
  expr: increase(budget_check_errors_total[10m]) > 0
  for: 10m
  labels: { severity: critical }
  annotations:
    summary: "Budget checks failing closed — users are being blocked by a system outage, not their budget"
```

**Platform-block visibility:** platform-sourced blocks notify every
affected user (inbox + bell, exactly-once per month). A dedicated
platform-admin paging path for *block creation* does not yet exist in
this repo's notification infrastructure — recorded as a follow-up in
the S-321 PR body for the platform admin (wire
`budget_blocks_created_total{source="platform"}` into paging).

## Consequences

- Users always see real numbers, including the overrun; trust is
  maintained by telling, not hiding.
- A storage outage on the budget-check path degrades to "no new turns
  start" (fail-closed) instead of "unbounded spend" — and the
  `budget_check_errors_total` alert makes that state loud rather than
  mysterious.
- Enforcement latency after a budget change is bounded by the CR
  fan-out (`budget_fanout_remirror_duration_seconds` makes it
  measurable; coalescing is deferred per the plan until agent counts
  per owner warrant it).
- The never-interrupt guarantee is a *tested* invariant, not a hope:
  T1 (busy pass-through under block + forced errors) and the operator
  `desiredReplicas` test guard it against future refactors.

## Alternatives considered

- **Mid-turn kill / token breaker at cap:** rejected — violates turn
  atomicity, produces corrupted work, and over-enforcement is a
  regression, not a safety win.
- **Fail-open on check errors:** rejected — a money control must not
  fail open; the worst case (early scale-down) is strictly cheaper
  than the fail-open worst case (unbounded turns).
- **Grace turn above 100%:** rejected — doubles the accepted overrun
  and makes it gameable (§6).
- **Originator-pays delegation billing:** rejected — see §4.
- **Transactional outbox for block+fan-out atomicity:** deferred —
  admission reads the DB live, so non-atomic fan-out cannot
  under-block the next turn; revisit if observed CR/block drift or
  transaction infra lands for other reasons.
