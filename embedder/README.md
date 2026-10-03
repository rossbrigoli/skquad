# skquad-embedder

The embedding service behind skquad agent-memory RAG: **llama.cpp
`llama-server` serving Qwen3-Embedding-0.6B** behind an OpenAI-compatible
`/v1/embeddings` API. It is a platform-internal component — nothing calls
it directly except through the LiteLLM gateway (see §4).

| Property        | Value                                                        |
|-----------------|--------------------------------------------------------------|
| Model           | `Qwen/Qwen3-Embedding-0.6B-GGUF` (Q8_0), baked into the image |
| Output          | 1024-dim float vectors, L2-normalized (`--embd-normalize 2`)  |
| Pooling         | **last-token** (`--pooling last`) — required by Qwen3           |
| Context         | 32,768 tokens per request (larger inputs → HTTP 400)           |
| Listen          | `:8080`, model alias `qwen3-embed-0.6b`                       |
| Auth            | none (cluster-internal only; never expose externally)           |

---

## 1. Image variants (ADR-0013)

One repo, three tag variants per release — same model, same API,
different compute backend:

| Runtime  | Base image (`BASE_IMAGE` build arg)     | Published tag              |
|----------|----------------------------------------|----------------------------|
| CUDA     | `ghcr.io/ggml-org/llama.cpp:server-cuda-b11347`   | `skquad-embedder:<v>-cuda`   |
| Vulkan   | `ghcr.io/ggml-org/llama.cpp:server-vulkan-b11347` | `skquad-embedder:<v>-vulkan` |
| CPU      | `ghcr.io/ggml-org/llama.cpp:server-b11347`         | `skquad-embedder:<v>-cpu`    |

Built by the `Images` workflow (`images.yml`, `embedder-*` matrix
entries). The GGUF model is downloaded at **image build** time and baked
in (`/models/qwen3-embed-0.6b-q8_0.gguf`), so pods start with zero
external downloads.

**Why three variants:** the Vulkan image ships only Mesa userspace ICDs
(intel/radeon/nouveau). On NVIDIA it silently **falls back to CPU**
(nouveau Vulkan doesn't support Turing+; no proprietary ICD). That
fallback is the entire reason ADR-0013 exists — see
`docs/adr/0013-embedder-runtime-selection.md`.

## 2. At installation time

Chart values (`charts/skquad/values.yaml`, `embedder.*`):

1. The chart renders a normal Deployment + Service
   (`skquad-embedder`, port 8080) with the **base** image and
   `embedder.runtime: auto` by default. A cluster with **no GPU at all**
   is a fully supported install: detection finds nothing → CPU shape.
2. The chart passes the per-runtime image map
   (`embedder.images.{cuda,vulkan,cpu}`) to the operator via
   `SKQUAD_EMBEDDER_IMAGE_{CUDA,VULKAN,CPU}` env vars.
3. The **operator** (`EmbedderGPUReconciler`) takes over the Deployment
   shape from here (§3). The chart-declared GPU shape is minimal on
   purpose: the operator owns image, GPU resources, tolerations,
   node affinity, runtimeClass, and the `/dev/dri` mount.
4. The **control plane** registers the embedder in the LiteLLM gateway
   at startup (`RegisterEmbedderGatewayModel`, idempotent, retried
   while the gateway boots) so all embedding traffic flows through the
   unified gateway path (§4).

Required gitops pairing (k3s-cluster `apps/app-skquad.yaml`):
`ignoreDifferences` on the embedder Deployment's
`/spec/template/spec/containers/0/image` (and the GPU-shape paths) —
otherwise ArgoCD self-heal fights the operator's live runtime switch
and loops the pod.

## 3. At runtime (operator reconcile)

Every reconcile tick the operator computes the **effective runtime**:

```
effective = admin override (ConfigMap skquad-embedder-config: data.runtime)
            ?? auto-detect (scan node .status.allocatable)
```

Auto-detect vendor priority: any `nvidia.com/gpu` → **cuda**; else any
`amd.com/gpu` / `gpu.intel.com/i915` → **vulkan**; else **cpu**.

The effective runtime determines the pod shape
(`applyRuntimeShape` / `applyRuntimeClass` / `applyDRIMount`):

| Runtime | Image tag    | GPU resource requested | Toleration | Node affinity | runtimeClass | `/dev/dri` mount |
|---------|--------------|----------------------|------------|---------------|--------------|------------------|
| cuda    | `<v>-cuda`   | `nvidia.com/gpu: 1`  | yes        | GPU nodes     | `nvidia` (driver injection — **required on k3s**) | **stripped** (CUDA doesn't use DRI) |
| vulkan  | `<v>-vulkan` | `amd.com/gpu`/`i915` if present | if resource | GPU nodes | none | **added** by operator (`dev-dri` hostPath) |
| cpu     | `<v>-cpu`    | none                 | no         | none          | none         | none |

Notes:

- The operator **owns** the `/dev/dri` mount: it adds it for vulkan and
  removes it for cuda/cpu. Do **not** declare `embedder.gpu.devicePaths`
  in gitops values — a chart-declared mount conflicts with the
  operator's strip-for-cuda and causes a pod-recreation loop on every
  ArgoCD sync (learned the hard way, S-212).
- Admin override is read live from the ConfigMap; changing
  `data.runtime` switches the runtime without any redeploy (the pod
  rolls once). The control-plane Settings UI writer for this ConfigMap
  is **pending automation** — today it is `kubectl patch`.
- Switching runtime rolls the embedder pod: embeddings are briefly
  unavailable; callers retry/backfill after.

## 4. How memory RAG uses it

**Single embedding path:** every embedding — write-time and query-time —
is produced by the control plane calling the **LiteLLM gateway's**
OpenAI-compatible `/v1/embeddings` with the registered embedder model
(`control-plane/internal/embeddings/client.go`). The gateway fronts this
service. Unified auth, routing, and metering; the embedder itself has no
auth. The client timeout is 180 s per call (large memories on CPU take
>90 s; GPU finishes in seconds).

```
WRITE:  agent memory_write / chat-reset transcript
          → control-plane: POST gateway /v1/embeddings {model, content}
          → gateway → skquad-embedder → 1024-d vector
          → stored in pgvector with the memory row (+ embedding_model tag)

SEARCH: agent tool memory_search(q, limit)          (agent-runtime builtin_tools.py)
          → GET /api/v1/agents/me/memory/search?q=…  (control-plane)
          → embed q through the SAME gateway path
          → pgvector cosine ranking, hard-scoped to the calling agent
          → trust gating in the store query: `rejected` rows and rows
            without embeddings are never recalled
          → ranked hits (id, content, score, trust_level, provenance)
```

Failure semantics (search): tool disabled → 403; feature not configured
→ 503 `memory_search_disabled`; embedder/gateway unreachable → 502
`embedder_unavailable`; empty `q` → 400.

## 5. Model changes — backfill

When `SKQUAD_MEMORY_EMBEDDING_MODEL` changes, existing rows carry the
old `embedding_model` and must be re-embedded.
`control-plane/cmd/embed-backfill` does this idempotently (rows whose
`embedding_model` ≠ configured model; re-run is a no-op).

**Current state (0.1.196):** the control-plane image ships only
`skquad-api`; `embed-backfill` must be built and run manually (e.g.
`go build -o embed-backfill ./cmd/embed-backfill` + port-forward the
gateway/DB). Automating it as a Helm hook Job / adding it to the CP
image is a tracked follow-up.

## 6. Verifying a deployment

```bash
# Pod shape + placement (expect runtimeClass/image per effective runtime)
kubectl -n skquad-system get deploy skquad-embedder \
  -o jsonpath='{.spec.template.spec.runtimeClassName} {.spec.template.spec.containers[0].image}{"\n"}'

# Direct smoke test (bypasses the gateway)
kubectl -n skquad-system port-forward deploy/skquad-embedder 18080:8080 &
curl -s localhost:18080/v1/embeddings -H 'content-type: application/json' \
  -d '{"model":"qwen3-embed-0.6b","input":"hello skquad"}' | jq '.data[0].embedding | length'   # → 1024

# GPU acceleration check: a ~3k-token request should finish ~1s on the
# RTX A1000 (CUDA) vs ~35s on CPU. Throughput measured: ~3,450 tok/s
# (CUDA) vs ~100 tok/s (CPU fallback).
time curl -s localhost:18080/v1/embeddings -H 'content-type: application/json' \
  -d "{\"model\":\"qwen3-embed-0.6b\",\"input\":\"$(python3 -c 'print(\"word \"*3000)')\"}" -o /dev/null

# End-to-end through the control plane
curl -s "$CP/api/v1/agents/me/memory/search?q=deploy+config&limit=3" -H "Authorization: Bearer <agent-token>"
```

If CUDA-labeled pods still run ~100 tok/s, the pod is CPU-falling-back:
check `runtimeClassName: nvidia` (without it k3s injects no driver —
`nvidia-smi` absent inside the container is the tell).

## 7. Known pitfalls

- **Vulkan image + NVIDIA = silent CPU fallback.** No proprietary ICD in
  the image; no error, just slow. Use `-cuda`.
- **CUDA on k3s needs `runtimeClassName: nvidia`** or the driver is not
  injected. The operator sets this automatically for the cuda runtime.
- **Don't set `embedder.gpu.devicePaths` in gitops** — operator owns
  `/dev/dri` per-runtime (§3).
- **Requests > 32k tokens return 400** (model context limit).
- **ArgoCD `ignoreDifferences` is mandatory** for operator-owned fields
  (§2) or self-heal loops the pod.
