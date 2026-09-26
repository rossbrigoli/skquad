// S-141: the baked-in version/commit must survive trimming, fall back to
// "unknown", and never leak a half-value into the UI.

import { afterEach, describe, expect, it, vi } from "vitest";
import { buildInfo, UNKNOWN, versionLabel, versionText } from "./buildInfo";

const ORIGINAL = { ...process.env };

afterEach(() => {
  process.env.NEXT_PUBLIC_SKQUAD_VERSION = ORIGINAL.NEXT_PUBLIC_SKQUAD_VERSION;
  process.env.NEXT_PUBLIC_SKQUAD_COMMIT = ORIGINAL.NEXT_PUBLIC_SKQUAD_COMMIT;
  vi.resetModules();
});

// buildInfo is evaluated at import time, so each case re-imports the module.
async function loadWith(version: string | undefined, commit: string | undefined) {
  if (version === undefined) delete process.env.NEXT_PUBLIC_SKQUAD_VERSION;
  else process.env.NEXT_PUBLIC_SKQUAD_VERSION = version;
  if (commit === undefined) delete process.env.NEXT_PUBLIC_SKQUAD_COMMIT;
  else process.env.NEXT_PUBLIC_SKQUAD_COMMIT = commit;
  vi.resetModules();
  return await import("./buildInfo");
}

describe("buildInfo", () => {
  it("reads the baked version and shortens the commit to 7 chars", async () => {
    const mod = await loadWith("0.1.100", "4ae923c5226229165b919d422f8ca056b39496b0");
    expect(mod.buildInfo.version).toBe("0.1.100");
    expect(mod.buildInfo.commit).toBe("4ae923c5226229165b919d422f8ca056b39496b0");
    expect(mod.buildInfo.shortCommit).toBe("4ae923c");
  });

  it("trims whitespace around baked values", async () => {
    const mod = await loadWith("  0.1.101  ", "  abc1234def  ");
    expect(mod.buildInfo.version).toBe("0.1.101");
    expect(mod.buildInfo.shortCommit).toBe("abc1234");
  });

  it("falls back to unknown when nothing is baked (npm run dev)", async () => {
    const mod = await loadWith(undefined, undefined);
    expect(mod.buildInfo.version).toBe(UNKNOWN);
    expect(mod.buildInfo.shortCommit).toBe(UNKNOWN);
  });

  it("treats an empty string as unknown, not as a version", async () => {
    const mod = await loadWith("", "   ");
    expect(mod.buildInfo.version).toBe(UNKNOWN);
    expect(mod.buildInfo.commit).toBe(UNKNOWN);
  });
});

describe("labels", () => {
  it("versionLabel prefixes v", () => {
    expect(versionLabel).toBe(`v${buildInfo.version}`);
  });

  it("buildLabel pairs version with commit", async () => {
    const mod = await loadWith("0.1.100", "4ae923c5226229165b919d422f8ca056b39496b0");
    expect(mod.buildLabel).toBe("0.1.100 (commit 4ae923c)");
  });

  it("buildLabel degrades gracefully without a commit", async () => {
    const mod = await loadWith("0.1.100", undefined);
    expect(mod.buildLabel).toBe("0.1.100 (commit unknown)");
  });
});

describe("versionText", () => {
  it("passes real values through", () => {
    expect(versionText("0.1.100")).toBe("0.1.100");
  });

  it("maps blank and missing to unknown", () => {
    expect(versionText("")).toBe(UNKNOWN);
    expect(versionText("   ")).toBe(UNKNOWN);
    expect(versionText(undefined)).toBe(UNKNOWN);
  });
});
