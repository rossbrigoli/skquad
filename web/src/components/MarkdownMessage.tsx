"use client";

// S-159: XSS-safe markdown rendering for chat messages.
//
// Pattern verified against the official react-markdown docs:
//   https://github.com/remarkjs/react-markdown
// react-markdown builds React elements from a syntax tree and never uses
// dangerouslySetInnerHTML; raw HTML in the source is not rendered unless
// rehype-raw is explicitly added (we do NOT add it). On top of that we run
// rehype-sanitize (https://github.com/sanity/rehype-sanitize) so the hast
// tree is scrubbed against the GitHub-safe schema before it becomes React
// elements — defense in depth for untrusted agent output.
// react-markdown's default urlTransform already blocks dangerous protocols
// (javascript:, data:, …); links are opened in a new tab with
// rel="noopener noreferrer" via the `a` component override documented in
// the same docs ("Use custom components").
//
// remark-gfm adds tables, strikethrough, task lists and bare-URL autolinks
// (the GFM flavour the docs demonstrate with remarkPlugins={[remarkGfm]}).
import { memo } from "react";
import Markdown from "react-markdown";
import remarkGfm from "remark-gfm";
import rehypeSanitize from "rehype-sanitize";
import type { Components } from "react-markdown";
import { MermaidDiagram } from "./MermaidDiagram";

/** External links open in a new tab; rel prevents opener/taber attacks. */
const CHAT_COMPONENTS: Components = {
  // `node` (the hast element react-markdown passes) is deliberately dropped
  // here so it is not spread onto the DOM <a> as an unknown attribute.
  // eslint-disable-next-line @typescript-eslint/no-unused-vars
  a({ node, children, ...props }) {
    return (
      <a {...props} target="_blank" rel="noopener noreferrer">
        {children}
      </a>
    );
  },
  // S-246: unwrap the <pre> wrapper when the fenced block is a mermaid
  // diagram. react-markdown hands `pre` the still-unrendered <code> element
  // (whose type is our `code` override below), so the detection is on the
  // child's className, not element identity. The `code` override then swaps
  // the content for <MermaidDiagram/>; every other fence keeps <pre>.
  // eslint-disable-next-line @typescript-eslint/no-unused-vars
  pre({ node, children, ...props }) {
    const only = Array.isArray(children) ? children[0] : children;
    const childClassName =
      typeof only === "object" && only !== null && "props" in only
        ? ((only as { props?: { className?: string } }).props?.className ?? "")
        : "";
    if (/(?:^|\s)language-mermaid(?:\s|$)/.test(childClassName)) {
      return <>{children}</>;
    }
    return <pre {...props}>{children}</pre>;
  },
  // S-246: ```mermaid fences render as SVG diagrams. Everything else
  // (inline code, other languages) keeps react-markdown's default rendering.
  // The sanitiser runs before this override, so `children` here is plain
  // text — MermaidDiagram then applies mermaid strict mode + DOMPurify.
  // eslint-disable-next-line @typescript-eslint/no-unused-vars
  code({ node, className, children, ...props }) {
    if (
      typeof children === "string" &&
      /(?:^|\s)language-mermaid(?:\s|$)/.test(className ?? "")
    ) {
      return <MermaidDiagram code={children.replace(/\n$/, "")} />;
    }
    return (
      <code className={className} {...props}>
        {children}
      </code>
    );
  },
};

/**
 * Renders markdown text for a chat bubble (agent or user).
 *
 * Styling lives in `.chat-markdown` in globals.css: headings are bounded so
 * an h1 in chat cannot explode the bubble, fenced code scrolls horizontally,
 * and long unbroken tokens wrap instead of breaking the layout.
 */
export const MarkdownMessage = memo(function MarkdownMessage({ text }: { readonly text: string }) {
  return (
    <div className="chat-markdown">
      <Markdown remarkPlugins={[remarkGfm]} rehypePlugins={[rehypeSanitize]} components={CHAT_COMPONENTS}>
        {text}
      </Markdown>
    </div>
  );
});
