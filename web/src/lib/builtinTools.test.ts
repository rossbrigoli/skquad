import { describe, expect, it } from "vitest";
import { ApiError } from "./api";
import { isPlatformAdmin } from "./aimodels";
import {
  BUILTIN_TOOL_NAMES,
  SEARCH_PROVIDERS,
  buildToolPayload,
  buildToolPolicy,
  emptyToolForm,
  formFromTool,
  formatToolUpdate,
  isBuiltinToolName,
  toolsFromList,
  toolSaveErrorMessage,
  validateToolForm,
  type BuiltinTool,
  type BuiltinToolName,
  type ToolFormValues,
} from "./builtinTools";

// BT-4 (S-151) — logic-layer tests for the Built-in Tools admin
// settings surface, per the pinned ADR-0012 contract.

function tool(name: BuiltinToolName, overrides: Partial<BuiltinTool> = {}): BuiltinTool {
  return {
    name,
    enabled: false,
    policy: {},
    updatedAt: "2026-09-27T08:00:00Z",
    updatedBy: "ross",
    ...overrides,
  };
}

function form(name: BuiltinToolName, overrides: Partial<ToolFormValues> = {}): ToolFormValues {
  return { ...emptyToolForm(name), ...overrides };
}

describe("gating (platform_admin only)", () => {
  it("shows the surface only for platform_admin", () => {
    expect(isPlatformAdmin("platform_admin")).toBe(true);
    expect(isPlatformAdmin("user")).toBe(false);
    expect(isPlatformAdmin(undefined)).toBe(false);
    expect(isPlatformAdmin(null)).toBe(false);
    expect(isPlatformAdmin("")).toBe(false);
  });

  it("knows exactly the eighteen contract tool names (S-232: every built-in must be listed; S-279 adds the file tools; S-280 adds process; S-281 adds cron; S-282 adds the session inspection tools; S-283 adds browser)", () => {
    expect([...BUILTIN_TOOL_NAMES]).toEqual([
      "exec",
      "web_fetch",
      "web_search",
      "send_message",
      "send_inbox",
      "notify_owner",
      "memory_search",
      "spawn_subagent",
      "read",
      "write",
      "edit",
      "apply_patch",
      "process",
      "cron",
      "sessions_list",
      "sessions_history",
      "session_status",
      "browser",
    ]);
    expect(isBuiltinToolName("exec")).toBe(true);
    expect(isBuiltinToolName("web_fetch")).toBe(true);
    expect(isBuiltinToolName("web_search")).toBe(true);
    expect(isBuiltinToolName("send_message")).toBe(true);
    expect(isBuiltinToolName("send_inbox")).toBe(true);
    expect(isBuiltinToolName("notify_owner")).toBe(true);
    expect(isBuiltinToolName("memory_search")).toBe(true);
    expect(isBuiltinToolName("spawn_subagent")).toBe(true);
    expect(isBuiltinToolName("browser")).toBe(true);
    expect(isBuiltinToolName("something_else")).toBe(false);
  });
});

// S-283: the browser builtin pins {timeoutSeconds, maxSnapshotChars,
// maxScreenshotBytes} runtime-side. Navigation/session ceilings are the
// resource grant's (ADR-0014) and must never appear in this policy.
describe("browser policy (S-283)", () => {
  it("builds the pinned browser policy shape", () => {
    const policy = buildToolPolicy(
      "browser",
      form("browser", {
        timeoutSeconds: "45",
        maxSnapshotChars: "9000",
        maxScreenshotBytes: "2097152",
      }),
    );
    expect(policy).toEqual({
      timeoutSeconds: 45,
      maxSnapshotChars: 9000,
      maxScreenshotBytes: 2097152,
    });
  });

  it("defaults hydrate from an empty stored policy", () => {
    const f = formFromTool(tool("browser", { enabled: true, policy: {} }));
    expect(f.enabled).toBe(true);
    expect(f.timeoutSeconds).toBe("30");
    expect(f.maxSnapshotChars).toBe("12000");
    expect(f.maxScreenshotBytes).toBe("5242880");
  });

  it("round-trips a stored browser policy", () => {
    const f = formFromTool(
      tool("browser", {
        policy: { timeoutSeconds: 20, maxSnapshotChars: 4000, maxScreenshotBytes: 1024 },
      }),
    );
    expect(buildToolPolicy("browser", f)).toEqual({
      timeoutSeconds: 20,
      maxSnapshotChars: 4000,
      maxScreenshotBytes: 1024,
    });
  });

  it("validates positive integers for every browser knob", () => {
    expect(validateToolForm("browser", form("browser"))).toBeNull();
    expect(validateToolForm("browser", form("browser", { maxSnapshotChars: "0" }))).toContain(
      "maxSnapshotChars",
    );
    expect(validateToolForm("browser", form("browser", { maxScreenshotBytes: "-5" }))).toContain(
      "maxScreenshotBytes",
    );
    expect(validateToolForm("browser", form("browser", { timeoutSeconds: "" }))).toContain(
      "timeoutSeconds",
    );
  });
});

describe("save payload shape (PATCH /admin/tools/{name})", () => {
  it("exec: {enabled, policy:{timeoutSeconds,maxOutputBytes,deniedPatterns}}", () => {
    const payload = buildToolPayload(
      "exec",
      form("exec", {
        enabled: true,
        timeoutSeconds: "45",
        maxOutputBytes: "8192",
        deniedPatterns: ["rm\\s+-rf\\s+/", "  ", "mkfs"],
      }),
    );
    expect(payload).toEqual({
      enabled: true,
      policy: {
        timeoutSeconds: 45,
        maxOutputBytes: 8192,
        deniedPatterns: ["rm\\s+-rf\\s+/", "mkfs"],
      },
    });
  });

  it("web_fetch: {enabled, policy:{timeoutSeconds,maxBytes,allowPrivateNetwork}}", () => {
    const payload = buildToolPayload(
      "web_fetch",
      form("web_fetch", { enabled: true, timeoutSeconds: "15", maxBytes: "1024", allowPrivateNetwork: true }),
    );
    expect(payload).toEqual({
      enabled: true,
      policy: { timeoutSeconds: 15, maxBytes: 1024, allowPrivateNetwork: true },
    });
  });

  it("web_search: {enabled, policy:{timeoutSeconds,maxResults,provider}}", () => {
    const payload = buildToolPayload(
      "web_search",
      form("web_search", { enabled: true, timeoutSeconds: "5", maxResults: "3", provider: "brave" }),
    );
    expect(payload).toEqual({
      enabled: true,
      policy: { timeoutSeconds: 5, maxResults: 3, provider: "brave" },
    });
  });

  // S-232: the newly-listed built-ins must PATCH with the policy keys the
  // server's validation pins for each of them.
  it("send_inbox / notify_owner: {timeoutSeconds,maxMessageChars} like send_message", () => {
    for (const name of ["send_inbox", "notify_owner"] as const) {
      const payload = buildToolPayload(
        name,
        form(name, { enabled: true, timeoutSeconds: "12", maxMessageChars: "4000" }),
      );
      expect(payload).toEqual({
        enabled: true,
        policy: { timeoutSeconds: 12, maxMessageChars: 4000 },
      });
    }
  });

  it("memory_search: {timeoutSeconds,maxResults}", () => {
    const payload = buildToolPayload(
      "memory_search",
      form("memory_search", { enabled: true, timeoutSeconds: "8", maxResults: "5" }),
    );
    expect(payload).toEqual({ enabled: true, policy: { timeoutSeconds: 8, maxResults: 5 } });
  });

  it("spawn_subagent: empty policy object (no tunables yet)", () => {
    const payload = buildToolPayload("spawn_subagent", form("spawn_subagent", { enabled: true }));
    expect(payload).toEqual({ enabled: true, policy: {} });
  });

  it("emits ONLY the keys pinned for each tool (no cross-tool leakage)", () => {
    const keys = (name: BuiltinToolName) =>
      Object.keys(buildToolPolicy(name, form(name))).sort((a, b) => a.localeCompare(b));
    expect(keys("exec")).toEqual(["deniedPatterns", "maxOutputBytes", "timeoutSeconds"]);
    expect(keys("web_fetch")).toEqual(["allowPrivateNetwork", "maxBytes", "timeoutSeconds"]);
    expect(keys("web_search")).toEqual(["maxResults", "provider", "timeoutSeconds"]);
    expect(keys("send_message")).toEqual(["maxMessageChars", "timeoutSeconds"]);
    expect(keys("send_inbox")).toEqual(["maxMessageChars", "timeoutSeconds"]);
    expect(keys("notify_owner")).toEqual(["maxMessageChars", "timeoutSeconds"]);
    expect(keys("memory_search")).toEqual(["maxResults", "timeoutSeconds"]);
    expect(keys("spawn_subagent")).toEqual([]);
    expect(keys("process")).toEqual(["maxLogBytes", "maxSessions"]);
    expect(keys("cron")).toEqual(["maxActiveJobs"]);
  });

  // S-281: cron pins exactly {maxActiveJobs} — no timeoutSeconds.
  it("cron: {maxActiveJobs} only (S-281)", () => {
    const payload = buildToolPayload(
      "cron",
      form("cron", { enabled: true, maxActiveJobs: "5" }),
    );
    expect(payload).toEqual({ enabled: true, policy: { maxActiveJobs: 5 } });
    expect(validateToolForm("cron", form("cron", { maxActiveJobs: "0" }))).toMatch(/maxActiveJobs/);
    expect(validateToolForm("cron", form("cron", { maxActiveJobs: "abc" }))).toMatch(/maxActiveJobs/);
    expect(validateToolForm("cron", form("cron"))).toBeNull();
    // The default form carries the control-plane default of 10.
    expect(emptyToolForm("cron").maxActiveJobs).toBe("10");
  });

  // S-282: the session inspection tools pin exactly {} — read-only
  // clients over existing endpoints, no admin-tunable policy knobs.
  it("session inspection tools: {} only (S-282)", () => {
    for (const name of ["sessions_list", "sessions_history", "session_status"] as const) {
      expect(Object.keys(buildToolPolicy(name, form(name)))).toEqual([]);
      const payload = buildToolPayload(name, form(name, { enabled: true }));
      expect(payload).toEqual({ enabled: true, policy: {} });
      expect(validateToolForm(name, form(name))).toBeNull();
    }
  });

  it("process: {maxSessions,maxLogBytes} (S-280)", () => {
    const payload = buildToolPayload(
      "process",
      form("process", { enabled: true, maxSessions: "8", maxLogBytes: "1048576" }),
    );
    expect(payload).toEqual({ enabled: true, policy: { maxSessions: 8, maxLogBytes: 1048576 } });
    expect(validateToolForm("process", form("process", { maxSessions: "0" }))).toMatch(/maxSessions/);
    expect(validateToolForm("process", form("process", { maxLogBytes: "abc" }))).toMatch(/maxLogBytes/);
    expect(validateToolForm("process", form("process"))).toBeNull();
  });

  it("disabled tools still save with enabled:false (merge semantics)", () => {
    const payload = buildToolPayload("exec", form("exec", { enabled: false }));
    expect(payload.enabled).toBe(false);
  });
});

describe("form hydration", () => {
  it("formFromTool keeps stored policy values", () => {
    const f = formFromTool(
      tool("exec", {
        enabled: true,
        policy: { timeoutSeconds: 90, maxOutputBytes: 4096, deniedPatterns: ["shutdown"] },
      }),
    );
    expect(f.enabled).toBe(true);
    expect(f.timeoutSeconds).toBe("90");
    expect(f.maxOutputBytes).toBe("4096");
    expect(f.deniedPatterns).toEqual(["shutdown"]);
  });

  it("formFromTool falls back to contract defaults for missing keys", () => {
    expect(emptyToolForm("exec").timeoutSeconds).toBe("60");
    expect(emptyToolForm("web_fetch").timeoutSeconds).toBe("30");
    expect(emptyToolForm("web_search").timeoutSeconds).toBe("20");
    const f = formFromTool(tool("web_fetch", { policy: {} }));
    expect(f.maxBytes).toBe("262144");
    expect(f.allowPrivateNetwork).toBe(false);
    const s = formFromTool(tool("web_search", { policy: {} }));
    expect(s.provider).toBe("duckduckgo");
    expect(s.maxResults).toBe("8");
  });

  it("formFromTool ignores non-string deniedPatterns entries", () => {
    const f = formFromTool(tool("exec", { policy: { deniedPatterns: ["ok", 42, null] } }));
    expect(f.deniedPatterns).toEqual(["ok"]);
  });

  it("formFromTool hydrates cron maxActiveJobs (S-281)", () => {
    const f = formFromTool(tool("cron", { enabled: true, policy: { maxActiveJobs: 7 } }));
    expect(f.maxActiveJobs).toBe("7");
    const d = formFromTool(tool("cron", { policy: {} }));
    expect(d.maxActiveJobs).toBe("10");
  });

  it("round-trips: form → policy → form is stable", () => {
    const original = form("web_search", { enabled: true, provider: "perplexity", maxResults: "12", timeoutSeconds: "25" });
    const policy = buildToolPolicy("web_search", original);
    const back = formFromTool(tool("web_search", { enabled: true, policy }));
    expect(buildToolPolicy("web_search", back)).toEqual(policy);
  });
});

describe("validation", () => {
  it("rejects non-positive / non-integer numbers", () => {
    expect(validateToolForm("exec", form("exec", { timeoutSeconds: "0" }))).toMatch(/timeoutSeconds/);
    expect(validateToolForm("exec", form("exec", { timeoutSeconds: "-3" }))).toMatch(/timeoutSeconds/);
    expect(validateToolForm("exec", form("exec", { timeoutSeconds: "1.5" }))).toMatch(/timeoutSeconds/);
    expect(validateToolForm("exec", form("exec", { timeoutSeconds: "" }))).toMatch(/timeoutSeconds/);
    expect(validateToolForm("web_fetch", form("web_fetch", { maxBytes: "abc" }))).toMatch(/maxBytes/);
    expect(validateToolForm("web_search", form("web_search", { maxResults: "0" }))).toMatch(/maxResults/);
  });

  it("rejects invalid regex in deniedPatterns", () => {
    const bad = form("exec", { deniedPatterns: ["[unclosed", "  "] });
    expect(validateToolForm("exec", bad)).toMatch(/invalid regex/);
    expect(validateToolForm("exec", form("exec", { deniedPatterns: ["", "  "] }))).toBeNull();
  });

  it("rejects unknown search providers", () => {
    expect(validateToolForm("web_search", form("web_search", { provider: "google" }))).toMatch(/provider/);
    for (const p of SEARCH_PROVIDERS) {
      expect(validateToolForm("web_search", form("web_search", { provider: p }))).toBeNull();
    }
  });

  it("accepts contract-default forms for all tools", () => {
    for (const name of BUILTIN_TOOL_NAMES) {
      expect(validateToolForm(name, emptyToolForm(name))).toBeNull();
    }
  });

  it("send_message: policy carries timeoutSeconds + maxMessageChars only (S-164)", () => {
    const payload = buildToolPayload(
      "send_message",
      form("send_message", { timeoutSeconds: "9", maxMessageChars: "1200" }),
    );
    expect(payload.policy).toEqual({ timeoutSeconds: 9, maxMessageChars: 1200 });
    expect(validateToolForm("send_message", form("send_message", { maxMessageChars: "0" }))).toMatch(/maxMessageChars/);
    expect(validateToolForm("send_message", form("send_message", { maxMessageChars: "abc" }))).toMatch(/maxMessageChars/);
  });
});

describe("error display", () => {
  it("surfaces the server's 400 validation message verbatim", () => {
    const err = new ApiError(400, 'policy.timeoutSeconds must be <= 300', { error: { message: 'policy.timeoutSeconds must be <= 300' } });
    expect(toolSaveErrorMessage(err)).toBe("policy.timeoutSeconds must be <= 300");
  });

  it("falls back for non-400 / non-Error values", () => {
    expect(toolSaveErrorMessage(new ApiError(500, "boom"))).toBe("boom");
    expect(toolSaveErrorMessage("nope", "save failed")).toBe("save failed");
    expect(toolSaveErrorMessage(new Error(""), "save failed")).toBe("save failed");
  });
});

describe("updatedAt / updatedBy display", () => {
  it("renders timestamp and actor", () => {
    const line = formatToolUpdate(tool("exec", { updatedAt: "2026-09-27T08:00:00Z", updatedBy: "ross" }));
    expect(line).toContain("2026");
    expect(line).toContain("by ross");
  });

  it("handles missing metadata gracefully", () => {
    const line = formatToolUpdate(tool("exec", { updatedAt: undefined, updatedBy: "" }));
    expect(line).toContain("Updated");
    expect(line).toContain("unknown");
  });
});

describe("list parsing", () => {
  it("extracts tools defensively", () => {
    expect(toolsFromList({ tools: [tool("exec")] })).toHaveLength(1);
    expect(toolsFromList({})).toEqual([]);
    expect(toolsFromList(null)).toEqual([]);
    expect(toolsFromList(undefined)).toEqual([]);
  });
});
