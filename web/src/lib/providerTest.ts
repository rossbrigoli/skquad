// S-180 — pre-save "Test" button logic for the LLM provider and AI
// model forms. Kept out of the React components so the payload shapes
// and result formatting are unit-testable (repo "logic layer" pattern).
//
// Contracts (control-plane connectivity.go):
//   POST /registry/llm-providers/test  {base_url, api_key?, provider_id?}
//   POST /ai-models/test               {provider_id, model_name}
// Both answer 200 {ok, reason, latency_ms, detail} once the probe ran;
// the API key is never echoed back by the server, and nothing here
// renders it either.

import { ApiError } from "./api";

export type TestResult = {
  ok: boolean;
  reason: string;
  latency_ms: number;
  detail?: string;
};

export const TEST_REASON_LABELS: Record<string, string> = {
  connected: "Connected",
  auth_failed: "Auth failed",
  timeout: "Timed out",
  unreachable: "Unreachable",
  provider_error: "Provider error",
  pong_mismatch: "No PONG",
};

// parseTestResult defensively normalises the server envelope. Anything
// malformed degrades to a provider_error so the UI never shows a blank
// or misleading status.
export function parseTestResult(body: unknown): TestResult {
  const raw = body as Partial<TestResult> | null;
  if (!raw || typeof raw !== "object") {
    return { ok: false, reason: "provider_error", latency_ms: 0, detail: "malformed test response" };
  }
  return {
    ok: raw.ok === true,
    reason: typeof raw.reason === "string" && raw.reason !== "" ? raw.reason : "provider_error",
    latency_ms: Number.isFinite(raw.latency_ms) ? Math.max(0, Math.trunc(raw.latency_ms as number)) : 0,
    detail: typeof raw.detail === "string" ? raw.detail : "",
  };
}

function truncate(text: string, max: number): string {
  const trimmed = (text ?? "").trim();
  if (trimmed.length <= max) return trimmed;
  return `${trimmed.slice(0, max)}…`;
}

// formatTestResult renders the one-line status shown next to the Test
// button: label, latency, and a truncated safe detail.
export function formatTestResult(result: TestResult): string {
  const label = TEST_REASON_LABELS[result.reason] ?? result.reason ?? "Unknown";
  const detail = result.detail ? ` — ${truncate(result.detail, 160)}` : "";
  return `${label} in ${result.latency_ms} ms${detail}`;
}

// testResultClass picks the CSS class (green ✓ / red ✗) for the status.
export function testResultClass(result: TestResult): string {
  return result.ok ? "test-result ok" : "test-result bad";
}

// buildProviderTestPayload builds the POST body from UNSAVED form
// values. Empty api_key with a providerId means "test the stored key"
// (edit form's leave-blank-to-keep semantics). Throws when there is
// nothing to test (no base URL and no provider to fall back to).
export function buildProviderTestPayload(v: {
  base_url: string;
  api_key: string;
  providerId?: string;
}): Record<string, string> {
  const payload: Record<string, string> = {};
  const baseUrl = (v.base_url ?? "").trim();
  const apiKey = (v.api_key ?? "").trim();
  const providerId = (v.providerId ?? "").trim();
  if (baseUrl === "" && providerId === "") {
    throw new Error("Enter a base URL (or save the provider) before testing");
  }
  if (baseUrl !== "") payload.base_url = baseUrl;
  if (apiKey !== "") payload.api_key = apiKey;
  if (providerId !== "") payload.provider_id = providerId;
  return payload;
}

// buildModelTestPayload builds the PONG round-trip body. The provider's
// key is resolved server-side from the provider_id — the browser never
// sends it for model tests.
export function buildModelTestPayload(providerId: string, modelName: string): {
  provider_id: string;
  model_name: string;
} {
  const id = (providerId ?? "").trim();
  const name = (modelName ?? "").trim();
  if (id === "" || name === "") {
    throw new Error("Select a provider and enter a model name before testing");
  }
  return { provider_id: id, model_name: name };
}

// failureResult converts a thrown request error (transport, 4xx on the
// test endpoint itself) into a displayable failed TestResult. ApiError
// bodies are NOT rendered raw — only the message — so nothing sensitive
// can leak through an unexpected error shape.
export function failureResult(err: unknown, fallback: string): TestResult {
  const message =
    err instanceof ApiError && err.message ? err.message : err instanceof Error ? err.message : fallback;
  return { ok: false, reason: "provider_error", latency_ms: 0, detail: truncate(message || fallback, 160) };
}
