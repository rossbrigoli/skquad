// S-189: jsdom behaviour tests for AgentInboxPanel (previously 0%).
// Covers: error/empty states, section counts, per-status row rendering
// (waiting / retry attempt / delivered / dead reason), consult-timeout
// badge, correlation chip, and the dead-letter replay round-trip
// (success note + refresh, ApiError failure note).
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";

const state = vi.hoisted(() => ({
  snapshot: null as unknown,
  error: "",
  refresh: vi.fn(),
}));

vi.mock("../lib/auth", () => ({
  useAuth: () => ({ token: "tok", mode: "token" }),
}));

vi.mock("../lib/useApi", () => ({
  useApi: () => ({
    data: state.snapshot,
    loading: false,
    error: state.error,
    refresh: state.refresh,
  }),
}));

vi.mock("../lib/api", () => {
  class ApiError extends Error {
    constructor(msg: string) {
      super(msg);
      this.name = "ApiError";
    }
  }
  return { apiPost: vi.fn(), ApiError };
});

import { apiPost } from "../lib/api";
import { AgentInboxPanel } from "./AgentInboxPanel";
import type { AgentInboxSnapshot, InboxMessageRow } from "../lib/inbox";

const mockedPost = vi.mocked(apiPost);

function msg(overrides: Partial<InboxMessageRow> = {}): InboxMessageRow {
  return {
    id: "m-000000001",
    from_type: "agent",
    from_id: "coder-1",
    to_agent_id: "ag-1",
    squad_id: "sq-1",
    type: "task.assign",
    status: "pending",
    attempts: 0,
    max_attempts: 3,
    created_at: "2026-10-01T04:00:00Z",
    ...overrides,
  };
}

function snapshot(overrides: Partial<AgentInboxSnapshot> = {}): AgentInboxSnapshot {
  return {
    pending_count: 0,
    retrying_count: 0,
    delivered_count: 0,
    dead_count: 0,
    pending: [],
    retrying: [],
    delivered: [],
    dead: [],
    ...overrides,
  };
}

beforeEach(() => {
  state.snapshot = null;
  state.error = "";
  state.refresh.mockReset();
  mockedPost.mockReset();
});

describe("AgentInboxPanel states", () => {
  it("shows the unavailable notice on error", () => {
    state.error = "503 upstream";
    render(<AgentInboxPanel agentId="ag-1" />);
    expect(screen.getByText("Inbox unavailable: 503 upstream")).toBeInTheDocument();
  });

  it("shows the empty copy when the snapshot has nothing", () => {
    render(<AgentInboxPanel agentId="ag-1" />);
    expect(
      screen.getByText(/inbox is empty — nothing pending, retrying, delivered, or dead/),
    ).toBeInTheDocument();
  });

  it("renders all four sections with counts and per-status rows", () => {
    state.snapshot = snapshot({
      pending_count: 1,
      oldest_pending_at: "2026-10-01T03:30:00Z",
      retrying_count: 1,
      delivered_count: 1,
      dead_count: 1,
      pending: [msg({ id: "p1", payload: { message: "please do the thing" } })],
      retrying: [
        msg({
          id: "r1",
          status: "pending",
          attempts: 2,
          max_attempts: 3,
          next_retry_at: "2026-10-01T05:00:00Z",
        }),
      ],
      delivered: [msg({ id: "d1", status: "delivered", delivered_at: "2026-10-01T04:30:00Z" })],
      dead: [
        msg({
          id: "x1",
          status: "dead",
          terminal_reason: "consult_timeout",
          correlation_id: "corr-77777777",
        }),
      ],
    });
    render(<AgentInboxPanel agentId="ag-1" />);
    expect(screen.getByRole("heading", { name: /Pending/ })).toHaveTextContent("(1)");
    expect(screen.getByRole("heading", { name: /Retrying/ })).toHaveTextContent("(1)");
    expect(screen.getByRole("heading", { name: /Recently delivered/ })).toHaveTextContent("(1)");
    expect(screen.getByRole("heading", { name: /Dead letters/ })).toHaveTextContent("(1)");
    expect(screen.getByText("please do the thing")).toBeInTheDocument();
    expect(screen.getByText(/attempt 2 of 3/)).toBeInTheDocument();
    expect(screen.getByText("delivered", { exact: true })).toBeInTheDocument();
    expect(screen.getByText(/died · reason: consult_timeout/)).toBeInTheDocument();
    expect(screen.getByText("consult timeout")).toBeInTheDocument();
    expect(screen.getByText(/thread corr-777/)).toBeInTheDocument();
    expect(screen.getByText("Oldest waiting since", { exact: false })).toBeInTheDocument();
  });

  it("shows the placeholder excerpt when there is no message body", () => {
    state.snapshot = snapshot({ pending_count: 1, pending: [msg({ id: "p0" })] });
    render(<AgentInboxPanel agentId="ag-1" />);
    expect(screen.getByText("(no message body)")).toBeInTheDocument();
  });

  it("truncates long message excerpts to 120 chars", () => {
    const long = "x".repeat(200);
    state.snapshot = snapshot({
      pending_count: 1,
      pending: [msg({ id: "pl", payload: { message: long } })],
    });
    render(<AgentInboxPanel agentId="ag-1" />);
    const shown = document.querySelector(".inbox-excerpt")?.textContent ?? "";
    expect(shown.length).toBe(121); // 120 chars + ellipsis
    expect(shown.endsWith("…")).toBe(true);
  });
});

describe("AgentInboxPanel replay", () => {
  it("replays a dead message, notes success and refreshes", async () => {
    state.snapshot = snapshot({
      dead_count: 1,
      dead: [msg({ id: "dead1", status: "dead", terminal_reason: "agent_gone" })],
    });
    mockedPost.mockResolvedValue(msg({ id: "dead1", status: "pending" }));
    const user = userEvent.setup();
    render(<AgentInboxPanel agentId="ag-1" />);
    await user.click(screen.getByRole("button", { name: "Replay" }));
    await waitFor(() =>
      expect(mockedPost).toHaveBeenCalledWith(
        "/agents/ag-1/messages/dead1/replay",
        "tok",
        {},
      ),
    );
    expect(await screen.findByText(/Replayed dead1…? — it is pending again/)).toBeInTheDocument();
    expect(state.refresh).toHaveBeenCalledTimes(1);
  });

  it("notes the ApiError message when replay fails", async () => {
    state.snapshot = snapshot({
      dead_count: 1,
      dead: [msg({ id: "dead2", status: "dead" })],
    });
    const { ApiError } = await import("../lib/api");
    mockedPost.mockRejectedValue(new ApiError("403 forbidden"));
    const user = userEvent.setup();
    render(<AgentInboxPanel agentId="ag-1" />);
    await user.click(screen.getByRole("button", { name: "Replay" }));
    expect(await screen.findByText("Replay failed: 403 forbidden")).toBeInTheDocument();
    expect(state.refresh).not.toHaveBeenCalled();
  });

  it("notes a generic failure for non-ApiError throws", async () => {
    state.snapshot = snapshot({
      dead_count: 1,
      dead: [msg({ id: "dead3", status: "dead" })],
    });
    mockedPost.mockRejectedValue(new TypeError("network"));
    const user = userEvent.setup();
    render(<AgentInboxPanel agentId="ag-1" />);
    await user.click(screen.getByRole("button", { name: "Replay" }));
    expect(await screen.findByText("Replay failed.")).toBeInTheDocument();
  });
});
