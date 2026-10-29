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
  | "spawn_subagent"
  | "read"
  | "write"
  | "edit"
  | "apply_patch"
  | "process"
  // S-281: built-in CRON (control-plane scheduled jobs).
  | "cron"
  // S-282: session inspection (read-only views over existing agent-scoped
  // endpoints: tasks, peers, message history, task context, agent chat).
  | "sessions_list"
  | "sessions_history"
  | "session_status"
  // S-283: built-in BROWSER (ADR-0014) — gateway-driven browsing via
  // the quarantined browser-service. Runtime-side bounds only; the
  // security-relevant ceilings (deny_hosts, max_pages, session caps)
  // live on the browser resource grant, not this policy.
  | "browser";

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
  // S-279: file tools, jailed to the agent's task workspace runtime-side.
  "read",
  "write",
  "edit",
  "apply_patch",
  // S-280: background process management (paired with exec background=true).
  "process",
  // S-281: built-in CRON — the scheduler lives in the control plane.
  "cron",
  // S-282: session inspection — read-only, self-scoped session/task views.
  "sessions_list",
  "sessions_history",
  "session_status",
  // S-283: built-in BROWSER — quarantined browsing through the tool gateway.
  "browser",
];

// S-282: the three session inspection tools share one trait — no policy
// knobs at all (empty allow-set server-side, "{}" only), because they are
// GET-only clients over existing endpoints with fixed sanitization.
export const SESSION_INSPECTION_TOOL_NAMES: readonly BuiltinToolName[] = [
  "sessions_list",
  "sessions_history",
  "session_status",
];

export function isSessionInspectionTool(name: BuiltinToolName): boolean {
  return SESSION_INSPECTION_TOOL_NAMES.includes(name);
}

// S-279: the four file tools share one policy shape {timeoutSeconds, maxBytes}.
export const FILE_TOOL_NAMES: readonly BuiltinToolName[] = ["read", "write", "edit", "apply_patch"];

export function isFileToolName(name: BuiltinToolName): boolean {
  return FILE_TOOL_NAMES.includes(name);
}

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
  // process only (S-280)
  maxSessions: string;
  maxLogBytes: string;
  // cron only (S-281)
  maxActiveJobs: string;
  // browser only (S-283)
  maxSnapshotChars: string;
  maxScreenshotBytes: string;
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
  // S-279 file tools
  read: { timeoutSeconds: 30 },
  write: { timeoutSeconds: 30 },
  edit: { timeoutSeconds: 30 },
  apply_patch: { timeoutSeconds: 30 },
  // S-280 background process management
  process: { timeoutSeconds: 15 },
  // S-281 cron: no per-call timeout is surfaced (the form hides it);
  // the entry keeps the Record total. The runtime uses a 15s HTTP timeout.
  cron: { timeoutSeconds: 15 },
  // S-282 session inspection: no surfaced knobs (policy is "{}"); the
  // runtime uses a 15s HTTP timeout on its GET round trips.
  sessions_list: { timeoutSeconds: 15 },
  sessions_history: { timeoutSeconds: 15 },
  session_status: { timeoutSeconds: 15 },
  // S-283 browser: bounds the gateway round trip.
  browser: { timeoutSeconds: 30 },
};

const EXEC_DEFAULTS = { timeoutSeconds: 60, maxOutputBytes: 65536 };
const WEB_FETCH_DEFAULTS = { timeoutSeconds: 30, maxBytes: 262144 };
const WEB_SEARCH_DEFAULTS = { timeoutSeconds: 20, maxResults: 8, provider: "duckduckgo" };
const SEND_MESSAGE_DEFAULTS = { timeoutSeconds: 15, maxMessageChars: 8000 };
// S-279: file tools share {timeoutSeconds, maxBytes}; maxBytes caps read
// output and total written bytes (for apply_patch, summed across files).
const FILE_TOOL_DEFAULTS = { timeoutSeconds: 30, maxBytes: 262144 };
// S-280: process pins {maxSessions, maxLogBytes} — concurrent session
// cap per task and per-session captured-log cap (process killed on excess).
const PROCESS_DEFAULTS = { maxSessions: 16, maxLogBytes: 5242880 };
// S-281: cron pins exactly {maxActiveJobs} — the per-agent cap on
// enabled scheduled jobs (mirrors the control-plane default of 10).
const CRON_DEFAULTS = { maxActiveJobs: 10 };
// S-283: browser runtime-side bounds — rendered page-text cap and the
// max screenshot size the runtime will persist. The service caps
// capture at its own ceiling; navigation/session ceilings are the
// resource grant's (ADR-0014).
const BROWSER_DEFAULTS = { maxSnapshotChars: 12000, maxScreenshotBytes: 5242880 };

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
    maxSessions: String(PROCESS_DEFAULTS.maxSessions),
    maxLogBytes: String(PROCESS_DEFAULTS.maxLogBytes),
    maxActiveJobs: String(CRON_DEFAULTS.maxActiveJobs),
    maxSnapshotChars: String(BROWSER_DEFAULTS.maxSnapshotChars),
    maxScreenshotBytes: String(BROWSER_DEFAULTS.maxScreenshotBytes),
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
  // S-279: read/write/edit/apply_patch carry {timeoutSeconds, maxBytes}.
  if (isFileToolName(tool.name)) {
    base.maxBytes = num(policy.maxBytes, String(FILE_TOOL_DEFAULTS.maxBytes));
  }
  // S-280: process carries {maxSessions, maxLogBytes}.
  if (tool.name === "process") {
    base.maxSessions = num(policy.maxSessions, base.maxSessions);
    base.maxLogBytes = num(policy.maxLogBytes, base.maxLogBytes);
  }
  // S-281: cron carries {maxActiveJobs}.
  if (tool.name === "cron") {
    base.maxActiveJobs = num(policy.maxActiveJobs, base.maxActiveJobs);
  }
  // S-283: browser carries {timeoutSeconds, maxSnapshotChars, maxScreenshotBytes}.
  if (tool.name === "browser") {
    base.maxSnapshotChars = num(policy.maxSnapshotChars, base.maxSnapshotChars);
    base.maxScreenshotBytes = num(policy.maxScreenshotBytes, base.maxScreenshotBytes);
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
  if (isFileToolName(name)) {
    // S-279: file tools pin exactly {timeoutSeconds, maxBytes}.
    return { timeoutSeconds: timeout, maxBytes: Number(form.maxBytes) };
  }
  if (name === "process") {
    // S-280: process pins exactly {maxSessions, maxLogBytes}.
    return { maxSessions: Number(form.maxSessions), maxLogBytes: Number(form.maxLogBytes) };
  }
  if (name === "cron") {
    // S-281: cron pins exactly {maxActiveJobs} — no timeoutSeconds
    // (the scheduler runs control-plane-side; there is no per-call timeout).
    return { maxActiveJobs: Number(form.maxActiveJobs) };
  }
  if (isSessionInspectionTool(name)) {
    // S-282: the session inspection tools pin exactly {} — read-only
    // clients with no admin-tunable policy (server empty allow-set).
    return {};
  }
  if (name === "browser") {
    // S-283: browser pins exactly {timeoutSeconds, maxSnapshotChars,
    // maxScreenshotBytes} — runtime-side bounds only. Navigation/session
    // ceilings (deny_hosts, max_pages, max_sessions_per_agent) are the
    // browser resource grant's job (ADR-0014), NOT this policy.
    return {
      timeoutSeconds: timeout,
      maxSnapshotChars: Number(form.maxSnapshotChars),
      maxScreenshotBytes: Number(form.maxScreenshotBytes),
    };
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
  if (name === "cron") {
    // S-281: cron's policy is {maxActiveJobs} only — timeoutSeconds is
    // hidden in the form and must not be validated here.
    return checkPositiveInt(form.maxActiveJobs, "maxActiveJobs");
  }
  if (isSessionInspectionTool(name)) {
    // S-282: nothing to validate — the policy is always {}.
    return null;
  }
  const timeoutErr = checkPositiveInt(form.timeoutSeconds, "timeoutSeconds");
  if (timeoutErr) return timeoutErr;
  return validateToolSpecificPolicy(name, form);
}

// validateToolSpecificPolicy checks the per-tool extra fields after the
// shared timeoutSeconds check (S3776: split out of validateToolForm).
// Mirrors the server's per-tool policy schemas.
function validateToolSpecificPolicy(name: BuiltinToolName, form: ToolFormValues): string | null {
  switch (name) {
    case "exec": {
      const bytesErr = checkPositiveInt(form.maxOutputBytes, "maxOutputBytes");
      if (bytesErr) return bytesErr;
      return checkDeniedPatterns(form.deniedPatterns);
    }
    case "web_fetch":
      return checkPositiveInt(form.maxBytes, "maxBytes");
    case "send_message":
    case "send_inbox":
    case "notify_owner":
      return checkPositiveInt(form.maxMessageChars, "maxMessageChars");
    case "memory_search":
      return checkPositiveInt(form.maxResults, "maxResults");
    case "spawn_subagent":
      return null;
    case "process": {
      // S-280: mirror the server's {maxSessions, maxLogBytes} schema.
      const sessionsErr = checkPositiveInt(form.maxSessions, "maxSessions");
      if (sessionsErr) return sessionsErr;
      return checkPositiveInt(form.maxLogBytes, "maxLogBytes");
    }
    case "browser": {
      // S-283: mirror the server's {timeoutSeconds, maxSnapshotChars,
      // maxScreenshotBytes} schema (timeout already checked above).
      const snapErr = checkPositiveInt(form.maxSnapshotChars, "maxSnapshotChars");
      if (snapErr) return snapErr;
      return checkPositiveInt(form.maxScreenshotBytes, "maxScreenshotBytes");
    }
    default: {
      if (isFileToolName(name)) {
        // S-279: mirror the server's {timeoutSeconds, maxBytes} schema.
        return checkPositiveInt(form.maxBytes, "maxBytes");
      }
      const resultsErr = checkPositiveInt(form.maxResults, "maxResults");
      if (resultsErr) return resultsErr;
      if (!SEARCH_PROVIDERS.includes(form.provider)) {
        return `provider must be one of: ${SEARCH_PROVIDERS.join(", ")}`;
      }
      return null;
    }
  }
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
