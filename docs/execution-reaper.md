# skquad — Task Execution Reaper Design

> **Status:** Implemented (2026-09-03) — Part A (runtime in-flight heartbeat) and
> Part B (control-plane reaper) are both in place; see
> [`implementation-status.md`](implementation-status.md).
>
> Companion to [`kanban-task-lifecycle.md`](kanban-task-lifecycle.md), which
> promises: *"Crash recovery is lease-based — if an agent pod dies mid-task,
> another runtime can pick the task up once the execution lease expires."*
>
> Today that promise is only half-true: the claim path can lazily reclaim an
> in-progress task whose lease lapsed, but **only the same agent, and only
> when it claims again**. If the agent itself is dead, the task sits
> `in-progress` forever and its execution stays `active` forever. This spec
> closes the gap.

---

## 1. Problem

1. **Stuck tasks.** A runtime that dies mid-task (pod crash, OOM, node loss,
   image pull failure) leaves its task `in-progress` with an `active`
   execution. No reaper exists, so nothing ever re-queues the work.
2. **Permanent "Stalled" UI state.** The board computes
   `stalled = execution active + lease expired`. Without recovery, the badge
   is correct but eternal.
3. **Unbounded `active` rows.** `task_executions` accumulates `active` rows
   whose lease lapsed long ago; the partial index
   `idx_task_executions_agent_active` and every `status = 'active'` query
   carry the dead weight.

### What already works (do not break)

- **Fencing is authoritative.** `HeartbeatTaskExecution` and
  `CompleteTaskExecution` require `status = 'active'` **and** a matching
  fencing token. A late call from a dead worker already fails cleanly.
- **Lazy per-agent reclaim.** `ClaimNextTask` prefers
  `claimReclaimableInProgress` (in-progress task, no active execution with an
  unexpired lease) before `claimTodoTask`. A live agent that outlived its own
  lease already recovers on its next claim.
- **Agent-level gating.** `agentHasActiveTaskExecution` checks
  `lease_expires_at > now()`, so a dead agent's expired lease does not block
  it from claiming new work.
- **The `expired` status already exists** in the domain
  (`TaskExecutionExpired`) and in the schema
  (`CHECK (status IN ('active','completed','blocked','expired'))`).
  **No migration is required.**

### The prerequisite gap

The agent runtime heartbeats **at claim and at completion only** — never
while the handler runs. With a 2-minute lease
(`defaultTaskExecutionLease`), any task longer than 2 minutes shows as
"Stalled" even though the worker is alive. A reaper deployed without fixing
this would **expire live long-running tasks** and re-queue work that is
already in flight. Part A below is therefore a hard prerequisite for Part B.

---

## 2. Goals / Non-goals

**Goals**

- G1: A task whose worker died is re-queued to `todo` automatically, within
  a bounded time (lease + grace + interval).
- G2: The reaper is safe under concurrent execution (control plane runs ≥2
  replicas) and idempotent.
- G3: Fencing invariants are preserved: a late heartbeat/complete from a
  dead worker can never resurrect, mutate, or double-complete an expired
  execution.
- G4: Memory and Postgres stores behave identically (dev mode parity).
- G5: No schema migration, no new API surface.

**Non-goals (v1)**

- No retry cap / dead-lettering (see §8, v2 candidates).
- No audit event or inbox notification when a task is re-queued.
- No change to the "Stalled" badge semantics (see §6 for the UI impact).
- No handling of `blocked` tasks — that is a terminal state chosen by the
  worker, not a lease failure.

---

## 3. Part A — Runtime: heartbeat during execution

**Where:** `agent-runtime/skquad_runtime/runtime.py`

While a task handler runs, a background thread refreshes the execution lease:

- **Start:** after the existing `heartbeat("busy", task)` at claim time.
- **Stop:** before the final `heartbeat("idle")` / `block_task` /
  `complete` call, in all exit paths (success, `TimeoutError`, handler
  exception, invalid status).
- **Interval:** `SKQUAD_HEARTBEAT_INTERVAL_SECONDS`, default **40**
  (≈ lease/3 for the 2-minute lease, so two consecutive missed ticks are
  required before the lease lapses).
- **Tick:** `POST /api/v1/agents/me/heartbeat` with
  `{ "status": "busy", "execution_id": ..., "fencing_token": ... }`.
- **Failure policy:** log a warning and keep retrying on the next tick.
  Transient network blips must not kill a live task. If the control plane is
  genuinely gone, the lease lapses, the reaper re-queues the task, and the
  late complete fails on fencing — an acceptable, self-healing outcome.
- **Ordering:** the stop must happen-before the terminal call so the thread
  cannot heartbeat after completion (a post-complete heartbeat fails on
  `status != 'active'` — harmless, but noisy).

**Runtime tests**

- A long task (handler sleeps past the lease) keeps its lease alive: the
  mocked control plane receives ≥2 heartbeats with the execution's fencing
  token during the run.
- The thread stops on every exit path (success, timeout, exception).
- Heartbeat failures are logged and do not interrupt the handler.

---

## 4. Part B — Control plane: execution reaper

### 4.1 Storage interface

```go
// ReapExpiredTaskExecutions marks active executions whose lease expired
// before cutoff as expired and re-queues their tasks (in-progress → todo)
// when no other live execution remains. Returns the number of executions
// reaped. Safe to run concurrently: the updates are conditional, so a
// heartbeat or complete that lands after cutoff wins and the row is left
// untouched.
ReapExpiredTaskExecutions(ctx context.Context, cutoff time.Time) (int, error)
```

### 4.2 Postgres implementation

Two conditional statements in one transaction (no SELECT-then-UPDATE):

```sql
-- 1. Expire the dead attempts.
UPDATE task_executions
SET status         = 'expired',
    completed_at   = now(),
    result_summary = 'lease expired without completion',
    updated_at     = now()
WHERE status = 'active'
  AND lease_expires_at < $1            -- $1 = cutoff
RETURNING task_id;

-- 2. Re-queue only tasks with no remaining live attempt.
UPDATE tasks
SET status     = 'todo',
    updated_at = now()
WHERE id = ANY($1)                     -- task ids from step 1
  AND status = 'in-progress'
  AND NOT EXISTS (
    SELECT 1 FROM task_executions e
    WHERE e.task_id = tasks.id
      AND e.status = 'active'
      AND e.lease_expires_at > now()
  );
```

**Why the `NOT EXISTS` guard:** a task can transiently hold two `active`
executions — the old one (lease lapsed, not yet reaped) and a new one from a
lazy reclaim. Reaping the old one must not yank the task back to `todo`
while the new attempt is live; the guard leaves the task `in-progress` until
the last live attempt is gone.

**Race analysis**

| Race | Outcome |
|------|---------|
| Heartbeat lands after cutoff, before reaper | Heartbeat's `UPDATE ... WHERE status='active' AND fencing_token=...` extends the lease; reaper's `lease_expires_at < cutoff` no longer matches → row untouched. |
| Complete lands after cutoff, before reaper | Complete sets `status='completed'` atomically with the task status → reaper matches 0 rows. |
| Reaper lands between a dead worker's last heartbeat and its (never-arriving) complete | Execution → `expired`, task → `todo`. Late complete fails on fencing/status. |
| Reaper runs on two replicas simultaneously | Both transactions run the same conditional updates; the second reaps 0 rows (or re-queues a task the first already moved, which the `status='in-progress'` condition makes a no-op). Idempotent. |
| Reclaim creates a new execution between the two statements | `NOT EXISTS` sees the fresh live lease → task not re-queued. |

### 4.3 Memory store

Same semantics under the existing store lock: iterate executions, apply the
same conditions (`status == active && LeaseExpiresAt.Before(cutoff)`), mark
expired, then re-queue tasks with the same "no live attempt remains" guard.
Returns the count.

### 4.4 Worker loop

Follows the existing outbox-worker pattern
(`internal/kube/outbox_worker.go`, started with `go` in `cmd/api/main.go`):

```go
// internal/httpapi/reaper.go
func RunExecutionReaper(ctx context.Context, store Store, interval, grace time.Duration) {
    // ticker loop:
    //   n, err := store.ReapExpiredTaskExecutions(ctx, time.Now().Add(-grace))
    //   log when n > 0 (task ids + execution ids); log-and-continue on err
}
```

- **Cutoff:** `now() - grace`. With the 2-minute lease and default grace of
  2 minutes, an execution is reaped **4 minutes after its last heartbeat**.
- **Config:**
  - `SKQUAD_REAPER_INTERVAL_SECONDS` — default **30** (how often the loop
    runs).
  - `SKQUAD_REAPER_GRACE_SECONDS` — default **120** (extra time beyond the
    lease before an execution is declared dead).
- **Always started** (memory and Postgres stores) so dev mode matches
  production.
- **Errors:** logged, never fatal to the loop.
- **Logging:** structured; one line per reap event with execution id, task
  id, agent id, and lease age. Silent when there is nothing to reap.

### 4.5 Lease constant

`defaultTaskExecutionLease` (currently a private constant in
`internal/httpapi/server.go`) is shared by claim, heartbeat, and the reaper's
default grace. Move it to `internal/config` (or a shared package) so the
three cannot drift.

---

## 5. Failure modes & invariants

| Scenario | Behaviour |
|----------|-----------|
| Worker dies mid-task | After lease + grace + interval (≈4.5 min default), execution → `expired`, task → `todo`. Same agent (if it returns) or a reassignment picks it up. |
| Worker alive, network blip > lease + grace | Task re-queued; worker's late complete fails on fencing. Work may be duplicated — accepted in v1 (see §7). |
| Worker alive, task longer than lease | Part A heartbeats keep the lease alive; nothing happens. |
| Control plane unreachable from runtime | Lease lapses, reaper (when CP returns) re-queues; late complete fails on fencing. Self-healing. |
| Two control-plane replicas | Conditional updates make the reaper idempotent; no leader election needed. |
| Task already `done`/`blocked`/`in-review` | Step 2's `status = 'in-progress'` condition leaves it alone. |
| `blocked` execution (worker chose to block) | Never reaped — only `active` rows match. |

**Invariant:** at most one *live* (active + unexpired lease) execution per
task at any time — preserved because claim already enforces it and the
reaper only removes liveness, never adds it.

---

## 6. UI impact

- The "Stalled" badge is currently derived from `active + lease expired`.
  After the reaper fires, the execution leaves the board's execution list
  (`ListBoardTaskExecutions` filters `status = 'active'`) and the task
  returns to the `todo` column.
- Net effect: **Stalled becomes a transient warning** (visible between lease
  expiry and reap, up to ≈ grace + interval ≈ 4.5 min) instead of a permanent
  state. The recovery is visible as the card moving back to `todo`.
- **Follow-up (out of scope):** show a "last attempt expired" hint on the
  task card, sourced from the newest execution's `result_summary`, so users
  can see *why* a task bounced back.

---

## 7. Accepted risk: duplicate work

If a partitioned worker is still running when the reaper re-queues its task,
a second agent may claim and execute the same task. Fencing prevents double
*completion* (the stale worker's terminal call fails), but the side effects
of the work itself may be duplicated.

Mitigations in v1: Part A heartbeats (a live, connected worker never lapses),
the 2-minute grace (two missed ticks required), and the expectation that task
handlers are idempotent or resumable.

v2 candidate: a `retry_count` on tasks with a max-retries config, after which
the reaper marks the task `blocked` with a summary instead of re-queueing.

---

## 8. v2 candidates (explicitly deferred)

- Retry cap + dead-letter (`blocked` with "retries exhausted").
- Audit event / squad inbox message on reap ("task X re-queued after lease
  expiry").
- Reaper metrics (reaped count, oldest active lease age) for
  [`observability-metering.md`](observability-metering.md).
- "Last attempt expired" board hint (§6).

---

## 9. Test plan

**Storage (Postgres + Memory, table-driven parity)**

1. Reap marks a lapsed execution `expired`, sets `completed_at` and
   `result_summary = 'lease expired without completion'`.
2. Reap resets an `in-progress` task to `todo`.
3. Reap leaves executions with unexpired leases untouched.
4. Reap leaves `completed`/`blocked` executions untouched.
5. Reap is idempotent: a second call with the same cutoff reaps 0.
6. A heartbeat landing after the cutoff prevents the reap (lease extended →
   row untouched).
7. A task with a second active execution holding a fresh lease is **not**
   re-queued to `todo`.
8. A task already `done`/`blocked`/`in-review` is not re-queued.

**API**

9. Board after reap: task appears in `todo` with no execution state
   (`execution_id` empty), i.e. `leaseState === "idle"` in the web app.

**Runtime**

10. Long task keeps its lease alive via periodic heartbeats (mocked client,
    short lease).
11. Heartbeat thread stops on success, timeout, and handler exception.
12. Heartbeat HTTP failures are logged and do not interrupt the handler.

**Integration (manual, against a live cluster)**

13. Claim a task, `kill -9` the runtime pod mid-task. Within ≈4.5 min the
    execution is `expired` and the task is `todo`; a fresh runtime re-claims
    it.

---

## 10. Rollout

1. **Part A first** (runtime image): deploy, verify long tasks no longer show
   "Stalled" while running.
2. **Part B second** (control plane image): deploy; the reaper is a no-op
   until an execution lapses beyond grace, so it is safe to ship without a
   feature flag.
3. No migration, no chart change beyond image tags (GitOps, as usual).

### Implementation notes (2026-09-03)

- Part A: `TaskLeaseHeartbeat` context manager in
  `agent-runtime/skquad_runtime/runtime.py`; `run_task_once` wraps the handler
  call. Tests: lease kept alive during long tasks, thread stopped on all exit
  paths, handler survives failing ticks, config default/override.
- Part B: `ReapExpiredTaskExecutions(ctx, cutoff)` on `TaskStore` (memory +
  Postgres, two conditional statements in one transaction with the
  `NOT EXISTS` guard); `RunExecutionReaper` worker started unconditionally in
  `cmd/api/main.go`; config `SKQUAD_REAPER_INTERVAL_SECONDS` (30) and
  `SKQUAD_REAPER_GRACE_SECONDS` (120) via `envSeconds`.
- The Postgres SQL was verified against a live pgvector/pg16 container
  (disposable, removed after verification): cutoff semantics, idempotency,
  and the two-active-attempt guard all behaved as specified.
- One deliberate deviation from the spec text: the store method takes an
  explicit `cutoff` (not a grace), so the store stays clock-free and
  testable; the worker applies `cutoff = now - grace`.

---

## 11. Open questions

- Should the grace default be a multiple of the lease (e.g. `2 × lease`)
  rather than an independent config value? Leaning yes — one knob, no drift.
- Should reap events be visible in the existing audit log immediately (cheap
  to add, useful for debugging the first rollout)? Leaning yes for v1.1.
