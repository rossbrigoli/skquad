// Chat slash-command autocomplete support.
//
// The command list is served by the control plane (GET /agents/{id}/commands
// — see control-plane/internal/httpapi/slash_commands.go), so commands added
// in the future surface in the composer suggestions without frontend changes.

import { apiGet } from "./api";

export interface SlashCommand {
  name: string;
  description: string;
  destructive: boolean;
}

// Fallback used for exact-match execution when the registry fetch fails,
// preserving the S-304 behavior (/reset, /restart) with no dropdown.
export const LEGACY_COMPOSER_COMMANDS: SlashCommand[] = [
  { name: "reset", description: "Reset the chat thread", destructive: true },
  { name: "restart", description: "Restart the agent", destructive: true },
];

interface CommandsResponse {
  agent_id?: string;
  commands?: SlashCommand[];
}

export async function fetchSlashCommands(agentId: string, token: string): Promise<SlashCommand[]> {
  const res = await apiGet<CommandsResponse>(`/agents/${agentId}/commands`, token);
  return res.commands ?? [];
}

// slashQuery returns the lowercase query (without the leading "/") when the
// whole draft is a single slash token, or null otherwise. "/res" suggests;
// "hello /res" or "/reset now" do not — suggestions only cover the command
// word itself.
export function slashQuery(draft: string): string | null {
  const m = /^\/([a-zA-Z0-9_-]*)$/.exec(draft);
  return m ? m[1].toLowerCase() : null;
}

export function filterSlashCommands(cmds: SlashCommand[], query: string): SlashCommand[] {
  const q = query.toLowerCase();
  return cmds.filter((c) => c.name.toLowerCase().startsWith(q));
}

// matchSlashCommand returns the command an exact draft (trimmed) refers to,
// or null when the draft is not a bare, known command. Used to decide at
// send time whether to execute an action instead of posting a message.
export function matchSlashCommand(draft: string, cmds: SlashCommand[]): SlashCommand | null {
  const t = draft.trim().toLowerCase();
  if (!t.startsWith("/")) return null;
  const word = t.slice(1);
  if (word === "") return null;
  return cmds.find((c) => c.name.toLowerCase() === word) ?? null;
}
