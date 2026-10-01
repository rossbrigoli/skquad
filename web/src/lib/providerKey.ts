// S-187: display formatting for the provider API-key hint.
//
// The control-plane never ships the real key: `api_key_masked` is a display-safe
// tail — "•••••" + the last 5 chars (control-plane/internal/httpapi/
// provider_keys.go, maskProviderKey). The provider dialog renders it as a
// fixed-length mask plus that tail (e.g. "••••••••••••abcde") so the field
// reads as a masked key instead of a sentence crammed into the placeholder.
// The "leave blank to keep the current key" semantics live in helper text,
// not in the mask itself.

/** Fixed number of mask bullets shown before the key tail. */
export const MASKED_KEY_MASK_CHARS = 12;

/** Number of visible tail characters (matches the control-plane mask tail). */
export const MASKED_KEY_TAIL_CHARS = 5;

/**
 * Render the control-plane masked hint as `••••••••••••xxxxx`.
 * Any leading mask characters from the backend are replaced by the fixed
 * mask; only the trailing visible characters survive. An empty/absent hint
 * renders as the mask alone (never leaks length or content).
 */
export function formatMaskedApiKey(masked?: string | null): string {
  const tail = (masked ?? "").replace(/^[•*]+/, "").slice(-MASKED_KEY_TAIL_CHARS);
  return "•".repeat(MASKED_KEY_MASK_CHARS) + tail;
}
