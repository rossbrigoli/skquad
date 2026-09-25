// S-122: pure helpers for the agent chat UI — ordering, tool-call rendering,
// and the tiny context-size status bar. Kept free of React so they are unit
// testable and reusable.
import type { Message } from "./api";

export type ChatToolCall = {
  name: string;
  arguments: unknown;
  ok: boolean;
  result: string;
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
    calls.push({
      name,
      arguments: entry.arguments ?? {},
      ok: entry.ok !== false,
      result: typeof entry.result === "string" ? entry.result : "",
    });
  }
  return calls;
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
