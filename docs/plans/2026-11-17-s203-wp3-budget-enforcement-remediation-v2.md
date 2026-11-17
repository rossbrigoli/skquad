# S-203 WP3 — Budget Enforcement Remediation Plan v2 (code-verified)

**Scope:** budget enforcement across `control-plane/internal/httpapi/budget_guard.go`,
`control-plane/internal/httpapi/server.go` (send/wake/task paths),
`control-plane/internal/kube/outbox_worker.go`, `control-plane/internal/storage/budget_enforcement.go`,
and `operator/internal/controller/agent_controller.go`.

**Supersedes:** the externally-reviewed remediation plan circulated 2026-11-17. That plan's
defect list was written against an incomplete reading of the codebase; this plan is verified
against the working tree (commit `2f8fee4` and later) with file:line citations.

**Status:** Ready to implement. Product decisions Q1–Q4 are answered in §6 (confirm before PR-2).

---

## 0. Binding intent (retained from the prior plan — unchanged, still correct)

> When a budget limit is reached, the agent **finishes the current turn normally**. The
> **next** turn must not start until the budget is raised or the period resets.

This is **admission control**, not a kill switch. Two symmetric failure modes are unacceptable:

1. **Under-enforcement** — the next turn starts despite the cap being crossed.
2. **Over-enforcement** — an admitted turn is interrupted, truncated, or cancelled.

Over-enforcement is a regression, not a safety win. Accepted consequences, not to be "fixed":

- **Overrun is expected.** Final spend = cap + cost of the turn that crossed it. Make it
  visible (WI-4), not prevented.
- **A turn is the whole turn** — all tool calls, LLM iterations, and sub-agent delegations
  spawned from it. No subdivision to tighten the cap.

---

## 1. Verified current state — read before implementing

The enforcement architecture today (all verified in-tree):

| # | Fact | Where |
|---|------|-------|
| F1 | **Admission chokepoint exists.** Every busy transition flows through `setAgentStatusAndMirror`, which refuses idle→busy for budget-blocked owners and passes through already-busy agents (running turns are never killed). Covers chat, task, and heartbeat start paths. | `httpapi/server.go` `setAgentStatusAndMirror` (~5944) |
| F2 | **Wake is separately gated.** `wakeAgent` refuses pre-warm for blocked owners with `reason: budgetStopNotice`. | `httpapi/server.go` (~4628) |
| F3 | **Operator never interrupts an active agent.** `desiredReplicas()` returns 1 whenever `Spec.DesiredActive`, *before* any idle-timeout logic; `updateIdleSince()` clears `IdleSince` while active. A zero idle timeout cannot kill an in-flight turn. | `operator/.../agent_controller.go` (~768–800) |
| F4 | **Cost timer override exists.** Blocked owner ⇒ `IdleTimeoutSec = 0` at CR-apply time; pod tears down the moment `desiredActive` goes false. | `kube/outbox_worker.go` (~266–281) |
| F5 | **Period + dual-source semantics correct.** Blocks scoped to `"2006-01"` UTC; stale-period rows never block; `blocked_by_user` / `blocked_by_platform` OR'd. | `domain/budget_enforcement.go` |
| F6 | **Notifications already exist and are exactly-once.** 80/90 warnings + 100 "stopped" notification, atomic marker claim per source/threshold/period, inbox + bell, personal vs platform text differ, "stopped at end of current turn" wording present. | `httpapi/budget_guard.go` `applyBudgetVerdict`, `notifyBudgetStopped`, `ClaimBudgetNotification` |
| F7 | **Owner semantics are deterministic (undocumented).** `IsAgentBudgetBlocked` resolves agent → squad → `owner_id` in one join. The **executing agent's squad owner** governs, for delegated and handed-off work alike. Metering matches. | `storage/budget_enforcement.go` (~197) |
| F8 | **No transaction infrastructure** in the storage layer (raw pgx pool; mutex MemoryStore; no `BeginTx`/`WithTx` exposure). | `internal/storage/*` |
| F9 | Existing tests: `kube/outbox_worker_budget_test.go`, `storage/budget_enforcement_test.go`, `kube/outbox_worker_idle_test.go`. | — |

**Consequence:** the *blocking* half of "finish the turn, block the next" already works.
What is broken or missing is narrower than the prior plan claimed.

## 2. Real gaps (verified)

### G1 — Fail-open on budget-check errors (two sites) — PRIMARY

- `budget_guard.go` `agentBudgetBlocked()`: on check error returns `false` (not blocked) →
  the next turn starts unchecked.
- `outbox_worker.go` `upsertAgentFromOutbox`: on check error keeps the normal idle timeout
  (comment explicitly justifies fail-open "same posture as the other derivations" — the
  analogy is invalid for a money control).

Worst case today: a check error at exactly the wrong moment lets an unbounded number of turns
start. Worst case after fail-closed: an idle pod scales down early and cold-starts — because
F1/F3 mean an admitted turn cannot be disturbed by a zero idle timeout or a refused busy
transition. Fail-closed is strictly cheaper. **Fix in PR-1.**

### G2 — The chat-send path lies to the client

`createAgentChatMessage` creates the message row **before** the busy transition is refused,
and `setAgentStatusAndMirror` returns `nil` on refusal (logs only). The client receives
`201 Created`; the user sees acceptance and then silence. The 100% notification fires at
block *creation* time, not at each rejected send, so a user blocked last week gets a stale
notice and today's message just vanishes. **Fix in PR-2.**

### G3 — Pending-message resurrection (created by G2)

Messages accepted during a block sit `pending` forever and can resurrect when the budget is
raised later — a surprise turn replaying stale input. Messages accepted *before* a block
legitimately queue (they were admitted while the user was in good standing); only
during-block-created rows are the problem, and G2's pre-create rejection eliminates them.
Legacy rows: expire-on-block for chat pending rows (WI-2). **Fix in PR-2.**

### G4 — Owner semantics undocumented (F7)

The rule exists in code but in no ADR or doc. Delegation/handoff behavior is therefore
accidental-by-documentation. Needs an explicit decision record, not new code. **Fix in PR-3.**

### G5 — No metrics/alerts on the enforcement path

No counters for blocks created, turns rejected, check errors (fail-closed firing), overrun,
or fan-out drain. Fail-closed without `budget_check_errors_total` is fail-mysteriously.
**Fix in PR-3.**

### Explicitly NOT gaps (prior-plan defects that do not survive verification)

- **Prior D1 ("no admission check exists")** — false; see F1/F2. Implementing it literally
  would build a second admission layer beside the existing chokepoint and create the exact
  divergence it warns about.
- **Prior D3 ("block write and CR rewrite not atomic ⇒ DB/enforcement divergence")** — the
  divergence cannot affect next-turn blocking because admission (F1) reads the DB live at
  transition time. A stale CR only affects idle-pod *cost*. Given F8 (no tx infra), a
  transactional-outbox retrofit is expensive and buys little. **Deferred** (§5).
- **Prior D4 ("blocks are invisible")** — largely false; see F6. The invisible part is only
  the send-path response (G2).
- **Prior D6 ("owner semantics undefined")** — false as stated; deterministic per F7.
  Documentation gap only (G4).

---

## 3. Target design (delta only)

Keep everything that works (F1–F7). Three changes:

1. **Fail-closed at both check sites**, with the never-interrupt guardrail:
   > Fail-closed may only prevent a turn from *starting* or keep an idle pod scaled down.
   > It may never cancel, truncate, or disturb an admitted turn. (Protected by F1's
   > already-busy pass-through and F3; asserted by T1/T3.)

   A check failure must be **typed distinctly** from a block: "budget could not be verified
   (retry)" ≠ "budget exhausted" — different user message, different metric, different
   operator urgency.

2. **Reject blocked chat sends before creating the message row.** Pre-check in
   `createAgentChatMessage` (and any user-facing message-creation path) returning a typed
   4xx; the chokepoint guard (F1) stays as defense-in-depth — the handler check produces the
   *response*, the chokepoint guarantees *enforcement*.

3. **Make the accepted trade legible:** overrun reporting, metrics, alerts, ADR.

**Accepted races (document, don't fix):** check→transition TOCTOU windows bound the
overslip to one turn, which is inside the already-accepted overrun envelope. No distributed
locking.

---

## 4. Work items

### PR-1 — Fail-closed flip + the never-interrupt assertions (core, small)

**WI-1: Typed three-state block check.**
Replace `agentBudgetBlocked(ctx, agentID) bool` with a check that distinguishes
`allowed | blocked | checkFailed`. All three call sites (chokepoint, wake, new send path)
handle the third state explicitly.

```go
type budgetVerdict int
const (
    BudgetAllowed budgetVerdict = iota
    BudgetBlocked
    BudgetUnverifiable
)
func (s *Server) checkAgentBudget(ctx context.Context, agentID string) (budgetVerdict, *BudgetBlockInfo)
```

`BudgetUnverifiable` at the chokepoint behaves like blocked for idle→busy transitions
(fail-closed) but already-busy still passes through (never-interrupt).

**WI-2: Flip the CR-sync branch.**
`upsertAgentFromOutbox`: on `IsAgentBudgetBlocked` error → `IdleTimeoutSec = 0`.
**Replace the stale fail-open comment** with the new rationale (money control; active-turn
protection lives in `desiredActive`, verified F3).

**WI-3: Tests (this PR carries the two most important tests in the program).**
- **T1 never-interrupt:** agent busy (`desiredActive=true`), cap crossed mid-turn, block
  created, CR re-synced with `IdleTimeoutSec=0`, forced check errors — assert pod not
  touched, turn completes. Operator-level test on `desiredReplicas` + control-plane-level
  test on the already-busy pass-through.
- **T3 fail-closed:** forced error at chokepoint ⇒ idle→busy refused with
  `BudgetUnverifiable`; forced error at CR sync ⇒ `IdleTimeoutSec == 0`.
- Existing `outbox_worker_budget_test.go` extended, not replaced.

### PR-2 — Typed send rejection + pending-message hygiene

**WI-4: Pre-create rejection in the chat send path.**
In `createAgentChatMessage`, after auth/access and **before** `CreateMessage`:

- `BudgetBlocked` → HTTP **402 Payment Required**, error code `budget_blocked`, JSON body:
  `{source: "user"|"platform", period, spent_usd, cap_usd, unblock_action}`.
  Reuse `budgetNotificationText` phrasing; include the five points: which budget, period,
  spent vs cap, unblock action, and explicit "your previous turn completed — nothing was
  lost."
- `BudgetUnverifiable` → HTTP **503**, error code `budget_verification_failed`, retry
  semantics. Distinct from exhaustion.
- `BudgetAllowed` → unchanged path.
- `wakeAgent` keeps its existing refusal but should also surface `unverifiable` distinctly
  (it currently collapses both into `budgetStopNotice`).

**WI-5: Pending chat-message expiry on block.**
When a block is created for user U, expire (do not delete — audit) chat messages currently
`pending` to U's agents, with reason `expired_budget`. Messages created *after* the block
can no longer exist (WI-4). Task rows are **not** expired — see WI-6.

**WI-6: Task-assignment semantics — queue is allowed, execution is gated.**
Deliberate asymmetry with chat: a task assigned to a blocked agent stays visible on the
board (planners legitimately assign work ahead of budget resets; bulk assignment must not
fail mid-batch). Execution is already prevented by the chokepoint (F1). Add the assignee-side
visibility: board/task detail shows `blocked: budget` when the assignee's owner is blocked.

**WI-7: Tests.**
- **T2 block-next-turn:** cap crossed mid-turn → turn completes → next chat send rejected
  402 before any message row, CR write, or pod start (assert absence, not just status).
- **T5 period-boundary:** blocked in period N, message in N+1 → admitted.
- **T6 dual-source:** platform-blocked user raises personal budget → still blocked; send
  returns `source: platform` and the admin-contact message.
- T7 expiry: pending chat rows expire on block; task rows survive; audit rows written.

### PR-3 — Legibility: ADR, metrics, overrun reporting

**WI-8: ADR 0016 — "Budget enforcement is admission control."**
Adopt the prior plan's §8 text with these corrections:
- Record the owner rule (Q1 below) explicitly.
- Cross-link `0003-scale-to-zero.md` (already references the `idleTimeout=0` teardown path).
- Record the accepted races (TOCTOU ≤ 1 turn; wake-vs-block interleavings).

**WI-9: Metrics + alerts.**
- `budget_blocks_created_total{source}`
- `turns_rejected_budget_total{source,site=send|wake|chokepoint}`
- `budget_check_errors_total{site=admission|crsync}` — **alert on sustained non-zero**
  (fail-closed firing = users blocked by an outage, not their budget)
- `budget_overrun_dollars{source}` (histogram)
- `budget_fanout_remirror_duration_seconds`
- Platform-block creation pages a platform admin (big red button).

**WI-10: Overrun reporting.**
Verify no UI/API surface clamps spend to cap (storage never clamps — MTD is actuals). Add
explicit overrun line: `Budget $50.00 · Spent $73.40 · Overrun $23.40 (final turn, Nov 2026)`.
Platform overrun reported separately from per-user.

**WI-11: Documentation sweep.**
- Q1 rule documented in ADR 0016 and `docs/observability-metering.md`.
- Update `budget_guard.go` header comment: it currently claims the block "prevents every
  start path" (true) but doesn't mention the send-path rejection being added (WI-4).
- Verify `docs/kanban-task-lifecycle.md` §9 still matches after PR-2.

---

## 5. Deferred — with revisit triggers

| Item | Why deferred | Revisit when |
|---|---|---|
| Transactional outbox for block+fan-out (prior WI-2) | Admission reads DB live (F1), so non-atomic fan-out cannot under-block the next turn; only cost impact. No tx infra exists (F8) — retrofit cost spans Postgres + MemoryStore. | Observed drift between block state and CR state in production, or when tx infra lands for other reasons. |
| Platform fan-out coalescing/acing (prior WI-7) | `wasBlocked` transition guard already prevents repeat storms; platform blocks are rare admin events; outbox retries. | Agent count per owner > ~100, or platform blocks become routine. |
| Originator-pays delegation billing (Q1 alternative) | Requires threading billing identity through every delegation edge. Current rule (F7) is coherent. | Cross-squad delegation becomes a first-class product feature. |

---

## 6. Product decisions (answered — confirm before PR-2)

**Q1 — Whose budget governs a delegated/handed-off turn?**
**A: The executing agent's squad owner (status quo, F7).** Within a squad this is one owner
anyway. Cross-squad delegation requires the delegate owner's access-control consent, so
cost-shifting is consented, not an escape hatch. Originator-pays rejected: large plumbing,
no current benefit. → ADR 0016.

**Q2 — Platform block: hard or soft?**
**A: Hard at turn granularity — which is what the architecture already provides.** In a
turn-based system, "block the next turn" and "let the session run until idle" are the same
thing. The never-interrupt guarantee *is* the softening. Take the hard stop.

**Q3 — Dollar, token, or both?**
**A: Dollars only for this WP.** The model is USD end-to-end. If token caps arrive later,
model them as separate budget rows with their own block reasons — not a flag matrix.

**Q4 — Grace at the threshold?**
**A: Hard block at ≥100%, no grace turn.** The overrun is already structurally one turn
(the crossing turn finishes). An explicit "one more turn" doubles it, is gameable, and turns
a bounded trade into a creeping one. 80/90 warnings provide the runway.

---

## 7. Test matrix (guarantee → test)

| Guarantee | Test | PR |
|---|---|---|
| Admitted turn is never interrupted | T1 (control-plane pass-through + operator `desiredReplicas`) | 1 |
| Check error never kills a running turn | T1 variant with forced errors | 1 |
| Next turn blocked after cap crossed | T2 (no message row, no CR write, no pod start) | 2 |
| Fail-closed both sites | T3 | 1 |
| Typed rejection ≠ exhaustion | T4 (402 vs 503, distinct bodies) | 2 |
| Month boundary resets | T5 | 2 |
| Dual-source independence | T6 | 2 |
| Pending chat expiry / task survival | T7 | 2 |
| Exactly-once notifications (existing) | keep green | all |

T1 and T2 are the two halves of the product requirement. If either regresses, the feature is
broken even when every other test passes.

---

## 8. Non-goals (carried forward, still binding)

- ❌ Mid-turn token/dollar breaker of any kind.
- ❌ Clamping reported spend to the cap.
- ❌ Making idle timeout the enforcement mechanism (it is the cost timer; the chokepoint is enforcement).
- ❌ Weakening `desiredActive=true` protection to make scale-down faster.
- ❌ Failing open on budget-check errors anywhere in this flow.
- ❌ Gating sub-agent delegations spawned from an admitted turn.
- ❌ Silent blocks.
- ❌ A second admission layer beside `setAgentStatusAndMirror` (see §2, prior D1).

---

## 9. Sequencing

| Phase | PR | Contents | Ships |
|---|---|---|---|
| 1 — Stop the leak | PR-1 | WI-1, WI-2, WI-3 (T1, T3) | immediately |
| 2 — Tell the user | PR-2 | WI-4, WI-5, WI-6, WI-7 (T2, T4–T7) | after Q1–Q4 confirmed |
| 3 — Make it legible | PR-3 | WI-8–WI-11 | independent, can parallel PR-2 |

Total scope is roughly **two small PRs plus a docs/metrics PR** — materially smaller than the
superseded plan's five, because most of that plan's core was already built and correct.
