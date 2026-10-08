"use client";

// S-246: render ```mermaid fences in chat as real SVG diagrams.
//
// SECURITY: diagram source is UNTRUSTED agent output, so two layers apply:
//  1. mermaid `securityLevel: "strict"` — the official default; encodes HTML
//     inside labels and disables click handlers.
//     https://mermaid.ai/open-source/config/usage.html#configuring-mermaid
//  2. DOMPurify on the produced SVG before it is injected into the DOM.
//     https://github.com/cure53/DOMPurify (SVG support is built in; we keep
//     the strict svg USE_PROFILE and do NOT widen allowlists — foreignObject
//     HTML is stripped).
//
// Mermaid is ~2 MB min+gzip-ish and must NOT ride along in the initial chat
// bundle, so it is lazy-loaded via dynamic import only when a diagram block
// actually renders. The promise and the rendered-SVG cache are module-level
// so repeated bubbles with the same source render once.

import { memo, useEffect, useId, useState } from "react";

type State =
  | { kind: "pending" }
  | { kind: "ready"; svg: string }
  | { kind: "error" };

/** Rendered + sanitized SVG keyed by the exact diagram source text. */
const svgCache = new Map<string, string>();

/** Single shared lazily-initialised mermaid instance. */
let mermaidPromise: Promise<typeof import("mermaid").default> | null = null;

function loadMermaid(): Promise<typeof import("mermaid").default> {
  if (!mermaidPromise) {
    mermaidPromise = import("mermaid").then((m) => {
      const mermaid = m.default;
      mermaid.initialize({
        startOnLoad: false,
        securityLevel: "strict",
        theme: "neutral",
      });
      return mermaid;
    });
  }
  return mermaidPromise;
}

/**
 * Renders one mermaid diagram. On parse/render failure the raw source is shown
 * as a copyable code block (never a blank bubble), so a broken diagram cannot
 * hide the agent's message.
 */
export const MermaidDiagram = memo(function MermaidDiagram({
  code,
}: {
  readonly code: string;
}) {
  const [state, setState] = useState<State>(() => {
    const cached = svgCache.get(code);
    return cached !== undefined ? { kind: "ready", svg: cached } : { kind: "pending" };
  });

  // useId() contains ':' characters which are awkward in DOM queries and in
  // mermaid's internal id handling — normalise to [A-Za-z0-9_-].
  const baseId = `mmd${useId().replace(/[^a-zA-Z0-9_-]/g, "_")}`;

  useEffect(() => {
    const cached = svgCache.get(code);
    if (cached !== undefined) {
      setState({ kind: "ready", svg: cached });
      return;
    }
    let cancelled = false;
    setState({ kind: "pending" });
    (async () => {
      try {
        const [mermaid, { default: DOMPurify }] = await Promise.all([
          loadMermaid(),
          import("dompurify"),
        ]);
        const { svg } = await mermaid.render(baseId, code);
        // mermaid >= 10 appends a hidden temp element to <body> during
        // render (id "d<id>" historically, "<id>-container" in newer
        // releases) — remove both shapes if present.
        document.getElementById(`d${baseId}`)?.remove();
        document.getElementById(`${baseId}-container`)?.remove();
        const clean = DOMPurify.sanitize(svg, {
          USE_PROFILES: { svg: true, svgFilters: true },
        });
        svgCache.set(code, clean);
        if (!cancelled) setState({ kind: "ready", svg: clean });
      } catch {
        if (!cancelled) setState({ kind: "error" });
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [code, baseId]);

  if (state.kind === "ready") {
    return (
      <div
        className="mermaid-diagram"
        // Safe: `state.svg` went through mermaid strict mode + DOMPurify above.
        // Raw agent text is NEVER injected as HTML.
        dangerouslySetInnerHTML={{ __html: state.svg }}
      />
    );
  }

  if (state.kind === "error") {
    return (
      <pre className="mermaid-fallback">
        <code className="language-mermaid">{code}</code>
      </pre>
    );
  }

  return <div className="mermaid-diagram mermaid-pending">Rendering diagram…</div>;
});
