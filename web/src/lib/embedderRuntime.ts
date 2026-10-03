// S-212 (ADR-0013 §4): platform-admin embedder runtime selection.
//
// The embedder (llama.cpp + Qwen3-Embedding-0.6B) ships three compute
// variants — cuda / vulkan / cpu. "auto" lets the operator pick by
// scanning node allocatable resources (nvidia → cuda, amd/intel →
// vulkan, else cpu). An explicit admin choice overrides detection.
// The control-plane persists the choice in platform_settings AND writes
// the operator's override ConfigMap in the same PUT; the operator
// switches the live embedder Deployment without a Helm redeploy.

import { apiGet, apiPut } from "./api";

export type EmbedderRuntime = "auto" | "cuda" | "vulkan" | "cpu";

export const EMBEDDER_RUNTIMES: EmbedderRuntime[] = ["auto", "cuda", "vulkan", "cpu"];

export const DEFAULT_EMBEDDER_RUNTIME: EmbedderRuntime = "auto";

export type EmbedderRuntimeSettings = {
  embedder_runtime?: string;
};

// validateEmbedderRuntime returns a human-readable error, or null when
// the value is one of the accepted runtimes (mirrors the control-plane
// PUT validation).
export function validateEmbedderRuntime(value: string): string | null {
  if (!EMBEDDER_RUNTIMES.includes(value as EmbedderRuntime)) {
    return `Must be one of: ${EMBEDDER_RUNTIMES.join(", ")}`;
  }
  return null;
}

export async function fetchEmbedderRuntime(token: string): Promise<EmbedderRuntime> {
  const res = await apiGet<EmbedderRuntimeSettings>("/admin/settings", token);
  const raw = (res.embedder_runtime ?? DEFAULT_EMBEDDER_RUNTIME).toLowerCase() as EmbedderRuntime;
  return validateEmbedderRuntime(raw) ? DEFAULT_EMBEDDER_RUNTIME : raw;
}

export async function saveEmbedderRuntime(token: string, runtime: EmbedderRuntime): Promise<EmbedderRuntime> {
  const error = validateEmbedderRuntime(runtime);
  if (error) {
    throw new Error(error);
  }
  const res = await apiPut<EmbedderRuntimeSettings>("/admin/settings", token, {
    embedder_runtime: runtime,
  });
  return (res.embedder_runtime ?? runtime) as EmbedderRuntime;
}
