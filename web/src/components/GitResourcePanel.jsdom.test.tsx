// TG-4b (S-244-series): jsdom behaviour tests for the BYO git
// resource registration panel + modal. Mirrors the REST panel posture:
// admin affordances, empty/error states, write-only token, repo
// allowlist validation, create/edit API wiring against
// /registry/git.
import { render, screen, waitFor } from "@testing-library/react";
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

import { GitResourcePanel, gitFormFromResource } from "./GitResourcePanel";

const gitRow = {
  id: "g1",
  name: "gh-main",
  description: "github host",
  status: "active",
  endpoint_config: { base_url: "https://github.com" },
  policy_ceiling: { repos_allow: ["acme/*", "ross/app"], allow_push: true, rate_per_min: 30 },
};

beforeEach(() => {
  env.items = [];
  env.error = "";
  env.loading = false;
  env.apiPost = vi.fn().mockResolvedValue({});
  env.apiPatch = vi.fn().mockResolvedValue({});
  env.refresh = vi.fn();
  env.onDeleteDeleted = null;
});

describe("gitFormFromResource", () => {
  it("prefills everything except the secret token", () => {
    const form = gitFormFromResource(gitRow as never);
    expect(form.name).toBe("gh-main");
    expect(form.baseUrl).toBe("https://github.com");
    expect(form.reposAllow).toBe("acme/*, ross/app");
    expect(form.allowPush).toBe(true);
    expect(form.ratePerMin).toBe("30");
    expect(form.secrets).toEqual({});
  });

  it("falls back safely on a bare row", () => {
    const form = gitFormFromResource({ id: "g2", name: "bare" } as never);
    expect(form.baseUrl).toBe("");
    expect(form.reposAllow).toBe("");
    expect(form.allowPush).toBe(false);
    expect(form.ratePerMin).toBe("");
  });
});

describe("GitResourcePanel states", () => {
  it("member sees the list but no admin affordances", () => {
    env.items = [gitRow];
    render(<GitResourcePanel isAdmin={false} />);
    expect(screen.getByText("gh-main")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Register/ })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Edit" })).not.toBeInTheDocument();
  });

  it("admin sees register, edit and delete affordances plus the ceiling summary", () => {
    env.items = [gitRow];
    render(<GitResourcePanel isAdmin />);
    expect(screen.getByRole("button", { name: "+ Register git resource" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Edit" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "delete-btn" })).toBeInTheDocument();
    env.onDeleteDeleted?.();
    expect(env.refresh).toHaveBeenCalled();
    expect(screen.getByText(/repos: acme\/\*, ross\/app/)).toBeInTheDocument();
    expect(screen.getByText(/push: allowed/)).toBeInTheDocument();
    expect(screen.getByText(/base: https:\/\/github\.com/)).toBeInTheDocument();
  });

  it("renders fallbacks for a bare row (no endpoint config, no ceiling, unknown status)", () => {
    env.items = [{ id: "g3", name: "bare", status: "unknown" }];
    render(<GitResourcePanel isAdmin />);
    expect(screen.getByText("base: — · auth: bearer (write-only)")).toBeInTheDocument();
    expect(screen.getByText(/none \(default-deny\)/)).toBeInTheDocument();
  });

  it("maps a deprecated row to the paused chip", () => {
    env.items = [{ ...gitRow, id: "g4", status: "deprecated" }];
    render(<GitResourcePanel isAdmin />);
    expect(screen.getByText("gh-main")).toBeInTheDocument();
  });

  it("shows the empty state when nothing is registered", async () => {
    render(<GitResourcePanel isAdmin />);
    await waitFor(() => expect(env.loading).toBe(false));
    expect(screen.getByText("No git resources registered")).toBeInTheDocument();
  });

  it("surfaces the list error", () => {
    env.error = "registry unreachable";
    render(<GitResourcePanel isAdmin />);
    expect(screen.getByText("registry unreachable")).toBeInTheDocument();
  });
});

describe("GitResourceModal register flow", () => {
  it("submits the CP payload with write-only auth and repo ceiling", async () => {
    const user = userEvent.setup();
    render(<GitResourcePanel isAdmin />);
    await user.click(screen.getByRole("button", { name: "+ Register git resource" }));

    await user.type(screen.getByLabelText("Name"), "gh-main");
    await user.type(screen.getByLabelText("Description"), "github host");
    await user.type(screen.getByLabelText("Base URL"), "https://github.com");
    await user.type(
      screen.getByLabelText("Access token (PAT / installation token)"),
      "ghp_fake-git-register-DO-NOT-USE",
    );
    await user.type(screen.getByLabelText(/Allowed repos/), "acme/*\nross/app");
    await user.click(screen.getByLabelText("Allow push"));
    await user.type(screen.getByLabelText("Rate / min"), "30");

    await user.click(screen.getByRole("button", { name: "Register" }));

    await waitFor(() => expect(env.apiPost).toHaveBeenCalledTimes(1));
    const [path, token, body] = env.apiPost.mock.calls[0] as [string, string, Record<string, unknown>];
    expect(path).toBe("/registry/git");
    expect(token).toBe("tok");
    expect(body.name).toBe("gh-main");
    expect(body.description).toBe("github host");
    expect(body.endpoint_config).toEqual({ base_url: "https://github.com" });
    expect(body.policy_ceiling).toEqual({ repos_allow: ["acme/*", "ross/app"], allow_push: true, rate_per_min: 30 });
    expect(body.auth).toEqual({ token: "ghp_fake-git-register-DO-NOT-USE" });
    expect(document.body.textContent).not.toContain("ghp_fake-git-register-DO-NOT-USE");
  });

  it("blocks submit when the repo allowlist is empty (default-deny)", async () => {
    const user = userEvent.setup();
    render(<GitResourcePanel isAdmin />);
    await user.click(screen.getByRole("button", { name: "+ Register git resource" }));

    await user.type(screen.getByLabelText("Name"), "gh-main");
    await user.type(screen.getByLabelText("Base URL"), "https://github.com");
    await user.type(screen.getByLabelText("Access token (PAT / installation token)"), "ghp_fake-git-norepos-DO-NOT-USE");
    await user.click(screen.getByRole("button", { name: "Register" }));

    expect(await screen.findByText(/At least one repo pattern/)).toBeInTheDocument();
    expect(env.apiPost).not.toHaveBeenCalled();
  });

  it("blocks submit when the token is missing", async () => {
    const user = userEvent.setup();
    render(<GitResourcePanel isAdmin />);
    await user.click(screen.getByRole("button", { name: "+ Register git resource" }));

    await user.type(screen.getByLabelText("Name"), "gh-main");
    await user.type(screen.getByLabelText("Base URL"), "https://github.com");
    await user.type(screen.getByLabelText(/Allowed repos/), "acme/*");
    await user.click(screen.getByRole("button", { name: "Register" }));

    expect(await screen.findByText(/Access token .* is required/)).toBeInTheDocument();
    expect(env.apiPost).not.toHaveBeenCalled();
  });

  it("surfaces a save failure and keeps the modal open", async () => {
    env.apiPost = vi.fn(async () => {
      throw new Error("registry rejected");
    });
    const user = userEvent.setup();
    render(<GitResourcePanel isAdmin />);
    await user.click(screen.getByRole("button", { name: "+ Register git resource" }));

    await user.type(screen.getByLabelText("Name"), "gh-main");
    await user.type(screen.getByLabelText("Base URL"), "https://github.com");
    await user.type(screen.getByLabelText("Access token (PAT / installation token)"), "ghp_fake-git-fail-DO-NOT-USE");
    await user.type(screen.getByLabelText(/Allowed repos/), "acme/*");
    await user.click(screen.getByRole("button", { name: "Register" }));

    expect(await screen.findByText("registry rejected")).toBeInTheDocument();
  });

  it("uses a generic error message for non-Error save failures", async () => {
    env.apiPost = vi.fn(async () => {
      throw "boom";
    });
    const user = userEvent.setup();
    render(<GitResourcePanel isAdmin />);
    await user.click(screen.getByRole("button", { name: "+ Register git resource" }));

    await user.type(screen.getByLabelText("Name"), "gh-main");
    await user.type(screen.getByLabelText("Base URL"), "https://github.com");
    await user.type(screen.getByLabelText("Access token (PAT / installation token)"), "ghp_fake-git-boom-DO-NOT-USE");
    await user.type(screen.getByLabelText(/Allowed repos/), "acme/*");
    await user.click(screen.getByRole("button", { name: "Register" }));

    expect(await screen.findByText("save failed")).toBeInTheDocument();
  });
});

describe("GitResourceModal edit flow", () => {
  it("prefills without the secret and patches WITHOUT auth when the token is left blank", async () => {
    env.items = [gitRow];
    const user = userEvent.setup();
    render(<GitResourcePanel isAdmin />);
    await user.click(screen.getByRole("button", { name: "Edit" }));

    expect((screen.getByLabelText("Name") as HTMLInputElement).value).toBe("gh-main");
    expect((screen.getByLabelText("Base URL") as HTMLInputElement).value).toBe("https://github.com");
    expect((screen.getByLabelText(/Allowed repos/) as HTMLTextAreaElement).value).toBe("acme/*, ross/app");
    expect((screen.getByLabelText("Allow push") as HTMLInputElement).checked).toBe(true);
    expect((screen.getByLabelText("Access token (PAT / installation token)") as HTMLInputElement).value).toBe("");

    await user.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(env.apiPatch).toHaveBeenCalledTimes(1));
    const [path, , body] = env.apiPatch.mock.calls[0] as [string, string, Record<string, unknown>];
    expect(path).toBe("/registry/git/g1");
    expect(body).not.toHaveProperty("auth");
  });

  it("sends rotated auth when a new token is entered", async () => {
    env.items = [gitRow];
    const user = userEvent.setup();
    render(<GitResourcePanel isAdmin />);
    await user.click(screen.getByRole("button", { name: "Edit" }));

    await user.type(screen.getByLabelText("Access token (PAT / installation token)"), "ghp_fake-git-rotate-DO-NOT-USE");
    await user.click(screen.getByRole("button", { name: "Save changes" }));

    await waitFor(() => expect(env.apiPatch).toHaveBeenCalledTimes(1));
    const [, , body] = env.apiPatch.mock.calls[0] as [string, string, Record<string, unknown>];
    expect(body.auth).toEqual({ token: "ghp_fake-git-rotate-DO-NOT-USE" });
  });

  it("cancel closes the modal without any API call", async () => {
    env.items = [gitRow];
    const user = userEvent.setup();
    render(<GitResourcePanel isAdmin />);
    await user.click(screen.getByRole("button", { name: "Edit" }));
    await user.click(screen.getByRole("button", { name: "Cancel" }));

    expect(screen.queryByLabelText("Access token (PAT / installation token)")).toBeNull();
    expect(env.apiPatch).not.toHaveBeenCalled();
    expect(env.apiPost).not.toHaveBeenCalled();
  });
});
