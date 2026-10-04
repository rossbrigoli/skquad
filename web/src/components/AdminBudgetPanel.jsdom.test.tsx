// S-189: jsdom behaviour tests for the admin Budget tab. Covers panel
// loading/error states, the platform knob form (no-op save, invalid
// input, clamped-save notice) and the per-user row editor (dirty-gated
// Set button, platform-max rejection, successful PUT).
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";

const env = vi.hoisted(() => ({
  payload: null as unknown,
  getError: null as unknown,
  apiPut: vi.fn(),
}));

vi.mock("../lib/api", async (importOriginal) => {
  const mod = await importOriginal<typeof import("../lib/api")>();
  return {
    ...mod,
    apiGet: vi.fn(async () => {
      if (env.getError) throw env.getError;
      return env.payload;
    }),
    apiPut: (...args: unknown[]) => env.apiPut(...args),
  };
});

import { AdminBudgetPanel } from "./AdminBudgetPanel";

const basePayload = {
  platform: {
    default_monthly_usd: 10,
    max_usd: 100,
    platform_monthly_limit_usd: null,
    platform_mtd_cost: 42.5,
  },
  users: [
    {
      user_id: "u1",
      name: "Ada",
      email: "ada@example.com",
      monthly_budget_usd: 20,
      mtd_cost: 5,
      remaining_usd: 15,
      over_budget: false,
    },
    {
      user_id: "u2",
      email: "bob@example.com",
      monthly_budget_usd: 10,
      mtd_cost: 14,
      remaining_usd: -4,
      over_budget: true,
    },
    {
      user_id: "u3",
      monthly_budget_usd: null,
      mtd_cost: 1,
      remaining_usd: null,
      over_budget: false,
    },
  ],
};

beforeEach(() => {
  env.payload = basePayload;
  env.getError = null;
  env.apiPut = vi.fn().mockResolvedValue({});
});

describe("AdminBudgetPanel load states", () => {
  it("shows the loading placeholder before the payload lands", () => {
    render(<AdminBudgetPanel token="tok" />);
    expect(screen.getByText("Loading budgets…")).toBeInTheDocument();
  });

  it("renders the platform form and user rows once loaded", async () => {
    render(<AdminBudgetPanel token="tok" />);
    expect(await screen.findByLabelText(/Default monthly budget/)).toHaveValue("10");
    expect(screen.getByLabelText(/Global maximum budget/)).toHaveValue("100");
    expect(screen.getByLabelText(/Platform-wide monthly limit/)).toHaveValue("");
    expect(screen.getByRole("heading", { name: /User budgets/ })).toHaveTextContent("platform MTD");
    expect(screen.getByText("Ada")).toBeInTheDocument();
  });

  it("shows the error notice when loading fails with no data", async () => {
    env.getError = new Error("403 forbidden");
    render(<AdminBudgetPanel token="tok" />);
    expect(await screen.findByText("403 forbidden")).toBeInTheDocument();
  });
});

describe("PlatformBudgetForm", () => {
  it("reports a no-op save without calling the API", async () => {
    render(<AdminBudgetPanel token="tok" />);
    const form = await screen.findByRole("region", { name: "Platform budget settings" });
    await userEvent.click(within(form).getByRole("button", { name: "Save platform budgets" }));
    expect(within(form).getByText("Nothing to save — no knob changed.")).toBeInTheDocument();
    expect(env.apiPut).not.toHaveBeenCalled();
  });

  it("rejects invalid knob input client-side", async () => {
    render(<AdminBudgetPanel token="tok" />);
    const form = await screen.findByRole("region", { name: "Platform budget settings" });
    await userEvent.clear(within(form).getByLabelText(/Default monthly budget/));
    await userEvent.type(within(form).getByLabelText(/Default monthly budget/), "abc");
    await userEvent.click(within(form).getByRole("button", { name: "Save platform budgets" }));
    expect(within(form).getByText(/Invalid value: must be a number/)).toBeInTheDocument();
    expect(env.apiPut).not.toHaveBeenCalled();
  });

  it("saves changed knobs and reports clamped user budgets", async () => {
    env.apiPut = vi.fn().mockResolvedValue({ clamped_user_budgets: 3 });
    render(<AdminBudgetPanel token="tok" />);
    const form = await screen.findByRole("region", { name: "Platform budget settings" });
    await userEvent.clear(within(form).getByLabelText(/Global maximum budget/));
    await userEvent.type(within(form).getByLabelText(/Global maximum budget/), "50");
    await userEvent.click(within(form).getByRole("button", { name: "Save platform budgets" }));
    await vi.waitFor(() =>
      expect(env.apiPut).toHaveBeenCalledWith("/admin/budgets/platform", "tok", { max_usd: 50 }),
    );
    expect(within(form).getByText("Saved. 3 user budgets were clamped down to the new max.")).toBeInTheDocument();
  });

  it("surfaces a save failure", async () => {
    env.apiPut = vi.fn().mockRejectedValue(new Error("server exploded"));
    render(<AdminBudgetPanel token="tok" />);
    const form = await screen.findByRole("region", { name: "Platform budget settings" });
    await userEvent.clear(within(form).getByLabelText(/Global maximum budget/));
    await userEvent.type(within(form).getByLabelText(/Global maximum budget/), "50");
    await userEvent.click(within(form).getByRole("button", { name: "Save platform budgets" }));
    expect(await within(form).findByText("server exploded")).toBeInTheDocument();
  });
});

describe("User budget rows", () => {
  it("labels rows via name → email → id and shows limit state", async () => {
    render(<AdminBudgetPanel token="tok" />);
    const list = await screen.findByRole("region", { name: "User budgets" });
    expect(within(list).getByText("ada@example.com")).toBeInTheDocument();
    // Over-budget user gets the "Over by" label.
    expect(within(list).getByText(/Over by/)).toBeInTheDocument();
    // No-limit user row.
    expect(within(list).getAllByText("No limit").length).toBe(1);
    // No name → email is the label.
    expect(within(list).getByText("bob@example.com")).toBeInTheDocument();
  });

  it("Set stays disabled until the value is dirty, then PUTs the new budget", async () => {
    const user = userEvent.setup();
    render(<AdminBudgetPanel token="tok" />);
    const input = await screen.findByLabelText("Monthly budget for Ada");
    const row = input.closest(".budget-user-row") as HTMLElement;
    expect(within(row).getByRole("button", { name: "Set" })).toBeDisabled();
    await user.clear(input);
    await user.type(input, "33");
    expect(within(row).getByRole("button", { name: "Set" })).toBeEnabled();
    await user.click(within(row).getByRole("button", { name: "Set" }));
    await vi.waitFor(() =>
      expect(env.apiPut).toHaveBeenCalledWith("/admin/budgets/users/u1", "tok", {
        monthly_budget_usd: 33,
      }),
    );
  });

  it("rejects values above the platform max without a request", async () => {
    const user = userEvent.setup();
    render(<AdminBudgetPanel token="tok" />);
    const input = await screen.findByLabelText("Monthly budget for Ada");
    const row = input.closest(".budget-user-row") as HTMLElement;
    await user.clear(input);
    await user.type(input, "500");
    await user.click(within(row).getByRole("button", { name: "Set" }));
    expect(within(row).getByText(/Exceeds the platform max/)).toBeInTheDocument();
    expect(env.apiPut).not.toHaveBeenCalled();
  });

  it("rejects clearing a per-user budget", async () => {
    const user = userEvent.setup();
    render(<AdminBudgetPanel token="tok" />);
    const input = await screen.findByLabelText("Monthly budget for Ada");
    const row = input.closest(".budget-user-row") as HTMLElement;
    await user.clear(input);
    await user.click(within(row).getByRole("button", { name: "Set" }));
    expect(within(row).getByText(/must be a number/)).toBeInTheDocument();
    expect(env.apiPut).not.toHaveBeenCalled();
  });
});
