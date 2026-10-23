# ADR-0016: Agent-Driven Board Transitions with Agent-Decided Review

- Status: Proposed (v1, for Ross's review)
- Date: 2026-10-24
- Deciders: Ross (pending), Sherlock
- Related: [kanban-task-lifecycle.md](../kanban-task-lifecycle.md), [collaboration-messaging.md](../collaboration-messaging.md), [ADR-0015](0015-three-party-grant-model.md), [subagent-task-execution.md](../subagent-task-execution.md)

## Context

Today the agent runtime drives all board movement through three fenced
lifecycle endpoints (`/agents/me/tasks/{id}/start|complete|block`). The LLM
never touches the board directly; the runtime loop decides terminal status,
and every completion lands in `in-review` — a human must approve to `done`.
Two observations motivate a change:

1. **Review friction.** Most task outcomes are straightforward and don't
   warrant a human approval click. Forcing everything through `in-review`
   trains the owner to rubber-stamp, which is worse than no review.
2. **Review is conversational.** When Ross *is* unhappy with an `in-review`
   card, the natural loop is: chat with the agent → agent understands the
   gap → agent reopens the card and fixes it. Today that loop doesn't
   exist: `in-review → in-progress` is not an agent-reachable transition.

Additional requirements from Ross:

- A **leader agent** should be able to create a plan and materialize it as
  multiple tasks on the board, assigned to different agents.
- Agents should **decide whether a task needs human review**; simple work
  goes straight to `done`.
- Bug observed: creating a task in `backlog` and assigning it surfaces the
  task in the agent's chat immediately, even though backlog is not
  claimable. Surfacing and claimability are inconsistent.

## Decision

### D1 — Board verbs for agents, not raw column writes

Expose an LLM-callable `board.*` builtin toolset. Each verb maps to a
validated control-plane transition. **No free-form `status` write is
exposed.** The server remains the single enforcement point (column
allowlists, leases, fencing, dependency guard S-278, budget guard S-203).

| Verb | Transition(s) | Notes |
|---|---|---|
| `board.create_task` | → `backlog` \| `todo` | Agent-created tasks default to `backlog` unless `ready=true` and creator has `can_prioritize` (leader). |
| `board.assign` | assignee change | Signals operator wake via existing `syncAffectedAgentsFromTaskChange`. |
| `board.claim` | `todo` → `in-progress` | Same semantics as today's `/start`: creates lease-backed `task_execution`. |
| `board.complete` | `in-progress` → `done` \| `in-review` | Carries `review_needed: bool` + `summary` (see D2). |
| `board.block` | `in-progress` → `blocked` | Carries `reason`. |
| `board.reopen` | `in-review` → `in-progress` | Assignee-only (see D3). Creates a **new** execution attempt. |
| `board.request_changes` | `in-review` → `todo` | Reviewer-flavoured return for reassignment; clears assignee. |

`done` remains reachable by agents **only** through `board.complete` with
self-approval recorded (D2). No verb lets an agent move arbitrary cards,
pull cards off other agents, or write `backlog` state.

### D2 — Agent-decided review with recorded justification

`board.complete(task_id, review_needed: bool, summary: string)`:

- `review_needed=false` → task moves to `done`. The summary is stored as
  the task `Result` with `ResultStatus=done` (S-181 model, unchanged)
  **plus** a `self_approved: true` audit field and a one-line
  `self_approval_justification` (required; the prompt instructs the agent
  to state why no human review is warranted).
- `review_needed=true` → `in-review`, exactly as today.
- **Policy knob:** `agents.self_approve_allowed` — **configurable on the
  agent configuration page** (default `true`), per-agent overridable;
  squad-level default via Helm. Agents with
  `self_approve_allowed=false` get a 409 `self_approval_disabled` when
  requesting `done` — their completions always land in `in-review`.
  Recommended `false` for deploy/infra-flavoured agents.
- **Condition (prompt-governed, not server-enforced):** self-approval is
  to be used **only for straightforward, self-evident tasks** — the
  composed prompt instructs the agent that anything involving judgment,
  irreversible side effects, or partial confidence must go to
  `in-review`. The server cannot classify "straightforward", so the
  `self_approval_justification` field plus the distinct UI marker make
  misuse visible and reviewable after the fact.
- The board UI renders self-approved `done` cards with a distinct marker
  so the owner can spot-check the population.

Rationale: making the review decision *explicit in the tool call* keeps it
auditable and policy-governable, rather than implicit in LLM phrasing.

### D3 — Reopen: chat-driven, assignee-scoped, new attempt

- `board.reopen(task_id, reason)` is valid **only** for the current
  assignee, only from `in-review` (not `done`; see D4 for why `done` is
  different), and requires a non-empty `reason`.
- Reopen creates a **new `task_execution` attempt** (new lease, new
  fencing token) — the prior completed attempt is retained for audit.
  Attempt count is visible on the task.
- The chat flow: owner messages the agent "not happy with X, do Y" →
  agent calls `board.reopen` → the task brief for the new attempt includes
  a `reviewer_feedback` section carrying Ross's message verbatim (see
  [subagent-task-execution.md](../subagent-task-execution.md) §4).
- **Reopen cap (circuit breaker):** max 3 agent-initiated reopens per
  task per rolling 24h (configurable `SKQUAD_REOPEN_CAP=3`). Beyond
  that, 429 `reopen_cap_exceeded` — the card stays `in-review` and the
  loop is broken for the agent. **The cap governs the agent only:** the
  human owner can move the card from the board UI without any limit.
  Rationale: prevents an agent that cannot fix the issue from churning
  in-review↔in-progress burning tokens indefinitely; after 3 failed
  attempts the task needs human intervention (rewrite, reassign, or
  manual fix).

### D4 — `done` is not agent-reopenable

Agents may not move `done` → anything. Rationale: `done` is the
owner-approved terminal state (or the agent's self-approved terminal
state, which the owner can challenge by reopening *before* approval —
but once a card is approved done, only a human reopens it). This keeps a
bright line: agents can iterate on *unapproved* work freely; approved
work is frozen. Self-approved `done` cards are reopenable by the
**human** only, same as approved ones.

### D5 — Leader agent is a squad capability, not a new agent type

Add squad-level capability `can_prioritize` + `can_assign_others`
(granted to one or more agents via squad settings; default: none). With
it, `board.create_task(ready=true)` and `board.assign(other_agent)` are
permitted on that squad's board. This reuses the existing
delegate/handoff materialization machinery
(`materializeDelegatedTask`) rather than forking it — the leader path is
the delegation path with the message step made optional. Without it,
agents create tasks only in `backlog` and can only assign to themselves.

### D6 — Backlog surfacing fix (bug)

`httpapi/server.go` (S-181 path, the `appendTaskThreadEvent` call reading
"Task assigned to you: …" in the task-update handler): assignee-change and create-with-assignee emit
`"Task assigned to you: <title>"` thread events **regardless of task
status**, so backlog tasks appear in agent chat immediately. Fix:

- Suppress the thread event while the task is in `backlog`.
- Emit it on the `backlog → todo` transition (the moment the task
  becomes claimable), attributed to the prioritizing human.
- Invariant established: **a task is invisible in agent chat until it is
  in a claimable column.** This matches the S-213/S-228 pickup rule —
  surfacing and claimability now use the same allowlist.

### D7 — Wake behaviour (confirmed, no change)

`agentHasPendingWork` already treats per-agent `todo || in-progress` as
pending and `syncAffectedAgentsFromTaskChange` fires on assignment. N
tasks assigned to N agents wake N pods independently. No change required;
recorded here so the requirement is pinned against regressions.

## Consequences

**Positive**

- Review effort concentrates where it matters; simple work self-closes
  with an audit trail.
- The chat→reopen→fix loop makes `in-review` a living state instead of a
  dead end.
- Leader-planned multi-agent sweeps become first-class.
- Backlog semantics become consistent across pickup and surfacing paths.

**Negative / costs**

- Larger agent tool surface → prompt must teach the verb table and the
  review-decision policy clearly, or agents will misclassify. Mitigate
  with tool descriptions + eval scenarios in the prompt test suite.
- Self-approval can be gamed by a confident-but-wrong LLM. Mitigated by
  `self_approve_allowed` policy, distinct UI marker, and the audit
  justification (retrospective review is possible even when
  pre-approval isn't).
- Reopen creates second/third attempts — read paths that assume one
  execution per task need auditing (web UI, metering rollups, run-audit
  PromptSHA joins).
- The four guards (column allowlist, deps, budget, fencing) now back
  seven verbs instead of three; the transition-validation tests grow
  roughly combinatorially. Mitigate with a table-driven transition
  matrix test in `httpapi`.

**Resolved decisions (Ross, 2026-10-24)**

1. **Approval is strictly human.** A leader agent may NOT move
   `in-review → done` on others' work. Leader gets `request_changes`
   (→ `todo` for reassignment) only. `done` via human approval, or via
   the assignee's own policy-gated self-approval (D2) — nothing else.
2. **`self_approve_allowed` is configurable on the agent configuration
   page**, default `true`, with the prompt-governed condition that
   self-approval is for straightforward, self-evident tasks only (D2).
3. **Reopen cap:** 3 per rolling 24h, configurable
   (`SKQUAD_REOPEN_CAP`). Circuit breaker for agent churn only; human
   board moves are unlimited (D3).
