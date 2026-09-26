// Client-side helpers for the S-138 agent storage field. These mirror the
// control-plane validation (control-plane/internal/domain/storage_size.go)
// so the form can fail fast with a friendly message; the server stays the
// authority and re-validates every value (including the platform max).

export const STORAGE_PRESETS = ["1Gi", "2Gi", "5Gi", "10Gi"];

export const DEFAULT_AGENT_STORAGE_SIZE = "2Gi";

// Kubernetes-style quantity: positive decimal number + binary/decimal suffix.
const STORAGE_QTY_RE = /^([0-9]+(?:\.[0-9]+)?)(Ki|Mi|Gi|Ti|Pi|Ei|K|M|G|T|P|E)$/;

export function isValidStorageSize(value: string): boolean {
  const v = value.trim();
  if (!STORAGE_QTY_RE.test(v)) {
    return false;
  }
  return parseFloat(v) > 0;
}

// storageDisplay renders the allocated size for detail/list views:
// "off" when durable storage is disabled, otherwise the recorded size
// (falling back to the platform default for older rows).
export function storageDisplay(enabled?: boolean, size?: string): string {
  if (!enabled) {
    return "off";
  }
  const trimmed = (size ?? "").trim();
  return trimmed !== "" ? trimmed : DEFAULT_AGENT_STORAGE_SIZE;
}
