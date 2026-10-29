// S-287: platform-configurable agent pod resources.
//
// Every agent pod gets explicit CPU/memory requests and limits. The
// platform admin edits the four values on Settings → Platform; the
// control-plane validates them (k8s quantity format, request <= limit),
// persists them to platform_settings, and mirrors them into the
// operator's override ConfigMap so running agents converge without a
// redeploy. This module mirrors the control-plane validation client-side
// so bad input is caught before a round-trip.

export const DEFAULT_AGENT_CPU_REQUEST = "100m";
export const DEFAULT_AGENT_CPU_LIMIT = "500m";
export const DEFAULT_AGENT_MEMORY_REQUEST = "128Mi";
export const DEFAULT_AGENT_MEMORY_LIMIT = "512Mi";

export type AgentResourceField =
  | "cpu_request"
  | "cpu_limit"
  | "memory_request"
  | "memory_limit";

export type AgentResourcesSettings = {
  cpu_request: string;
  cpu_limit: string;
  memory_request: string;
  memory_limit: string;
};

export function defaultAgentResources(): AgentResourcesSettings {
  return {
    cpu_request: DEFAULT_AGENT_CPU_REQUEST,
    cpu_limit: DEFAULT_AGENT_CPU_LIMIT,
    memory_request: DEFAULT_AGENT_MEMORY_REQUEST,
    memory_limit: DEFAULT_AGENT_MEMORY_LIMIT,
  };
}

// CPU quantities: plain decimal cores or milli-cores ("100m").
const CPU_QUANTITY_RE = /^(\d+(?:\.\d+)?)(m?)$/;
// Memory quantities: decimal with binary (Ki/Mi/…) or decimal (K/M/…) suffix.
const MEMORY_QUANTITY_RE = /^(\d+(?:\.\d+)?)(Ki|Mi|Gi|Ti|Pi|Ei|K|M|G|T|P|E)?$/;

const MEMORY_MULTIPLIERS: Record<string, number> = {
  "": 1,
  K: 1e3,
  M: 1e6,
  G: 1e9,
  T: 1e12,
  P: 1e15,
  E: 1e18,
  Ki: 1 << 10,
  Mi: 1 << 20,
  Gi: 1 << 30,
  Ti: 1 << 40,
  Pi: 1 << 50,
  Ei: 1 << 60,
};

// parseCpuMillis mirrors the control-plane ParseCPUQuantity: returns
// millicores or null when the text is not a positive CPU quantity.
export function parseCpuMillis(raw: string): number | null {
  const m = CPU_QUANTITY_RE.exec(raw.trim());
  if (!m) return null;
  const value = Number(m[1]);
  if (!Number.isFinite(value) || value <= 0) return null;
  const millis = m[2] === "m" ? value : value * 1000;
  return millis;
}

// parseMemoryBytes mirrors the control-plane ParseStorageSize: returns
// bytes or null when the text is not a positive memory quantity.
export function parseMemoryBytes(raw: string): number | null {
  const m = MEMORY_QUANTITY_RE.exec(raw.trim());
  if (!m) return null;
  const value = Number(m[1]);
  if (!Number.isFinite(value) || value <= 0) return null;
  return value * MEMORY_MULTIPLIERS[m[2] ?? ""];
}

const FIELD_LABELS: Record<AgentResourceField, string> = {
  cpu_request: "CPU request",
  cpu_limit: "CPU limit",
  memory_request: "Memory request",
  memory_limit: "Memory limit",
};

// validateAgentResources validates the full four-field set and returns
// a list of human-readable errors (empty when valid). Mirrors the
// control-plane rules: every field required, valid k8s quantity format,
// and each request must not exceed its limit.
export function validateAgentResources(settings: AgentResourcesSettings): string[] {
  const errors: string[] = [];

  const cpuReq = parseCpuMillis(settings.cpu_request);
  const cpuLim = parseCpuMillis(settings.cpu_limit);
  const memReq = parseMemoryBytes(settings.memory_request);
  const memLim = parseMemoryBytes(settings.memory_limit);

  if (settings.cpu_request.trim() === "") errors.push(`${FIELD_LABELS.cpu_request} is required.`);
  else if (cpuReq === null)
    errors.push(`${FIELD_LABELS.cpu_request} must be a Kubernetes CPU quantity like 100m or 0.5.`);

  if (settings.cpu_limit.trim() === "") errors.push(`${FIELD_LABELS.cpu_limit} is required.`);
  else if (cpuLim === null)
    errors.push(`${FIELD_LABELS.cpu_limit} must be a Kubernetes CPU quantity like 500m or 1.`);

  if (settings.memory_request.trim() === "") errors.push(`${FIELD_LABELS.memory_request} is required.`);
  else if (memReq === null)
    errors.push(`${FIELD_LABELS.memory_request} must be a Kubernetes memory quantity like 128Mi or 1Gi.`);

  if (settings.memory_limit.trim() === "") errors.push(`${FIELD_LABELS.memory_limit} is required.`);
  else if (memLim === null)
    errors.push(`${FIELD_LABELS.memory_limit} must be a Kubernetes memory quantity like 512Mi or 1Gi.`);

  if (cpuReq !== null && cpuLim !== null && cpuReq > cpuLim) {
    errors.push(`${FIELD_LABELS.cpu_request} must not exceed ${FIELD_LABELS.cpu_limit}.`);
  }
  if (memReq !== null && memLim !== null && memReq > memLim) {
    errors.push(`${FIELD_LABELS.memory_request} must not exceed ${FIELD_LABELS.memory_limit}.`);
  }
  return errors;
}

// normalizeAgentResources coerces a fetched block to full settings,
// falling back to the built-in defaults for missing/blank fields.
export function normalizeAgentResources(raw: Partial<AgentResourcesSettings> | undefined): AgentResourcesSettings {
  const d = defaultAgentResources();
  const pick = (v: string | undefined, fallback: string): string => {
    const trimmed = (v ?? "").trim();
    return trimmed === "" ? fallback : trimmed;
  };
  return {
    cpu_request: pick(raw?.cpu_request, d.cpu_request),
    cpu_limit: pick(raw?.cpu_limit, d.cpu_limit),
    memory_request: pick(raw?.memory_request, d.memory_request),
    memory_limit: pick(raw?.memory_limit, d.memory_limit),
  };
}
