// TG-8 slice D: pure-logic tests for the approval-workflow helpers.
// Pins the client contract against the control-plane JSON shapes
// (slice B GrantRequest / slice C1 PendingConfirmation + StandingGrant).
import { describe, expect, it } from "vitest";
import {
  confirmationDecisionChip,
  confirmationDecisionLabel,
  countFindingsBySeverity,
  dateInputToIso,
  defaultExpiryDateString,
  findingSeverityClass,
  grantRequestStateMeta,
  matchConfirmationForInboxMessage,
  shortArgsHash,
  standingGrantStatus,
  summarizeScope,
  STANDING_GRANT_DEFAULT_DAYS,
  type PendingConfirmation,
  type StandingGrant,
} from "./grantsWorkflow";

const NOW = new Date("2026-10-07T01:00:00Z");

function grant(overrides: Partial<StandingGrant> = {}): StandingGrant {
  return {
    id: "sg1",
    resource_id: "res1",
    agent_id: "ag1",
    tool: "deploy",
    expires_at: "2027-01-05T23:59:59Z",
    created_by: "u1",
    created_at: "2026-10-07T00:00:00Z",
    ...overrides,
  };
}

function conf(overrides: Partial<PendingConfirmation> = {}): PendingConfirmation {
  return {
    id: "c1",
    resource_id: "res1",
    agent_id: "ag1",
    tool: "deploy",
    args_hash: "abcdef1234567890",
    state: "pending",
    requested_by: "u1",
    created_at: "2026-10-07T00:00:00Z",
    ...overrides,
  };
}

describe("defaultExpiryDateString", () => {
  it("defaults the standing-grant picker to +90 days (spec §C)", () => {
    expect(STANDING_GRANT_DEFAULT_DAYS).toBe(90);
    expect(defaultExpiryDateString(NOW)).toBe("2027-01-05");
  });
});

describe("dateInputToIso", () => {
  it("converts a picker date to end-of-day UTC RFC3339", () => {
    expect(dateInputToIso("2027-01-05")).toBe("2027-01-05T23:59:59Z");
  });
  it("rejects malformed values with the empty string", () => {
    expect(dateInputToIso("")).toBe("");
    expect(dateInputToIso("2027-1-5")).toBe("");
    expect(dateInputToIso("yesterday")).toBe("");
  });
});

describe("standingGrantStatus", () => {
  it("live while unrevoked and unexpired", () => {
    expect(standingGrantStatus(grant(), NOW)).toBe("live");
  });
  it("expired once expires_at has passed", () => {
    expect(standingGrantStatus(grant({ expires_at: "2026-10-06T00:00:00Z" }), NOW)).toBe("expired");
  });
  it("revoked wins over expired (audit distinction)", () => {
    expect(
      standingGrantStatus(
        grant({ revoked_at: "2026-10-06T12:00:00Z", expires_at: "2026-10-06T00:00:00Z" }),
        NOW,
      ),
    ).toBe("revoked");
  });
});

describe("confirmation decision helpers", () => {
  it("labels every terminal state", () => {
    expect(confirmationDecisionLabel("approved_once")).toBe("Approved (one-shot)");
    expect(confirmationDecisionLabel("approved_standing")).toBe("Approved — standing grant");
    expect(confirmationDecisionLabel("denied")).toBe("Denied");
    expect(confirmationDecisionLabel("expired")).toBe("Expired");
    expect(confirmationDecisionLabel("pending")).toBe("");
  });
  it("maps decisions to chip variants", () => {
    expect(confirmationDecisionChip("approved_standing")).toContain("chip-done");
    expect(confirmationDecisionChip("denied")).toContain("chip-blocked");
    expect(confirmationDecisionChip("expired")).toContain("chip-paused");
  });
});

describe("grantRequestStateMeta", () => {
  it("covers the slice-B state machine", () => {
    expect(grantRequestStateMeta("pending_owner").label).toBe("awaiting owner");
    expect(grantRequestStateMeta("pending_admin").label).toBe("awaiting admin");
    expect(grantRequestStateMeta("approved").className).toContain("chip-done");
    expect(grantRequestStateMeta("denied").className).toContain("chip-blocked");
  });
});

describe("findings helpers", () => {
  it("counts severities for the row summary", () => {
    expect(
      countFindingsBySeverity([
        { code: "metadata_path", severity: "block", detail: "x" },
        { code: "new_http_method", severity: "warn", detail: "y" },
        { code: "ceiling_widened", severity: "warn", detail: "z" },
      ]),
    ).toEqual({ block: 1, warn: 2 });
  });
  it("tolerates null/undefined findings", () => {
    expect(countFindingsBySeverity(null)).toEqual({ block: 0, warn: 0 });
    expect(countFindingsBySeverity(undefined)).toEqual({ block: 0, warn: 0 });
  });
  it("block renders red, everything else amber", () => {
    expect(findingSeverityClass("block")).toContain("finding-block");
    expect(findingSeverityClass("warn")).toContain("finding-warn");
  });
});

describe("matchConfirmationForInboxMessage", () => {
  it("joins via the confirmation's inbox_message_id (the inbox payload has none)", () => {
    const list = [conf({ id: "c1", inbox_message_id: "m1" }), conf({ id: "c2", inbox_message_id: "m2" })];
    expect(matchConfirmationForInboxMessage(list, "m2")?.id).toBe("c2");
  });
  it("returns null when nothing links to the message", () => {
    expect(matchConfirmationForInboxMessage([conf()], "mX")).toBeNull();
    expect(matchConfirmationForInboxMessage(null, "m1")).toBeNull();
  });
});

describe("shortArgsHash", () => {
  it("truncates like the backend display (12 chars + …)", () => {
    expect(shortArgsHash("abcdef1234567890")).toBe("abcdef123456…");
    expect(shortArgsHash("short")).toBe("short");
  });
});

describe("summarizeScope", () => {
  it("renders the requested-scope fields the linter snapshot knows", () => {
    const rows = summarizeScope({
      hosts: ["api.example.com", "db.internal"],
      http_methods: ["GET", "POST"],
      mcp_tools: ["search"],
      has_credential: true,
      numeric_caps: { max_pages: 10, rate: 60 },
    });
    expect(rows).toEqual([
      { label: "Hosts", value: "api.example.com, db.internal" },
      { label: "HTTP methods", value: "GET, POST" },
      { label: "MCP tools", value: "search" },
      { label: "Credential", value: "yes" },
      { label: "Numeric caps", value: "max_pages=10, rate=60" },
    ]);
  });
  it("skips empty/absent fields and degrades on junk", () => {
    expect(summarizeScope({ hosts: [] })).toEqual([]);
    expect(summarizeScope(null)).toEqual([]);
    expect(summarizeScope("junk")).toEqual([]);
  });
});
