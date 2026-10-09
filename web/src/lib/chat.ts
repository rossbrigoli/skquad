// S-122: pure helpers for the agent chat UI — ordering, tool-call rendering,
// and the tiny context-size status bar. Kept free of React so they are unit
// testable and reusable.
import type { Message } from "./api";

export type ChatToolCall = {
  name: string;
  arguments: unknown;
  ok: boolean;
  result: string;
  // S-163: subagent transparency — the full captured thread when the
  // tool is spawn_subagent (emitted by the runtime since S-163).
  subagent?: SubagentInfo;
};

export type SubagentThreadEntry = {
  role: "user" | "assistant" | "tool" | "notice";
  content: string;
  name?: string;
  ok?: boolean;
  tool_calls?: { name: string; arguments?: unknown }[];
};

export type SubagentInfo = {
  thread: SubagentThreadEntry[];
  turns: number;
  steps: string[];
};

/** Chronological order (oldest first); the chat box anchors the newest at the
 *  bottom via CSS so new messages push up like ChatGPT/Telegram. */
export function sortChatMessages(messages: Message[]): Message[] {
  return [...messages].sort((a, b) => (a.created_at ?? "").localeCompare(b.created_at ?? ""));
}

/** Parse `payload.tool_calls` (emitted by the agent runtime since S-122)
 *  into renderable tool calls. Malformed entries are skipped, never thrown. */
export function chatToolCalls(msg: Message): ChatToolCall[] {
  const raw = msg.payload?.tool_calls;
  if (!Array.isArray(raw)) return [];
  const calls: ChatToolCall[] = [];
  for (const item of raw) {
    if (!item || typeof item !== "object" || Array.isArray(item)) continue;
    const entry = item as Record<string, unknown>;
    const name = typeof entry.name === "string" && entry.name.trim() !== "" ? entry.name.trim() : "tool";
    const call: ChatToolCall = {
      name,
      arguments: entry.arguments ?? {},
      ok: entry.ok !== false,
      result: typeof entry.result === "string" ? entry.result : "",
    };
    const sub = parseSubagent(entry.subagent);
    if (sub) call.subagent = sub;
    calls.push(call);
  }
  return calls;
}

/** S-189: stable React keys for lists without natural ids. Content-derived,
 * with an occurrence suffix so exact duplicates stay unique. Only valid for
 * lists that are not reordered/edited in place while rendered. */
export function uniqueContentKeys<T>(items: T[], keyOf: (item: T) => string): string[] {
  const seen = new Map<string, number>();
  return items.map((item) => {
    const base = keyOf(item);
    const n = (seen.get(base) ?? 0) + 1;
    seen.set(base, n);
    return n > 1 ? `${base}#${n}` : base;
  });
}

function toolCallsFromRaw(raw: unknown): { name: string; arguments: unknown }[] | null {
  if (!Array.isArray(raw)) {
    return null;
  }
  return raw
    .filter((c): c is Record<string, unknown> => !!c && typeof c === "object")
    .map((c) => ({
      name: typeof c.name === "string" ? c.name : "tool",
      arguments: c.arguments,
    }));
}

function threadEntryFromItem(item: unknown): SubagentThreadEntry | null {
  if (!item || typeof item !== "object" || Array.isArray(item)) {
    return null;
  }
  const entry = item as Record<string, unknown>;
  const role = entry.role;
  if (role !== "user" && role !== "assistant" && role !== "tool" && role !== "notice") {
    return null;
  }
  const parsed: SubagentThreadEntry = {
    role,
    content: typeof entry.content === "string" ? entry.content : "",
  };
  if (typeof entry.name === "string") parsed.name = entry.name;
  if (typeof entry.ok === "boolean") parsed.ok = entry.ok;
  const calls = toolCallsFromRaw(entry.tool_calls);
  if (calls) parsed.tool_calls = calls;
  return parsed;
}

/** Lenient parse of the S-163 `subagent` payload; returns null when the
 *  shape is unusable (older runtimes, truncated payloads, junk). */
export function parseSubagent(raw: unknown): SubagentInfo | null {
  if (!raw || typeof raw !== "object" || Array.isArray(raw)) return null;
  const value = raw as Record<string, unknown>;
  const rawThread = Array.isArray(value.thread) ? value.thread : [];
  const thread: SubagentThreadEntry[] = [];
  for (const item of rawThread) {
    const entry = threadEntryFromItem(item);
    if (entry) thread.push(entry);
  }
  const turns = typeof value.turns === "number" && Number.isFinite(value.turns) ? value.turns : 0;
  const steps = Array.isArray(value.steps)
    ? value.steps.filter((s): s is string => typeof s === "string")
    : [];
  if (thread.length === 0 && steps.length === 0) return null;
  return { thread, turns, steps };
}

/** One-line summary for the subagent chip: "3 steps (a → b → c), 4 turns". */
export function subagentSummary(info: SubagentInfo): string {
  const parts: string[] = [];
  if (info.steps.length > 0) {
    const shown = info.steps.length > 6 ? info.steps.slice(0, 6).concat("…") : info.steps;
    parts.push(`${info.steps.length} step${info.steps.length === 1 ? "" : "s"} (${shown.join(" → ")})`);
  } else {
    parts.push("no tool steps");
  }
  parts.push(`${info.turns} turn${info.turns === 1 ? "" : "s"}`);
  return parts.join(", ");
}

/** Last-known context size (prompt tokens) for this conversation: the
 *  `context_tokens` the runtime recorded on the most recent agent reply —
 *  i.e. the prompt size of the last actual LLM call. Null until the agent
 *  has answered at least once on a runtime that reports usage. */
export function chatContextTokens(messages: Message[]): number | null {
  const sorted = sortChatMessages(messages);
  for (let i = sorted.length - 1; i >= 0; i--) {
    const msg = sorted[i];
    if (msg.from_type !== "agent") continue;
    const value = msg.payload?.context_tokens;
    if (typeof value === "number" && Number.isFinite(value) && value >= 0) return value;
  }
  return null;
}

export function truncateText(value: string, max: number): string {
  if (max <= 0 || value.length <= max) return value;
  return value.slice(0, max).trimEnd() + "…";
}

function collapseWhitespace(value: string): string {
  let out = "";
  let inWhitespace = true;
  for (const char of value) {
    if (char.trim() === "") {
      inWhitespace = true;
      continue;
    }
    if (out !== "" && inWhitespace) {
      out += " ";
    }
    out += char;
    inWhitespace = false;
  }
  return out;
}

/** One-line summary of tool arguments for the collapsed row. */
export function summarizeToolArgs(args: unknown, max = 90): string {
  let text: string;
  if (typeof args === "string") {
    text = args;
  } else {
    try {
      text = JSON.stringify(args);
    } catch {
      text = String(args);
    }
  }
  if (!text || text === "{}" || text === "null") return "";
  return truncateText(collapseWhitespace(text), max);
}

/** Pretty-printed, bounded JSON for the expanded tool-call detail. */
export function prettyToolArgs(args: unknown, max = 2000): string {
  let text: string;
  if (typeof args === "string") {
    text = args;
  } else {
    try {
      text = JSON.stringify(args, null, 2) ?? String(args);
    } catch {
      text = String(args);
    }
  }
  return truncateText(text, max);
}

/** Display string for the context-size status bar. */
export function formatContextTokens(value: number): string {
  return value.toLocaleString("en-US");
}

// S-154: if an agent never replies (crash, lost message), the composer must
// not stay locked forever — the pending-turn lock expires after this window.
export const CHAT_TURN_LOCK_MS = 5 * 60 * 1000;

// S-175: terminal message statuses — a user message in one of these states
// can never produce an agent reply, so it must not keep the turn "pending".
// "cancelled" is set by the chat stop button; dead/expired messages were
// never answered either.
const TURN_TERMINAL_STATUSES = new Set(["cancelled", "dead", "expired"]);

/** S-262: an *interim* progress reply (S-195 `payload.interim` flag) is
 *  not the end of the agent's turn — more messages are still coming.
 *  Strict boolean check: anything else (missing flag, non-`true` values,
 *  user-authored messages) counts as not interim. */
export function isInterimChatReply(msg: Message): boolean {
  return msg.from_type === "agent" && msg.payload?.interim === true;
}

/** S-265: the terminal closure the runtime posts when the LLM call fails
 *  (`payload.turn_error`, see runtime `_close_failed_turn`). Renderers use
 *  this to style the message as an error; it is NOT interim, so
 *  `agentTurnPending` releases the composer as soon as it arrives. */
export function isTurnErrorMessage(msg: Message): boolean {
  return msg.from_type === "agent" && msg.payload?.turn_error === true;
}

/** True while the agent's turn is still in flight. S-154 baseline: the
 *  chronologically-last message is from a user (no agent reply after it).
 *  S-262 extends this through multi-message turns: a last message that is
 *  an *interim* agent progress note (S-195) also keeps the turn pending —
 *  the turn completes only on the final (non-interim) reply or the
 *  `turn_error` closure the runtime posts when the turn dies. In both cases
 *  the lock window is measured from the last message's `created_at` and
 *  terminal statuses release immediately, so a dropped final reply can
 *  never lock the composer forever (existing expiry fail-safe reused). */
export function agentTurnPending(
  messages: Message[],
  nowMs: number,
  lockWindowMs: number = CHAT_TURN_LOCK_MS,
): boolean {
  const sorted = sortChatMessages(messages);
  const last = sorted[sorted.length - 1];
  if (!last) return false;
  const turnInFlight = last.from_type === "user" || isInterimChatReply(last);
  if (!turnInFlight) return false;
  if (TURN_TERMINAL_STATUSES.has(last.status)) return false;
  const sentAt = Date.parse(last.created_at ?? "");
  if (!Number.isFinite(sentAt)) return false;
  return nowMs - sentAt < lockWindowMs;
}
