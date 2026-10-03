# ADR-0013 — Embedder Runtime Selection (CUDA / Vulkan / CPU)

- **Status:** Accepted — implemented and live (releases 0.1.193–0.1.196, S-212)
- **Date:** 2026-10-03
- **Ops docs:** [`embedder/README.md`](../../embedder/README.md) (install/runtime behaviour, RAG flow, verification)
- **Relates to:** S-212 (memory_search RAG), ADR-0012 (built-in tools)

## Context

The `skquad-embedder` (llama.cpp serving Qwen3-Embedding-0.6B for agent
memory RAG) shipped as a single **Vulkan** image. On the lab's NVIDIA
RTX A1000 that image silently falls back to CPU: the container ships
only Mesa Vulkan ICDs (intel/radeon/nouveau/lvp/…) and **no NVIDIA
proprietary Vulkan ICD**, and `nouveau` Vulkan does not support Turing+.
Result: embeddings run at CPU speed (a 68 KB / ~17k-token memory row
does not finish in >5 min).

The cluster's GPU inventory changes over time (no GPU at install → later
an AMD node → later an NVIDIA node). The runtime that accelerates the
embedder depends on the vendor present. We need the embedder runtime to
be **selectable and switchable without a Helm redeploy**, both
automatically at install and manually by a platform admin.

## Decision

Introduce a single knob — **`embedder.runtime` ∈ {`auto`, `cuda`,
`vulkan`, `cpu`}** — that drives BOTH the container image variant AND
the pod's GPU resource/mount shape.

### 1. Image variants (three, published per release)

| Runtime  | Base (llama.cpp)        | Image tag variant            |
|----------|-------------------------|----------------------------|
| cuda     | `server-cuda-b11347`    | `skquad-embedder:<v>-cuda` |
| vulkan   | `server-vulkan-b11347`  | `skquad-embedder:<v>-vulkan` |
| cpu      | `server-b11347`         | `skquad-embedder:<v>-cpu`  |

Same model (Qwen3-Embedding-0.6B-Q8_0), same OpenAI-compatible
`/v1/embeddings`, same 1024-dim output. Only the compute backend
differs. Tag-suffix variants on one repo (not three repos) keeps the
chart/CI simple.

### 2. Runtime → pod shape (operator-owned)

| Runtime | GPU resource requested | Taint toleration | Device mounts | Notes |
|---------|-----------------------|----------------|---------------|-------|
| cuda    | `nvidia.com/gpu: 1`   | yes            | (CUDA injected by nvidia runtime) | `NVIDIA_VISIBLE_DEVICES` auto |
| vulkan  | `amd.com/gpu`/`gpu.intel.com/i915` if present, else none | yes (if resource) | `/dev/dri` | vendor-neutral GPU via DRM render node |
| cpu     | none                  | no             | none | explicit CPU |

### 3. Auto-detection (operator, `auto` mode)

Scan node `.status.allocatable` in vendor priority:
1. any node has `nvidia.com/gpu` → **cuda**
2. else any node has `amd.com/gpu` or `gpu.intel.com/i915` → **vulkan**
3. else → **cpu**

At install `runtime` defaults to `auto`, so the platform self-selects
with no admin action.

### 4. Admin override (Settings menu)

`embedder.runtime` is stored in `platform_settings` (key
`skquad.embedder.runtime`), editable by a platform admin from the
Settings → Scaling screen: a dropdown **Auto / CUDA / Vulkan / CPU**,
with a read-only "detected GPUs" hint (vendor + count) so the admin
knows what `auto` would pick. Precedence: explicit admin choice
(cuda/vulkan/cpu) overrides detection; `auto` defers to detection.

### 5. Control-plane → operator handoff: a ConfigMap

The operator has no DB access. On `PUT /admin/settings`, the
control-plane writes the admin's choice into ConfigMap
`skquad-embedder-config` (`data.runtime`). The operator reads that
ConfigMap each reconcile tick. This mirrors the existing
"control-plane owns intent, operator reconciles" split (same shape as
the agent-mirror outbox, but a single resource so a plain ConfigMap is
simpler than an outbox event).

### 6. GitOps ownership: ArgoCD `ignoreDifferences`

The embedder Deployment's runtime-controlled fields
(`image`, GPU `resources`, `tolerations`, `affinity`, the `/dev/dri`
volume) are owned by the **operator**, not the chart. The skquad
Application declares `ignoreDifferences` for those JSON paths so
ArgoCD self-heal does not fight the operator's live runtime switch.
The chart still owns everything else (replicas, probes, non-GPU env).

## Component changes

- **embedder/**: parameterize the Dockerfile by `BASE_IMAGE` (already
  supports it); add CI matrix to build+publish the three tag variants.
- **operator/**: extend `EmbedderGPUReconciler` to (a) read the
  `skquad-embedder-config` ConfigMap, (b) compute effective runtime
  (override ?? auto-detect), (c) patch the container **image** to the
  runtime variant, (d) apply the runtime→shape mapping (resource,
  toleration, affinity, `/dev/dri`). New `runtime` values replace the
  coarse `mode` (map auto→auto, gpu→(cuda|vulkan by vendor), cpu→cpu).
- **control-plane/**: new `PlatformSettingEmbedderRuntime` key;
  `getAdminSettings`/`putAdminSettings` gain a `runtime` field
  (validated ∈ auto|cuda|vulkan|cpu); write the ConfigMap on change;
  a `GET /admin/embedder` (or extend settings) returns detected GPUs.
- **web/**: `EmbedderRuntimePanel` in the Scaling tab — dropdown +
  detected-GPU hint + save.
- **chart/**: `embedder.runtime` value (default `auto`), image-variant
  templating, and `ignoreDifferences` in the skquad Application.

## Consequences

+ Embeddings accelerate on whatever GPU exists; switchable live.
+ Install-time zero-config (auto); admin can force a vendor later.
+ Clean ownership: operator owns runtime shape, GitOps ignores it.
− Three images per release (build/CI cost, ~larger registry footprint).
− Switching runtime rolls the embedder pod (brief embedding unavailability;
  backfill/queue drains after).
− Requires the CUDA image to actually use the A1000 (validate at deploy;
  the whole reason we are here is a Vulkan-on-NVIDIA surprise).

## Open questions for Ross

1. Image scheme: tag-suffix variants on one repo (`-cuda/-vulkan/-cpu`)
   — OK, or separate repos?
2. When admin picks a runtime whose GPU is absent (e.g. cuda with no
   NVIDIA node), do we **pend** the pod (explicit intent) or **fall
   back to cpu** with a warning? (Recommend: pend + surface in UI, so
   the admin sees it didn't land.)
3. Keep the old `mode: auto|gpu|cpu` as a deprecated alias, or hard
   replace? (Recommend: hard replace, migrate the one gitops value.)
