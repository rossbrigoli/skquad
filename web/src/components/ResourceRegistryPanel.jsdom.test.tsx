// S-189: jsdom behaviour tests for the shared resource registry panel
// and its register/edit modal. Covers admin vs member affordances,
// empty/error states, meta fallbacks, create/edit API wiring, JSON
// manifest validation and delete-refresh.
import { fireEvent, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";

const env = vi.hoisted(() => ({
  items: [] as unknown[],
  error: "",
  loading: false,
  apiPost: vi.fn(),
  apiPatch: vi.fn(),
  refresh: vi.fn(),
  onDeleteDeleted: null as null | (() => void),
}));

vi.mock("../lib/auth", () => ({
  useAuth: () => ({ token: "tok", mode: "token", user: { id: "u1" }, logout: vi.fn() }),
}));

vi.mock("../lib/useApi", () => ({
  useApi: () => ({ data: env.items, loading: env.loading, error: env.error, refresh: env.refresh }),
}));

vi.mock("../lib/api", async (importOriginal) => {
  const mod = await importOriginal<typeof import("../lib/api")>();
  return {
    ...mod,
    apiPost: (...args: unknown[]) => env.apiPost(...args),
    apiPatch: (...args: unknown[]) => env.apiPatch(...args),
  };
});

vi.mock("./DeleteResourceButton", () => ({
  DeleteResourceButton: ({ onDeleted }: { onDeleted: () => void }) => {
    env.onDeleteDeleted = onDeleted;
    return <button type="button" onClick={onDeleted}>delete-btn</button>;
  },
}));

import { ResourceRegistryPanel, resourceLabel } from "./ResourceRegistryPanel";

beforeEach(() => {
  env.items = [];
  env.error = "";
  env.loading = false;
  env.apiPost = vi.fn().mockResolvedValue({});
  env.apiPatch = vi.fn().mockResolvedValue({});
  env.refresh = vi.fn();
  env.onDeleteDeleted = null;
});

describe("resourceLabel", () => {
  it("singularises and un-hyphens registry type keys", () => {
    expect(resourceLabel("skills")).toBe("skill");
    expect(resourceLabel("knowledge-bases")).toBe("knowledge base");
    expect(resourceLabel("workspaces")).toBe("workspace");
  });
});

describe("ResourceRegistryPanel states", () => {
  it("member sees the list but no admin affordances", () => {
    env.items = [{ id: "r1", name: "kb-one", status: "active", description: "a kb" }];
    render(<ResourceRegistryPanel resourceType="knowledge-bases" label="Knowledge Bases" isAdmin={false} />);
    expect(screen.getByText("kb-one")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Register/ })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Edit" })).not.toBeInTheDocument();
  });

  it("admin sees register, edit and delete affordances", () => {
    env.items = [{ id: "r1", name: "kb-one", status: "active" }];
    render(<ResourceRegistryPanel resourceType="knowledge-bases" label="Knowledge Bases" isAdmin />);
    expect(screen.getByRole("button", { name: "+ Register knowledge base" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Edit" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "delete-btn" })).toBeInTheDocument();
  });

  it("falls back through endpoint then em-dash for the meta line", () => {
    env.items = [
      { id: "r1", name: "with-endpoint", status: "active", endpoint: "https://x.test" },
      { id: "r2", name: "bare", status: "deprecated" },
    ];
    render(<ResourceRegistryPanel resourceType="apis" label="APIs" isAdmin={false} />);
    expect(screen.getByText("https://x.test")).toBeInTheDocument();
    expect(screen.getByText("—")).toBeInTheDocument();
  });

  it("empty and error states", () => {
    const { unmount } = render(
      <ResourceRegistryPanel resourceType="skills" label="Skills" isAdmin={false} />,
    );
    expect(screen.getByText("No skills registered")).toBeInTheDocument();
    unmount();
    env.error = "registry unavailable";
    env.items = [{ id: "r1", name: "x", status: "active" }];
    render(<ResourceRegistryPanel resourceType="skills" label="Skills" isAdmin={false} />);
    expect(screen.getByText("registry unavailable")).toBeInTheDocument();
  });

  it("delete triggers a refresh", () => {
    env.items = [{ id: "r1", name: "s1", status: "active" }];
    render(<ResourceRegistryPanel resourceType="skills" label="Skills" isAdmin />);
    screen.getByRole("button", { name: "delete-btn" }).click();
    expect(env.refresh).toHaveBeenCalled();
  });
});

describe("ResourceModal create + edit", () => {
  it("create: disabled until named, posts trimmed body with parsed manifest", async () => {
    const user = userEvent.setup();
    env.items = [{ id: "r1", name: "s1", status: "active" }];
    render(<ResourceRegistryPanel resourceType="skills" label="Skills" isAdmin />);
    await user.click(screen.getByRole("button", { name: "+ Register skill" }));
    const dialog = screen.getByRole("dialog", { name: "Register skill" });
    const submit = within(dialog).getByRole("button", { name: "Register" });
    expect(submit).toBeDisabled();
    await user.type(within(dialog).getByLabelText(/Name/), " new-skill ");
    fireEvent.change(within(dialog).getByLabelText(/Manifest/), { target: { value: '{"version": 1}' } });
    expect(submit).toBeEnabled();
    await user.click(submit);
    await vi.waitFor(() =>
      expect(env.apiPost).toHaveBeenCalledWith("/registry/skills", "tok", {
        name: "new-skill",
        description: "",
        endpoint: "",
        auth_ref: "",
        manifest: '{"version": 1}',
      }),
    );
    await vi.waitFor(() => expect(env.refresh).toHaveBeenCalled());
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });

  it("create: invalid JSON manifest surfaces the parse error", async () => {
    const user = userEvent.setup();
    env.items = [{ id: "r1", name: "s1", status: "active" }];
    render(<ResourceRegistryPanel resourceType="skills" label="Skills" isAdmin />);
    await user.click(screen.getByRole("button", { name: "+ Register skill" }));
    const dialog = screen.getByRole("dialog", { name: "Register skill" });
    await user.type(within(dialog).getByLabelText(/Name/), "ok");
    fireEvent.change(within(dialog).getByLabelText(/Manifest/), { target: { value: "{not json" } });
    await user.click(within(dialog).getByRole("button", { name: "Register" }));
    const errEl = dialog.querySelector(".notice.error");
    await vi.waitFor(() => expect(errEl?.textContent).toContain("JSON"));
    expect(env.apiPost).not.toHaveBeenCalled();
  });

  it("edit: prefills and PATCHes the existing resource", async () => {
    const user = userEvent.setup();
    env.items = [
      { id: "r7", name: "api-x", status: "active", description: "d", endpoint: "https://e", auth_ref: "vault/1" },
    ];
    render(<ResourceRegistryPanel resourceType="apis" label="APIs" isAdmin />);
    await user.click(screen.getByRole("button", { name: "Edit" }));
    const dialog = screen.getByRole("dialog", { name: /Edit “api-x”/ });
    expect(within(dialog).getByLabelText(/Endpoint/)).toHaveValue("https://e");
    expect(within(dialog).getByLabelText(/Auth ref/)).toHaveValue("vault/1");
    await user.clear(within(dialog).getByLabelText(/Name/));
    await user.type(within(dialog).getByLabelText(/Name/), "api-y");
    await user.click(within(dialog).getByRole("button", { name: "Save changes" }));
    await vi.waitFor(() =>
      expect(env.apiPatch).toHaveBeenCalledWith("/registry/apis/r7", "tok", {
        name: "api-y",
        description: "d",
        endpoint: "https://e",
        auth_ref: "vault/1",
        manifest: undefined,
      }),
    );
  });

  it("cancel closes without any API call", async () => {
    const user = userEvent.setup();
    env.items = [{ id: "r1", name: "s1", status: "active" }];
    render(<ResourceRegistryPanel resourceType="skills" label="Skills" isAdmin />);
    await user.click(screen.getByRole("button", { name: "+ Register skill" }));
    const dialog = screen.getByRole("dialog", { name: "Register skill" });
    await user.click(within(dialog).getByRole("button", { name: "Cancel" }));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(env.apiPost).not.toHaveBeenCalled();
  });
});
