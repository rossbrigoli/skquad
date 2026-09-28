"use client";

// S-158: create-time template picker. Renders a dropdown of the templates
// that apply to the given target ("squad" | "agent"); selecting one fires
// onApply with the template content so the form can pre-populate its
// prompt field. The selection is a one-shot copy, not a live binding.

import { useEffect, useState } from "react";
import { useAuth } from "../lib/auth";
import { listPromptTemplates, type PromptTemplate } from "../lib/promptTemplates";

export function PromptTemplatePicker({
  target,
  onApply,
}: {
  readonly target: "squad" | "agent";
  readonly onApply: (content: string, templateName: string) => void;
}) {
  const { token, mode } = useAuth();
  const [templates, setTemplates] = useState<PromptTemplate[] | null>(null);
  const [failed, setFailed] = useState(false);
  const [selected, setSelected] = useState("");

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

  if (failed) return null;
  if (templates === null) return null;
  if (templates.length === 0) return null;

  return (
    <label className="field">
      <span>Start from a template</span>
      <select
        value={selected}
        onChange={(e) => {
          const id = e.target.value;
          setSelected(id);
          const template = templates.find((t) => t.id === id);
          if (template) onApply(template.content, template.name);
        }}
      >
        <option value="">— none —</option>
        {templates.map((t) => (
          <option key={t.id} value={t.id}>
            {t.name}
            {t.description ? ` — ${t.description}` : ""}
          </option>
        ))}
      </select>
    </label>
  );
}
