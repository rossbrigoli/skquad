// Unit tests for the slash-command autocomplete helpers. The registry
// itself lives in the control plane; these cover the composer-side parsing.
import { describe, expect, it } from "vitest";
import {
  filterSlashCommands,
  LEGACY_COMPOSER_COMMANDS,
  matchSlashCommand,
  slashQuery,
  type SlashCommand,
} from "./slashCommands";

const CMDS: SlashCommand[] = [
  { name: "reset", description: "Reset the chat thread", destructive: true },
  { name: "restart", description: "Restart the agent", destructive: true },
  { name: "wake", description: "Wake the agent now", destructive: false },
];

describe("slashQuery", () => {
  it("returns the lowercase query for a bare slash token", () => {
    expect(slashQuery("/")).toBe("");
    expect(slashQuery("/re")).toBe("re");
    expect(slashQuery("/RESET")).toBe("reset");
    expect(slashQuery("/my-cmd_2")).toBe("my-cmd_2");
  });

  it("returns null when the draft is not a single slash token", () => {
    expect(slashQuery("hello")).toBeNull();
    expect(slashQuery("hello /re")).toBeNull();
    expect(slashQuery("/reset now")).toBeNull();
    expect(slashQuery(" /re")).toBeNull();
    expect(slashQuery("")).toBeNull();
  });
});

describe("filterSlashCommands", () => {
  it("prefix-matches case-insensitively", () => {
    expect(filterSlashCommands(CMDS, "").map((c) => c.name)).toEqual(["reset", "restart", "wake"]);
    expect(filterSlashCommands(CMDS, "RE").map((c) => c.name)).toEqual(["reset", "restart"]);
    expect(filterSlashCommands(CMDS, "wake").map((c) => c.name)).toEqual(["wake"]);
    expect(filterSlashCommands(CMDS, "zzz")).toEqual([]);
  });
});

describe("matchSlashCommand", () => {
  it("matches an exact bare command, ignoring case and trailing space", () => {
    expect(matchSlashCommand("/reset", CMDS)?.name).toBe("reset");
    expect(matchSlashCommand("  /Restart  ", CMDS)?.name).toBe("restart");
    expect(matchSlashCommand("/wake", CMDS)?.destructive).toBe(false);
  });

  it("does not match partial, non-slash, or unknown drafts", () => {
    expect(matchSlashCommand("/res", CMDS)).toBeNull();
    expect(matchSlashCommand("/", CMDS)).toBeNull();
    expect(matchSlashCommand("hello", CMDS)).toBeNull();
    expect(matchSlashCommand("/frobnicate", CMDS)).toBeNull();
    expect(matchSlashCommand("/reset now", CMDS)).toBeNull();
  });
});

describe("LEGACY_COMPOSER_COMMANDS", () => {
  it("keeps the S-304 reset/restart execution contract alive without a registry", () => {
    expect(matchSlashCommand("/reset", LEGACY_COMPOSER_COMMANDS)?.name).toBe("reset");
    expect(matchSlashCommand("/restart", LEGACY_COMPOSER_COMMANDS)?.name).toBe("restart");
    expect(matchSlashCommand("/wake", LEGACY_COMPOSER_COMMANDS)).toBeNull();
    for (const c of LEGACY_COMPOSER_COMMANDS) expect(c.destructive).toBe(true);
  });
});
