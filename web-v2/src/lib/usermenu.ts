// Pure helpers for the user profile popover (S-117).
// Kept DOM-free so they are unit-testable without React providers.

// initialsFor builds the avatar placeholder text: up to two uppercase
// letters from the user's name ("Ada Lovelace" → "AL"). Falls back to the
// first character of the email local-part, then "?" when nothing usable.
export function initialsFor(name?: string | null, email?: string | null): string {
  const words = (name || "")
    .trim()
    .split(/\s+/)
    .filter((w) => w.length > 0 && /\p{L}|\p{N}/u.test(w));
  if (words.length > 0) {
    const first = Array.from(words[0])[0].toUpperCase();
    const second = words.length > 1 ? Array.from(words[1])[0].toUpperCase() : "";
    return `${first}${second}`;
  }
  const local = (email || "").split("@")[0]?.trim();
  if (local) return Array.from(local)[0].toUpperCase();
  return "?";
}

// displayRole normalises the role string for the popover; unknown/empty
// roles render as "member" so the field is never blank.
export function displayRole(role?: string | null): string {
  const r = (role || "").trim();
  return r === "" ? "member" : r;
}
