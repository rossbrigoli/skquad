import { describe, expect, it } from "vitest";

import {
  DEFAULT_AGENT_STORAGE_SIZE,
  STORAGE_PRESETS,
  isValidStorageSize,
  storageDisplay,
} from "./agentStorage";

describe("isValidStorageSize", () => {
  it("accepts the UI presets", () => {
    for (const preset of STORAGE_PRESETS) {
      expect(isValidStorageSize(preset)).toBe(true);
    }
  });

  it("accepts other k8s-style quantities", () => {
    expect(isValidStorageSize("500M")).toBe(true);
    expect(isValidStorageSize("1Ti")).toBe(true);
    expect(isValidStorageSize("0.5Gi")).toBe(true);
    expect(isValidStorageSize(" 2Gi ")).toBe(true);
  });

  it("rejects malformed, zero and negative sizes", () => {
    for (const bad of ["", "abc", "2GB", "2gi", "0", "0Gi", "-1Gi", "1 Gi", "Gi", "1.2.3Gi"]) {
      expect(isValidStorageSize(bad)).toBe(false);
    }
  });
});

describe("storageDisplay", () => {
  it("shows off when storage is disabled", () => {
    expect(storageDisplay(false, "5Gi")).toBe("off");
    expect(storageDisplay(undefined, "5Gi")).toBe("off");
  });

  it("shows the allocated size when enabled", () => {
    expect(storageDisplay(true, "5Gi")).toBe("5Gi");
  });

  it("falls back to the platform default when size is missing", () => {
    expect(storageDisplay(true, "")).toBe(DEFAULT_AGENT_STORAGE_SIZE);
    expect(storageDisplay(true, undefined)).toBe(DEFAULT_AGENT_STORAGE_SIZE);
  });
});
