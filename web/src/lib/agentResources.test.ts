import { describe, expect, it } from "vitest";

import {
  DEFAULT_AGENT_CPU_LIMIT,
  DEFAULT_AGENT_CPU_REQUEST,
  DEFAULT_AGENT_MEMORY_LIMIT,
  DEFAULT_AGENT_MEMORY_REQUEST,
  defaultAgentResources,
  normalizeAgentResources,
  parseCpuMillis,
  parseMemoryBytes,
  validateAgentResources,
} from "./agentResources";

// S-287: client-side mirrors of the control-plane k8s quantity rules.

describe("parseCpuMillis", () => {
  it("parses milli-cores and whole/decimal cores", () => {
    expect(parseCpuMillis("100m")).toBe(100);
    expect(parseCpuMillis("500m")).toBe(500);
    expect(parseCpuMillis("1")).toBe(1000);
    expect(parseCpuMillis("0.5")).toBe(500);
    expect(parseCpuMillis(" 250m ")).toBe(250);
  });

  it("rejects malformed, zero, and negative values", () => {
    for (const raw of ["", " ", "abc", "100x", "-100m", "0", "0m", ".5", "1e3", "1.2.3", "1Gi", "m"]) {
      expect(parseCpuMillis(raw)).toBeNull();
    }
  });
});

describe("parseMemoryBytes", () => {
  it("parses binary and decimal suffixes", () => {
    expect(parseMemoryBytes("128Mi")).toBe(128 * 1024 * 1024);
    expect(parseMemoryBytes("1Gi")).toBe(1024 * 1024 * 1024);
    expect(parseMemoryBytes("512M")).toBe(512e6);
    expect(parseMemoryBytes("1024")).toBe(1024);
  });

  it("rejects malformed, zero, and negative values", () => {
    for (const raw of ["", "abc", "512MB", "-1Mi", "0", "0Mi", ".5Gi", "1e3", "1.2.3Ki"]) {
      expect(parseMemoryBytes(raw)).toBeNull();
    }
  });
});

describe("validateAgentResources", () => {
  it("accepts the built-in defaults", () => {
    expect(validateAgentResources(defaultAgentResources())).toEqual([]);
  });

  it("accepts equal request and limit with mixed formats", () => {
    expect(
      validateAgentResources({
        cpu_request: "1",
        cpu_limit: "1000m",
        memory_request: "1Gi",
        memory_limit: "1024Mi",
      }),
    ).toEqual([]);
  });

  it("flags request exceeding limit", () => {
    const errors = validateAgentResources({
      cpu_request: "600m",
      cpu_limit: "500m",
      memory_request: "2Gi",
      memory_limit: "512Mi",
    });
    expect(errors).toContain("CPU request must not exceed CPU limit.");
    expect(errors).toContain("Memory request must not exceed Memory limit.");
  });

  it("flags empty and malformed fields", () => {
    const errors = validateAgentResources({
      cpu_request: "",
      cpu_limit: "500X",
      memory_request: "128Mi",
      memory_limit: "512Mi",
    });
    expect(errors).toContain("CPU request is required.");
    expect(errors.some((e) => e.startsWith("CPU limit must be"))).toBe(true);
  });
});

describe("normalizeAgentResources", () => {
  it("fills defaults for missing/blank fields", () => {
    expect(normalizeAgentResources(undefined)).toEqual(defaultAgentResources());
    expect(normalizeAgentResources({ cpu_limit: "  " })).toEqual(defaultAgentResources());
    expect(
      normalizeAgentResources({ cpu_request: " 250m ", memory_limit: "1Gi" }),
    ).toEqual({
      cpu_request: "250m",
      cpu_limit: DEFAULT_AGENT_CPU_LIMIT,
      memory_request: DEFAULT_AGENT_MEMORY_REQUEST,
      memory_limit: "1Gi",
    });
  });

  it("defaults are the documented values", () => {
    expect(DEFAULT_AGENT_CPU_REQUEST).toBe("100m");
    expect(DEFAULT_AGENT_CPU_LIMIT).toBe("500m");
    expect(DEFAULT_AGENT_MEMORY_REQUEST).toBe("128Mi");
    expect(DEFAULT_AGENT_MEMORY_LIMIT).toBe("512Mi");
  });
});
