"use client";

import { useEffect, useState } from "react";
import { apiPost } from "./api";
import { useAuth } from "./auth";
import { PROMPT_VALIDATE_DEBOUNCE_MS, type PromptScope, type PromptValidateResponse } from "./prompt";

// Debounced dry-run of the save-time battery (POST /prompt/validate) for a
// draft. Every content change restarts the timer; stale responses are
// dropped via the `active` guard so a slow validate for old text can
// never clobber the result for new text. A failed validate request clears
// the result rather than showing a stale verdict — the save path still
// re-checks server-side.
export function usePromptValidation(
  scope: PromptScope,
  content: string,
  delayMs: number = PROMPT_VALIDATE_DEBOUNCE_MS,
): { result: PromptValidateResponse | null; validating: boolean } {
  const { token, mode } = useAuth();
  const [result, setResult] = useState<PromptValidateResponse | null>(null);
  const [validating, setValidating] = useState(false);

  useEffect(() => {
    let active = true;
    // Initial fetch/validation is an effect by design; setState lands async.
    // eslint-disable-next-line react-hooks/set-state-in-effect
    setValidating(true);
    const timer = window.setTimeout(() => {
      apiPost<PromptValidateResponse>(
        "/prompt/validate",
        mode === "oidc" ? "" : token,
        { scope, content },
      )
        .then((res) => {
          if (active) setResult(res);
        })
        .catch(() => {
          if (active) setResult(null);
        })
        .finally(() => {
          if (active) setValidating(false);
        });
    }, delayMs);
    return () => {
      active = false;
      window.clearTimeout(timer);
    };
  }, [scope, content, token, mode, delayMs]);

  return { result, validating };
}
