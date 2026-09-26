import { describe, expect, it } from "vitest";
import { displayRole, initialsFor } from "./usermenu";

describe("initialsFor", () => {
  it("takes first letters of first two name words", () => {
    expect(initialsFor("Ada Lovelace", "ada@example.com")).toBe("AL");
    expect(initialsFor("grace brewster hopper", "g@example.com")).toBe("GB");
  });

  it("uses a single letter for one-word names", () => {
    expect(initialsFor("Ross", "ross@example.com")).toBe("R");
    expect(initialsFor("  ross  ", null)).toBe("R");
  });

  it("skips non-alphanumeric leading words", () => {
    expect(initialsFor("-- Ross Brigoli", null)).toBe("RB");
  });

  it("falls back to the email local-part without a name", () => {
    expect(initialsFor("", "brigs@example.com")).toBe("B");
    expect(initialsFor(null, "u@x.io")).toBe("U");
  });

  it("returns ? when nothing usable exists", () => {
    expect(initialsFor(null, null)).toBe("?");
    expect(initialsFor("", "")).toBe("?");
    expect(initialsFor("   ", "  ")).toBe("?");
  });

  it("handles unicode names", () => {
    expect(initialsFor("Élodie Martin", null)).toBe("ÉM");
    expect(initialsFor("日本 太郎", null)).toBe("日太");
  });
});

describe("displayRole", () => {
  it("passes through real roles", () => {
    expect(displayRole("platform_admin")).toBe("platform_admin");
    expect(displayRole("agent")).toBe("agent");
  });

  it("defaults empty/missing roles to member", () => {
    expect(displayRole("")).toBe("member");
    expect(displayRole(null)).toBe("member");
    expect(displayRole()).toBe("member");
    expect(displayRole("   ")).toBe("member");
  });
});
