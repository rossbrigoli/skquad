// S-189: jsdom behaviour tests for the merged Platform settings tab.
// The lib/platformSettings diff/merge logic is real here (unit-tested in
// its own suite); the transport (apiGet/apiPut) is mocked so we can lock
// the WIRE contract: one PUT carrying ONLY the changed keys, the
// post-save confirmation text, load/save error surfacing, and the
// dirty-state gating of the single Save button.
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";

const env = vi.hoisted(() => {
  // Hoisted with the mock factory: the component imports ApiError from
  // lib/api and our mock must expose a real class for `instanceof`.
  class FakeApiError extends Error {
    status: number;
    constructor(status: number, message: string) {
      super(message);
      this.status = status;
    }
  }
  return {
    token: "tok",
    mode: "token" as string,
    loadResponse: null as Record<string, unknown> | null,
    loadError: null as Error | null,
    putCalls: [] as { path: string; token: string; body: unknown }[],
    putError: null as Error | null,
    FakeApiError,
  };
});

const FakeApiError = env.FakeApiError;

vi.mock("../lib/auth", () => ({
  useAuth: () => ({ token: env.token, mode: env.mode }),
}));

vi.mock("../lib/api", () => ({
  ApiError: env.FakeApiError,
  apiGet: async (path: string, token: string) => {
    if (env.loadError) throw env.loadError;
    void path;
    void token;
    return env.loadResponse ?? {};
  },
  apiPut: async (path: string, token: string, body: unknown) => {
    env.putCalls.push({ path, token, body });
    if (env.putError) throw env.putError;
    return body;
  },
}));

import { PlatformSettingsTab } from "./PlatformSettingsTab";

beforeEach(() => {
  env.token = "tok";
  env.mode = "token";
  env.loadResponse = { idle_scale_to_zero_minutes: 20, embedder_runtime: "cuda" };
  env.loadError = null;
  env.putCalls = [];
  env.putError = null;
});

async function mount() {
  render(<PlatformSettingsTab />);
  await screen.findByText("Current platform setting: 20 minute(s).");
}

describe("PlatformSettingsTab load", () => {
  it("starts disabled and shows current values once loaded", async () => {
    render(<PlatformSettingsTab />);
    expect(screen.getByRole("button", { name: "Save" })).toBeDisabled();
    await screen.findByText("Current platform setting: 20 minute(s).");
    expect(screen.getByLabelText("Scale to zero after minutes of inactivity")).toHaveValue("20");
    expect(screen.getByLabelText("Embedder compute runtime")).toHaveValue("cuda");
    expect(screen.getByText("Current platform setting: CUDA (NVIDIA).")).toBeInTheDocument();
    // Clean form → Save stays disabled.
    expect(screen.getByRole("button", { name: "Save" })).toBeDisabled();
  });

  it("missing keys fall back to platform defaults", async () => {
    env.loadResponse = {};
    render(<PlatformSettingsTab />);
    await screen.findByText("Current platform setting: 15 minute(s).");
    expect(screen.getByLabelText("Embedder compute runtime")).toHaveValue("auto");
  });

  it("load failure surfaces a role=alert error", async () => {
    env.loadError = new Error("boom");
    render(<PlatformSettingsTab />);
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("Load failed: boom");
    expect(screen.getByRole("button", { name: "Save" })).toBeDisabled();
  });

  it("oidc mode sends the empty bearer token (cookie session)", async () => {
    env.mode = "oidc";
    let seenToken: string | undefined;
    // Re-mock capture isn't possible post-import; assert via the PUT path instead.
    env.loadResponse = { idle_scale_to_zero_minutes: 5, embedder_runtime: "cpu" };
    render(<PlatformSettingsTab />);
    await screen.findByText("Current platform setting: 5 minute(s).");
    await userEvent.clear(screen.getByLabelText("Scale to zero after minutes of inactivity"));
    await userEvent.type(screen.getByLabelText("Scale to zero after minutes of inactivity"), "7");
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    await vi.waitFor(() => expect(env.putCalls).toHaveLength(1));
    seenToken = env.putCalls[0].token;
    expect(seenToken).toBe("");
  });
});

describe("PlatformSettingsTab save", () => {
  it("sends ONE PUT with only the changed idle minutes and confirms", async () => {
    await mount();
    const input = screen.getByLabelText("Scale to zero after minutes of inactivity");
    await userEvent.clear(input);
    await userEvent.type(input, "30");
    expect(screen.getByText("Unsaved changes")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    await vi.waitFor(() => expect(env.putCalls).toHaveLength(1));
    expect(env.putCalls[0].path).toBe("/admin/settings");
    expect(env.putCalls[0].body).toEqual({ idle_scale_to_zero_minutes: 30 });
    expect(screen.getByText("Saved: agents scale to zero after 30 minute(s) without activity.")).toBeInTheDocument();
    expect(screen.queryByText("Unsaved changes")).not.toBeInTheDocument();
  });

  it("runtime change to auto confirms with the detection wording", async () => {
    await mount();
    await userEvent.selectOptions(screen.getByLabelText("Embedder compute runtime"), "auto");
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    await vi.waitFor(() => expect(env.putCalls).toHaveLength(1));
    expect(env.putCalls[0].body).toEqual({ embedder_runtime: "auto" });
    expect(screen.getByText(/whatever GPU it detects/)).toBeInTheDocument();
  });

  it("explicit runtime change confirms with the runtime name", async () => {
    await mount();
    await userEvent.selectOptions(screen.getByLabelText("Embedder compute runtime"), "vulkan");
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    await vi.waitFor(() => expect(env.putCalls).toHaveLength(1));
    expect(env.putCalls[0].body).toEqual({ embedder_runtime: "vulkan" });
    expect(screen.getByText("Saved: the embedder rolls onto the VULKAN runtime.")).toBeInTheDocument();
  });

  it("both changes ride the same PUT", async () => {
    await mount();
    const input = screen.getByLabelText("Scale to zero after minutes of inactivity");
    await userEvent.clear(input);
    await userEvent.type(input, "45");
    await userEvent.selectOptions(screen.getByLabelText("Embedder compute runtime"), "cpu");
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    await vi.waitFor(() => expect(env.putCalls).toHaveLength(1));
    expect(env.putCalls[0].body).toEqual({ idle_scale_to_zero_minutes: 45, embedder_runtime: "cpu" });
  });

  it("save failure surfaces the ApiError message", async () => {
    await mount();
    env.putError = new FakeApiError(500, "kaboom");
    const input = screen.getByLabelText("Scale to zero after minutes of inactivity");
    await userEvent.clear(input);
    await userEvent.type(input, "60");
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("Save failed: kaboom");
  });

  it("non-ApiError save failure gets the generic message", async () => {
    await mount();
    env.putError = new TypeError("network went away");
    const input = screen.getByLabelText("Scale to zero after minutes of inactivity");
    await userEvent.clear(input);
    await userEvent.type(input, "90");
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("Save failed.");
  });

  it("out-of-range minutes blocks submit with the validation error", async () => {
    await mount();
    // Make the form dirty via runtime so Save is enabled while minutes are invalid.
    await userEvent.selectOptions(screen.getByLabelText("Embedder compute runtime"), "cpu");
    const input = screen.getByLabelText("Scale to zero after minutes of inactivity");
    await userEvent.clear(input);
    await userEvent.type(input, "999");
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("Must be between 1 and 240 minutes.");
    // Nothing was PUT — validation stopped the request.
    expect(env.putCalls).toHaveLength(0);
  });

  it("typing clears a stale error and note", async () => {
    await mount();
    env.putError = new FakeApiError(500, "kaboom");
    const input = screen.getByLabelText("Scale to zero after minutes of inactivity");
    await userEvent.clear(input);
    await userEvent.type(input, "60");
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    await screen.findByRole("alert");
    env.putError = null;
    await userEvent.type(input, "5");
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });
});
