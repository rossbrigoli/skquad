// S-214: the Prompt Templates screen must use the shared app design
// system instead of the one-off .panel/.table classes (which never had
// CSS definitions), and expose checkbox multi-select with a top bulk
// delete. These tests lock the structure in via static markup plus
// source assertions (same harness style as DeadLettersPanel.test).
import { readFileSync } from "node:fs";
import type { ReactNode } from "react";
import { describe, expect, it, vi } from "vitest";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";

vi.mock("../lib/auth", async () => {
  const { createContext } = await import("react");
  const Ctx = createContext<{ token: string; mode: string; user: unknown }>({
    token: "tok",
    mode: "token",
    user: { role: "platform_admin" },
  });
  return {
    useAuth: () => ({ token: "tok", mode: "token", user: { role: "platform_admin" } }),
    TokenProvider: ({ children }: { children: ReactNode }) =>
      createElement(Ctx.Provider, { value: { token: "tok", mode: "token", user: { role: "platform_admin" } } }, children),
  };
});

import { PromptTemplatesPanel } from "./PromptTemplatesPanel";
import type { PromptTemplate } from "../lib/promptTemplates";

const source = readFileSync(new URL("./PromptTemplatesPanel.tsx", import.meta.url), "utf8");

function tpl(id: string, name: string, overrides: Partial<PromptTemplate> = {}): PromptTemplate {
  return {
    id,
    name,
    description: `desc ${name}`,
    content: "content",
    applies_to: "agent",
    created_by: "ross",
    created_at: "2026-10-01T00:00:00Z",
    updated_at: "2026-10-01T00:00:00Z",
    ...overrides,
  };
}

const templates = [tpl("t1", "Alpha"), tpl("t2", "Beta", { applies_to: "squad" })];

describe("PromptTemplatesPanel design-system coherence (S-214)", () => {
  const html = renderToStaticMarkup(
    createElement(PromptTemplatesPanel, { templates, loading: false }),
  );

  it("uses the shared .section-head + .btn-primary header", () => {
    expect(html).toContain('class="section-head"');
    expect(html).toContain('class="btn btn-primary"');
  });

  it("renders rows with the shared .entity-list/.entity-row classes", () => {
    expect(html).toContain('class="entity-list"');
    expect((html.match(/class="entity-row"/g) ?? []).length).toBe(2);
  });

  it("no longer references the phantom .panel/.table classes", () => {
    expect(source).not.toContain('className="panel"');
    expect(source).not.toContain('className="panel-header"');
    expect(source).not.toContain('className="table"');
    expect(source).not.toContain('className="error-banner"');
  });

  it("uses ConfirmDialog instead of window.confirm and no page reloads", () => {
    expect(source).toContain("ConfirmDialog");
    expect(source).not.toContain("window.confirm");
    expect(source).not.toContain("window.location.reload");
  });
});

describe("PromptTemplatesPanel multi-select (S-214)", () => {
  const html = renderToStaticMarkup(
    createElement(PromptTemplatesPanel, { templates, loading: false }),
  );

  it("renders a checkbox per template row", () => {
    expect((html.match(/class="entity-checkbox"/g) ?? []).length).toBe(2);
    expect(html).toContain('aria-label="Select template Alpha"');
    expect(html).toContain('aria-label="Select template Beta"');
  });

  it("renders a header select-all checkbox and a bulk delete button", () => {
    expect(html).toContain('aria-label="Select all templates"');
    expect(html).toContain("Delete selected");
    // Bulk delete starts disabled with an empty selection.
    expect(html).toMatch(/class="btn btn-sm btn-danger" disabled="" aria-label="Delete 0 selected templates"/);
  });

  it("shows the empty state with no templates", () => {
    const emptyHtml = renderToStaticMarkup(
      createElement(PromptTemplatesPanel, { templates: [], loading: false }),
    );
    expect(emptyHtml).toContain("No prompt templates yet");
    expect(emptyHtml).not.toContain("Delete selected");
  });
});
