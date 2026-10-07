// TG-8 slice D: API-client tests — every wrapper must hit the exact
// control-plane endpoint (verified against grant_requests.go /
// confirmations.go) with the right method and body.
import { beforeEach, describe, expect, it, vi } from "vitest";

const calls = vi.hoisted(() => ({
  get: [] as [string, string][],
  post: [] as [string, string, unknown][],
  del: [] as [string, string][],
}));

vi.mock("./api", () => ({
  apiGet: async (path: string, token: string) => {
    calls.get.push([path, token]);
    return [];
  },
  apiPost: async (path: string, token: string, body: unknown) => {
    calls.post.push([path, token, body]);
    return {};
  },
  apiDelete: async (path: string, token: string) => {
    calls.del.push([path, token]);
  },
}));

import {
  approveConfirmationOnce,
  approveConfirmationStanding,
  approveGrantAdmin,
  approveGrantOwner,
  denyConfirmation,
  denyGrantRequest,
  listConfirmations,
  listGrantRequests,
  listStandingGrants,
  revokeStandingGrant,
} from "./grantsApi";

beforeEach(() => {
  calls.get = [];
  calls.post = [];
  calls.del = [];
});

describe("grant-request client (slice B endpoints)", () => {
  it("lists owner-scoped requests", async () => {
    await listGrantRequests("tok", { mine: "owner" });
    expect(calls.get).toEqual([["/grant-requests?mine=owner", "tok"]]);
  });
  it("lists admin queue with a state filter", async () => {
    await listGrantRequests("tok", { mine: "admin", state: "pending_admin" });
    expect(calls.get[0][0]).toBe("/grant-requests?mine=admin&state=pending_admin");
  });
  it("lists without filters when none given", async () => {
    await listGrantRequests("tok");
    expect(calls.get[0][0]).toBe("/grant-requests");
  });
  it("approve-owner POSTs to /approve-owner with an empty body", async () => {
    await approveGrantOwner("tok", "gr1");
    expect(calls.post).toEqual([["/grant-requests/gr1/approve-owner", "tok", {}]]);
  });
  it("approve-admin POSTs to /approve-admin", async () => {
    await approveGrantAdmin("tok", "gr2");
    expect(calls.post).toEqual([["/grant-requests/gr2/approve-admin", "tok", {}]]);
  });
  it("deny POSTs the reason", async () => {
    await denyGrantRequest("tok", "gr3", "not today");
    expect(calls.post).toEqual([["/grant-requests/gr3/deny", "tok", { reason: "not today" }]]);
  });
});

describe("confirmation client (slice C1 endpoints)", () => {
  it("lists my pending confirmations", async () => {
    await listConfirmations("tok", { mine: true, state: "pending" });
    expect(calls.get).toEqual([["/confirmations?mine=true&state=pending", "tok"]]);
  });
  // TG-8 coverage top-up: the falsy side of the filter flags — no query
  // string must be appended when nothing is set (or when mine is false).
  it("lists all confirmations with no query string when no filters are given", async () => {
    await listConfirmations("tok");
    expect(calls.get).toEqual([["/confirmations", "tok"]]);
  });
  it("ignores a falsey mine flag and empty state", async () => {
    await listConfirmations("tok", { mine: false, state: "" });
    expect(calls.get[0][0]).toBe("/confirmations");
  });
  it("approve-once hits /approve-once", async () => {
    await approveConfirmationOnce("tok", "c1");
    expect(calls.post).toEqual([["/confirmations/c1/approve-once", "tok", {}]]);
  });
  it("approve-standing sends the expiry when provided", async () => {
    await approveConfirmationStanding("tok", "c2", "2027-01-05T23:59:59Z");
    expect(calls.post).toEqual([
      ["/confirmations/c2/approve-standing", "tok", { expiry: "2027-01-05T23:59:59Z" }],
    ]);
  });
  it("approve-standing sends an empty body when the expiry is omitted (backend default applies)", async () => {
    await approveConfirmationStanding("tok", "c3");
    expect(calls.post).toEqual([["/confirmations/c3/approve-standing", "tok", {}]]);
  });
  it("deny sends the reason", async () => {
    await denyConfirmation("tok", "c4", "too risky");
    expect(calls.post).toEqual([["/confirmations/c4/deny", "tok", { reason: "too risky" }]]);
  });
});

describe("standing-grant client", () => {
  it("lists grants", async () => {
    await listStandingGrants("tok");
    expect(calls.get).toEqual([["/standing-grants", "tok"]]);
  });
  it("revokes via DELETE", async () => {
    await revokeStandingGrant("tok", "sg9");
    expect(calls.del).toEqual([["/standing-grants/sg9", "tok"]]);
  });
});
