// TG-4b (S-244-series): BYO git resource registration helpers.
//
// Mirrors the control-plane git contract (validate.go GitConfigKeys /
// GitCeilingKeys + rest_credentials.go custody): the endpoint_config is
// just a base URL (e.g. https://github.com), the policy ceiling is
// {repos_allow, allow_push, rate_per_min}, and custody is ALWAYS a
// bearer token (GitHub fine-grained PAT / App installation token) sent
// write-only in the `auth` payload — never re-displayed.
//
// Unlike REST there are no method/path allowlists: the repo allowlist IS
// the surface. repos_allow is required and non-empty (default-deny).

import { splitList } from "./restResources";

// GIT_AUTH_FIELDS mirrors the fixed bearer custody: one secret field.
export const GIT_AUTH_FIELDS: readonly { field: string; label: string; secret: boolean }[] = [
  { field: "token", label: "Access token (PAT / installation token)", secret: true },
];

export type GitEndpointConfig = {
  base_url: string;
};

export type GitCeiling = {
  repos_allow?: string[];
  allow_push?: boolean;
  rate_per_min?: number;
};

export type GitResourceForm = {
  name: string;
  description: string;
  baseUrl: string;
  reposAllow: string;
  allowPush: boolean;
  ratePerMin: string;
  secrets: Record<string, string>;
};

export function emptyGitForm(): GitResourceForm {
  return {
    name: "",
    description: "",
    baseUrl: "",
    reposAllow: "",
    allowPush: false,
    ratePerMin: "",
    secrets: {},
  };
}

function optionalInt(value: string): number | undefined {
  const trimmed = value.trim();
  if (trimmed === "") return undefined;
  const n = Number(trimmed);
  if (!Number.isInteger(n) || n <= 0) throw new Error(`"${trimmed}" is not a positive integer`);
  return n;
}

// validateGitForm returns a human-readable error or null. Mirrors the
// control-plane validation: absolute http(s) base URL, non-empty
// repos_allow (required, default-deny), positive-integer rate. The
// bearer token is required unless `requireToken` is false (edit with no
// rotation — the patch then leaves the stored Secret untouched).
export function validateGitForm(form: GitResourceForm, requireToken = true): string | null {
  if (form.name.trim() === "") return "Name is required";
  if (form.baseUrl.trim() === "") return "Base URL is required";
  let parsed: URL;
  try {
    parsed = new URL(form.baseUrl.trim());
  } catch {
    return "Base URL must be an absolute http(s) URL";
  }
  if (parsed.protocol !== "https:" && parsed.protocol !== "http:") return "Base URL must be http(s)";
  if (splitList(form.reposAllow).length === 0)
    return "At least one repo pattern is required (e.g. myorg/*, myorg/app) — default-deny";
  if (requireToken) {
    for (const f of GIT_AUTH_FIELDS) {
      if ((form.secrets[f.field] ?? "").trim() === "") return `${f.label} is required`;
    }
  }
  try {
    optionalInt(form.ratePerMin);
  } catch (err) {
    return err instanceof Error ? err.message : "invalid numeric bound";
  }
  return null;
}

// buildGitResourcePayload builds the CP create/patch body for a git
// resource. `includeAuth` is false on edit when no secret was re-entered
// (patch then leaves the stored Secret untouched).
export function buildGitResourcePayload(form: GitResourceForm, includeAuth: boolean): Record<string, unknown> {
  const endpointConfig: GitEndpointConfig = { base_url: form.baseUrl.trim() };

  const ceiling: GitCeiling = {
    repos_allow: splitList(form.reposAllow),
    allow_push: form.allowPush,
  };
  const rate = optionalInt(form.ratePerMin);
  if (rate !== undefined) ceiling.rate_per_min = rate;

  const payload: Record<string, unknown> = {
    name: form.name.trim(),
    description: form.description.trim(),
    endpoint_config: endpointConfig,
    policy_ceiling: ceiling,
    risk_tier: "medium",
    egress_class: "public",
  };
  if (includeAuth) {
    const auth: Record<string, string> = {};
    for (const f of GIT_AUTH_FIELDS) {
      const v = (form.secrets[f.field] ?? "").trim();
      if (v !== "") auth[f.field] = v;
    }
    payload.auth = auth;
  }
  return payload;
}

// gitCeilingSummary renders a one-line human summary of a git ceiling
// for read-only panels (mirrors restCeilingSummary).
export function gitCeilingSummary(ceiling: GitCeiling | undefined): string {
  const c = ceiling ?? {};
  const bits: string[] = [];
  bits.push(`repos: ${(c.repos_allow ?? []).join(", ") || "none (default-deny)"}`);
  bits.push(c.allow_push ? "push: allowed" : "push: read-only");
  bits.push(c.rate_per_min ? `≤ ${c.rate_per_min}/min` : "no rate limit");
  return bits.join(" · ");
}
