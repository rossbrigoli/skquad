// TG-4b (S-244-series): git registration helper tests. Mirrors the
// control-plane contract (validate.go GitConfigKeys/GitCeilingKeys,
// git_credentials_test.go payload shape): base_url only in
// endpoint_config; ceiling {repos_allow (required, non-empty),
// allow_push, rate_per_min?}; write-only bearer token in `auth`.
import { describe, expect, it } from "vitest";

import {
  buildGitResourcePayload,
  emptyGitForm,
  GIT_AUTH_FIELDS,
  gitCeilingSummary,
  validateGitForm,
  type GitResourceForm,
} from "./gitResources";

function filledForm(overrides: Partial<GitResourceForm> = {}): GitResourceForm {
  return {
    ...emptyGitForm(),
    name: "gh-main",
    baseUrl: "https://github.com",
    reposAllow: "acme/*, ross/app",
    secrets: { token: "ghp_fake_TOKEN-do-not-use" },
    ...overrides,
  };
}

describe("emptyGitForm", () => {
  it("starts blank with push disabled", () => {
    const f = emptyGitForm();
    expect(f.name).toBe("");
    expect(f.allowPush).toBe(false);
    expect(f.secrets).toEqual({});
  });
});

describe("GIT_AUTH_FIELDS", () => {
  it("is a single secret bearer-token field", () => {
    expect(GIT_AUTH_FIELDS).toHaveLength(1);
    expect(GIT_AUTH_FIELDS[0].field).toBe("token");
    expect(GIT_AUTH_FIELDS[0].secret).toBe(true);
  });
});

describe("validateGitForm", () => {
  it("accepts a complete form", () => {
    expect(validateGitForm(filledForm())).toBeNull();
  });

  it("requires a name", () => {
    expect(validateGitForm(filledForm({ name: "  " }))).toBe("Name is required");
  });

  it("requires a base URL", () => {
    expect(validateGitForm(filledForm({ baseUrl: "" }))).toBe("Base URL is required");
  });

  it("rejects an unparseable base URL", () => {
    expect(validateGitForm(filledForm({ baseUrl: "not a url" }))).toBe("Base URL must be an absolute http(s) URL");
  });

  it("rejects non-http(s) protocols", () => {
    expect(validateGitForm(filledForm({ baseUrl: "ftp://github.com" }))).toBe("Base URL must be http(s)");
  });

  it("requires at least one repo pattern (default-deny)", () => {
    expect(validateGitForm(filledForm({ reposAllow: " , \n  , " }))).toContain("At least one repo pattern");
  });

  it("requires the bearer token", () => {
    expect(validateGitForm(filledForm({ secrets: { token: "  " } }))).toContain("is required");
  });

  it("rejects a non-positive-integer rate", () => {
    expect(validateGitForm(filledForm({ ratePerMin: "0" }))).toContain("not a positive integer");
    expect(validateGitForm(filledForm({ ratePerMin: "abc" }))).toContain("not a positive integer");
  });

  it("accepts a blank rate (no limit)", () => {
    expect(validateGitForm(filledForm({ ratePerMin: "" }))).toBeNull();
  });

  it("skips the token requirement when requireToken is false (edit without rotation)", () => {
    expect(validateGitForm(filledForm({ secrets: {} }), false)).toBeNull();
  });

  it("still enforces repos_allow when requireToken is false", () => {
    expect(validateGitForm(filledForm({ reposAllow: "", secrets: {} }), false)).toContain("At least one repo pattern");
  });
});

describe("buildGitResourcePayload", () => {
  it("builds the CP create body with trimmed fields and auth", () => {
    const payload = buildGitResourcePayload(
      filledForm({ name: "  gh-main  ", baseUrl: " https://github.com ", reposAllow: "acme/*\nross/app\n", secrets: { token: "  ghp_fake_TOKEN-do-not-use  " } }),
      true,
    );
    expect(payload.name).toBe("gh-main");
    expect(payload.endpoint_config).toEqual({ base_url: "https://github.com" });
    expect(payload.policy_ceiling).toEqual({ repos_allow: ["acme/*", "ross/app"], allow_push: false });
    expect(payload.auth).toEqual({ token: "ghp_fake_TOKEN-do-not-use" });
    expect(payload.risk_tier).toBe("medium");
    expect(payload.egress_class).toBe("public");
  });

  it("omits auth when includeAuth is false (edit without rotation)", () => {
    const payload = buildGitResourcePayload(filledForm(), false);
    expect(payload).not.toHaveProperty("auth");
  });

  it("includes allow_push true when toggled", () => {
    const payload = buildGitResourcePayload(filledForm({ allowPush: true }), true);
    expect((payload.policy_ceiling as { allow_push: boolean }).allow_push).toBe(true);
  });

  it("omits rate_per_min when blank and includes it when set", () => {
    const noRate = buildGitResourcePayload(filledForm(), true).policy_ceiling as Record<string, unknown>;
    expect(noRate).not.toHaveProperty("rate_per_min");
    const withRate = buildGitResourcePayload(filledForm({ ratePerMin: "30" }), true).policy_ceiling as Record<string, unknown>;
    expect(withRate.rate_per_min).toBe(30);
  });
});

describe("gitCeilingSummary", () => {
  it("shows default-deny when no repos are allowed", () => {
    expect(gitCeilingSummary({})).toContain("none (default-deny)");
    expect(gitCeilingSummary(undefined)).toContain("read-only");
    expect(gitCeilingSummary(undefined)).toContain("no rate limit");
  });

  it("renders repos, push and rate", () => {
    const s = gitCeilingSummary({ repos_allow: ["acme/*"], allow_push: true, rate_per_min: 60 });
    expect(s).toContain("acme/*");
    expect(s).toContain("push: allowed");
    expect(s).toContain("≤ 60/min");
  });
});
