# ADR-0009: Workspace as Git — Repos Are the Shared Squad Workspace

- **Status:** Accepted (v1 scope)
- **Date:** 2026-09-22
- **Deciders:** Ross, Sherlock

## Context

Squads need a shared, durable place for agents to produce real artifacts —
code, docs, generated files — that survives the ephemeral agent pod and is
reviewable by humans. Ross's decision (2026-09-21): **a git repository is
the shared workspace for a squad.** An agent granted a workspace clones it,
does its work on an isolated branch, and pushes; a human reviews/merges.

This builds on the existing generic registry (`RegistryResource`) and the
Layer-2 agent permission model (`AgentPermission` grants any resource type
to an agent), so a workspace is "just another registry resource" for
grant/revocation purposes.

## Decision

### Registry: `project_workspace` of kind `git`

A workspace is a `RegistryResource` with `type = project_workspace` and a
manifest declaring `kind: "git"`:

| Field      | Meaning                                              |
|------------|------------------------------------------------------|
| `endpoint` | Repo URL (HTTPS or SSH)                              |
| `auth_ref` | Reference to a git credential (Secret), opaque to CP |
| `manifest.kind` | `"git"` (required)                              |
| `manifest.default_branch` | Base branch to branch from (required)   |

Registration **validates** this contract: a `project_workspace` must have
`kind=git`, a non-empty `endpoint`, a non-empty `default_branch`, and a
non-empty `auth_ref`. (Enforced in `createRegistryResource`.)

### Permissions: granted like any resource

Workspaces are granted to agents via the existing
`PUT /api/v1/agents/{id}/permissions` with
`resource_type = project_workspace`. **No new permission machinery.**
Revoking the grant immediately removes the agent's ability to link a task
to that workspace (the report endpoint re-checks the grant + active status).

### Task ↔ workspace linkage

A task records the workspace it ran against and what the agent pushed, as
an audit trail (migration `0009_task_workspace_link`):

- `workspace_resource_id` — the registry workspace
- `workspace_branch` — the branch the agent pushed (e.g. `skquad/<agent>/<task-id>`)
- `workspace_commit_sha` — the head commit pushed

The runtime reports these via `POST /api/v1/agents/me/tasks/{id}/workspace`.
The endpoint enforces: the task must be **assigned to the calling agent**,
and the workspace must be **granted to that agent and active** — so an
agent cannot forge refs to a workspace it cannot access.

### Conflict policy (v1): branch-per-agent-task, human merge

- Each task works on its own branch `skquad/<agent>/<task-id>` off
  `default_branch`.
- **No automatic merge.** Merging to the default branch is a human/owner
  step (PR/review). This keeps v1 simple and avoids silent merge conflicts
  between concurrent agents.

### Credential model

`auth_ref` is an **opaque reference** resolved to a Kubernetes Secret
mounted into the agent pod. The control plane does not store the
credential itself. **Finalized: HTTPS token** (Ross, 2026-09-22). The
Secret holds the git HTTPS token under the key `token` in the agent's
namespace; `auth_ref` is `k8s://<namespace>/<secret-name>` (a bare
Secret name in the same namespace also resolves).

**Propagation chain (implemented):**

1. Owner registers a `project_workspace` (kind `git`) with `auth_ref`
   pointing at the token Secret, and grants it to agents via
   `PUT /api/v1/agents/{id}/permissions`.
2. `SetAgentPermissions` (memory + Postgres) enqueues an `upsert_agent`
   Kubernetes outbox event so grant changes re-converge the CR.
3. The outbox worker **derives** `spec.workspaceSecrets[]` at apply time
   from the agent's live grants: only `active`, `kind=git` resources with
   a resolvable `auth_ref` produce an entry; stale/inactive/non-git
   grants are skipped so one bad grant cannot block the sync. The list is
   **derived, not persisted** on the `agents` table — grants stay the
   single source of truth.
4. The operator mounts each Secret read-only at
   `/var/run/skquad/workspaces/<resourceId>/token`.
5. The runtime reads the token from that mount at task time, clones into
   `skquad/<agent>/<task-id>`, commits, pushes, and reports refs via
   `POST /api/v1/agents/me/tasks/{id}/workspace`. The token is scrubbed
   from `.git/config` after every operation.

## Consequences

- **(+)** Real, durable, human-reviewable artifacts; git is the source of truth.
- **(+)** Reuses the registry + permission model — minimal new surface.
- **(+)** Branch-per-task isolates concurrent agents; no cross-agent clobbering.
- **(+)** Task→branch→commit gives a clean audit trail.
- **(-)** No auto-merge means humans must merge; many branches can accumulate.
- **(-)** Runtime needs git + credentials in the pod (attack surface) —
  mitigated by per-workspace scoped credentials, read-mostly default, and
  grant re-checking.
- **Mitigation:** branch naming is namespaced per agent+task; owner reviews
  and prunes merged branches.

## Resolved decision (2026-09-22)

- **Git credential type:** **HTTPS token** (Ross). Secret shape: key
  `token` in the agent namespace; `auth_ref` = `k8s://<ns>/<secret>` or a
  bare Secret name. SSH deploy keys remain possible later because
  `auth_ref` stayed opaque.

## Alternatives Considered

- **Object-store / DB blob workspace** — no git tooling needed, but loses
  diff/review/branch semantics humans rely on. **Rejected** for v1.
- **Shared working branch per squad** — fewer branches, but concurrent
  agents collide. **Rejected** in favour of branch-per-task.
- **Automatic merge on completion** — smoother, but conflict handling is a
  large, risky surface. **Deferred** past v1.
