import { afterEach, describe, expect, it, vi } from "vitest";

import {
  DEFAULT_EMBEDDER_RUNTIME,
  EMBEDDER_RUNTIMES,
  fetchEmbedderRuntime,
  saveEmbedderRuntime,
  validateEmbedderRuntime,
} from "./embedderRuntime";
import * as api from "./api";

// S-212 — logic-layer tests for the embedder runtime admin setting.

vi.mock("./api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./api")>();
  return {
    ...actual,
    apiGet: vi.fn(),
    apiPut: vi.fn(),
  };
});

describe("validateEmbedderRuntime", () => {
  it("accepts the four accepted runtimes", () => {
    for (const r of EMBEDDER_RUNTIMES) {
      expect(validateEmbedderRuntime(r)).toBeNull();
    }
  });

  it("rejects unknown values", () => {
    expect(validateEmbedderRuntime("metal")).toMatch(/Must be one of/);
    expect(validateEmbedderRuntime("")).toMatch(/Must be one of/);
    expect(validateEmbedderRuntime("CUDA")).toMatch(/Must be one of/); // case-sensitive at the boundary
  });
});

describe("fetchEmbedderRuntime", () => {
  afterEach(() => vi.restoreAllMocks());

  it("returns the stored runtime", async () => {
    vi.spyOn(api, "apiGet").mockResolvedValue({ embedder_runtime: "cuda" });
    await expect(fetchEmbedderRuntime("t")).resolves.toBe("cuda");
  });

  it("defaults to auto when unset", async () => {
    vi.spyOn(api, "apiGet").mockResolvedValue({});
    await expect(fetchEmbedderRuntime("t")).resolves.toBe(DEFAULT_EMBEDDER_RUNTIME);
  });

  it("falls back to auto on an unknown stored value", async () => {
    vi.spyOn(api, "apiGet").mockResolvedValue({ embedder_runtime: "quantum" });
    await expect(fetchEmbedderRuntime("t")).resolves.toBe(DEFAULT_EMBEDDER_RUNTIME);
  });
});

describe("saveEmbedderRuntime", () => {
  afterEach(() => vi.restoreAllMocks());

  it("PUTs the runtime and returns the echoed value", async () => {
    const put = vi.spyOn(api, "apiPut").mockResolvedValue({ embedder_runtime: "vulkan" });
    await expect(saveEmbedderRuntime("t", "vulkan")).resolves.toBe("vulkan");
    expect(put).toHaveBeenCalledWith("/admin/settings", "t", { embedder_runtime: "vulkan" });
  });

  it("throws before the network on an invalid value", async () => {
    const put = vi.spyOn(api, "apiPut");
    await expect(saveEmbedderRuntime("t", "directx" as never)).rejects.toThrow(/Must be one of/);
    expect(put).not.toHaveBeenCalled();
  });
});
