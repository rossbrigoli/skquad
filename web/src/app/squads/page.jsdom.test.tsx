// S-189: jsdom behaviour tests for the Squads list page. Covers the
// admin vs member data paths (/squads?all=true + /users vs /squads),
// owner labels, empty/error states, and the New-squad modal flow
// (disabled submit, trimmed payload, API error surfacing, refresh on
// success).
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";

const env = vi.hoisted(() => ({
  user: null as null | { id: string; name?: string; role?: string },
  squads: [] as unknown[] | null,
  users: [] as unknown[] | null,
  error: "",
  apiPost: vi.fn(),
  apiPaths: [] as string[],
  refresh: vi.fn(),
}));

vi.mock("next/link", () => ({
  default: ({ href, children }: { href: string; children: React.ReactNode }) => (
    <a href={href}>{children}</a>
  ),
}));

vi.mock("../../components/AuthGate", () => ({
  AuthGate: ({ children }: { children: React.ReactNode }) => <>{children}</>,
}));

vi.mock("../../components/AppShell", () => ({
  AppShell: ({ children }: { children: React.ReactNode }) => <>{children}</>,
}));

vi.mock("../../components/PromptTemplatePicker", () => ({
  PromptTemplatePicker: ({ onApply }: { onApply: (content: string) => void }) => (
    <button type="button" onClick={() => onApply("template content")}>
      apply-template
    </button>
  ),
}));

vi.mock("../../lib/auth", () => ({
  useAuth: () => ({ user: env.user, token: "tok", mode: "token", logout: vi.fn() }),
}));

vi.mock("../../lib/api", () => ({
  apiPost: (...args: unknown[]) => env.apiPost(...args),
}));

vi.mock("../../lib/useApi", () => ({
  useApi: (path: string) => {
    env.apiPaths.push(path);
    let data: unknown = null;
    if (path === "/squads" || path === "/squads?all=true") data = env.squads;
    if (path === "/users") data = env.users;
    return { data, loading: false, error: env.error, refresh: env.refresh };
  },
}));

import SquadsPage from "./page";

beforeEach(() => {
  env.user = { id: "u1", name: "Ross" };
  env.squads = [];
  env.users = [];
  env.error = "";
  env.apiPost = vi.fn().mockResolvedValue({ id: "sq9" });
  env.apiPaths = [];
  env.refresh = vi.fn();
});

describe("SquadsPage list states", () => {
  it("member path fetches /squads and renders rows with mission + namespace", () => {
    env.squads = [
      { id: "sq1", name: "Alpha", mission: "ship it", namespace: "squad-alpha" },
      { id: "sq2", name: "Beta" },
    ];
    render(<SquadsPage />);
    expect(env.apiPaths).toContain("/squads");
    expect(env.apiPaths).not.toContain("/squads?all=true");
    const alpha = screen.getByRole("link", { name: /Alpha/ });
    expect(alpha).toHaveAttribute("href", "/squads/sq1");
    expect(alpha.textContent).toContain("ship it");
    expect(screen.getByRole("link", { name: /Beta/ }).textContent).toContain("no mission set");
    expect(screen.getByText("squad-alpha")).toBeInTheDocument();
  });

  it("admin path fetches /squads?all=true and labels owners", () => {
    env.user = { id: "u1", name: "Ross", role: "platform_admin" };
    env.squads = [
      { id: "sq1", name: "Alpha", owner_id: "u2" },
      { id: "sq2", name: "Beta", owner_id: "u1" },
      { id: "sq3", name: "Gamma", owner_id: "u3" },
      { id: "sq4", name: "Delta", owner_id: "u9" },
    ];
    env.users = [
      { id: "u2", name: "Ada" },
      { id: "u1", name: "Ross" },
      { id: "u3", email: "bob@example.com" },
    ];
    render(<SquadsPage />);
    expect(env.apiPaths).toContain("/squads?all=true");
    expect(env.apiPaths).toContain("/users");
    expect(screen.getByRole("link", { name: /Alpha/ }).textContent).toContain("Ada");
    // Own squads are labelled "me" rather than by name.
    expect(screen.getByRole("link", { name: /Beta/ }).textContent).toContain("me");
    // Owner without a name falls back to the email prefix.
    expect(screen.getByRole("link", { name: /Gamma/ }).textContent).toContain("bob");
    // Unknown owner falls back to the raw id.
    expect(screen.getByRole("link", { name: /Delta/ }).textContent).toContain("u9");
  });

  it("shows the empty state when there are no squads", async () => {
    const user = userEvent.setup();
    render(<SquadsPage />);
    expect(screen.getByText("No squads yet")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "+ Create your first squad" }));
    expect(screen.getByRole("dialog", { name: /New squad/ })).toBeInTheDocument();
  });

  it("surfaces the API error notice", () => {
    env.error = "boom";
    render(<SquadsPage />);
    expect(screen.getByText("boom")).toBeInTheDocument();
  });
});

describe("SquadsPage create modal", () => {
  it("opens from the header button, disables submit until a name is set, posts trimmed fields", async () => {
    const user = userEvent.setup();
    render(<SquadsPage />);
    await user.click(screen.getByRole("button", { name: "+ New squad" }));
    const dialog = screen.getByRole("dialog", { name: /New squad/ });
    const submit = within(dialog).getByRole("button", { name: "Create squad" });
    expect(submit).toBeDisabled();

    await user.type(within(dialog).getByLabelText(/Name/), "  release-team  ");
    await user.type(within(dialog).getByLabelText(/Mission/), "ship releases");
    expect(submit).toBeEnabled();

    await user.click(submit);
    await vi.waitFor(() =>
      expect(env.apiPost).toHaveBeenCalledWith("/squads", "tok", {
        name: "release-team",
        mission: "ship releases",
        prompt: "",
      }),
    );
    // onCreated closes the modal and refreshes the list.
    await vi.waitFor(() => expect(env.refresh).toHaveBeenCalled());
    expect(screen.queryByRole("dialog", { name: /New squad/ })).not.toBeInTheDocument();
  });

  it("applies a template into the context field", async () => {
    const user = userEvent.setup();
    render(<SquadsPage />);
    await user.click(screen.getByRole("button", { name: "+ New squad" }));
    await user.click(screen.getByRole("button", { name: "apply-template" }));
    const dialog = screen.getByRole("dialog", { name: /New squad/ });
    expect(within(dialog).getByLabelText(/Squad context/)).toHaveValue("template content");
    // Manual edits after the template still work (onChange branch).
    await user.type(within(dialog).getByLabelText(/Squad context/), " + extra");
    expect(within(dialog).getByLabelText(/Squad context/)).toHaveValue("template content + extra");
  });

  it("closes via the modal close button without creating", async () => {
    const user = userEvent.setup();
    render(<SquadsPage />);
    await user.click(screen.getByRole("button", { name: "+ New squad" }));
    await user.click(screen.getByRole("button", { name: "Close" }));
    expect(screen.queryByRole("dialog", { name: /New squad/ })).not.toBeInTheDocument();
    expect(env.apiPost).not.toHaveBeenCalled();
  });

  it("shows the API error inside the modal and stays open", async () => {
    env.apiPost = vi.fn().mockRejectedValue(new Error("duplicate name"));
    const user = userEvent.setup();
    render(<SquadsPage />);
    await user.click(screen.getByRole("button", { name: "+ New squad" }));
    const dialog = screen.getByRole("dialog", { name: /New squad/ });
    await user.type(within(dialog).getByLabelText(/Name/), "dup");
    await user.click(within(dialog).getByRole("button", { name: "Create squad" }));
    await vi.waitFor(() =>
      expect(within(dialog).getByText("duplicate name")).toBeInTheDocument(),
    );
    expect(screen.getByRole("dialog", { name: /New squad/ })).toBeInTheDocument();
  });

  it("non-Error rejection falls back to the generic create-failed message", async () => {
    env.apiPost = vi.fn().mockRejectedValue("weird failure");
    const user = userEvent.setup();
    render(<SquadsPage />);
    await user.click(screen.getByRole("button", { name: "+ New squad" }));
    const dialog = screen.getByRole("dialog", { name: /New squad/ });
    await user.type(within(dialog).getByLabelText(/Name/), "x");
    await user.click(within(dialog).getByRole("button", { name: "Create squad" }));
    await vi.waitFor(() =>
      expect(within(dialog).getByText("create failed")).toBeInTheDocument(),
    );
  });
});

describe("SquadsPage fallback paths", () => {
  it("admin with no user directory still lists squads by owner id", () => {
    env.user = { id: "u1", role: "platform_admin" };
    env.squads = [{ id: "sq1", name: "Alpha", owner_id: "u2" }];
    env.users = null;
    render(<SquadsPage />);
    expect(screen.getByRole("link", { name: /Alpha/ }).textContent).toContain("u2");
  });

  it("null squads payload renders the empty state", () => {
    env.squads = null;
    render(<SquadsPage />);
    expect(screen.getByText("No squads yet")).toBeInTheDocument();
  });
});
