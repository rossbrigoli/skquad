// S-189: jsdom behaviour tests for the Settings page (admin + member
// surfaces). Covers tab resolution/dispatch, the merged AI Models
// hierarchy (groups, orphans, deprecate cascade, delete-with-conflict),
// provider create/edit modal with pre-save connection test, AI model
// register modal (provider model list, metadata prefill, duplicate
// error), and the Access tab (role promote/demote with last-admin guard,
// model grant editing with force retry).
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError } from "../../lib/api";

const env = vi.hoisted(() => ({
  role: "platform_admin",
  api: {} as Record<string, { data?: unknown; loading?: boolean; error?: string }>,
  refreshed: [] as string[],
  push: vi.fn(),
  apiGet: vi.fn(),
  apiPost: vi.fn(),
  apiPatch: vi.fn(),
  apiPut: vi.fn(),
  apiDelete: vi.fn(),
}));

vi.mock("next/navigation", () => ({
  useRouter: () => ({ push: env.push }),
}));

vi.mock("../../components/AuthGate", () => ({
  AuthGate: ({ children }: { children: React.ReactNode }) => <>{children}</>,
}));

vi.mock("../../components/AppShell", () => ({
  AppShell: ({ children }: { children: React.ReactNode }) => <>{children}</>,
}));

// Heavy admin panels have their own suites — stub them so this file only
// exercises the settings page's own dispatch and inline logic.
vi.mock("../../components/PromptSettingsTab", () => ({
  OrganizationPromptTab: () => <div data-testid="org-prompt" />,
}));
vi.mock("../../components/DeadLettersPanel", () => ({
  DeadLettersPanel: () => <div data-testid="dead-letters" />,
}));
vi.mock("../../components/PlatformSettingsTab", () => ({
  PlatformSettingsTab: () => <div data-testid="platform-tab" />,
}));
vi.mock("../../components/NotificationPreferencesPanel", () => ({
  NotificationPreferencesPanel: () => <div data-testid="notif-prefs" />,
}));
vi.mock("../../components/PromptTemplatesPanel", () => ({
  PromptTemplatesPanel: (props: { templates: unknown[] }) => (
    <div data-testid="templates-panel" data-count={props.templates.length} />
  ),
}));

vi.mock("../../lib/auth", () => ({
  useAuth: () => ({
    token: "tok",
    mode: "dev",
    authed: true,
    user: { id: "u1", name: "Ross", email: "ross@x.io", role: env.role },
  }),
}));

vi.mock("../../lib/useApi", () => ({
  useApi: (path: string) => {
    const entry = env.api[path] ?? { data: null, loading: false, error: "" };
    return {
      data: entry.data ?? null,
      loading: entry.loading ?? false,
      error: entry.error ?? "",
      refresh: () => {
        env.refreshed.push(path);
      },
    };
  },
}));

vi.mock("../../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../lib/api")>();
  return {
    ...actual, // keep the real ApiError class for instanceof checks
    apiGet: (...args: unknown[]) => env.apiGet(...(args as [string, string])),
    apiPost: (...args: unknown[]) => env.apiPost(...(args as [string, string, unknown])),
    apiPatch: (...args: unknown[]) => env.apiPatch(...(args as [string, string, unknown])),
    apiPut: (...args: unknown[]) => env.apiPut(...(args as [string, string, unknown])),
    apiDelete: (...args: unknown[]) => env.apiDelete(...(args as [string, string])),
  };
});

import SettingsPage from "./page";

const provider = {
  id: "p1",
  name: "openai-prod",
  kind: "openai",
  base_url: "https://api.openai.com/v1",
  status: "active",
  has_api_key: true,
  api_key_masked: "•••••1abcd",
};
const deprecatedProvider = { ...provider, id: "p2", name: "old-ai", base_url: "https://old.ai/v1", status: "deprecated" };
const model = {
  id: "m1",
  provider_id: "p1",
  display_name: "GPT-6 Sol",
  model_name: "gpt-6-sol",
  context_window: 200000,
  supports_tools: true,
  supports_vision: false,
  pricing: {
    input_per_1m: "2.50",
    output_per_1m: "10.00",
    cached_input_per_1m: "0.25",
    cache_write_per_1m: "3.00",
  },
  long_context_threshold_tokens: 272000,
  status: "active",
};
const orphanModel = { ...model, id: "m2", provider_id: "gone", display_name: "Orphan", model_name: "orphan-1" };
const adminUser = { id: "u1", email: "ross@x.io", name: "Ross", role: "platform_admin" };
const memberUser = { id: "u2", email: "kim@x.io", name: "Kim", role: "user" };

function setApi(path: string, data: unknown, opts: { loading?: boolean; error?: string } = {}) {
  env.api[path] = { data, ...opts };
}

beforeEach(() => {
  env.role = "platform_admin";
  env.api = {};
  env.refreshed = [];
  vi.resetAllMocks();
  env.push = vi.fn();
  env.apiGet = vi.fn().mockResolvedValue({});
  env.apiPost = vi.fn().mockResolvedValue({});
  env.apiPatch = vi.fn().mockResolvedValue({});
  env.apiPut = vi.fn().mockResolvedValue({});
  env.apiDelete = vi.fn().mockResolvedValue(undefined);
  setApi("/registry/ai-providers", [provider]);
  setApi("/ai-models", [model]);
  setApi("/users", [adminUser, memberUser]);
  setApi("/ai-models?status=active", [model]);
  setApi("/users/u2/models", [model]);
});

describe("SettingsPage member (non-admin) surface", () => {
  beforeEach(() => {
    env.role = "user";
  });

  it("shows the role notice and only member tabs", () => {
    render(<SettingsPage />);
    expect(screen.getByText("Settings")).toBeInTheDocument();
    expect(screen.getByText(/signed in as/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "AI providers" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Notifications" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "AI Models" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Access" })).not.toBeInTheDocument();
  });

  it("renders the read-only provider list with status chips", () => {
    setApi("/registry/ai-providers", [provider, deprecatedProvider]);
    render(<SettingsPage />);
    expect(screen.getByText("openai-prod")).toBeInTheDocument();
    expect(screen.getByText("openai · https://api.openai.com/v1")).toBeInTheDocument();
    expect(screen.getByText("old-ai")).toBeInTheDocument();
    // Member sees no Edit/Deprecate/Delete controls.
    expect(screen.queryByRole("button", { name: "Edit" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Deprecate" })).not.toBeInTheDocument();
  });

  it("empty + error states for the member provider list", () => {
    setApi("/registry/ai-providers", []);
    const { unmount } = render(<SettingsPage />);
    expect(screen.getByText("No AI providers registered")).toBeInTheDocument();
    unmount();
    setApi("/registry/ai-providers", null, { error: "registry down" });
    render(<SettingsPage />);
    expect(screen.getByText("registry down")).toBeInTheDocument();
  });

  it("member can open the Notifications tab", async () => {
    const user = userEvent.setup();
    render(<SettingsPage />);
    await user.click(screen.getByRole("button", { name: "Notifications" }));
    expect(screen.getByTestId("notif-prefs")).toBeInTheDocument();
  });
});

describe("SettingsPage admin tab dispatch", () => {
  it("maps the default providers tab to the merged AI Models view", () => {
    render(<SettingsPage />);
    expect(screen.getByRole("button", { name: "AI Models" })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "AI Models" })).toBeInTheDocument();
  });

  it("switches between all admin tabs", async () => {
    const user = userEvent.setup();
    render(<SettingsPage />);
    await user.click(screen.getByRole("button", { name: "Access" }));
    expect(screen.getByRole("heading", { name: "Users & access" })).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Prompt" }));
    expect(screen.getByTestId("org-prompt")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Prompt Templates" }));
    expect(screen.getByTestId("templates-panel")).toHaveAttribute("data-count", "0");
    await user.click(screen.getByRole("button", { name: "Dead letters" }));
    expect(screen.getByTestId("dead-letters")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Platform" }));
    expect(screen.getByTestId("platform-tab")).toBeInTheDocument();
  });

  it("Resources tab navigates to the real resources route", async () => {
    const user = userEvent.setup();
    render(<SettingsPage />);
    await user.click(screen.getByRole("button", { name: "Resources" }));
    expect(env.push).toHaveBeenCalledWith("/settings/resources");
  });
});

describe("ModelHierarchyTab rendering", () => {
  it("groups models under their provider and shows orphans separately", () => {
    setApi("/ai-models", [model, orphanModel]);
    render(<SettingsPage />);
    expect(screen.getByText("Unassigned models")).toBeInTheDocument();
    expect(screen.getByText("Orphan")).toBeInTheDocument();
    // Model row shows name + capability meta + rates.
    const row = screen.getByText("GPT-6 Sol").closest(".model-nested-row") as HTMLElement;
    expect(row).toHaveTextContent("gpt-6-sol");
    expect(row).toHaveTextContent("200,000 tokens");
    expect(row).toHaveTextContent("tools ✓");
    expect(row).toHaveTextContent("no vision");
  });

  it("provider with no models shows the empty hint", () => {
    setApi("/ai-models", []);
    render(<SettingsPage />);
    expect(screen.getByText("No models registered under this provider yet.")).toBeInTheDocument();
  });

  it("empty hierarchy shows the onboarding EmptyState", () => {
    setApi("/registry/ai-providers", []);
    setApi("/ai-models", []);
    render(<SettingsPage />);
    expect(screen.getByText("No AI models registered")).toBeInTheDocument();
  });

  it("surfaces provider and model fetch errors", () => {
    setApi("/registry/ai-providers", null, { error: "prov boom" });
    setApi("/ai-models", null, { error: "model boom" });
    render(<SettingsPage />);
    expect(screen.getByText("prov boom")).toBeInTheDocument();
    expect(screen.getByText("model boom")).toBeInTheDocument();
  });
});

describe("Model deprecate + delete flows", () => {
  it("deprecating a model shows the cascade report and can dismiss it", async () => {
    const user = userEvent.setup();
    env.apiPost.mockResolvedValue({ affected_users: 2, affected_agents: 1 });
    render(<SettingsPage />);
    const row = screen.getByText("GPT-6 Sol").closest(".model-nested-row") as HTMLElement;
    await user.click(row.querySelector("button.btn-danger") as HTMLElement);
    const report = await screen.findByText(/agent\(s\) and .* user\(s\) affected/);
    expect(report).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Dismiss" }));
    expect(screen.queryByText(/agent\(s\) and .* user\(s\) affected/)).not.toBeInTheDocument();
  });

  it("deprecate failure surfaces the error text", async () => {
    const user = userEvent.setup();
    env.apiPost.mockRejectedValue(new Error("nope"));
    render(<SettingsPage />);
    const row = screen.getByText("GPT-6 Sol").closest(".model-nested-row") as HTMLElement;
    await user.click(row.querySelector("button.btn-danger") as HTMLElement);
    expect(await screen.findByText("Deprecate failed: nope")).toBeInTheDocument();
  });

  it("delete with a 409 in_use conflict asks to confirm, then retries with force", async () => {
    const user = userEvent.setup();
    const usage = [
      { user_id: "u2", user_email: "kim@x.io", agent_id: "a1", agent_name: "coder", squad_id: "s1", slot: "primary" },
    ];
    env.apiDelete
      .mockRejectedValueOnce(new ApiError(409, "model in use", { error: "in_use", usage }))
      .mockResolvedValueOnce(undefined);
    render(<SettingsPage />);
    const row = screen.getByText("GPT-6 Sol").closest(".model-nested-row") as HTMLElement;
    const deleteBtn = [...row.querySelectorAll("button")].find((b) => b.textContent === "Delete") as HTMLElement;
    await user.click(deleteBtn);
    const dialog = await screen.findByRole("button", { name: "Delete and revoke" });
    await user.click(dialog);
    expect(env.apiDelete).toHaveBeenLastCalledWith("/ai-models/m1?force=true", "tok");
    expect(env.refreshed).toContain("/ai-models");
  });

  it("delete error that is not a conflict shows an inline alert", async () => {
    const user = userEvent.setup();
    env.apiDelete.mockRejectedValue(new Error("forbidden"));
    render(<SettingsPage />);
    const row = screen.getByText("GPT-6 Sol").closest(".model-nested-row") as HTMLElement;
    const deleteBtn = [...row.querySelectorAll("button")].find((b) => b.textContent === "Delete") as HTMLElement;
    await user.click(deleteBtn);
    expect(await screen.findByRole("alert")).toHaveTextContent("forbidden");
  });

  it("deprecating a provider from the group header posts the deprecate route", async () => {
    const user = userEvent.setup();
    render(<SettingsPage />);
    const header = screen.getByText("openai-prod").closest(".provider-header-row") as HTMLElement;
    await user.click(header.querySelector("button.btn-danger") as HTMLElement);
    expect(env.apiPost).toHaveBeenCalledWith("/registry/ai-providers/p1/deprecate", "tok", {});
  });
});

describe("ProviderModal (register + edit + connection test)", () => {
  it("register flow: validation, test connection, and payload omits blank key", async () => {
    const user = userEvent.setup();
    env.apiPost.mockResolvedValue({ ok: true, reason: "ok", latency_ms: 42, detail: "" });
    render(<SettingsPage />);
    await user.click(screen.getByRole("button", { name: "+ Register provider" }));
    const dialog = await screen.findByRole("dialog", { name: "Register AI provider" });
    const submit = screen.getByRole("button", { name: "Register" }) as HTMLButtonElement;
    expect(submit).toBeDisabled();
    await user.type(within(dialog).getByLabelText(/Name/), "new-ai");
    await user.type(within(dialog).getByLabelText(/Base URL/), "https://new.ai/v1");
    expect(submit).toBeEnabled();
    await user.click(screen.getByRole("button", { name: "Test connection" }));
    expect(await screen.findByText(/42 ms/)).toBeInTheDocument();
    await user.click(submit);
    await waitFor(() =>
      expect(env.apiPost).toHaveBeenCalledWith("/registry/ai-providers", "tok", {
        name: "new-ai",
        kind: "openai",
        base_url: "https://new.ai/v1",
      }),
    );
  });

  it("edit flow sends the PATCH and includes a typed key only", async () => {
    const user = userEvent.setup();
    render(<SettingsPage />);
    const header = screen.getByText("openai-prod").closest(".provider-header-row") as HTMLElement;
    await user.click(within(header).getByRole("button", { name: "Edit" }));
    const dialog = await screen.findByRole("dialog", { name: /Edit provider/ });
    const nameInput = within(dialog).getByLabelText(/Name/) as HTMLInputElement;
    expect(nameInput.value).toBe("openai-prod");
    // Masked key placeholder shows the stored key tail.
    const keyInput = within(dialog).getByLabelText(/API key/) as HTMLInputElement;
    expect(keyInput.placeholder).toContain("1abcd");
    await user.clear(nameInput);
    await user.type(nameInput, "renamed");
    await user.type(keyInput, "sk-secret");
    await user.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() =>
      expect(env.apiPatch).toHaveBeenCalledWith("/registry/ai-providers/p1", "tok", {
        name: "renamed",
        kind: "openai",
        base_url: "https://api.openai.com/v1",
        api_key: "sk-secret",
      }),
    );
  });

  it("failed save surfaces the error and keeps the modal open", async () => {
    const user = userEvent.setup();
    env.apiPost.mockRejectedValue(new Error("dup name"));
    render(<SettingsPage />);
    await user.click(screen.getByRole("button", { name: "+ Register provider" }));
    const dialog = await screen.findByRole("dialog", { name: "Register AI provider" });
    await user.type(within(dialog).getByLabelText(/Name/), "x");
    await user.type(within(dialog).getByLabelText(/Base URL/), "https://x");
    await user.click(screen.getByRole("button", { name: "Register" }));
    expect(await screen.findByText("dup name")).toBeInTheDocument();
    expect(screen.getByRole("dialog", { name: "Register AI provider" })).toBeInTheDocument();
  });
});

describe("AIModelModal register flow", () => {
  it("selecting a provider loads its model list into the datalist", async () => {
    const user = userEvent.setup();
    env.apiGet.mockImplementation(async (path: string) => {
      if (path.includes("/models")) return { models: ["gpt-6-sol", "gpt-6-mini"] };
      return {};
    });
    render(<SettingsPage />);
    await user.click(screen.getByRole("button", { name: "+ Register model" }));
    const dialog = await screen.findByRole("dialog", { name: "Register AI model" });
    await user.selectOptions(within(dialog).getByLabelText(/Provider \(credential holder\)/), "p1");
    await waitFor(() => expect(env.apiGet).toHaveBeenCalledWith("/registry/ai-providers/p1/models", "tok"));
    const datalist = await screen.findByRole("listbox", { hidden: true });
    expect(datalist).toBeInTheDocument();
    expect(within(datalist).getAllByRole("option", { hidden: true }).map((o) => o.getAttribute("value"))).toEqual([
      "gpt-6-sol",
      "gpt-6-mini",
    ]);
  });

  it("model list fetch failure falls back to a manual name input", async () => {
    const user = userEvent.setup();
    env.apiGet.mockRejectedValue(new Error("list down"));
    render(<SettingsPage />);
    await user.click(screen.getByRole("button", { name: "+ Register model" }));
    const dialog = await screen.findByRole("dialog", { name: "Register AI model" });
    await user.selectOptions(within(dialog).getByLabelText(/Provider \(credential holder\)/), "p1");
    expect(await screen.findByText(/type the model name manually/)).toBeInTheDocument();
  });

  it("test model + register success, and duplicate error path", async () => {
    const user = userEvent.setup();
    env.apiPost
      .mockResolvedValueOnce({ ok: true, reason: "ok", latency_ms: 7, detail: "PONG" })
      .mockRejectedValueOnce(new ApiError(409, "exists", { error: { code: "duplicate_model" } }))
      .mockResolvedValueOnce({});
    render(<SettingsPage />);
    await user.click(screen.getByRole("button", { name: "+ Register model" }));
    const dialog = await screen.findByRole("dialog", { name: "Register AI model" });
    await user.selectOptions(within(dialog).getByLabelText(/Provider \(credential holder\)/), "p1");
    const nameInput = within(dialog).getByPlaceholderText(/Select a model/) as HTMLInputElement;
    await user.type(nameInput, "gpt-6-sol");
    for (const rate of within(dialog).getAllByPlaceholderText("0.00")) {
      await user.type(rate, "1.00");
    }
    await user.click(screen.getByRole("button", { name: "Test model" }));
    expect(await screen.findByText(/PONG/)).toBeInTheDocument();
    // First save attempt hits the duplicate error.
    await user.click(screen.getByRole("button", { name: "Register" }));
    expect(await screen.findByText(/^Duplicate model:/)).toBeInTheDocument();
    // Second save succeeds and closes the modal.
    await user.click(screen.getByRole("button", { name: "Register" }));
    await waitFor(() => expect(screen.queryByRole("dialog", { name: "Register AI model" })).not.toBeInTheDocument());
    expect(env.refreshed).toContain("/ai-models");
  });

  it("fetch-from-provider prefills metadata and reports what was applied", async () => {
    const user = userEvent.setup();
    env.apiGet.mockImplementation(async (path: string) => {
      if (path.includes("model-metadata")) {
        return { context_window: 128000, pricing: { input_per_1m: 1.25 } };
      }
      return { models: [] };
    });
    render(<SettingsPage />);
    await user.click(screen.getByRole("button", { name: "+ Register model" }));
    const dialog = await screen.findByRole("dialog", { name: "Register AI model" });
    await user.selectOptions(within(dialog).getByLabelText(/Provider \(credential holder\)/), "p1");
    await user.type(within(dialog).getByPlaceholderText(/Select a model/), "meta-model");
    await user.click(screen.getByRole("button", { name: "Fetch from provider" }));
    const ctx = within(dialog).getByPlaceholderText("200000") as HTMLInputElement;
    await waitFor(() => expect(ctx.value).toBe("128000"));
    expect(await screen.findByText(/Context window/)).toBeInTheDocument();
  });
});

describe("AccessTab users, roles and grants", () => {
  it("lists users with roles and marks the current user", async () => {
    render(<SettingsPage />);
    const user = userEvent.setup();
    await user.click(screen.getByRole("button", { name: "Access" }));
    expect(screen.getByText("(you)")).toBeInTheDocument();
    expect(screen.getByText("ross@x.io · role platform_admin")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Demote" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Make admin" })).toBeInTheDocument();
  });

  it("demote goes through confirm and PATCHes the role", async () => {
    const user = userEvent.setup();
    render(<SettingsPage />);
    await user.click(screen.getByRole("button", { name: "Access" }));
    await user.click(screen.getByRole("button", { name: "Demote" }));
    const dialog = await screen.findByRole("dialog", { name: /Demote Ross/ });
    // The self-role warning is shown in the dialog body.
    expect(within(dialog).getByText(/changing your OWN role/i)).toBeInTheDocument();
    await user.click(within(dialog).getByRole("button", { name: "Demote" }));
    await waitFor(() => expect(env.apiPatch).toHaveBeenCalledWith("/users/u1/role", "tok", { role: "user" }));
  });

  it("last-admin 409 surfaces the lockout warning", async () => {
    const user = userEvent.setup();
    env.apiPatch.mockRejectedValue(new ApiError(409, "last_admin", {}));
    render(<SettingsPage />);
    await user.click(screen.getByRole("button", { name: "Access" }));
    await user.click(screen.getByRole("button", { name: "Demote" }));
    const dialog = await screen.findByRole("dialog", { name: /Demote Ross/ });
    await user.click(within(dialog).getByRole("button", { name: "Demote" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("Last platform admin");
  });

  it("grant editor toggles models, shows the diff and saves with PUT", async () => {
    const user = userEvent.setup();
    setApi("/ai-models?status=active", [model, orphanModel]);
    render(<SettingsPage />);
    await user.click(screen.getByRole("button", { name: "Access" }));
    const manageBtns = screen.getAllByRole("button", { name: "Manage models" });
    await user.click(manageBtns[1]); // second row = Kim (u2)
    expect(screen.getByRole("heading", { name: /Models granted to Kim/ })).toBeInTheDocument();
    const orphanCheck = screen.getByRole("checkbox", { name: /Orphan/ }) as HTMLInputElement;
    expect(orphanCheck.checked).toBe(false);
    await user.click(orphanCheck);
    expect(screen.getByText("1 adding · 0 removing")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Save grants" }));
    await waitFor(() =>
      expect(env.apiPut).toHaveBeenCalledWith("/users/u2/models", "tok", { model_ids: ["m1", "m2"] }),
    );
    expect(await screen.findByText("Grants saved and virtual keys converged.")).toBeInTheDocument();
  });

  it("grant removal blocked by in_use requires the force confirm", async () => {
    const user = userEvent.setup();
    const usage = [
      { user_id: "u2", user_email: "kim@x.io", agent_id: "a1", agent_name: "coder", squad_id: "s1", slot: "primary" },
    ];
    env.apiPut
      .mockRejectedValueOnce(new ApiError(409, "in use", { error: "in_use", usage }))
      .mockResolvedValueOnce({});
    render(<SettingsPage />);
    await user.click(screen.getByRole("button", { name: "Access" }));
    const manageBtns = screen.getAllByRole("button", { name: "Manage models" });
    await user.click(manageBtns[1]);
    const grantedCheck = screen.getByRole("checkbox", { name: /GPT-6 Sol/ }) as HTMLInputElement;
    await user.click(grantedCheck);
    await user.click(screen.getByRole("button", { name: "Save grants" }));
    const forceBtn = await screen.findByRole("button", { name: "Revoke and converge keys" });
    await user.click(forceBtn);
    await waitFor(() =>
      expect(env.apiPut).toHaveBeenLastCalledWith("/users/u2/models?force=true", "tok", { model_ids: [] }),
    );
  });

  it("empty and error states for the users list", async () => {
    const user = userEvent.setup();
    setApi("/users", []);
    const { unmount } = render(<SettingsPage />);
    await user.click(screen.getByRole("button", { name: "Access" }));
    expect(screen.getByText("No users found")).toBeInTheDocument();
    unmount();
    setApi("/users", null, { error: "db down" });
    render(<SettingsPage />);
    await user.click(screen.getByRole("button", { name: "Access" }));
    expect(screen.getByText(/Could not load users/)).toBeInTheDocument();
  });
});

