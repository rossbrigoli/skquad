# Agent Workspace Storage Operations (S-135 … S-139)

Operational reference for the per-agent durable workspace PVCs: sizing,
quotas, disk-full behaviour, orphan garbage collection, retention,
monitoring, and the backup stance.

## Architecture recap

- **S-135** — `Agent.spec.storage {enabled,size,storageClass,mountPath}`;
  the operator create-or-adopts `agent-<name>-workspace` (RWO) in the
  squad namespace before the Deployment, uses `Recreate` strategy, and
  cleans the PVC via the agent finalizer unless opted out.
- **S-136** — the runtime resolves the PVC as its workspace base; layout
  `git/`, `tasks/`, `scratch/` under the mount.
- **S-137** — task journal + crash-resume + TTL GC of task dirs.
- **S-138** — API/UI size selection; platform knobs on the api-server.
- **S-139** (this doc) — operational safety: operator-side cap
  admission, disk-full handling, orphan PVC GC, metrics.

## Backup stance: git is the backup, the PVC is a cache

The durable workspace PVC survives pod restarts and crashes, but it is
**not** the system of record:

- Everything that matters is **committed and pushed to git** by the task
  finalization step (`finalize_task_workspace`). Git history is the
  backup.
- `tasks/<task-id>/` scratch artifacts are durable-but-not-sacred: they
  survive crashes (resume notes, journal) and are TTL-collected
  (`SKQUAD_TASK_DIR_TTL_DAYS`). Losing the volume loses only scratch
  state, never committed work.
- **CSI snapshot option:** on clusters whose CSI driver supports
  `VolumeSnapshot`, operators may snapshot `agent-*-workspace` claims
  for forensics or point-in-time copies. The platform deliberately does
  **not** automate snapshots — it is a cluster-flavour decision (see
  `docs/observability-metering.md` for the same philosophy on metrics
  infra). Restores are manual: rebind or recreate the PVC, then let the
  agent resume.

## Knobs

| Layer | Knob | Default | Effect |
|---|---|---|---|
| Helm (api-server) | `apiServer.defaultAgentStorageSize` → `SKQUAD_DEFAULT_AGENT_STORAGE_SIZE` | `2Gi` | UI/API default size |
| Helm (api-server) | `apiServer.maxAgentStorageSize` → `SKQUAD_MAX_AGENT_STORAGE` | `10Gi` | API validation ceiling |
| Helm (api-server) | `apiServer.storageClass` → `SKQUAD_STORAGE_CLASS` | `""` (cluster default) | Platform storage class |
| Helm (operator) | `operator.maxAgentStorage` → `SKQUAD_MAX_AGENT_STORAGE` | `10Gi` | **Operator admission cap** (S-139) |
| Operator | `SKQUAD_ORPHAN_PVC_SWEEP_INTERVAL_MINUTES` | `15` | Orphan GC sweep interval |
| Runtime | `SKQUAD_MIN_FREE_BYTES` | `104857600` (100 MiB) | Disk-full floor per task |
| Runtime | `SKQUAD_TASK_DIR_TTL_DAYS` | (see S-137) | Task dir TTL GC |
| Runtime | `SKQUAD_WORKSPACE_MOUNT_PATH` | `/workspace` | Injected by the operator when storage is enabled (S-139 follow-up to S-136) so runtime auto-resolution matches a non-default `spec.storage.mountPath` |
| Annotation | `skquad.io/retain-pvc: "true"` on the **Agent** | absent | Keep PVC when the Agent is deleted (S-135) |
| Annotation | `skquad.io/retain-pvc: "true"` on the **PVC** | absent | Orphan GC reports but never deletes (S-139) |
| Label | `skquad.io/workspace-pvc: "true"` on the PVC | set by operator | Identifies platform-managed workspace claims (S-139) |

Keep the operator cap and the API cap consistent (`operator.maxAgentStorage`
= `apiServer.maxAgentStorageSize`) unless you deliberately want the
operator stricter than the API. An invalid operator cap value falls back
to `10Gi`.

## Size-cap admission (operator)

The API validates sizes, but a directly-applied or restored Agent CR must
not conjure an oversized volume. During reconcile, if
`spec.storage.size > SKQUAD_MAX_AGENT_STORAGE`:

- **No PVC is created** (an existing claim is never resized or deleted by
  this check).
- `WorkspaceReady=False`, reason `StorageSizeExceedsPlatformMax`, with
  the requested vs. max sizes in the message.
- A Kubernetes **Warning event** `StorageSizeExceedsPlatformMax` is
  recorded on the Agent.
- The agent never starts (readiness blocks on the workspace condition).

Fix by shrinking `spec.storage.size` or raising the operator cap and
re-reconciling.

## Disk-full behaviour (runtime)

Before starting a task (and again after creating the task dir), the
runtime checks `shutil.disk_usage()` on the resolved workspace base:

- Free space `< SKQUAD_MIN_FREE_BYTES` → the task is **blocked cleanly**
  with summary `workspace filesystem full: <base> has <n> bytes free,
  below the <floor>-byte floor (SKQUAD_MIN_FREE_BYTES)`. No handler code
  runs, so there is no silent truncation; the blocked status and summary
  surface in the agent/task status.
- If the stat itself fails (unusual filesystem), the task **proceeds**
  with a warning — the probe must never wedge otherwise-healthy agents.

Operational response: expand the PVC/CSI pool (note: shrinking a bound
claim never happens automatically), or lower the floor only if you
understand the truncation risk.

## Orphaned PVC garbage collection (operator)

A workspace PVC is *orphaned* when it carries
`skquad.io/workspace-pvc="true"` in a platform-managed squad namespace
but no Agent CR with the matching `skquad.io/agent-id` label exists
(force-deleted agent, restore leftovers, etc.).

The sweep runs **once at operator startup and every
`SKQUAD_ORPHAN_PVC_SWEEP_INTERVAL_MINUTES` (default 15)** on the
leader only. Guards — the GC never touches unrelated PVCs:

1. only PVCs labeled `skquad.io/workspace-pvc="true"`;
2. only inside namespaces that host a Squad CR;
3. only when the PVC's `skquad.io/agent-id` matches **no** live Agent;
4. never while a Deployment in the namespace still references the claim
   by name (a live/pending pod may mount it) — skipped and logged;
5. PVCs annotated `skquad.io/retain-pvc="true"` are **reported, never
   deleted** (log line + `WorkspacePVCRetained` event).

Everything passing the guards is deleted with a `WorkspacePVCOrphanCleaned`
event. Note: deleting a Squad deletes its namespace, so Squad-level
cleanup does not rely on this sweep; the GC targets agent-shaped leaks.

**Migration note:** PVCs created before S-139 lack the marker label.
They are invisible to the GC until the owning agent reconciles once, at
which point the operator migrates the label onto the existing claim in
place (adoption is otherwise unchanged).

## Monitoring

The operator already serves Prometheus metrics via the controller-runtime
metrics server (`--metrics-bind-address`, default `:8080`, port named
`metrics`). S-139 adds three collectors on that same endpoint (no new
infra):

| Metric | Type | Meaning |
|---|---|---|
| `skquad_workspace_pvc_created_total` | counter | Workspace PVCs created (adoptions excluded) |
| `skquad_workspace_pvc_pending` | gauge | Labeled workspace PVCs in `Pending` at the last sweep — stuck > 0 ⇒ provisioning cannot bind (alert hook) |
| `skquad_workspace_orphans_cleaned_total` | counter | Orphaned workspace PVCs deleted |

**Gaps (documented, not built):**

- The chart ships **no ServiceMonitor / scrape config**; the
  `observability.enabled` toggle and `docs/observability-metering.md`
  describe the intended Prometheus wiring. Until that exists, scrape
  `skquad-operator:8080/metrics` manually or via external config.
- **Per-PVC usage/capacity** (bytes used vs. requested) is *not*
  exposed by the operator — that belongs to kube-state-metrics
  (`kubelet_volume_stats_*`) plus cAdvisor/CSI metrics, which are
  cluster-level concerns. The Pending gauge above is the cheap
  platform-side signal.
- Runtime disk-full events are surfaced as blocked-task summaries and
  runtime logs, not as Prometheus metrics (the runtime does not expose
  a metrics endpoint today).
