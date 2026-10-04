// S-189: jsdom behaviour tests for the Squad Context page. Covers the
// one-shot prefill from the loaded squad, the combined mission+context
// save (buildContextSavePayload → apiPatch), the effective-prompt
// preview modal, empty-agent state, and revision restore wiring.
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";

const env = vi.hoisted(() => ({
  squad: null as Record<string, unknown> | null,
  agents: [] as unknown[],
  apiPatch: vi.fn(),
  refresh: vi.fn(),
}));

vi.mock("next/navigation", () => ({
  useParams: () => ({ id: "sq1" }),
}));

vi.mock("../../../../components/AuthGate", () => ({
  AuthGate: ({ children }: { children: React.ReactNode }) => <>{children}</>,
}));

vi.mock("../../../../components/AppShell", () => ({
  AppShell: ({ children }: { children: React.ReactNode }) => <>{children}</>,
}));

// The tier editor is covered by its own suite; expose its props as
// buttons so the page-level save wiring is observable.
vi.mock("../../../../components/PromptTierEditor", () => ({
  PromptTierEditor: ({ content, onChange, onSave, saveLabel }: {
    content: string;
    onChange: (v: string) => void;
    onSave: () => void | Promise<void>;
    saveLabel: string;
  }) => (
    <div>
      <textarea aria-label="tier-editor" value={content} onChange={(e) => onChange(e.target.value)} />
      <button type="button" onClick={() => void onSave()}>
        {saveLabel}
      </button>
    </div>
  ),
}));

vi.mock("../../../../components/PromptRevisionsPanel", () => ({
  PromptRevisionsPanel: ({ onRestore }: { onRestore: (content: string) => void }) => (
    <button type="button" onClick={() => onRestore("restored text")}>
      restore-revision
    </button>
  ),
}));

vi.mock("../../../../components/EffectivePromptPanel", () => ({
  EffectivePromptPanel: ({ agentId }: { agentId: string }) => (
    <div>effective-panel:{agentId}</div>
  ),
}));

vi.mock("../../../../lib/auth", () => ({
  useAuth: () => ({ user: { id: "u1" }, token: "tok", mode: "token", logout: vi.fn() }),
}));

vi.mock("../../../../lib/api", () => ({
  apiPatch: (...args: unknown[]) => env.apiPatch(...args),
}));

vi.mock("../../../../lib/useApi", () => ({
  useApi: (path: string) => {
    let data: unknown = null;
    if (path === "/squads") data = env.squad ? [env.squad] : [];
    if (path.startsWith("/squads/sq1/agents")) data = env.agents;
    return { data, loading: false, error: "", refresh: env.refresh };
  },
}));

import SquadPromptPage from "./page";

beforeEach(() => {
  env.squad = { id: "sq1", name: "Alpha", mission: "ship it", prompt: "squad context" };
  env.agents = [
    { id: "ag1", name: "coder", role: "builder" },
    { id: "ag2", name: "reviewer" },
  ];
  env.apiPatch = vi.fn().mockResolvedValue({});
  env.refresh = vi.fn();
});

describe("SquadPromptPage prefill + save", () => {
  it("prefills mission and context from the loaded squad", async () => {
    render(<SquadPromptPage />);
    // React 19 defers passive effects; wait for the one-shot prefill.
    await waitFor(() => expect(screen.getByLabelText(/Mission/)).toHaveValue("ship it"));
    expect(screen.getByLabelText("tier-editor")).toHaveValue("squad context");
  });

  it("save sends mission + prompt in one PATCH and refreshes", async () => {
    const user = userEvent.setup();
    render(<SquadPromptPage />);
    await waitFor(() => expect(screen.getByLabelText(/Mission/)).toHaveValue("ship it"));
    await user.clear(screen.getByLabelText(/Mission/));
    await user.type(screen.getByLabelText(/Mission/), "  new mission  ");
    await user.click(screen.getByRole("button", { name: "Save context" }));
    await vi.waitFor(() =>
      expect(env.apiPatch).toHaveBeenCalledWith("/squads/sq1", "tok", {
        mission: "new mission",
        prompt: "squad context",
      }),
    );
    expect(env.refresh).toHaveBeenCalled();
  });

  it("restore from revisions replaces the editor content", async () => {
    const user = userEvent.setup();
    render(<SquadPromptPage />);
    await user.click(screen.getByRole("button", { name: "restore-revision" }));
    expect(screen.getByLabelText("tier-editor")).toHaveValue("restored text");
  });
});

describe("SquadPromptPage preview section", () => {
  it("lists agents with role fallback and opens the preview modal", async () => {
    const user = userEvent.setup();
    render(<SquadPromptPage />);
    const rows = screen.getAllByRole("button", { name: "Preview effective prompt" });
    expect(rows).toHaveLength(2);
    expect(screen.getByText("builder")).toBeInTheDocument();
    expect(screen.getByText("no role set")).toBeInTheDocument();
    await user.click(rows[0]);
    const dialog = screen.getByRole("dialog", { name: /Effective prompt — coder/ });
    expect(within(dialog).getByText("effective-panel:ag1")).toBeInTheDocument();
    await user.click(within(dialog).getByRole("button", { name: "Close" }));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });

  it("shows the empty state when the squad has no agents", () => {
    env.agents = [];
    render(<SquadPromptPage />);
    expect(screen.getByText("No agents in this squad")).toBeInTheDocument();
  });
});
