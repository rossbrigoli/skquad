import { describe, expect, it } from "vitest";

import {
  SLASH_COMMANDS,
  cycleSlashIndex,
  filterSlashCommands,
  matchSlashCommand,
  slashCommandTokens,
  slashQuery,
  slashSuggestionsVisible,
} from "./slashCommands";

describe("slashCommandTokens", () => {
  it("derives the placeholder list from the registry", () => {
    expect(slashCommandTokens()).toEqual(["/reset", "/restart"]);
  });

  it("tracks a newly registered command without code changes elsewhere", () => {
    const withNew = [
      ...SLASH_COMMANDS,
      { command: "/status", action: "restart" as const, description: "d", glyph: "📊", destructive: false },
    ];
    expect(slashCommandTokens(withNew)).toContain("/status");
    expect(filterSlashCommands("stat", withNew).map((c) => c.command)).toEqual(["/status"]);
  });
});

describe("slashQuery", () => {
  it("returns empty query for a bare slash", () => {
    expect(slashQuery("/")).toBe("");
  });

  it("returns the lower-cased remainder of a single slash token", () => {
    expect(slashQuery("/RE")).toBe("re");
    expect(slashQuery("/reset")).toBe("reset");
  });

  it("is null once the draft leaves slash context", () => {
    expect(slashQuery("hello")).toBeNull();
    expect(slashQuery("")).toBeNull();
    expect(slashQuery("not /reset")).toBeNull();
  });

  it("is null once the slash token is followed by more words", () => {
    // A lone command runs an action; "/reset now" is prose.
    expect(slashQuery("/reset now")).toBeNull();
    expect(slashQuery("/re\tstart")).toBeNull();
  });

  it("keeps slash context while trailing spaces are being typed", () => {
    expect(slashQuery("/res ")).toBe("res");
  });
});

describe("filterSlashCommands", () => {
  it("empty query lists everything", () => {
    expect(filterSlashCommands("")).toEqual([...SLASH_COMMANDS].sort((a, b) => a.command.length - b.command.length || a.command.localeCompare(b.command)));
  });

  it("prefix-matches case-insensitively", () => {
    expect(filterSlashCommands("RE").map((c) => c.command)).toEqual(["/reset"]);
    expect(filterSlashCommands("res").map((c) => c.command)).toEqual(["/reset"]);
    expect(filterSlashCommands("resta").map((c) => c.command)).toEqual(["/restart"]);
  });

  it("does not match mid-word", () => {
    expect(filterSlashCommands("esta")).toEqual([]);
  });

  it("orders the shortest completion first", () => {
    const cmds = [
      { command: "/restarting", action: "restart" as const, description: "d", glyph: "x", destructive: false },
      { command: "/reset", action: "reset" as const, description: "d", glyph: "x", destructive: false },
    ];
    expect(filterSlashCommands("re", cmds).map((c) => c.command)).toEqual(["/reset", "/restarting"]);
  });
});

describe("matchSlashCommand", () => {
  it("matches an exact command, trimmed and case-insensitive", () => {
    expect(matchSlashCommand("/reset")?.action).toBe("reset");
    expect(matchSlashCommand("  /Restart  ")?.action).toBe("restart");
  });

  it("does not match partials or prose", () => {
    expect(matchSlashCommand("/rese")).toBeNull();
    expect(matchSlashCommand("/reset please")).toBeNull();
    expect(matchSlashCommand("reset")).toBeNull();
    expect(matchSlashCommand("")).toBeNull();
  });
});

describe("slashSuggestionsVisible", () => {
  it("shows for a bare slash and for partial matches", () => {
    expect(slashSuggestionsVisible("/")).toBe(true);
    expect(slashSuggestionsVisible("/res")).toBe(true);
  });

  it("hides once the command is fully typed", () => {
    // The popup must not swallow Enter when the user is done typing.
    expect(slashSuggestionsVisible("/reset")).toBe(false);
    expect(slashSuggestionsVisible("/restart")).toBe(false);
  });

  it("hides when nothing matches and outside slash context", () => {
    expect(slashSuggestionsVisible("/zzz")).toBe(false);
    expect(slashSuggestionsVisible("hello")).toBe(false);
    expect(slashSuggestionsVisible("")).toBe(false);
  });
});

describe("cycleSlashIndex", () => {
  it("steps forward and wraps at the end", () => {
    expect(cycleSlashIndex(0, 3, 1)).toBe(1);
    expect(cycleSlashIndex(2, 3, 1)).toBe(0);
  });

  it("steps backward and wraps at the start", () => {
    expect(cycleSlashIndex(1, 3, -1)).toBe(0);
    expect(cycleSlashIndex(0, 3, -1)).toBe(2);
  });

  it("is safe on an empty list", () => {
    expect(cycleSlashIndex(0, 0, 1)).toBe(0);
  });
});
