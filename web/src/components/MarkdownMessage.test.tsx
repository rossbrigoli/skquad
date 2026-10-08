// S-159: tests for the markdown chat renderer. Mirrors the repo's
// node-environment test philosophy (see src/lib/chat.test.ts): we render
// the component to a static HTML string with react-dom/server and assert on
// the markup — no jsdom needed, and sanitization behaviour is fully
// observable in the output.
import { describe, expect, it, vi } from "vitest";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { MarkdownMessage } from "./MarkdownMessage";

// S-246: the mermaid renderer lazy-loads a ~2 MB library, so this node-project
// test mocks the component and asserts ROUTING only (which fences become
// diagrams). MermaidDiagram's own behaviour lives in its jsdom suite.
vi.mock("./MermaidDiagram", () => ({
  MermaidDiagram: ({ code }: { readonly code: string }) =>
    createElement("div", { "data-testid": "mermaid-mock" }, code),
}));

function render(markdown: string): string {
  return renderToStaticMarkup(createElement(MarkdownMessage, { text: markdown }));
}

describe("MarkdownMessage", () => {
  it("wraps output in the .chat-markdown container", () => {
    const html = render("hello");
    expect(html).toContain('class="chat-markdown"');
    expect(html).toContain("<p>hello</p>");
  });

  it("renders bold and italic", () => {
    const html = render("**bold** and *italic*");
    expect(html).toContain("<strong>bold</strong>");
    expect(html).toContain("<em>italic</em>");
  });

  it("renders inline code", () => {
    const html = render("run `npm test` now");
    expect(html).toContain("<code>npm test</code>");
  });

  it("renders fenced code blocks inside <pre>", () => {
    const html = render("```js\nconst x = 1;\n```");
    expect(html).toContain("<pre>");
    expect(html).toContain("<code");
    expect(html).toContain("const x = 1;");
  });

  it("opens links in a new tab with rel=noopener noreferrer", () => {
    const html = render("[docs](https://example.com/page)");
    expect(html).toContain('href="https://example.com/page"');
    expect(html).toContain('target="_blank"');
    expect(html).toContain('rel="noopener noreferrer"');
  });

  it("renders headings (bounded by CSS, tags still present)", () => {
    const html = render("# Title\n\n## Sub");
    expect(html).toContain("<h1>Title</h1>");
    expect(html).toContain("<h2>Sub</h2>");
  });

  it("renders unordered/ordered lists and blockquotes", () => {
    const html = render("- one\n- two\n\n> quoted");
    expect(html).toContain("<ul>");
    expect(html).toContain("<li>one</li>");
    expect(html).toContain("<blockquote>");
    expect(html).toContain("quoted");
    const ordered = render("1. first\n2. second");
    expect(ordered).toContain("<ol>");
    expect(ordered).toContain("<li>first</li>");
  });

  it("renders GFM tables (remark-gfm)", () => {
    const html = render("| a | b |\n| - | - |\n| 1 | 2 |");
    expect(html).toContain("<table>");
    expect(html).toContain("<th>a</th>");
    expect(html).toContain("<td>1</td>");
  });

  it("autolinks bare URLs via GFM", () => {
    const html = render("see https://skquad.lab for more");
    expect(html).toContain('href="https://skquad.lab"');
    expect(html).toContain('rel="noopener noreferrer"');
  });

  it("strips raw HTML — no script tags or handlers survive", () => {
    const html = render('<script>alert("xss")</script><img src=x onerror="alert(1)">');
    expect(html).not.toContain("script");
    expect(html).not.toContain("alert");
    expect(html).not.toContain("onerror");
  });

  it("neutralises javascript: URLs in links", () => {
    const html = render("[click](javascript:alert(1))");
    expect(html).not.toContain("javascript:");
    expect(html).not.toContain("alert");
  });

  it("renders an empty wrapper for empty text", () => {
    expect(render("")).toBe('<div class="chat-markdown"></div>');
  });

  it("escapes HTML entities in plain text", () => {
    const html = render("a < b & c > d");
    expect(html).toContain("&lt;");
    expect(html).toContain("&amp;");
    expect(html).not.toContain("< b");
  });

  // S-246: mermaid fence routing
  it("routes ```mermaid fences to MermaidDiagram (not a code block)", () => {
    const html = render("```mermaid\ngraph TD; A-->B;\n```");
    expect(html).toContain('data-testid="mermaid-mock"');
    expect(html).toContain("graph TD; A--&gt;B;");
    expect(html).not.toContain("<pre>");
  });

  it("trims exactly one trailing newline from mermaid source", () => {
    const html = render("```mermaid\ngraph TD; A-->B;\n```");
    expect(html).toContain('data-testid="mermaid-mock">graph TD; A--&gt;B;<');
  });

  it("does NOT route non-mermaid fences", () => {
    const html = render("```javascript\nlet mermaidish = 1;\n```");
    expect(html).not.toContain('data-testid="mermaid-mock"');
    expect(html).toContain("<pre>");
  });

  it("does NOT route inline code mentioning mermaid", () => {
    const html = render("use `mermaid` for diagrams");
    expect(html).not.toContain('data-testid="mermaid-mock"');
    expect(html).toContain("<code>mermaid</code>");
  });
});
