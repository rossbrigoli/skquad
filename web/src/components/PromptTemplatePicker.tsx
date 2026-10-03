"use client";

// S-158: create-time template picker. S-215 rebuilt it as a
// "From template" button beside the prompt field that opens a small
// menu of the templates applying to the given target
// ("squad" | "agent"). Selecting one fires onApply with the *final*
// prompt content: an empty draft is replaced by the template, while a
// non-empty draft gets the template appended below a separator line so
// existing work is never clobbered. The selection remains a one-shot
// copy, not a live binding.

import { useEffect, useRef, useState } from "react";
import { useAuth } from "../lib/auth";
import { listPromptTemplates, type PromptTemplate } from "../lib/promptTemplates";

// S-215: separator written between existing draft text and an appended
// template prompt. Kept exported so tests and callers share one source.
export const TEMPLATE_SEPARATOR = "---------- From Template --------------";

export function applyTemplate(current: string, content: string): string {
  if (current.trim() === "") return content;
  return `${current}\n${TEMPLATE_SEPARATOR}\n${content}`;
}

export function PromptTemplatePicker({
  target,
  currentPrompt,
  onApply,
}: {
  readonly target: "squad" | "agent";
  readonly currentPrompt: string;
  readonly onApply: (content: string, templateName: string) => void;
}) {
  const { token, mode } = useAuth();
  const [templates, setTemplates] = useState<PromptTemplate[] | null>(null);
  const [failed, setFailed] = useState(false);
  const [open, setOpen] = useState(false);
  const rootRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    let cancelled = false;
    // Same token convention as the other panels: OIDC relies on the
    // cookie session, dev mode passes the bearer token.
    const authedToken = mode === "oidc" ? "" : token;
    listPromptTemplates(authedToken, target)
      .then((list) => {
        if (!cancelled) setTemplates(list);
      })
      .catch(() => {
        if (!cancelled) setFailed(true);
      });
    return () => {
      cancelled = true;
    };
  }, [target, token, mode]);

  // S-215: dismiss the menu on outside click or Escape, like a native menu.
  useEffect(() => {
    if (!open) return;
    const onDocClick = (e: MouseEvent) => {
      if (rootRef.current && !rootRef.current.contains(e.target as Node)) setOpen(false);
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") setOpen(false);
    };
    document.addEventListener("mousedown", onDocClick);
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("mousedown", onDocClick);
      document.removeEventListener("keydown", onKey);
    };
  }, [open]);

  if (failed) return null;
  if (templates === null) return null;
  if (templates.length === 0) return null;

  return (
    <div className="template-picker" ref={rootRef}>
      <button
        type="button"
        className="btn btn-sm"
        aria-haspopup="menu"
        aria-expanded={open}
        onClick={() => setOpen((v) => !v)}
      >
        From template
      </button>
      {open ? (
        <div className="template-menu" role="menu" aria-label="Prompt templates">
          {templates.map((t) => (
            <button
              key={t.id}
              type="button"
              role="menuitem"
              className="template-menu-item"
              onClick={() => {
                onApply(applyTemplate(currentPrompt, t.content), t.name);
                setOpen(false);
              }}
            >
              {t.name}
              {t.description ? <span className="template-menu-desc">{t.description}</span> : null}
            </button>
          ))}
        </div>
      ) : null}
    </div>
  );
}
