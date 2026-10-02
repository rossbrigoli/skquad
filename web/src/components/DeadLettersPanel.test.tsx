// S-205: the Dead Letters page was always empty because the panel used a
// raw fetch against NEXT_PUBLIC_SKQUAD_API_BASE_URL instead of apiGet.
// Under OIDC (the only deployed mode) apiBaseUrl() is overridden to the
// server-side /proxy which attaches the bearer token; the direct call
// carried no credentials, 401'd, and the list never populated. The
// unstyled bare <select>/<input> filters also broke the design system.
//
// These tests lock both fixes in:
//   1. the panel must go through apiGet (proxy-aware) — never a raw
//      NEXT_PUBLIC base-URL fetch;
//   2. every filter control must use the app's standard .field wrapper
//      (same styled inputs/selects as the settings dialogs).
import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { TokenProvider } from "../lib/auth";
import { DeadLettersPanel } from "./DeadLettersPanel";

const source = readFileSync(new URL("./DeadLettersPanel.tsx", import.meta.url), "utf8");

describe("DeadLettersPanel transport (S-205 root cause)", () => {
  it("lists dead letters via apiGet so the OIDC /proxy base is honoured", () => {
    expect(source).toContain("apiGet<DeadLettersResponse>");
    expect(source).toContain("/admin/dead-letters");
  });

  it("never builds a raw fetch from NEXT_PUBLIC_SKQUAD_API_BASE_URL", () => {
    // The old bug: bypassing apiBaseUrl() meant no /proxy under OIDC.
    expect(source).not.toMatch(/fetch\(`\$\{process\.env\.NEXT_PUBLIC_SKQUAD_API_BASE_URL/);
    expect(source).not.toContain("NEXT_PUBLIC_SKQUAD_API_BASE_URL");
  });
});

describe("DeadLettersPanel filter styling (S-205)", () => {
  const html = renderToStaticMarkup(
    createElement(TokenProvider, null, createElement(DeadLettersPanel)),
  );

  it("wraps every filter control in the standard .field component", () => {
    const fieldCount = (html.match(/class="field"/g) ?? []).length;
    // squad, agent, type, reason, since, until
    expect(fieldCount).toBe(6);
  });

  it("lays the filters out with the standard .field-row grid", () => {
    expect(html).toContain('class="field-row"');
  });

  it("keeps the message-type dropdown options wired to the domain types", () => {
    for (const t of ["consult", "delegate", "handoff", "ping", "reply"]) {
      expect(html).toContain(`>${t}</option>`);
    }
  });
});
