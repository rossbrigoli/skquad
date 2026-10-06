// TG-4 (S-250): BYO REST resource registration + constraint helpers.
//
// Mirrors the control-plane contract (rest_credentials.go + egresspolicy):
// the `auth` payload is write-only — secret fields are sent on create/patch
// and NEVER re-displayed (reads expose nothing). Ceiling fields follow the
// rest ceiling shape; effective constraints fold ceiling ∧ grant the same
// way the gateway does (methods intersect, grant path_allow wins, deny
// unions, numerics take the tightest bound).

export const REST_AUTH_KINDS = ["none", "bearer", "api_key_header", "basic", "oauth2_client_credentials"] as const;
export type RestAuthKind = (typeof REST_AUTH_KINDS)[number];

export const REST_HTTP_METHODS = ["GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"] as const;

// restAuthFields mirrors control-plane restAuthFields: secret fields per
// auth kind. `label` is for the form; `field` is the wire key.
export const REST_AUTH_FIELDS: Record<RestAuthKind, readonly { field: string; label: string; secret: boolean }[]> = {
  none: [],
  bearer: [{ field: "token", label: "Bearer token", secret: true }],
  api_key_header: [{ field: "token", label: "API key", secret: true }],
  basic: [
    { field: "username", label: "Username", secret: false },
    { field: "password", label: "Password", secret: true },
  ],
  oauth2_client_credentials: [
    { field: "client_id", label: "Client ID", secret: false },
    { field: "client_secret", label: "Client secret", secret: true },
    { field: "token_url", label: "Token URL (https://…/oauth/token)", secret: false },
  ],
};

export type RestEndpointConfig = {
  base_url: string;
  auth_kind: RestAuthKind;
  header_name?: string;
};

export type RestCeiling = {
  methods?: string[];
  path_allow?: string[];
  path_deny?: string[];
  max_request_bytes?: number;
  max_response_bytes?: number;
  rate_per_min?: number;
  egress_class?: "public" | "internal";
};

export type RestResourceForm = {
  name: string;
  description: string;
  baseUrl: string;
  authKind: RestAuthKind;
  headerName: string;
  methods: string[];
  pathAllow: string;
  pathDeny: string;
  maxRequestBytes: string;
  maxResponseBytes: string;
  ratePerMin: string;
  egressClass: "public" | "internal";
  secrets: Record<string, string>;
};

export function emptyRestForm(): RestResourceForm {
  return {
    name: "",
    description: "",
    baseUrl: "",
    authKind: "bearer",
    headerName: "",
    methods: ["GET"],
    pathAllow: "",
    pathDeny: "",
    maxRequestBytes: "",
    maxResponseBytes: "",
    ratePerMin: "",
    egressClass: "public",
    secrets: {},
  };
}

// splitList parses a comma/newline-separated pattern list into trimmed,
// non-empty entries.
export function splitList(value: string): string[] {
  return value
    .split(/[,\n]/)
    .map((s) => s.trim())
    .filter((s) => s !== "");
}

function optionalInt(value: string): number | undefined {
  const trimmed = value.trim();
  if (trimmed === "") return undefined;
  const n = Number(trimmed);
  if (!Number.isInteger(n) || n <= 0) throw new Error(`"${trimmed}" is not a positive integer`);
  return n;
}

// validateRestForm returns a human-readable error or null.
export function validateRestForm(form: RestResourceForm): string | null {
  if (form.name.trim() === "") return "Name is required";
  if (form.baseUrl.trim() === "") return "Base URL is required";
  let parsed: URL;
  try {
    parsed = new URL(form.baseUrl.trim());
  } catch {
    return "Base URL must be an absolute http(s) URL";
  }
  if (parsed.protocol !== "https:" && parsed.protocol !== "http:") return "Base URL must be http(s)";
  if (form.authKind === "api_key_header" && form.headerName.trim() === "") return "Header name is required for api_key_header auth";
  if (form.authKind !== "api_key_header" && form.headerName.trim() !== "") return "Header name only applies to api_key_header auth";
  for (const f of REST_AUTH_FIELDS[form.authKind]) {
    if ((form.secrets[f.field] ?? "").trim() === "") return `${f.label} is required`;
  }
  try {
    optionalInt(form.maxRequestBytes);
    optionalInt(form.maxResponseBytes);
    optionalInt(form.ratePerMin);
  } catch (err) {
    return err instanceof Error ? err.message : "invalid numeric bound";
  }
  return null;
}

// buildRestResourcePayload builds the CP create/patch body. `includeAuth`
// is false on edit when no secret fields were re-entered (patch leaves the
// stored secret untouched).
export function buildRestResourcePayload(form: RestResourceForm, includeAuth: boolean): Record<string, unknown> {
  const endpointConfig: RestEndpointConfig = {
    base_url: form.baseUrl.trim(),
    auth_kind: form.authKind,
  };
  if (form.authKind === "api_key_header") endpointConfig.header_name = form.headerName.trim();

  const ceiling: RestCeiling = { egress_class: form.egressClass };
  if (form.methods.length > 0) ceiling.methods = [...form.methods];
  const allow = splitList(form.pathAllow);
  const deny = splitList(form.pathDeny);
  if (allow.length > 0) ceiling.path_allow = allow;
  if (deny.length > 0) ceiling.path_deny = deny;
  const reqBytes = optionalInt(form.maxRequestBytes);
  const respBytes = optionalInt(form.maxResponseBytes);
  const rate = optionalInt(form.ratePerMin);
  if (reqBytes !== undefined) ceiling.max_request_bytes = reqBytes;
  if (respBytes !== undefined) ceiling.max_response_bytes = respBytes;
  if (rate !== undefined) ceiling.rate_per_min = rate;

  const payload: Record<string, unknown> = {
    name: form.name.trim(),
    description: form.description.trim(),
    endpoint_config: endpointConfig,
    policy_ceiling: ceiling,
    risk_tier: "medium",
    egress_class: form.egressClass,
  };
  if (includeAuth) {
    const auth: Record<string, string> = {};
    for (const f of REST_AUTH_FIELDS[form.authKind]) {
      const v = (form.secrets[f.field] ?? "").trim();
      if (v !== "") auth[f.field] = v;
    }
    payload.auth = auth;
  }
  return payload;
}

// RestEffectiveConstraints is the read-only effective view.
export type RestEffectiveConstraints = {
  methods: string[];
  path_allow: string[];
  path_deny: string[];
  max_request_bytes: number;
  max_response_bytes: number;
  rate_per_min: number;
  egress_class: string;
};

// foldRestConstraints computes the effective (ceiling ∧ grant) view the
// gateway enforces — mirrors control-plane rest_tool_surface.go /
// tool-gateway rest.EffectivePolicy folding for read-only display.
export function foldRestConstraints(ceiling: RestCeiling | undefined, grant: RestCeiling | undefined): RestEffectiveConstraints {
  const c = ceiling ?? {};
  const g = grant ?? {};
  const methods = intersect(g.methods ? upperAll(g.methods) : undefined, upperAll(c.methods ?? []));
  const pathAllow = g.path_allow && g.path_allow.length > 0 ? g.path_allow : (c.path_allow ?? []);
  const pathDeny = [...(c.path_deny ?? []), ...(g.path_deny ?? [])];
  const minOf = (vals: (number | undefined)[], fallback: number): number => {
    const nums = vals.filter((v): v is number => typeof v === "number" && v > 0);
    return nums.length > 0 ? Math.min(...nums) : fallback;
  };
  const rates = [c.rate_per_min, g.rate_per_min].filter((v): v is number => typeof v === "number" && v > 0);
  return {
    methods,
    path_allow: pathAllow,
    path_deny: pathDeny,
    max_request_bytes: minOf([c.max_request_bytes, g.max_request_bytes], 65536),
    max_response_bytes: minOf([c.max_response_bytes, g.max_response_bytes], 262144),
    rate_per_min: rates.length > 0 ? Math.min(...rates) : 0,
    egress_class: c.egress_class === "internal" && g.egress_class === "internal" ? "internal" : "public",
  };
}

function upperAll(list: string[]): string[] {
  return list.map((s) => s.toUpperCase());
}

function intersect(a: string[] | undefined, b: string[]): string[] {
  if (a === undefined) return b;
  const set = new Set(a);
  return b.filter((v) => set.has(v));
}

// grantConstraintsLabel renders a compact read-only summary of a grant's
// narrowing constraints (unset fields mean "no narrowing", unlike a
// ceiling's default-deny). Empty string when nothing is narrowed.
export function grantConstraintsLabel(constraints: unknown): string {
  const c =
    constraints && typeof constraints === "object" && !Array.isArray(constraints)
      ? (constraints as Record<string, unknown>)
      : {};
  const bits: string[] = [];
  if (Array.isArray(c.methods) && c.methods.length > 0) bits.push(`methods: ${c.methods.join(", ")}`);
  if (Array.isArray(c.path_allow) && c.path_allow.length > 0) bits.push(`allow: ${c.path_allow.join(", ")}`);
  if (Array.isArray(c.path_deny) && c.path_deny.length > 0) bits.push(`deny: ${c.path_deny.join(", ")}`);
  if (typeof c.rate_per_min === "number" && c.rate_per_min > 0) bits.push(`≤ ${c.rate_per_min}/min`);
  if (typeof c.max_request_bytes === "number") bits.push(`req ≤ ${Math.round(c.max_request_bytes / 1024)} KiB`);
  if (typeof c.max_response_bytes === "number") bits.push(`resp ≤ ${Math.round(c.max_response_bytes / 1024)} KiB`);
  return bits.join(" · ");
}

// restCeilingSummary renders a one-line human summary of a ceiling for
// read-only panels.
export function restCeilingSummary(ceiling: RestCeiling | RestEffectiveConstraints | undefined): string {
  const c = ceiling ?? {};
  const bits: string[] = [];
  bits.push(`methods: ${(c.methods ?? []).join(", ") || "none (default-deny)"}`);
  bits.push(`allow: ${(c.path_allow ?? []).join(", ") || "—"}`);
  if ((c.path_deny ?? []).length > 0) bits.push(`deny: ${c.path_deny?.join(", ")}`);
  if (c.max_request_bytes) bits.push(`req ≤ ${Math.round(c.max_request_bytes / 1024)} KiB`);
  if (c.max_response_bytes) bits.push(`resp ≤ ${Math.round(c.max_response_bytes / 1024)} KiB`);
  bits.push(c.rate_per_min ? `≤ ${c.rate_per_min}/min` : "no rate limit");
  bits.push(`egress: ${c.egress_class ?? "public"}`);
  return bits.join(" · ");
}
