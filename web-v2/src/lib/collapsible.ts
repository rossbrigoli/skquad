// Pure helpers backing the Collapsible component (S-118).
// Sections are COLLAPSED BY DEFAULT; only an explicit session-stored "1"
// (set by the user expanding the section) opens them. Storage is injected so
// these helpers are testable in a plain node environment.

export interface KVStorage {
  getItem(key: string): string | null;
  setItem(key: string, value: string): void;
}

export function collapseKey(id: string): string {
  return `skquad:collapsible:${id}`;
}

/** Expansion state for a section: collapsed unless the session says "1".
 * A stored "0" means the user explicitly collapsed it and wins over `fallback`. */
export function initialExpanded(
  id: string,
  storage: KVStorage | null | undefined,
  fallback = false,
): boolean {
  try {
    const raw = storage?.getItem(collapseKey(id));
    if (raw === "1") return true;
    if (raw === "0") return false;
    return fallback;
  } catch {
    return false;
  }
}

/** Persist a user's expansion choice for the session. Failures are ignored. */
export function persistExpanded(
  storage: KVStorage | null | undefined,
  id: string,
  expanded: boolean,
): void {
  try {
    storage?.setItem(collapseKey(id), expanded ? "1" : "0");
  } catch {
    // private-mode / quota errors: expansion still works in-memory.
  }
}
