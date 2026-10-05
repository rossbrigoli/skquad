// BT-4 (S-151): Built-in platform tools — admin settings logic.
//
// Pure, browser-independent helpers for the "Built-in Tools" admin
// settings surface, per the pinned contract in
// docs/adr/0012-builtin-platform-tools.md. API shapes are pinned there;
// nothing in this file may deviate from §2 (admin API) or §3 (policies).
//
// Control-plane surface consumed here:
//   GET   /api/v1/admin/tools            -> { tools: [ToolConfig] }
//   PATCH /api/v1/admin/tools/{name}     -> { enabled?, policy? } (merge)
//        200 full tool config | 400 invalid policy | 403 not admin

import { ApiError } from "./api";

export type BuiltinToolName =
  | "exec"
  | "web_fetch"
  | "web_search"
  | "send_message"
  | "send_inbox"
  | "notify_owner"
  | "memory_search"
  | "spawn_subagent";

// S-232: every built-in the control-plane catalog ships (domain.BuiltinToolNames)
// must be listed here too — anything missing silently vanishes from the
// Settings Tools page (that was the bug).
export const BUILTIN_TOOL_NAMES: readonly BuiltinToolName[] = [
  "exec",
  "web_fetch",
  "web_search",
  "send_message",
  "send_inbox",
  "notify_owner",
  "memory_search",
  "spawn_subagent",
];

export const SEARCH_PROVIDERS: readonly string[] = ["duckduckgo", "brave", "perplexity"];

// BuiltinTool mirrors the admin API's tool config row (ADR-0012 §2).
export type BuiltinTool = {
  name: BuiltinToolName;
  enabled: boolean;
  policy: Record<string, unknown>;
  updatedAt?: string;
  updatedBy?: string;
};

export type BuiltinToolsList = { tools?: BuiltinTool[] };

// ToolFormValues is the controlled-input shape for a tool card. Every
// numeric field is a string in the form (raw input) and is converted to a
// number only in buildToolPolicy, so typing never fights the state.
export type ToolFormValues = {
  enabled: boolean;
  timeoutSeconds: string;
  // exec only
  maxOutputBytes: string;
  deniedPatterns: string[];
  // web_fetch only
  maxBytes: string;
  allowPrivateNetwork: boolean;
  // web_search only
  provider: string;
  maxResults: string;
  // send_message only (S-164)
  maxMessageChars: string;
};

// Contract defaults (ADR-0012 §3). Used as empty-form placeholders and
// fallbacks when a loaded policy omits a key.
export const TOOL_DEFAULTS: Record<BuiltinToolName, { timeoutSeconds: number }> = {
  exec: { timeoutSeconds: 60 },
  web_fetch: { timeoutSeconds: 30 },
  web_search: { timeoutSeconds: 20 },
  send_message: { timeoutSeconds: 15 },
  send_inbox: { timeoutSeconds: 15 },
  notify_owner: { timeoutSeconds: 15 },
  memory_search: { timeoutSeconds: 20 },
  spawn_subagent: { timeoutSeconds: 15 },
};

const EXEC_DEFAULTS = { timeoutSeconds: 60, maxOutputBytes: 65536 };
const WEB_FETCH_DEFAULTS = { timeoutSeconds: 30, maxBytes: 262144 };
const WEB_SEARCH_DEFAULTS = { timeoutSeconds: 20, maxResults: 8, provider: "duckduckgo" };
const SEND_MESSAGE_DEFAULTS = { timeoutSeconds: 15, maxMessageChars: 8000 };

export function isBuiltinToolName(name: string): name is BuiltinToolName {
  return (BUILTIN_TOOL_NAMES as readonly string[]).includes(name);
}

function num(raw: unknown, fallback: string): string {
  return typeof raw === "number" && Number.isFinite(raw) ? String(raw) : fallback;
}

function str(raw: unknown, fallback: string): string {
  return typeof raw === "string" && raw !== "" ? raw : fallback;
}

// emptyToolForm returns a fresh form for a tool, pre-filled with the
// contract defaults.
export function emptyToolForm(name: BuiltinToolName): ToolFormValues {
  return {
    enabled: false,
    timeoutSeconds: String(TOOL_DEFAULTS[name].timeoutSeconds),
    maxOutputBytes: String(EXEC_DEFAULTS.maxOutputBytes),
    deniedPatterns: [],
    maxBytes: String(WEB_FETCH_DEFAULTS.maxBytes),
    allowPrivateNetwork: false,
    provider: WEB_SEARCH_DEFAULTS.provider,
    maxResults: String(WEB_SEARCH_DEFAULTS.maxResults),
    maxMessageChars: String(SEND_MESSAGE_DEFAULTS.maxMessageChars),
  };
}

// formFromTool hydrates a card form from a loaded tool config, keeping
// any stored policy values and falling back to contract defaults.
export function formFromTool(tool: BuiltinTool): ToolFormValues {
  const base = emptyToolForm(tool.name);
  const policy = tool.policy ?? {};
  base.enabled = !!tool.enabled;
  base.timeoutSeconds = num(policy.timeoutSeconds, base.timeoutSeconds);
  if (tool.name === "exec") {
    base.maxOutputBytes = num(policy.maxOutputBytes, base.maxOutputBytes);
    base.deniedPatterns = Array.isArray(policy.deniedPatterns)
      ? policy.deniedPatterns.filter((p): p is string => typeof p === "string")
      : [];
  }
  if (tool.name === "web_fetch") {
    base.maxBytes = num(policy.maxBytes, base.maxBytes);
    base.allowPrivateNetwork = !!policy.allowPrivateNetwork;
  }
  if (tool.name === "web_search") {
    base.maxResults = num(policy.maxResults, base.maxResults);
    base.provider = str(policy.provider, base.provider);
  }
  if (tool.name === "send_message") {
    base.maxMessageChars = num(policy.maxMessageChars, base.maxMessageChars);
  }
  // S-232: send_inbox/notify_owner share the send_message policy keys;
  // memory_search carries maxResults (top-k recall).
  if (tool.name === "send_inbox" || tool.name === "notify_owner") {
    base.maxMessageChars = num(policy.maxMessageChars, base.maxMessageChars);
  }
  if (tool.name === "memory_search") {
    base.maxResults = num(policy.maxResults, base.maxResults);
  }
  return base;
}

// buildToolPolicy converts form values into the pinned policy JSON for
// the tool. Only keys valid for that tool are emitted — sending an
// unknown key would be a contract deviation (and a server 400).
export function buildToolPolicy(name: BuiltinToolName, form: ToolFormValues): Record<string, unknown> {
  const timeout = Number(form.timeoutSeconds);
  if (name === "exec") {
    return {
      timeoutSeconds: timeout,
      maxOutputBytes: Number(form.maxOutputBytes),
      deniedPatterns: form.deniedPatterns.map((p) => p.trim()).filter((p) => p !== ""),
    };
  }
  if (name === "web_fetch") {
    return {
      timeoutSeconds: timeout,
      maxBytes: Number(form.maxBytes),
      allowPrivateNetwork: form.allowPrivateNetwork,
    };
  }
  if (name === "send_message" || name === "send_inbox" || name === "notify_owner") {
    return { timeoutSeconds: timeout, maxMessageChars: Number(form.maxMessageChars) };
  }
  if (name === "memory_search") {
    return { timeoutSeconds: timeout, maxResults: Number(form.maxResults) };
  }
  if (name === "spawn_subagent") {
    // S-232: no admin-tunable policy keys yet — the subagent inherits
    // the parent runtime's limits. Sending "{}" keeps the PATCH valid
    // against the server's empty allow-set for this tool.
    return {};
  }
  return {
    timeoutSeconds: timeout,
    maxResults: Number(form.maxResults),
    provider: form.provider,
  };
}

// buildToolPayload is the PATCH body: { enabled, policy } (merge
// semantics per ADR-0012 §2).
export function buildToolPayload(name: BuiltinToolName, form: ToolFormValues): {
  enabled: boolean;
  policy: Record<string, unknown>;
} {
  return { enabled: form.enabled, policy: buildToolPolicy(name, form) };
}

// validateToolForm is the client-side pre-flight mirror of the server's
// policy validation. Returns the first error message, or null when the
// form is submittable. The server remains authoritative; this only
// prevents obviously-bad round-trips.
function checkDeniedPatterns(patterns: string[]): string | null {
  for (const pattern of patterns) {
    const trimmed = pattern.trim();
    if (trimmed === "") continue;
    try {
      new RegExp(trimmed);
    } catch {
      return `deniedPatterns: invalid regex: ${trimmed}`;
    }
  }
  return null;
}

export function validateToolForm(name: BuiltinToolName, form: ToolFormValues): string | null {
  const timeoutErr = checkPositiveInt(form.timeoutSeconds, "timeoutSeconds");
  if (timeoutErr) return timeoutErr;
  if (name === "exec") {
    const bytesErr = checkPositiveInt(form.maxOutputBytes, "maxOutputBytes");
    if (bytesErr) return bytesErr;
    return checkDeniedPatterns(form.deniedPatterns);
  }
  if (name === "web_fetch") {
    return checkPositiveInt(form.maxBytes, "maxBytes");
  }
  if (name === "send_message" || name === "send_inbox" || name === "notify_owner") {
    return checkPositiveInt(form.maxMessageChars, "maxMessageChars");
  }
  if (name === "memory_search") {
    return checkPositiveInt(form.maxResults, "maxResults");
  }
  if (name === "spawn_subagent") {
    return null;
  }
  const resultsErr = checkPositiveInt(form.maxResults, "maxResults");
  if (resultsErr) return resultsErr;
  if (!SEARCH_PROVIDERS.includes(form.provider)) {
    return `provider must be one of: ${SEARCH_PROVIDERS.join(", ")}`;
  }
  return null;
}

function checkPositiveInt(raw: string, label: string): string | null {
  const value = Number(raw);
  if (raw.trim() === "" || !Number.isInteger(value) || value <= 0) {
    return `${label} must be a positive whole number`;
  }
  return null;
}

// toolSaveErrorMessage surfaces the server's validation message on 400
// (pinned shape: {"error":{"message":...}} or {"message":...}, already
// unwrapped into ApiError.message by api.ts). Anything else gets the
// generic fallback.
export function toolSaveErrorMessage(err: unknown, fallback = "save failed"): string {
  if (err instanceof ApiError && err.status === 400) {
    return err.message || "invalid policy";
  }
  return err instanceof Error && err.message ? err.message : fallback;
}

// formatToolUpdate renders the per-card "updated … by …" line.
export function formatToolUpdate(tool: BuiltinTool): string {
  const when = tool.updatedAt ? new Date(tool.updatedAt) : null;
  const stamp = when && !Number.isNaN(when.getTime()) ? when.toLocaleString() : "—";
  const by = tool.updatedBy && tool.updatedBy !== "" ? tool.updatedBy : "unknown";
  return `Updated ${stamp} by ${by}`;
}

// toolsFromList defensively extracts the tool list from the GET payload.
export function toolsFromList(payload: BuiltinToolsList | null | undefined): BuiltinTool[] {
  return payload?.tools ?? [];
}
