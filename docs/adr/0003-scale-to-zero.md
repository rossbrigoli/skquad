# ADR-0003: Scale-to-Zero — Operator-Driven

- **Status:** Accepted
- **Date:** 2026-08-22
- **Deciders:** Ross, Sherlock

## Context

Each agent runs in its own pod, and **agent pods must scale to zero when idle**
(NFR-2). On vanilla Kubernetes, scaling a Deployment to 0 replicas is trivial,
but deciding **when** to scale up/down is not native. We need a mechanism that:

- Scales an agent **0 → 1** when it has work (a task is assigned, or a
  message/ping is queued).
- Scales an agent **1 → 0** when it is idle (task done, queue drained, idle
  timeout elapsed).
- Works on **vanilla K8s** (no OLM).

## Decision

Use **operator-driven scale-to-zero**. The skquad **operator** (which already
manages squad/agent lifecycle) is the single authority for agent scaling:

- **Scale up (0 → 1):** when the control plane records that an agent has
  pending work — a task assigned to it, or a message/ping queued for it — it
  sets a "desired active" annotation/condition on the agent's Deployment (or a
  custom `Agent` resource). The operator reconciles replicas to 1.
- **Scale down (1 → 0):** when the agent reports it is idle (task complete,
  message queue drained) **and** an **idle timeout** elapses with no new work,
  the operator scales replicas to 0.
- The **idle timeout** is configurable (per agent or platform default).

The operator is the only component that changes agent replica counts, so there
is a single, auditable source of truth for scaling.

## Consequences

- **(+)** Single authority for scaling → predictable, auditable, no races.
- **(+)** Reuses the operator we already need for lifecycle → no new component.
- **(+)** Works on vanilla K8s (just sets Deployment replicas).
- **(+)** Idle timeout is configurable → balance cost vs. latency.
- **(−)** Scale-up latency = pod start time (cold start). Mitigate with
  pre-warming / image caching if latency becomes an issue.
- **(−)** The operator must correctly track "pending work" (tasks + messages).
- **Mitigation:** the control plane is the source of truth for pending work;
  the operator only reacts to it. Add metrics for scale-up/down latency.

## Alternatives Considered

- **KEDA** — event-driven autoscaling with native scale-to-zero (e.g. on queue
  length). **Rejected** for v1 because it adds a second scaling authority and
  an extra dependency; the operator already has the context (tasks + messages)
  and is the natural place. **Revisit** if we need richer triggers (cron,
  external queues) or the operator approach proves fragile.
- **Always-on pods** — simplest, but violates the scale-to-zero requirement and
  wastes resources across many squads. **Rejected.**

## Related

- The idle timeout in this ADR is the **pod cost timer**. It is distinct
  from the per-task **execution lease** (worker liveness + fencing, 2 min
  renewed every 40 s). See
  [`kanban-task-lifecycle.md` §9](../kanban-task-lifecycle.md#9-execution-lease-vs-scale-to-zero-idle-timeout)
  for how the two interact, including the `idleTimeout=0` budget-block
  teardown path (S-203 WP3).
