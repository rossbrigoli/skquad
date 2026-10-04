"use client";

import { useEffect, useState } from "react";
import { ApiError } from "../lib/api";
import {
  extractPromptError,
  promptUserMessage,
  saveBlockedByValidation,
  type PromptScope,
} from "../lib/prompt";
import { usePromptValidation } from "../lib/usePromptValidation";
import { TokenMeter } from "./TokenMeter";

// PromptTierEditor: the shared layer-editor surface (org / squad / agent).
// Controlled component — the parent owns `content` so a revision
// 'restore' can prefill it. Validation runs debounced against
// POST /prompt/validate; the three save-time error codes
// (reserved tokens / unknown template vars / token cap) surface inline
// before the save is even attempted, and again if the save races one in.
export function PromptTierEditor({
  scope,
  label,
  content,
  onChange,
  onSave,
  saveLabel = "Save prompt",
  placeholder = "",
  hint,
  disabled = false,
  showSaveButton = true,
  onBlockedChange,
}: {
  readonly scope: PromptScope;
  readonly label: string;
  readonly content: string;
  readonly onChange: (next: string) => void;
  readonly onSave: () => Promise<void>;
  readonly saveLabel?: string;
  readonly placeholder?: string;
  readonly hint?: string;
  readonly disabled?: boolean;
  // S-169: the agent Configuration tab owns a single Save for the whole
  // form, so the editor's own button can be hidden; `onBlockedChange`
  // lets that outer Save mirror the editor's blocked state.
  readonly showSaveButton?: boolean;
  readonly onBlockedChange?: (blocked: boolean) => void;
}) {
  const { result, validating } = usePromptValidation(scope, content);
  const [busy, setBusy] = useState(false);
  const [saveError, setSaveError] = useState("");
  const [savedNote, setSavedNote] = useState("");

  const validationError = result && !result.valid && result.error
    ? promptUserMessage({ code: result.error.code, message: result.error.message })
    : "";
  const blocked = saveBlockedByValidation(result);
  const softWarn = result?.soft_warn ?? 0;
  const hardCap = result?.hard_cap ?? 0;

  useEffect(() => {
    onBlockedChange?.(blocked);
  }, [blocked, onBlockedChange]);

  return (
    <div className="prompt-editor">
      <label className="field">
        <span>{label}</span>
        <textarea
          className="prompt-textarea"
          rows={12}
          value={content}
          onChange={(e) => {
            setSavedNote("");
            setSaveError("");
            onChange(e.target.value);
          }}
          placeholder={placeholder}
          disabled={disabled || busy}
        />
        {hint ? <span className="field-hint">{hint}</span> : null}
      </label>

      {result ? <TokenMeter tokens={result.tokens} softWarn={softWarn} hardCap={hardCap} levelOverride={validating ? "checking" : null} /> : null}

      {validationError ? (
        <div className="notice error" role="alert">
          {validationError}
        </div>
      ) : null}
      {result?.warnings?.map((w) => (
        <output key={w} className="notice warn" style={{ display: "block" }}>
          {w}
        </output>
      ))}
      {saveError ? (
        <div className="notice error" role="alert">
          {saveError}
        </div>
      ) : null}
      {savedNote ? (
        <output className="notice" aria-live="polite">
          {savedNote}
        </output>
      ) : null}

      <div style={{ display: "flex", gap: "var(--space-2)", alignItems: "center" }}>
        {showSaveButton ? (
          <button
            type="button"
            className="btn btn-primary"
            disabled={disabled || busy || blocked}
            onClick={() => {
              setBusy(true);
              setSaveError("");
              setSavedNote("");
              onSave()
                .then(() => {
                  setSavedNote("Saved.");
                })
                .catch((err: unknown) => {
                  const promptErr = extractPromptError(err instanceof ApiError ? err : undefined);
                  let detail: string;
                  if (promptErr) {
                    detail = promptUserMessage(promptErr);
                  } else if (err instanceof Error) {
                    detail = err.message;
                  } else {
                    detail = "save failed";
                  }
                  setSaveError(detail);
                })
                .finally(() => {
                  setBusy(false);
                });
            }}
          >
            {busy ? "Saving…" : saveLabel}
          </button>
        ) : null}
        {blocked ? <span className="entity-meta">Fix the errors above before saving.</span> : null}
      </div>
    </div>
  );
}
