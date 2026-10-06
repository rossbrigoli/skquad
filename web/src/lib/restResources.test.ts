// TG-4 (S-250): BYO REST form helpers — payload building, validation,
// and the effective (ceiling ∧ grant) fold mirroring the backend.
import { describe, expect, it } from "vitest";

import {
  buildRestResourcePayload,
  emptyRestForm,
  foldRestConstraints,
  grantConstraintsLabel,
  restCeilingSummary,
  splitList,
  validateRestForm,
  type RestResourceForm,
} from "./restResources";

function formWith(overrides: Partial<RestResourceForm>): RestResourceForm {
  return { ...emptyRestForm(), ...overrides };
}

describe("validateRestForm", () => {
  it("requires name and base URL", () => {
    expect(validateRestForm(formWith({ secrets: { token: "t" } }))).toMatch(/Name/);
    expect(validateRestForm(formWith({ name: "x", secrets: { token: "t" } }))).toMatch(/Base URL/);
  });

  it("rejects non-http schemes", () => {
    const err = validateRestForm(formWith({ name: "x", baseUrl: "ftp://x", secrets: { token: "t" } }));
    expect(err).toMatch(/http/);
  });

  it("requires header_name only for api_key_header", () => {
    expect(validateRestForm(formWith({ name: "x", baseUrl: "https://a.b", authKind: "api_key_header", secrets: { token: "t" } }))).toMatch(/Header name/);
    expect(
      validateRestForm(formWith({ name: "x", baseUrl: "https://a.b", authKind: "bearer", headerName: "X-Ke", secrets: { token: "t" } })),
    ).toMatch(/only applies/);
  });

  it("requires all secret fields per kind", () => {
    expect(
      validateRestForm(formWith({ name: "x", baseUrl: "https://a.b", authKind: "basic", secrets: { username: "u" } })),
    ).toMatch(/Password/);
    expect(
      validateRestForm(
        formWith({
          name: "x",
          baseUrl: "https://a.b",
          authKind: "oauth2_client_credentials",
          secrets: { client_id: "c", client_secret: "s", token_url: "https://a.b/token" },
        }),
      ),
    ).toBeNull();
  });

  it("rejects non-positive numeric bounds", () => {
    expect(
      validateRestForm(formWith({ name: "x", baseUrl: "https://a.b", secrets: { token: "t" }, ratePerMin: "0" })),
    ).toMatch(/positive integer/);
  });
});

describe("buildRestResourcePayload", () => {
  it("builds create payload with auth", () => {
    const payload = buildRestResourcePayload(
      formWith({
        name: " GitHub Issues ",
        baseUrl: " https://api.github.com ",
        authKind: "bearer",
        methods: ["GET", "POST"],
        pathAllow: "/repos/**, /search",
        pathDeny: "/admin/**",
        ratePerMin: "30",
        secrets: { token: " ghp_x " },
      }),
      true,
    );
    expect(payload.name).toBe("GitHub Issues");
    expect(payload.endpoint_config).toEqual({ base_url: "https://api.github.com", auth_kind: "bearer" });
    expect(payload.policy_ceiling).toEqual({
      egress_class: "public",
      methods: ["GET", "POST"],
      path_allow: ["/repos/**", "/search"],
      path_deny: ["/admin/**"],
      rate_per_min: 30,
    });
    expect(payload.auth).toEqual({ token: "ghp_x" });
  });

  it("omits auth on edit when not rotating", () => {
    const payload = buildRestResourcePayload(formWith({ name: "x", baseUrl: "https://a.b", secrets: { token: "t" } }), false);
    expect(payload).not.toHaveProperty("auth");
  });

  it("includes header_name for api_key_header", () => {
    const payload = buildRestResourcePayload(
      formWith({ name: "x", baseUrl: "https://a.b", authKind: "api_key_header", headerName: "X-Api-Key", secrets: { token: "k" } }),
      true,
    );
    expect((payload.endpoint_config as Record<string, unknown>).header_name).toBe("X-Api-Key");
  });
});

describe("foldRestConstraints", () => {
  it("intersects methods, grant path_allow wins, deny unions, numerics min", () => {
    const eff = foldRestConstraints(
      { methods: ["GET", "POST", "PUT"], path_allow: ["/a/**", "/b/*"], path_deny: ["/admin/**"], max_request_bytes: 8192, rate_per_min: 60 },
      { methods: ["get", "post"], path_allow: ["/a/**"], path_deny: ["/a/secret*"], max_request_bytes: 4096, rate_per_min: 30 },
    );
    expect(eff.methods).toEqual(["GET", "POST"]);
    expect(eff.path_allow).toEqual(["/a/**"]);
    expect(eff.path_deny).toEqual(["/admin/**", "/a/secret*"]);
    expect(eff.max_request_bytes).toBe(4096);
    expect(eff.max_response_bytes).toBe(262144);
    expect(eff.rate_per_min).toBe(30);
  });

  it("falls back to ceiling when grant unset", () => {
    const eff = foldRestConstraints({ methods: ["GET"], path_allow: ["/x"], rate_per_min: 90 }, undefined);
    expect(eff.methods).toEqual(["GET"]);
    expect(eff.path_allow).toEqual(["/x"]);
    expect(eff.rate_per_min).toBe(90);
  });

  it("default-deny methods when ceiling unset", () => {
    const eff = foldRestConstraints({}, { methods: ["GET"] });
    expect(eff.methods).toEqual([]);
  });

  it("egress internal only when both layers internal", () => {
    expect(foldRestConstraints({ egress_class: "internal" }, { egress_class: "internal" }).egress_class).toBe("internal");
    expect(foldRestConstraints({ egress_class: "internal" }, { egress_class: "public" }).egress_class).toBe("public");
  });
});

describe("restCeilingSummary / splitList", () => {
  it("summarizes a ceiling", () => {
    const s = restCeilingSummary({ methods: ["GET"], path_allow: ["/issues/**"], rate_per_min: 60 });
    expect(s).toContain("GET");
    expect(s).toContain("/issues/**");
    expect(s).toContain("60/min");
  });

  it("flags default-deny methods", () => {
    expect(restCeilingSummary({})).toContain("default-deny");
  });

  it("splitList trims and drops empties", () => {
    expect(splitList(" a ,, b\nc ")).toEqual(["a", "b", "c"]);
  });

  it("grantConstraintsLabel summarizes narrowing, empty when none", () => {
    expect(grantConstraintsLabel({ methods: ["GET"], rate_per_min: 10 })).toBe("methods: GET · ≤ 10/min");
    expect(grantConstraintsLabel({})).toBe("");
    expect(grantConstraintsLabel(undefined)).toBe("");
    expect(grantConstraintsLabel({ path_allow: ["/issues/**"] })).toBe("allow: /issues/**");
  });
});

// TG-4c coverage: edge branches in the REST helpers (defensive paths the
// happy-path suite above does not reach).
describe("restResources edge branches", () => {
  function validForm(overrides: Partial<RestResourceForm> = {}): RestResourceForm {
    return formWith({
      name: "GitHub API",
      baseUrl: "https://api.github.com",
      authKind: "bearer",
      secrets: { token: "fake-token-DO-NOT-USE" },
      ...overrides,
    });
  }

  it("validateRestForm surfaces numeric-bound messages", () => {
    expect(validateRestForm(validForm({ maxRequestBytes: "abc" }))).toContain("is not a positive integer");
    expect(validateRestForm(validForm({ ratePerMin: "0" }))).toContain("is not a positive integer");
    expect(validateRestForm(validForm({ maxResponseBytes: "1.5" }))).toContain("is not a positive integer");
  });

  it("validateRestForm rejects unparseable and non-http base URLs", () => {
    expect(validateRestForm(validForm({ baseUrl: "not a url" }))).toBe("Base URL must be an absolute http(s) URL");
    expect(validateRestForm(validForm({ baseUrl: "ftp://example.com" }))).toBe("Base URL must be http(s)");
  });

  it("buildRestResourcePayload omits unset bounds and empty lists", () => {
    const f = validForm({
      authKind: "none",
      secrets: {},
      methods: [],
      pathAllow: "",
      pathDeny: "",
      maxRequestBytes: "",
      maxResponseBytes: "",
      ratePerMin: "",
    });
    const p = buildRestResourcePayload(f, false) as { policy_ceiling: Record<string, unknown>; auth?: unknown };
    expect(p.policy_ceiling).toEqual({ egress_class: f.egressClass });
    expect(p.auth).toBeUndefined();
  });

  it("buildRestResourcePayload skips empty secret fields when includeAuth is true", () => {
    const f = validForm({ authKind: "basic", secrets: { username: "u", password: "" } });
    const p = buildRestResourcePayload(f, true) as { auth: Record<string, string> };
    expect(p.auth).toEqual({ username: "u" });
  });

  it("foldRestConstraints tolerates undefined ceiling and grant", () => {
    const e = foldRestConstraints(undefined, undefined);
    expect(e.methods).toEqual([]);
    expect(e.path_allow).toEqual([]);
    expect(e.path_deny).toEqual([]);
    expect(e.max_request_bytes).toBe(65536);
    expect(e.max_response_bytes).toBe(262144);
    expect(e.rate_per_min).toBe(0);
    expect(e.egress_class).toBe("public");
  });

  it("foldRestConstraints uppercases before intersecting methods", () => {
    const e = foldRestConstraints({ methods: ["get", "post"] }, { methods: ["GET"] });
    expect(e.methods).toEqual(["GET"]);
  });

  it("grantConstraintsLabel renders deny lists and byte caps", () => {
    expect(grantConstraintsLabel({ path_deny: ["/admin/**"] })).toBe("deny: /admin/**");
    expect(grantConstraintsLabel({ max_request_bytes: 2048, max_response_bytes: 5120 })).toBe(
      "req ≤ 2 KiB · resp ≤ 5 KiB",
    );
  });

  it("grantConstraintsLabel tolerates non-object inputs", () => {
    expect(grantConstraintsLabel(null)).toBe("");
    expect(grantConstraintsLabel([1, 2])).toBe("");
    expect(grantConstraintsLabel("nope")).toBe("");
  });

  it("restCeilingSummary handles undefined and full ceilings", () => {
    const empty = restCeilingSummary(undefined);
    expect(empty).toContain("none (default-deny)");
    expect(empty).toContain("no rate limit");
    expect(empty).toContain("egress: public");

    const full = restCeilingSummary({
      methods: ["GET"],
      path_allow: ["/issues/**"],
      path_deny: ["/admin/**"],
      max_request_bytes: 2048,
      max_response_bytes: 4096,
      rate_per_min: 5,
      egress_class: "internal",
    });
    expect(full).toContain("deny: /admin/**");
    expect(full).toContain("req ≤ 2 KiB");
    expect(full).toContain("resp ≤ 4 KiB");
    expect(full).toContain("≤ 5/min");
    expect(full).toContain("egress: internal");
  });
});
