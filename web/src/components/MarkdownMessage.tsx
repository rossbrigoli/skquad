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
