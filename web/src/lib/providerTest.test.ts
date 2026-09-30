// S-180 — unit tests for the pre-save Test button logic layer.

import { describe, expect, it } from "vitest";
import { ApiError } from "./api";
import {
  buildModelTestPayload,
  buildProviderTestPayload,
  failureResult,
  formatTestResult,
  parseTestResult,
  testResultClass,
  type TestResult,
} from "./providerTest";

describe("parseTestResult", () => {
  it("normalises a well-formed envelope", () => {
    const r = parseTestResult({ ok: true, reason: "connected", latency_ms: 42, detail: "2 model(s) visible" });
    expect(r).toEqual({ ok: true, reason: "connected", latency_ms: 42, detail: "2 model(s) visible" });
  });

  it("degrades malformed bodies to provider_error", () => {
    for (const bad of [null, undefined, "nope", 42, {}]) {
      const r = parseTestResult(bad);
      expect(r.ok).toBe(false);
      expect(r.reason).toBe("provider_error");
      expect(r.latency_ms).toBe(0);
    }
  });

  it("clamps weird latency values", () => {
    expect(parseTestResult({ ok: true, reason: "connected", latency_ms: -5 }).latency_ms).toBe(0);
    expect(parseTestResult({ ok: true, reason: "connected", latency_ms: 12.7 }).latency_ms).toBe(12);
    expect(parseTestResult({ ok: true, reason: "connected", latency_ms: "x" }).latency_ms).toBe(0);
  });
});

describe("formatTestResult", () => {
  it("renders label, latency and detail", () => {
    const r: TestResult = { ok: false, reason: "auth_failed", latency_ms: 91, detail: "HTTP 401" };
    expect(formatTestResult(r)).toBe("Auth failed in 91 ms — HTTP 401");
  });

  it("truncates long details", () => {
    const r: TestResult = { ok: false, reason: "provider_error", latency_ms: 1, detail: "x".repeat(400) };
    const out = formatTestResult(r);
    expect(out.length).toBeLessThan(200);
    expect(out.endsWith("…")).toBe(true);
  });

  it("falls back to the raw reason for unknown labels", () => {
    expect(formatTestResult({ ok: false, reason: "weird", latency_ms: 3 })).toBe("weird in 3 ms");
  });
});

describe("testResultClass", () => {
  it("maps ok → green, failure → red", () => {
    expect(testResultClass({ ok: true, reason: "connected", latency_ms: 1 })).toBe("test-result ok");
    expect(testResultClass({ ok: false, reason: "timeout", latency_ms: 1 })).toBe("test-result bad");
  });
});

describe("buildProviderTestPayload", () => {
  it("sends base_url + api_key from unsaved form values", () => {
    expect(
      buildProviderTestPayload({ base_url: " https://api.openai.com/v1 ", api_key: " sk-1 " }),
    ).toEqual({ base_url: "https://api.openai.com/v1", api_key: "sk-1" });
  });

  it("omits a blank key and falls back to provider_id (edit form)", () => {
    expect(buildProviderTestPayload({ base_url: "http://x/v1", api_key: "", providerId: "p1" })).toEqual({
      base_url: "http://x/v1",
      provider_id: "p1",
    });
    expect(buildProviderTestPayload({ base_url: "", api_key: "", providerId: "p1" })).toEqual({
      provider_id: "p1",
    });
  });

  it("throws when there is nothing to test", () => {
    expect(() => buildProviderTestPayload({ base_url: "  ", api_key: "k" })).toThrow(/base URL/);
  });
});

describe("buildModelTestPayload", () => {
  it("builds the PONG round-trip body", () => {
    expect(buildModelTestPayload(" p1 ", " gpt-x ")).toEqual({ provider_id: "p1", model_name: "gpt-x" });
  });

  it("throws when provider or model is missing", () => {
    expect(() => buildModelTestPayload("", "m")).toThrow();
    expect(() => buildModelTestPayload("p", "")).toThrow();
  });
});

describe("failureResult", () => {
  it("uses the ApiError message but never the raw body", () => {
    const err = new ApiError(502, "bad gateway", { secret: "do-not-render" });
    const r = failureResult(err, "test failed");
    expect(r.ok).toBe(false);
    expect(r.reason).toBe("provider_error");
    expect(JSON.stringify(r)).not.toContain("do-not-render");
  });

  it("falls back for non-Error throws", () => {
    expect(failureResult("boom", "fallback detail").detail).toBe("fallback detail");
  });
});
