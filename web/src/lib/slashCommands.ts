// S-304 follow-up: the chat composer's slash commands, as data.
//
// S-304 hardcoded "/reset" and "/restart" inside the composer's send
// handler, so adding a third command meant touching the send path, the
// placeholder, and the wake-on-typing guard. This module is the single
// registry: a new command is one entry in SLASH_COMMANDS plus a handler
// in the page's action switch. Autocomplete, the placeholder, and the
// dispatch all derive from it.
//
// Kept free of React (same convention as lib/chat.ts) so the matching
// rules are unit-testable without a DOM.

/** The control-plane action a slash command maps to. The composer never
 *  performs the action itself — it routes the key to the page, which
 *  owns the confirm dialog and the API call. */
export type SlashCommandAction = "reset" | "restart";

export type SlashCommand = {
  /** Command as typed, including the leading slash. Lower-case; matching
   *  is case-insensitive but insertion uses this spelling. */
  readonly command: string;
  readonly action: SlashCommandAction;
  /** One-liner shown next to the command in the suggestion popup. */
  readonly description: string;
  /** Emoji glyph for the popup row. */
  readonly glyph: string;
  /** True when the command mutates agent state and the page should ask
   *  for confirmation before running it. */
  readonly destructive: boolean;
};

// The registry. Ordering is the suggestion ordering (alphabetical by
// command keeps it predictable as the list grows).
export const SLASH_COMMANDS: readonly SlashCommand[] = [
  {
    command: "/reset",
    action: "reset",
    description: "Archive this chat thread to memory and start fresh",
    glyph: "🧹",
    destructive: true,
  },
  {
    command: "/restart",
    action: "restart",
    description: "Evict the agent's pods and start them fresh",
    glyph: "🔄",
    destructive: true,
  },
];

/** The command tokens, e.g. ["/reset", "/restart"] — used for the
 *  composer placeholder so it stays in sync with the registry. */
export function slashCommandTokens(
  commands: readonly SlashCommand[] = SLASH_COMMANDS,
): string[] {
  return commands.map((c) => c.command);
}

/**
 * Is the draft currently in "slash context"? True only when the draft
 * starts with "/" and is still a single token — once the user types a
 * space the line is prose again ("/reset now" is a message, matching
 * the S-304 dispatch rule that only a lone command runs an action).
 *
 * Returns the lower-cased query (without the slash) or null.
 */
export function slashQuery(draft: string): string | null {
  if (!draft.startsWith("/")) return null;
  const token = draft.trim();
  if (token === "/") return "";
  if (/\s/.test(token)) return null;
  return token.slice(1).toLowerCase();
}

/**
 * Commands whose name starts with the query (prefix match, so "/res"
 * suggests "/reset" but not "/restart"). An empty query lists
 * everything. Results are sorted by command length then alphabetically,
 * so the shortest/most-likely completion is selected first.
 */
export function filterSlashCommands(
  query: string,
  commands: readonly SlashCommand[] = SLASH_COMMANDS,
): SlashCommand[] {
  const q = query.toLowerCase();
  return commands
    .filter((c) => c.command.slice(1).toLowerCase().startsWith(q))
    .sort((a, b) => a.command.length - b.command.length || a.command.localeCompare(b.command));
}

/**
 * The command the draft IS — an exact, complete match (trimmed,
 * case-insensitive). This is what `send()` dispatches on. Returns null
 * for anything else, including partials and commands with trailing text.
 */
export function matchSlashCommand(
  draft: string,
  commands: readonly SlashCommand[] = SLASH_COMMANDS,
): SlashCommand | null {
  const token = draft.trim().toLowerCase();
  if (!token.startsWith("/") || /\s/.test(token)) return null;
  return commands.find((c) => c.command.toLowerCase() === token) ?? null;
}

/**
 * Whether the suggestion popup should be showing for this draft.
 *
 * Hidden when: not in slash context, no matches, or the draft is already
 * an exact command (fully typed — the popup would only add noise and
 * steal Enter from the send path).
 */
export function slashSuggestionsVisible(
  draft: string,
  commands: readonly SlashCommand[] = SLASH_COMMANDS,
): boolean {
  const query = slashQuery(draft);
  if (query === null) return false;
  if (matchSlashCommand(draft, commands) !== null) return false;
  return filterSlashCommands(query, commands).length > 0;
}

/**
 * Clamp an arrow-key selection index into the match list. Wraps at both
 * ends so Down at the bottom lands on the top, like every terminal
 * completion menu.
 */
export function cycleSlashIndex(current: number, length: number, step: number): number {
  if (length <= 0) return 0;
  return (((current + step) % length) + length) % length;
}
