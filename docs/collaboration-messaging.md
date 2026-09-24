# skquad — Agent Collaboration & Messaging Design

> **Status:** Draft v1 · **Decision:** [ADR-0004](adr/0004-message-bus.md)
>
> Agents collaborate **asynchronously**. Within a squad they **pull tasks** and
> **message each other** (delegate, consult). Across squads, a **user-defined
> allow-list** (access grants) controls which agents may talk to which. All
> messaging is **queued**, so a **busy agent is not disturbed** (protecting its
> task context).
>
> The queue, runtime inbox path, retry scheduling, expiry, and dead-letter
> transitions are implemented. Automatic `delegate`/`handoff` task
> materialization and richer consult/reply workflows remain tracked follow-up
> work; see [`implementation-status.md`](implementation-status.md).

---

## 1. Principles

- **Async first** — agents never block on each other. Messages are queued and
  delivered when the recipient is free.
- **Context protection** — a working agent's task context is not interrupted by
  incoming messages; they wait in the agent's **inbox**.
- **Governed** — cross-squad communication requires an **access grant** issued
  by the relevant squad owner(s).
- **Task-centric** — collaboration often manifests as **tasks** (delegation,
  handoff) on a board, plus **messages** (consultation, pings).

---

## 2. Message Types

| Type | Purpose | Typical effect |
|------|---------|----------------|
| **consult** | Ask another agent a question / request input (e.g. code review). | Recipient answers; no task created. |
| **delegate** | Hand a unit of work to another agent. | Creates/assigns a **task** to the recipient. |
| **handoff** | Cross-squad: add a task to another squad's board + ping an agent. | Creates a **task** on the target board + a **ping** message. |
| **ping** | Wake / notify an agent (e.g. "you have a task"). | Triggers scale-up + inbox check. |
| **reply** | Response to a `consult`. | Delivered to the original sender's inbox. |

---

## 3. Message Model

```
message(
  id,
  from_type,        # agent | user
  from_id,
  to_agent_id,      # recipient agent
  squad_id,         # recipient's squad
  type,             # consult | delegate | handoff | ping | reply
  payload,          # JSON (question, task ref, context, etc.)
  status,           # pending | delivered | expired
  correlation_id,   # links a reply to its consult
  created_at,
  delivered_at
)
```

- Messages are stored in the **message queue** (Postgres-backed for v1 — see
  ADR-0004).
- Each agent has an **inbox** (messages where `to_agent_id = agent` and
  `status = pending`).
- Current implementation includes the control-plane queue API:
  `POST /api/v1/agents/me/messages`, `GET /api/v1/agents/me/messages`,
  `POST /api/v1/agents/me/messages/:id/ack`, and user-facing
  `POST`/`GET /api/v1/agents/:id/chat`.
- **Chat reply payload (S-122):** agent-authored `reply` messages posted from
  the chat handler carry extra keys in their JSON payload alongside `message`:
  `tool_calls` (array of `{name, arguments, ok, result}` — one per tool
  invocation during the turn, result truncated) and `context_tokens` (the
  prompt-token size of the final LLM call). The control plane stores payloads
  verbatim (JSONB, no schema change), and the web chat UI renders these for
  tool-call visibility and the context-size status bar.

---

## 4. Intra-Squad Collaboration

Agents in the **same squad** (same namespace) collaborate:

- **Pull tasks** from the shared **Kanban board** (see
  [kanban-task-lifecycle.md](kanban-task-lifecycle.md)).
- **Consult** — an agent sends a `consult` message to a peer (e.g. "review this
  diff"). The peer answers with a `reply` when free.
- **Delegate** — an agent sends a `delegate` message, which **creates/assigns a
  task** to the peer. The peer picks it up when free.

```
Agent A (working on task T)
  → needs a code review
  → sends `consult` to Agent B (reviewer)
  → B is busy → message queued in B's inbox
  → B finishes its task → drains inbox → reviews → sends `reply` to A
  → A (now free or on next task) receives the review
```

> Because messages are **queued**, a busy reviewer is **not interrupted** — the
> consultation waits until the reviewer is free.

---

## 5. Cross-Squad Collaboration

Agents in **different squads** (different namespaces) can collaborate **only if
permitted** by **access grants**.

- The **human user** (squad owner) defines **which agents may talk to other
  squads** (via `AccessGrant` — see [identity-security.md](identity-security.md)).
- A permitted agent can:
  - **Add a task to another squad's board** (a `handoff`).
  - **Ping an agent** in the other squad to pick it up.

```
Agent A (squad 1)
  → has a sub-task that belongs to squad 2
  → checks access grant (may squad 1's agent talk to squad 2's agent?)
  → if permitted:
      → creates a task on squad 2's board
      → sends a `ping`/`handoff` to Agent B (squad 2)
  → operator scales Agent B 0 → 1
  → B picks up the task
```

- **Enforcement:** the **control plane** checks the access grant **before**
  enqueuing a cross-squad message or allowing a task to be created on another
  squad's board. Without a grant, the request is **rejected** and **audited**.

---

## 6. Delivery & Context Protection

- **While an agent is working** (busy), incoming messages are **queued** in its
  inbox — **not delivered**. This protects the task context.
- **When the agent becomes idle** (task complete), it **drains its inbox**:
  - `ping` / `handoff` → pick up the referenced task (scale-up if needed).
  - `consult` → answer using the LLM + context, send a `reply`.
  - `delegate` → pick up the delegated task.
- **Ordering:** messages are delivered in **creation order** (FIFO) per inbox.
- **Expiry:** messages can have a TTL; expired messages are dropped (and
  audited) to avoid stale work.

```
Agent idle → check inbox
  → for each pending message (FIFO):
      → handle (pick up task / answer consult / ...)
  → if new work resulted → become busy again
  → else → remain idle (→ scale-to-zero after timeout)
```

---

## 7. Relationship to the Operator (scale-up)

- A **ping** or a **task assignment** to an idle agent **signals the operator**
  to scale that agent **0 → 1** (see [deployment-operator.md](deployment-operator.md)).
- The agent, once up, **drains its inbox** and starts the work.
- This is how a **cross-squad handoff** wakes a sleeping agent.

Current implementation note: the control plane mirrors pending inbox messages
into the target Agent CR activity signal. The runtime fetches retry-due inbox
messages, invokes an injected handler, acknowledges messages after successful
handling, and reports handler failures so the control plane can increment
attempts, schedule the next retry, expire stale messages, or dead-letter
messages whose retry budget is exhausted. Automatic delegate/handoff task
materialization remains a follow-up slice.

---

## 8. Relationship to Other Components

- **Message queue** (Postgres-backed, ADR-0004) — stores + delivers messages.
- **Control plane / API server** — enqueues messages after enforcing access
  grants; creates tasks for delegation/handoff.
- **Operator** — scales agents up on ping/assignment.
- **Agent runtime** — drains the inbox when idle; sends messages.
- **Kanban board** — delegation/handoff create tasks on boards.
- **Identity & AuthZ** — access grants govern cross-squad messaging.
- **Postgres** — stores messages.
- **Audit** — cross-squad messages and task creation are audited.

---

## 9. Consistency & Failure

- **At-least-once delivery** — a message is marked `delivered` only after the
  recipient acknowledges; on crash, it is re-delivered (idempotent handling).
- **Idempotent handling** — agents handle a message idempotently (e.g. by
  `correlation_id` / message id) to avoid duplicate work.
- **Retry and dead-letter** — failed handler attempts remain `pending` but are
  hidden from the runtime until `next_retry_at`; after `max_attempts` they move
  to `dead` with `terminal_reason`. Expired messages move to `expired` and no
  longer wake the target agent.
- **No lost messages** — the Postgres-backed queue is durable.

---

## 10. Open Points

- **v2 broker** — move to **NATS** behind the same interface if throughput /
  latency requirements grow (ADR-0004).
- **Group messaging** — broadcast to multiple agents (start 1:1).
- **Message UI** — a view of an agent's inbox / conversation history in the web
  app (later).
- **Consultation SLAs** — timeouts/escalation for unanswered consults (later).
