// TG-8 slice D: jsdom behaviour tests for the inbox confirmation card.
// Locks: the inbox_message_id join, the THREE buttons, each button's
// endpoint + body, the +90d expiry default, optimistic disable/badge,
// and error revert.
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";

const env = vi.hoisted(() => ({
  confirmations: [] as unknown[],
  approveOnce: vi.fn(),
  approveStanding: vi.fn(),
  deny: vi.fn(),
}));

vi.mock("../lib/auth", () => ({
  useAuth: () => ({ token: "tok", mode: "token", user: { id: "u1", role: "member" } }),
}));

vi.mock("../lib/grantsApi", () => ({
  listConfirmations: async () => env.confirmations,
  approveConfirmationOnce: (...args: unknown[]) => env.approveOnce(...args),
  approveConfirmationStanding: (...args: unknown[]) => env.approveStanding(...args),
  denyConfirmation: (...args: unknown[]) => env.deny(...args),
}));

import { ConfirmationDecisionCard } from "./ConfirmationDecisionCard";
import { defaultExpiryDateString } from "../lib/grantsWorkflow";

function pendingConf(overrides: Record<string, unknown> = {}) {
  return {
    id: "c1",
    resource_id: "res1",
    agent_id: "ag1",
    tool: "deploy_prod",
    args_hash: "abcdef1234567890",
    state: "pending",
    inbox_message_id: "m1",
    requested_by: "u1",
    created_at: new Date().toISOString(),
    ...overrides,
  };
}

beforeEach(() => {
  env.confirmations = [];
  env.approveOnce = vi.fn().mockResolvedValue({});
  env.approveStanding = vi.fn().mockResolvedValue({ confirmation: {}, standing_grant: {} });
  env.deny = vi.fn().mockResolvedValue({});
});

describe("join + visibility", () => {
  it("renders nothing when no confirmation links to the message", async () => {
    env.confirmations = [pendingConf({ inbox_message_id: "other" })];
    const { container } = render(<ConfirmationDecisionCard messageId="m1" />);
    await waitFor(() => expect(container.firstChild).toBeNull());
  });

  it("renders nothing when the list is empty", async () => {
    const { container } = render(<ConfirmationDecisionCard messageId="m1" />);
    await waitFor(() => expect(container.firstChild).toBeNull());
  });

  it("renders the three decision buttons for a linked pending confirmation", async () => {
    env.confirmations = [pendingConf()];
    render(<ConfirmationDecisionCard messageId="m1" />);
    expect(await screen.findByRole("button", { name: "Deny" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Approve" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Approve This and Future" })).toBeInTheDocument();
    expect(screen.getByText("deploy_prod")).toBeInTheDocument();
    expect(screen.getByText(/abcdef123456…/)).toBeInTheDocument();
  });
});

describe("approve one-shot", () => {
  it("calls approve-once and flips to the decided badge with buttons gone", async () => {
    env.confirmations = [pendingConf()];
    render(<ConfirmationDecisionCard messageId="m1" />);
    const btn = await screen.findByRole("button", { name: "Approve" });
    await userEvent.click(btn);
    await waitFor(() => expect(env.approveOnce).toHaveBeenCalledWith("tok", "c1"));
    expect(await screen.findByText("Approved (one-shot)")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Approve" })).not.toBeInTheDocument();
  });
});

describe("deny with reason", () => {
  it("opens the reason input and posts the reason", async () => {
    env.confirmations = [pendingConf()];
    render(<ConfirmationDecisionCard messageId="m1" />);
    await userEvent.click(await screen.findByRole("button", { name: "Deny" }));
    const input = await screen.findByLabelText("Denial reason");
    await userEvent.type(input, "not while staging is down");
    await userEvent.click(screen.getByRole("button", { name: "Confirm Deny" }));
    await waitFor(() =>
      expect(env.deny).toHaveBeenCalledWith("tok", "c1", "not while staging is down"),
    );
    expect(await screen.findByText("Denied")).toBeInTheDocument();
  });

  it("falls back to 'denied' when the reason is blank", async () => {
    env.confirmations = [pendingConf()];
    render(<ConfirmationDecisionCard messageId="m1" />);
    await userEvent.click(await screen.findByRole("button", { name: "Deny" }));
    await userEvent.click(screen.getByRole("button", { name: "Confirm Deny" }));
    await waitFor(() => expect(env.deny).toHaveBeenCalledWith("tok", "c1", "denied"));
  });
});

describe("approve this and future", () => {
  it("shows an expiry picker defaulted to +90 days and posts the end-of-day ISO", async () => {
    env.confirmations = [pendingConf()];
    render(<ConfirmationDecisionCard messageId="m1" />);
    await userEvent.click(await screen.findByRole("button", { name: "Approve This and Future" }));
    const picker = (await screen.findByLabelText("Standing grant expiry date")) as HTMLInputElement;
    expect(picker.value).toBe(defaultExpiryDateString());
    await userEvent.click(screen.getByRole("button", { name: "Confirm standing approval" }));
    await waitFor(() =>
      expect(env.approveStanding).toHaveBeenCalledWith(
        "tok",
        "c1",
        `${defaultExpiryDateString()}T23:59:59Z`,
      ),
    );
    expect(await screen.findByText("Approved — standing grant")).toBeInTheDocument();
  });

  it("honours a custom expiry date", async () => {
    env.confirmations = [pendingConf()];
    render(<ConfirmationDecisionCard messageId="m1" />);
    await userEvent.click(await screen.findByRole("button", { name: "Approve This and Future" }));
    const picker = (await screen.findByLabelText("Standing grant expiry date")) as HTMLInputElement;
    // jsdom's date input doesn't take typed segments; set the value directly.
    fireEvent.change(picker, { target: { value: "2026-12-24" } });
    await userEvent.click(screen.getByRole("button", { name: "Confirm standing approval" }));
    await waitFor(() =>
      expect(env.approveStanding).toHaveBeenCalledWith("tok", "c1", "2026-12-24T23:59:59Z"),
    );
  });
});

describe("error handling", () => {
  it("reverts the optimistic decision and shows an error toast", async () => {
    env.approveOnce = vi.fn().mockRejectedValue(new Error("409 invalid_transition"));
    env.confirmations = [pendingConf()];
    render(<ConfirmationDecisionCard messageId="m1" />);
    await userEvent.click(await screen.findByRole("button", { name: "Approve" }));
    const toast = await screen.findByRole("alert");
    expect(toast).toHaveTextContent("409 invalid_transition");
    // Buttons come back so the owner can retry.
    expect(screen.getByRole("button", { name: "Approve" })).toBeInTheDocument();
    expect(screen.queryByText("Approved (one-shot)")).not.toBeInTheDocument();
  });

  // TG-8 coverage top-up: defensive branches — non-array list payload
  // and the non-Error rejection fallback in errMessage().
  it("hides the card when the confirmations list is not an array", async () => {
    env.confirmations = "not-an-array" as unknown as unknown[];
    const { container } = render(<ConfirmationDecisionCard messageId="m1" />);
    await waitFor(() => expect(container.firstChild).toBeNull());
  });

  it("falls back to the generic message when the rejection is not an Error", async () => {
    env.approveOnce = vi.fn().mockRejectedValue("boom");
    env.confirmations = [pendingConf()];
    render(<ConfirmationDecisionCard messageId="m1" />);
    await userEvent.click(await screen.findByRole("button", { name: "Approve" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("decision failed");
    expect(screen.getByRole("button", { name: "Approve" })).toBeInTheDocument();
  });
});
