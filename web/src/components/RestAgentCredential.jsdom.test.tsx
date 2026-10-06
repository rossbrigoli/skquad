// TG-4c (S-259): jsdom tests for the per-agent REST credential control.
// Fake token values only; asserts the write-only posture (inputs never
// prefilled, values never rendered) and the own-vs-default indicator.
import React from "react";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";

const env = vi.hoisted(() => ({
  probe: {
    resource_id: "r1",
    agent_id: "a1",
    auth_kind: "bearer" as string,
    has_own_credential: false,
    source: "resource_default" as string,
  },
  apiGet: vi.fn(),
  apiPut: vi.fn(),
  apiDelete: vi.fn(),
}));

vi.mock("../lib/api", async (importOriginal) => {
  const mod = await importOriginal<typeof import("../lib/api")>();
  return {
    ...mod,
    apiGet: (...args: unknown[]) => env.apiGet(...args),
    apiPut: (...args: unknown[]) => env.apiPut(...args),
    apiDelete: (...args: unknown[]) => env.apiDelete(...args),
  };
});

import { RestAgentCredential } from "./RestAgentCredential";

beforeEach(() => {
  env.probe = {
    resource_id: "r1",
    agent_id: "a1",
    auth_kind: "bearer",
    has_own_credential: false,
    source: "resource_default",
  };
  env.apiGet = vi.fn(async () => env.probe);
  env.apiPut = vi.fn(async () => {
    env.probe = { ...env.probe, has_own_credential: true, source: "agent" };
  });
  env.apiDelete = vi.fn(async () => {
    env.probe = { ...env.probe, has_own_credential: false, source: "resource_default" };
  });
});

describe("RestAgentCredential", () => {
  it("shows the resource-default indicator and a Set own button", async () => {
    render(<RestAgentCredential resourceId="r1" agentId="a1" token="tok" />);
    const badge = await screen.findByTestId("cred-indicator");
    expect(badge.textContent).toContain("resource default");
    expect(badge.textContent).toContain("bearer");
    expect(screen.getByRole("button", { name: "Set own" })).toBeTruthy();
  });

  it("set-own flow: write-only input, PUT payload, refreshed to own", async () => {
    const user = userEvent.setup();
    render(<RestAgentCredential resourceId="r1" agentId="a1" token="tok" />);
    await screen.findByTestId("cred-indicator");
    await user.click(screen.getByRole("button", { name: "Set own" }));

    const input = screen.getByLabelText("Bearer token") as HTMLInputElement;
    expect(input.type).toBe("password");
    expect(input.value).toBe(""); // never prefilled
    await user.type(input, "fake-pat-agentA-DO-NOT-USE");
    await user.click(screen.getByRole("button", { name: "Save credential" }));

    await waitFor(() => expect(env.apiPut).toHaveBeenCalledTimes(1));
    const [path, token, body] = env.apiPut.mock.calls[0] as [string, string, { auth: Record<string, string> }];
    expect(path).toBe("/registry/rest/r1/agent-credentials/a1");
    expect(token).toBe("tok");
    expect(body.auth).toEqual({ token: "fake-pat-agentA-DO-NOT-USE" });

    await waitFor(() => expect(screen.getByTestId("cred-indicator").textContent).toContain("this agent's own"));
    // The value is never rendered as text anywhere.
    expect(document.body.textContent).not.toContain("fake-pat-agentA-DO-NOT-USE");
  });

  it("clear flow falls back to the resource default", async () => {
    env.probe = { ...env.probe, has_own_credential: true, source: "agent" };
    const user = userEvent.setup();
    render(<RestAgentCredential resourceId="r1" agentId="a1" token="tok" />);
    await screen.findByTestId("cred-indicator");
    expect(screen.getByTestId("cred-indicator").textContent).toContain("this agent's own");
    expect(screen.getByRole("button", { name: "Rotate own" })).toBeTruthy();

    await user.click(screen.getByRole("button", { name: "Clear (use default)" }));
    await waitFor(() => expect(env.apiDelete).toHaveBeenCalledWith("/registry/rest/r1/agent-credentials/a1", "tok"));
    await waitFor(() => expect(screen.getByTestId("cred-indicator").textContent).toContain("resource default"));
  });

  it("renders nothing when the credential probe fails", async () => {
    env.apiGet = vi.fn(async () => {
      throw new Error("probe unavailable");
    });

    const { container } = render(<RestAgentCredential resourceId="r1" agentId="a1" token="tok" />);

    await waitFor(() => expect(env.apiGet).toHaveBeenCalledTimes(1));
    expect(container.textContent).toBe("");
  });

  it("hides credential actions for auth_kind none", async () => {
    env.probe = { ...env.probe, auth_kind: "none", has_own_credential: false, source: "resource_default" };

    render(<RestAgentCredential resourceId="r1" agentId="a1" token="tok" />);

    const badge = await screen.findByTestId("cred-indicator");
    expect(badge.textContent).toContain("resource default (none)");
    expect(screen.queryByRole("button", { name: "Set own" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Rotate own" })).toBeNull();
  });

  it("falls back to no editable fields for an unknown auth kind", async () => {
    env.probe = { ...env.probe, auth_kind: "bogus-kind", has_own_credential: false, source: "resource_default" };

    render(<RestAgentCredential resourceId="r1" agentId="a1" token="tok" />);

    expect((await screen.findByTestId("cred-indicator")).textContent).toContain("bogus-kind");
    expect(screen.queryByRole("button", { name: "Set own" })).toBeNull();
  });

  it("renders non-secret fields as text and secret fields as password", async () => {
    env.probe = { ...env.probe, auth_kind: "basic", has_own_credential: false, source: "resource_default" };
    const user = userEvent.setup();
    render(<RestAgentCredential resourceId="r1" agentId="a1" token="tok" />);
    await screen.findByTestId("cred-indicator");
    await user.click(screen.getByRole("button", { name: "Set own" }));

    expect((screen.getByLabelText("Username") as HTMLInputElement).type).toBe("text");
    expect((screen.getByLabelText("Password") as HTMLInputElement).type).toBe("password");
  });

  it("shows save errors without echoing secret values", async () => {
    env.apiPut = vi.fn(async () => {
      throw new Error("store failed");
    });
    const user = userEvent.setup();
    render(<RestAgentCredential resourceId="r1" agentId="a1" token="tok" />);
    await screen.findByTestId("cred-indicator");
    await user.click(screen.getByRole("button", { name: "Set own" }));
    await user.type(screen.getByLabelText("Bearer token"), "fake-pat-save-error-DO-NOT-USE");
    await user.click(screen.getByRole("button", { name: "Save credential" }));

    expect(await screen.findByRole("alert")).toHaveTextContent("store failed");
    expect(document.body.textContent).not.toContain("fake-pat-save-error-DO-NOT-USE");
    expect(screen.getByRole("button", { name: "Save credential" })).toBeTruthy();
  });

  it("uses a generic save error for non-Error failures", async () => {
    env.apiPut = vi.fn(async () => {
      throw "boom";
    });
    const user = userEvent.setup();
    render(<RestAgentCredential resourceId="r1" agentId="a1" token="tok" />);
    await screen.findByTestId("cred-indicator");
    await user.click(screen.getByRole("button", { name: "Set own" }));
    await user.click(screen.getByRole("button", { name: "Save credential" }));

    expect(await screen.findByRole("alert")).toHaveTextContent("could not store the credential");
  });

  it("cancel exits edit mode and clears write-only input state", async () => {
    const user = userEvent.setup();
    render(<RestAgentCredential resourceId="r1" agentId="a1" token="tok" />);
    await screen.findByTestId("cred-indicator");
    await user.click(screen.getByRole("button", { name: "Set own" }));
    await user.type(screen.getByLabelText("Bearer token"), "fake-pat-cancel-DO-NOT-USE");
    await user.click(screen.getByRole("button", { name: "Cancel" }));

    expect(screen.queryByLabelText("Bearer token")).toBeNull();
    await user.click(screen.getByRole("button", { name: "Set own" }));
    expect((screen.getByLabelText("Bearer token") as HTMLInputElement).value).toBe("");
  });

  it("shows clear errors and keeps the own credential indicator", async () => {
    env.probe = { ...env.probe, has_own_credential: true, source: "agent" };
    env.apiDelete = vi.fn(async () => {
      throw new Error("clear failed");
    });
    const user = userEvent.setup();
    render(<RestAgentCredential resourceId="r1" agentId="a1" token="tok" />);
    await screen.findByTestId("cred-indicator");
    await user.click(screen.getByRole("button", { name: "Clear (use default)" }));

    expect(await screen.findByRole("alert")).toHaveTextContent("clear failed");
    expect(screen.getByTestId("cred-indicator").textContent).toContain("this agent's own");
  });

  it("uses a generic clear error for non-Error failures", async () => {
    env.probe = { ...env.probe, has_own_credential: true, source: "agent" };
    env.apiDelete = vi.fn(async () => {
      throw "boom";
    });
    const user = userEvent.setup();
    render(<RestAgentCredential resourceId="r1" agentId="a1" token="tok" />);
    await screen.findByTestId("cred-indicator");
    await user.click(screen.getByRole("button", { name: "Clear (use default)" }));

    expect(await screen.findByRole("alert")).toHaveTextContent("could not clear the credential");
  });
});
