// S-189: jsdom behaviour tests for the built-in tool config surface
// (BuiltinToolConfig + ToolCard + DeniedPatternsEditor). Covers loading
// / unseeded / error notices, per-tool form variants, save success and
// failure paths (client validation, 403, 400), and the denied-pattern
// row editor.
import React from "react";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError } from "../lib/api";
import type { BuiltinTool } from "../lib/builtinTools";

const env = vi.hoisted(() => ({
  list: null as unknown,
  loading: false,
  error: "",
  apiPatch: vi.fn(),
}));

vi.mock("../lib/auth", () => ({
  useAuth: () => ({ token: "tok", mode: "token", user: { id: "u1" }, logout: vi.fn() }),
}));

vi.mock("../lib/useApi", () => ({
  useApi: () => ({ data: env.list, loading: env.loading, error: env.error, refresh: vi.fn() }),
}));

vi.mock("../lib/api", async (importOriginal) => {
  const mod = await importOriginal<typeof import("../lib/api")>();
  return { ...mod, apiPatch: (...args: unknown[]) => env.apiPatch(...args) };
});

import { BuiltinToolConfig, DeniedPatternsEditor, ToolCard } from "./BuiltinToolConfig";

const execTool: BuiltinTool = {
  name: "exec",
  enabled: true,
  policy: { timeoutSeconds: 45, maxOutputBytes: 4096, deniedPatterns: ["rm\\s+-rf"] },
  updatedAt: "2026-10-01T04:00:00Z",
  updatedBy: "ross",
};

beforeEach(() => {
  env.list = null;
  env.loading = false;
  env.error = "";
  env.apiPatch = vi.fn();
});

describe("BuiltinToolConfig load states", () => {
  it("shows the loading notice while fetching", () => {
    env.loading = true;
    render(<BuiltinToolConfig name="exec" />);
    expect(screen.getByText("Loading tool configuration…")).toBeInTheDocument();
  });

  it("shows the unseeded notice when the tool is missing", () => {
    env.list = { tools: [] };
    render(<BuiltinToolConfig name="exec" />);
    expect(screen.getByText(/has not seeded/)).toBeInTheDocument();
  });

  it("surfaces the list error notice", () => {
    env.error = "nope";
    render(<BuiltinToolConfig name="exec" />);
    expect(screen.getByText("nope")).toBeInTheDocument();
  });

  it("renders the card for the matching tool from the list", () => {
    env.list = { tools: [execTool] };
    render(<BuiltinToolConfig name="exec" />);
    expect(screen.getByText("exec — run terminal commands")).toBeInTheDocument();
    expect(screen.getByText("Updated", { exact: false })).toHaveTextContent("ross");
  });

  it("saves through apiPatch with the built payload and overlays the result", async () => {
    env.list = { tools: [execTool] };
    const updated: BuiltinTool = { ...execTool, enabled: false };
    env.apiPatch = vi.fn().mockResolvedValue(updated);
    const user = userEvent.setup();
    render(<BuiltinToolConfig name="exec" />);
    // S-241: the enable control is the shared ToggleSwitch (role=switch),
    // not a bare checkbox.
    expect(screen.queryByRole("checkbox", { name: "Enable exec" })).not.toBeInTheDocument();
    expect(screen.getByRole("switch", { name: "Enable exec" })).toBeChecked();
    await user.click(screen.getByRole("switch", { name: "Enable exec" }));
    await user.click(screen.getByRole("button", { name: "Save exec" }));
    await vi.waitFor(() =>
      expect(env.apiPatch).toHaveBeenCalledWith("/admin/tools/exec", "tok", {
        enabled: false,
        policy: { timeoutSeconds: 45, maxOutputBytes: 4096, deniedPatterns: ["rm\\s+-rf"] },
      }),
    );
    expect(screen.getByText(/status: disabled/)).toBeInTheDocument();
    expect(screen.getByText("Saved.")).toBeInTheDocument();
  });
});

describe("ToolCard per-tool form variants", () => {
  it("web_fetch shows max bytes + private-network toggle", () => {
    render(
      <ToolCard
        tool={{ name: "web_fetch", enabled: false, policy: { maxBytes: 123, allowPrivateNetwork: true } }}
        onSave={vi.fn()}
        onUpdated={vi.fn()}
      />,
    );
    expect(screen.getByLabelText(/Max bytes/)).toHaveValue(123);
    expect(screen.getByRole("checkbox", { name: /Allow private network/ })).toBeChecked();
    expect(screen.queryByLabelText(/Max output/)).not.toBeInTheDocument();
  });

  it("web_search shows provider select with contract providers", () => {
    render(
      <ToolCard
        tool={{ name: "web_search", enabled: true, policy: { provider: "brave", maxResults: 5 } }}
        onSave={vi.fn()}
        onUpdated={vi.fn()}
      />,
    );
    const select = screen.getByLabelText(/Provider/) as HTMLSelectElement;
    expect(select.value).toBe("brave");
    expect(screen.getByRole("option", { name: "perplexity" })).toBeInTheDocument();
    expect(screen.getByLabelText(/Max results/)).toHaveValue(5);
  });

  it("send_message shows max message chars only", () => {
    render(
      <ToolCard
        tool={{ name: "send_message", enabled: true, policy: { maxMessageChars: 4000 } }}
        onSave={vi.fn()}
        onUpdated={vi.fn()}
      />,
    );
    expect(screen.getByLabelText(/Max message chars/)).toHaveValue(4000);
    expect(screen.queryByLabelText(/Denied patterns/)).not.toBeInTheDocument();
  });
});

describe("ToolCard save paths", () => {
  it("blocks submit on client-side validation without calling the API", async () => {
    const onSave = vi.fn();
    const user = userEvent.setup();
    render(<ToolCard tool={execTool} onSave={onSave} onUpdated={vi.fn()} />);
    await user.clear(screen.getByLabelText(/Timeout/));
    await user.click(screen.getByRole("button", { name: "Save exec" }));
    expect(onSave).not.toHaveBeenCalled();
    expect(screen.getByRole("alert")).toHaveTextContent("timeoutSeconds must be a positive whole number");
  });

  it("rejects invalid denied regex client-side", async () => {
    const onSave = vi.fn();
    const user = userEvent.setup();
    render(
      <ToolCard
        tool={{ ...execTool, policy: { ...execTool.policy, deniedPatterns: ["[bad("] } }}
        onSave={onSave}
        onUpdated={vi.fn()}
      />,
    );
    await user.click(screen.getByRole("button", { name: "Save exec" }));
    expect(onSave).not.toHaveBeenCalled();
    expect(screen.getByRole("alert")).toHaveTextContent("invalid regex");
  });

  it("maps a 403 to the platform-admin hint", async () => {
    const user = userEvent.setup();
    render(
      <ToolCard
        tool={execTool}
        onSave={vi.fn().mockRejectedValue(new ApiError(403, "nope"))}
        onUpdated={vi.fn()}
      />,
    );
    await user.click(screen.getByRole("button", { name: "Save exec" }));
    await vi.waitFor(() =>
      expect(screen.getByRole("alert")).toHaveTextContent("requires the platform_admin role"),
    );
  });

  it("surfaces the server 400 message inline", async () => {
    const user = userEvent.setup();
    render(
      <ToolCard
        tool={execTool}
        onSave={vi.fn().mockRejectedValue(new ApiError(400, "policy.maxOutputBytes too big"))}
        onUpdated={vi.fn()}
      />,
    );
    await user.click(screen.getByRole("button", { name: "Save exec" }));
    await vi.waitFor(() =>
      expect(screen.getByRole("alert")).toHaveTextContent("policy.maxOutputBytes too big"),
    );
  });
});

describe("DeniedPatternsEditor", () => {
  it("adds, edits and removes pattern rows", async () => {
    // Stateful wrapper so the controlled editor actually re-renders.
    function Wrapper() {
      const [rows, setRows] = React.useState<string[]>(["first"]);
      return (
        <div>
          <DeniedPatternsEditor rows={rows} disabled={false} onChange={setRows} />
          <output>{JSON.stringify(rows)}</output>
        </div>
      );
    }
    const user = userEvent.setup();
    render(<Wrapper />);
    await user.type(screen.getByDisplayValue("first"), "x");
    expect(screen.getByRole("status")).toHaveTextContent('["firstx"]');

    await user.click(screen.getByRole("button", { name: "+ Add pattern" }));
    await user.type(screen.getAllByRole("textbox")[1], "second");
    expect(screen.getByRole("status")).toHaveTextContent('"second"');

    await user.click(screen.getByRole("button", { name: "Remove denied pattern 1" }));
    expect(screen.getByRole("status")).toHaveTextContent('["second"]');
  });

  it("disables all controls while busy", () => {
    render(<DeniedPatternsEditor rows={["a"]} disabled onChange={vi.fn()} />);
    expect(screen.getByDisplayValue("a")).toBeDisabled();
    expect(screen.getByRole("button", { name: "Remove denied pattern 1" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "+ Add pattern" })).toBeDisabled();
  });
});
