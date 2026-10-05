// S-189: jsdom behaviour tests for the unified tool configuration page
// (/settings/resources/tools/[toolId]). Locks the builtin-vs-registry
// routing on the toolId, the admin gating for built-in config, and the
// registered-tool inline edit: prefill, dirty gating, trimmed PATCH
// payload, manifest JSON validation, read-only mode, and the delete
// redirect.
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";

const env = vi.hoisted(() => ({
  toolId: "exec",
  role: "platform_admin",
  resource: null as Record<string, unknown> | null,
  loading: false,
  error: "",
  patchCalls: [] as { path: string; token: string; body: unknown }[],
  patchError: null as Error | null,
  refresh: vi.fn(),
  push: vi.fn(),
}));

vi.mock("next/link", () => ({
  default: ({ href, children }: { href: string; children: React.ReactNode }) => (
    <a href={href}>{children}</a>
  ),
}));

vi.mock("next/navigation", () => ({
  useParams: () => ({ toolId: env.toolId }),
  useRouter: () => ({ push: env.push }),
}));

vi.mock("../../../../../components/AuthGate", () => ({
  AuthGate: ({ children }: { children: React.ReactNode }) => <>{children}</>,
}));

vi.mock("../../../../../components/AppShell", () => ({
  AppShell: ({ children }: { children: React.ReactNode }) => <>{children}</>,
}));

// The pinned builtin policy form has its own suite; here we assert routing
// and gating only, so a stub is enough.
vi.mock("../../../../../components/BuiltinToolConfig", () => ({
  BuiltinToolConfig: ({ name }: { name: string }) => (
    <div data-testid="builtin-config">{`builtin:${name}`}</div>
  ),
}));

// The grant-aware delete flow is covered by its own tests; stub the
// callback wiring (onDeleted) instead.
vi.mock("../../../../../components/DeleteResourceButton", () => ({
  DeleteResourceButton: ({ path, onDeleted }: { path: string; onDeleted: () => void }) => (
    <div data-testid="delete-btn" data-path={path}>
      <button type="button" onClick={onDeleted}>
        stub-delete
      </button>
    </div>
  ),
}));

vi.mock("../../../../../lib/auth", () => ({
  useAuth: () => ({ token: "tok", mode: "token", user: { id: "u1", role: env.role } }),
}));

vi.mock("../../../../../lib/useApi", () => ({
  useApi: () => ({
    data: env.resource,
    loading: env.loading,
    error: env.error,
    refresh: env.refresh,
  }),
}));

vi.mock("../../../../../lib/api", () => ({
  apiPatch: async (path: string, token: string, body: unknown) => {
    env.patchCalls.push({ path, token, body });
    if (env.patchError) throw env.patchError;
    return {};
  },
}));

import ToolConfigPage from "./page";

function registryTool(overrides: Record<string, unknown> = {}) {
  return {
    id: "rt-1",
    type: "tool",
    name: "Weather Tool",
    description: "gets weather",
    endpoint: "https://weather.example/api",
    auth_ref: "vault/weather",
    manifest: { version: 1 },
    status: "active",
    ...overrides,
  };
}

beforeEach(() => {
  env.toolId = "exec";
  env.role = "platform_admin";
  env.resource = null;
  env.loading = false;
  env.error = "";
  env.patchCalls = [];
  env.patchError = null;
  env.refresh = vi.fn();
  env.push = vi.fn();
});

describe("builtin tool routing", () => {
  it("admins get the pinned builtin policy form", () => {
    render(<ToolConfigPage />);
    expect(screen.getByTestId("builtin-config")).toHaveTextContent("builtin:exec");
    expect(screen.getByRole("link", { name: "← Back to Tools" })).toHaveAttribute(
      "href",
      "/settings/resources/tools",
    );
  });

  it("non-admins get the role notice and no policy form", () => {
    env.role = "member";
    render(<ToolConfigPage />);
    expect(screen.getByText(/Built-in tool configuration requires the/)).toBeInTheDocument();
    expect(screen.getByText("platform_admin")).toBeInTheDocument();
    expect(screen.queryByTestId("builtin-config")).not.toBeInTheDocument();
  });

  it("array toolId param uses the first entry", () => {
    env.toolId = ["web_search", "extra"] as unknown as string;
    render(<ToolConfigPage />);
    expect(screen.getByTestId("builtin-config")).toHaveTextContent("builtin:web_search");
  });
});

function manifestBox(): HTMLElement {
  return screen.getByLabelText("Manifest (JSON, optional)");
}

const manifestText = JSON.stringify({ version: 1 }, null, 2);

describe("registry tool load states", () => {
  beforeEach(() => {
    env.toolId = "rt-1";
  });

  it("shows the loading notice before data arrives", () => {
    env.loading = true;
    render(<ToolConfigPage />);
    expect(screen.getByText("Loading tool…")).toBeInTheDocument();
  });

  it("shows the not-found empty state on error", () => {
    env.error = "404 not found";
    render(<ToolConfigPage />);
    expect(screen.getByText("Tool not found")).toBeInTheDocument();
    expect(screen.getByText("404 not found")).toBeInTheDocument();
  });

  it("prefills the form and maps status to a chip", () => {
    env.resource = registryTool();
    render(<ToolConfigPage />);
    expect(screen.getByRole("heading", { name: "Weather Tool" })).toBeInTheDocument();
    expect(screen.getByDisplayValue("gets weather")).toBeInTheDocument();
    expect(screen.getByDisplayValue("https://weather.example/api")).toBeInTheDocument();
    expect(screen.getByDisplayValue("vault/weather")).toBeInTheDocument();
    expect(manifestBox()).toHaveValue(manifestText);
    // active → idle chip (green "Idle" label).
    expect(screen.getByText("Idle")).toBeInTheDocument();
  });

  it("deprecated status maps to the paused chip", () => {
    env.resource = registryTool({ status: "deprecated" });
    render(<ToolConfigPage />);
    expect(screen.getByText("Paused")).toBeInTheDocument();
  });

  it("unknown status maps to the error chip", () => {
    env.resource = registryTool({ status: "weird" });
    render(<ToolConfigPage />);
    expect(screen.getByText("Error")).toBeInTheDocument();
  });
});

describe("registry tool editing", () => {
  beforeEach(() => {
    env.toolId = "rt-1";
    env.resource = registryTool();
  });

  it("save is disabled until a field changes, then PATCHes trimmed values", async () => {
    render(<ToolConfigPage />);
    const save = screen.getByRole("button", { name: "Save changes" });
    expect(save).toBeDisabled();
    const name = screen.getByDisplayValue("Weather Tool");
    await userEvent.clear(name);
    await userEvent.type(name, "  Storm Watch  ");
    expect(save).toBeEnabled();
    await userEvent.click(save);
    await vi.waitFor(() => expect(env.patchCalls).toHaveLength(1));
    expect(env.patchCalls[0].path).toBe("/registry/tools/rt-1");
    expect(env.patchCalls[0].body).toEqual({
      name: "Storm Watch",
      description: "gets weather",
      endpoint: "https://weather.example/api",
      auth_ref: "vault/weather",
      manifest: JSON.stringify({ version: 1 }, null, 2),
    });
    expect(env.refresh).toHaveBeenCalled();
    expect(screen.getByText("Saved.")).toBeInTheDocument();
  });

  it("blank manifest is omitted from the payload", async () => {
    render(<ToolConfigPage />);
    await userEvent.clear(manifestBox());
    await userEvent.click(screen.getByRole("button", { name: "Save changes" }));
    await vi.waitFor(() => expect(env.patchCalls).toHaveLength(1));
    expect((env.patchCalls[0].body as Record<string, unknown>).manifest).toBeUndefined();
  });

  it("invalid manifest JSON surfaces the parse error and skips the PATCH", async () => {
    render(<ToolConfigPage />);
    const manifest = manifestBox();
    await userEvent.clear(manifest);
    await userEvent.type(manifest, "{{not json"); // "{{" → literal "{"
    await userEvent.click(screen.getByRole("button", { name: "Save changes" }));
    const alert = await screen.findByRole("alert");
    // The JSON.parse SyntaxError surfaces verbatim; nothing is PATCHed.
    expect((alert.textContent ?? "").length).toBeGreaterThan(0);
    expect(env.patchCalls).toHaveLength(0);
  });

  it("non-object manifest is rejected with a readable message", async () => {
    render(<ToolConfigPage />);
    const manifest = manifestBox();
    await userEvent.clear(manifest);
    await userEvent.type(manifest, "42");
    await userEvent.click(screen.getByRole("button", { name: "Save changes" }));
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("Manifest must be a JSON object or array");
    expect(env.patchCalls).toHaveLength(0);
  });

  it("save failure surfaces the error", async () => {
    env.patchError = new Error("409 conflict");
    render(<ToolConfigPage />);
    const name = screen.getByDisplayValue("Weather Tool");
    await userEvent.clear(name);
    await userEvent.type(name, "Changed");
    await userEvent.click(screen.getByRole("button", { name: "Save changes" }));
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("409 conflict");
  });

  it("empty name keeps save disabled", async () => {
    render(<ToolConfigPage />);
    const name = screen.getByDisplayValue("Weather Tool");
    await userEvent.clear(name);
    await userEvent.type(name, "   ");
    expect(screen.getByRole("button", { name: "Save changes" })).toBeDisabled();
  });

  it("non-admins get read-only fields, no save and no delete", () => {
    env.role = "member";
    render(<ToolConfigPage />);
    expect(screen.getByText(/You have read-only access/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Save changes" })).not.toBeInTheDocument();
    expect(screen.queryByTestId("delete-btn")).not.toBeInTheDocument();
    expect(screen.getByDisplayValue("Weather Tool")).toBeDisabled();
  });

  it("admin delete redirects to the tools tiles page", async () => {
    env.resource = registryTool();
    render(<ToolConfigPage />);
    const del = screen.getByTestId("delete-btn");
    expect(del).toHaveAttribute("data-path", "/registry/tools/rt-1");
    await userEvent.click(within(del).getByRole("button", { name: "stub-delete" }));
    expect(env.push).toHaveBeenCalledWith("/settings/resources/tools");
  });
});
