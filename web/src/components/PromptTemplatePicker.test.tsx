// S-215: create-dialog upgrades. Locks in:
//  - applyTemplate: empty draft → template replaces; non-empty draft →
//    template appended below the "From Template" separator.
//  - PromptTemplatePicker renders a "From template" button (menu closed
//    in static markup).
//  - AgentFormModal uses the wider dialog variant, a .prompt-tall prompt
//    textarea, and the .field-checkbox left-aligned durable-storage row.
import { readFileSync } from "node:fs";
import type { ReactNode } from "react";
import { describe, expect, it, vi } from "vitest";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";

vi.mock("../lib/auth", async () => {
  const { createContext } = await import("react");
  const value = { token: "tok", mode: "token", user: { role: "member" } };
  const Ctx = createContext(value);
  return {
    useAuth: () => value,
    TokenProvider: ({ children }: { children: ReactNode }) =>
      createElement(Ctx.Provider, { value }, children),
  };
});

import { applyTemplate, TEMPLATE_SEPARATOR } from "./PromptTemplatePicker";
import { AgentFormModal } from "./AgentForm";

describe("applyTemplate (S-215)", () => {
  it("replaces an empty draft with the template content", () => {
    expect(applyTemplate("", "TEMPLATE")).toBe("TEMPLATE");
  });

  it("treats whitespace-only drafts as empty", () => {
    expect(applyTemplate("   \n ", "TEMPLATE")).toBe("TEMPLATE");
  });

  it("appends below a separator when the draft is non-empty", () => {
    expect(applyTemplate("my draft", "TEMPLATE")).toBe(
      `my draft\n${TEMPLATE_SEPARATOR}\nTEMPLATE`,
    );
  });

  it("separator matches the card-specified format", () => {
    expect(TEMPLATE_SEPARATOR).toBe("---------- From Template --------------");
  });
});

describe("PromptTemplatePicker button (S-215)", () => {
  // Static render can't show the menu (templates load in an effect), so
  // lock the button contract via source assertions, per repo style.
  const source = readFileSync(new URL("./PromptTemplatePicker.tsx", import.meta.url), "utf8");

  it("renders a From template button with menu semantics", () => {
    expect(source).toContain("From template");
    expect(source).toContain('aria-haspopup="menu"');
    expect(source).toContain('role="menu"');
    expect(source).toContain('role="menuitem"');
  });

  it("routes selection through applyTemplate", () => {
    expect(source).toContain("applyTemplate(currentPrompt, t.content)");
  });
});

describe("AgentFormModal create-dialog upgrades (S-215)", () => {
  const html = renderToStaticMarkup(
    createElement(AgentFormModal, {
      title: "New agent",
      submitLabel: "Create agent",
      onSubmit: async () => undefined,
      onClose: () => undefined,
    }),
  );

  it("uses the wider dialog variant", () => {
    expect(html).toContain("modal-card modal-wider");
  });

  it("prompt textarea is twice as tall (.prompt-tall)", () => {
    expect(html).toContain('class="prompt-tall"');
  });

  it("durable storage checkbox row is left-aligned (.field-checkbox)", () => {
    expect(html).toContain('class="field-checkbox"');
    const source = readFileSync(new URL("./AgentForm.tsx", import.meta.url), "utf8");
    // The old inline-styled label (which inherited the blanket 100%-width
    // checkbox bug) must be gone.
    expect(source).not.toContain('alignItems: "center", gap: "0.5rem"');
  });
});
