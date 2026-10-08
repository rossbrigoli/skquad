// S-189: jsdom behaviour tests for the Squad cockpit page (Overview).
// Covers metric-tile derivation (busy/idle/error agents, running vs
// stalled leases, done counts), the stalled section, the delete-squad
// confirm flow (apiDelete + redirect), and the new-agent modal submit.
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";

const env = vi.hoisted(() => ({
  squad: null as Record<string, unknown> | null,
  agents: [] as unknown[],
  tasks: [] as unknown[],
  metering: null as unknown,
  meteringError: "",
  meteringLoading: false,
  audit: [] as unknown[],
  apiPost: vi.fn(),
  apiDelete: vi.fn(),
  push: vi.fn(),
  refresh: vi.fn(),
  modelsNull: false,
}));

vi.mock("next/link", () => ({
  default: ({ href, children }: { href: string; children: React.ReactNode }) => (
    <a href={href}>{children}</a>
  ),
}));

vi.mock("next/navigation", () => ({
  useParams: () => ({ id: "sq1" }),
  useRouter: () => ({ push: env.push }),
}));

vi.mock("../../../components/AuthGate", () => ({
  AuthGate: ({ children }: { children: React.ReactNode }) => <>{children}</>,
}));

vi.mock("../../../components/AppShell", () => ({
  AppShell: ({ children }: { children: React.ReactNode }) => <>{children}</>,
}));

// The create-agent dialog is exercised by its own suite; here we only need
// its submit callback wiring.
vi.mock("../../../components/AgentForm", () => ({
  AgentFormModal: ({ onSubmit, onClose }: {
    onSubmit: (v: Record<string, unknown>) => Promise<void>;
    onClose: () => void;
  }) => (
    <div role="dialog" aria-label="New agent">
      <button type="button" onClick={() => void onSubmit({ name: "coder" })}>
        modal-submit
      </button>
      <button type="button" onClick={onClose}>
        modal-close
      </button>
    </div>
  ),
}));

vi.mock("../../../lib/auth", () => ({
  useAuth: () => ({ user: { id: "u1" }, token: "tok", mode: "token", logout: vi.fn() }),
}));

vi.mock("../../../lib/api", () => ({
  apiPost: (...args: unknown[]) => env.apiPost(...args),
  apiDelete: (...args: unknown[]) => env.apiDelete(...args),
}));

vi.mock("../../../lib/useApi", () => ({
  useApi: (path: string) => {
    let data: unknown = null;
    let error = "";
    let loading = false;
    if (path === "/squads") data = env.squad ? [env.squad] : [];
    if (path.startsWith("/squads/sq1/agents")) data = env.agents;
    if (path.startsWith("/squads/sq1/board")) data = { tasks: env.tasks };
    if (path.startsWith("/squads/sq1/metering")) {
      data = env.metering;
      error = env.meteringError;
      loading = env.meteringLoading;
    }
    if (path.startsWith("/squads/sq1/audit")) data = env.audit;
    if (path === "/models/me") data = env.modelsNull ? null : [];
    return { data, loading, error, refresh: env.refresh };
  },
}));

import SquadCockpitPage from "./page";

const future = new Date(Date.now() + 600_000).toISOString();
const past = new Date(Date.now() - 600_000).toISOString();

beforeEach(() => {
  env.squad = { id: "sq1", name: "Alpha Squad" };
  env.agents = [
    { id: "ag1", name: "coder", status: "busy" },
    { id: "ag2", name: "reviewer", status: "idle" },
    { id: "ag3", name: "broken", status: "error" },
  ];
  env.tasks = [
    { id: "t1", title: "running task", status: "in_progress", assignee_agent_id: "ag1", execution_id: "e1", lease_expires_at: future },
    { id: "t2", title: "stalled task", status: "in_progress", assignee_agent_id: "ag2", execution_id: "e2", lease_expires_at: past },
    { id: "t3", title: "done task", status: "done" },
  ];
  env.metering = { cost: 12.5, currency: "USD" };
  env.meteringError = "";
  env.audit = [];
  env.apiPost = vi.fn().mockResolvedValue({ id: "ag9" });
  env.apiDelete = vi.fn().mockResolvedValue(undefined);
  env.push = vi.fn();
  env.refresh = vi.fn();
});

function tile(label: string): HTMLElement {
  const tiles = screen
    .getAllByText(label)
    .map((el) => (el.closest(".metric") ?? el.parentElement) as HTMLElement);
  return tiles[0];
}

describe("SquadCockpitPage metrics", () => {
  it("derives agent, WIP and done counts from the loaded data", () => {
    render(<SquadCockpitPage />);
    expect(screen.getByRole("heading", { name: "Alpha Squad" })).toBeInTheDocument();
    // Agents tile: 3 total, 1 busy, 1 idle, 1 error suffix.
    const agentsTile = tile("Agents");
    expect(agentsTile).toHaveTextContent("3");
    expect(agentsTile).toHaveTextContent("1 busy");
    expect(agentsTile).toHaveTextContent("1 idle · 1 error");
    // Work in flight counts only live leases; stalled gets its own suffix.
    const wipTile = tile("Work in flight");
    expect(wipTile).toHaveTextContent("1");
    expect(wipTile).toHaveTextContent("1 stalled");
    // Tasks done 1 of 3.
    expect(tile("Tasks done")).toHaveTextContent("1");
    expect(tile("Tasks done")).toHaveTextContent("of 3 total");
  });

  it("shows the stalled section with holder + expired lease, linked to the task", () => {
    render(<SquadCockpitPage />);
    const stalled = screen.getByRole("link", { name: /stalled task/ });
    expect(stalled).toHaveAttribute("href", "/squads/sq1/tasks/t2");
    expect(stalled).toHaveTextContent("last held by reviewer");
  });

  it("hides the stalled section when nothing is stalled", () => {
    env.tasks = [env.tasks[0]];
    render(<SquadCockpitPage />);
    expect(screen.queryByText("Stalled")).not.toBeInTheDocument();
  });

  it("metering error shows the restricted-view hint", () => {
    env.metering = null;
    env.meteringError = "403";
    render(<SquadCockpitPage />);
    expect(tile("Squad spend")).toHaveTextContent("owner and platform admins only");
  });

  it("metering still loading shows the placeholder value", () => {
    env.meteringLoading = true;
    render(<SquadCockpitPage />);
    expect(tile("Squad spend")).toHaveTextContent("…");
  });

  it("activity feed resolves agent actors via the squad roster", async () => {
    env.audit = [
      { id: "a1", actor_type: "agent", actor_id: "ag1", action: "completed", resource_type: "task", resource_id: "t1" },
      { id: "a2", actor_type: "user", actor_id: "ross", action: "created", resource_type: "agent", resource_id: "ag2" },
    ];
    // The feed lives in a collapsed-by-default section — expand it first.
    render(<SquadCockpitPage />);
    await userEvent.click(screen.getByRole("button", { name: /Recent activity/ }));
    // actor_type=agent → resolved roster name inside the feed title.
    const coderActor = screen.getAllByText("coder").find((el) => el.closest(".entity-title"));
    expect(coderActor).toBeTruthy();
    expect(coderActor?.parentElement).toHaveTextContent("completed");
    // actor_type=user with no roster match → raw actor id.
    const userActor = screen.getByText("ross");
    expect(userActor.parentElement).toHaveTextContent("created");
  });
  it("stalled task with no assignee reads 'unassigned'", () => {
    env.tasks = [
      { id: "t5", title: "orphan", status: "in_progress", execution_id: "e5", lease_expires_at: past },
    ];
    render(<SquadCockpitPage />);
    expect(screen.getByRole("link", { name: /orphan/ })).toHaveTextContent("last held by unassigned");
  });

  it("healthy roster with a stalled task omits the error suffix", () => {
    env.agents = [{ id: "ag1", name: "coder", status: "idle" }];
    render(<SquadCockpitPage />);
    const wip = screen
      .getAllByText("Work in flight")
      .map((el) => el.closest(".metric") as HTMLElement)[0];
    expect(wip).toHaveTextContent("1 stalled");
    const agentsTile = screen
      .getAllByText("Agents")
      .map((el) => el.closest(".metric") as HTMLElement)[0];
    expect(agentsTile.textContent).not.toContain("error");
  });

  it("null audit and model payloads still render", () => {
    env.audit = null as unknown as unknown[];
    env.modelsNull = true;
    render(<SquadCockpitPage />);
    expect(screen.getByRole("button", { name: "+ New agent" })).toBeInTheDocument();
  });
});

describe("SquadCockpitPage delete flow", () => {
  it("confirms by name, deletes via API and redirects to /squads", async () => {
    const user = userEvent.setup();
    render(<SquadCockpitPage />);
    await user.click(screen.getByRole("button", { name: "Delete squad" }));
    const dialog = screen.getByRole("dialog");
    await user.type(within(dialog).getByRole("textbox"), "Alpha Squad");
    await user.click(within(dialog).getByRole("button", { name: "Delete" }));
    await vi.waitFor(() => expect(env.apiDelete).toHaveBeenCalledWith("/squads/sq1", "tok"));
    expect(env.push).toHaveBeenCalledWith("/squads");
  });

  it("delete button is disabled when the squad is missing", () => {
    env.squad = null;
    render(<SquadCockpitPage />);
    expect(screen.getByRole("button", { name: "Delete squad" })).toBeDisabled();
  });

  it("cancelling the confirm dialog skips the delete", async () => {
    const user = userEvent.setup();
    render(<SquadCockpitPage />);
    await user.click(screen.getByRole("button", { name: "Delete squad" }));
    const dialog = screen.getByRole("dialog");
    await user.click(within(dialog).getByRole("button", { name: "Cancel" }));
    expect(env.apiDelete).not.toHaveBeenCalled();
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });
});

describe("SquadCockpitPage layout (S-243)", () => {
  it("has no delete button in the top header", () => {
    const { container } = render(<SquadCockpitPage />);
    const head = screen.getByRole("heading", { name: "Alpha Squad" }).closest(".section-head");
    expect(head).toBeTruthy();
    expect(head?.querySelector("button")).toBeNull();
    // No danger button anywhere inside the header block.
    expect(head?.querySelector(".btn-danger")).toBeNull();
    expect(container.querySelector(".section-head .btn-danger")).toBeNull();
  });

  it("renders a Danger zone section at the bottom containing the delete button", () => {
    const { container } = render(<SquadCockpitPage />);
    const zone = screen.getByRole("region", { name: "Danger zone" });
    expect(zone).toBeInTheDocument();
    expect(zone.className).toContain("danger-zone");
    expect(within(zone).getByRole("button", { name: "Delete squad" })).toHaveClass("btn-danger");
    // Bottom of the page: comes after the Agents section and Recent activity.
    const agentsHeading = screen.getByRole("heading", { name: "Agents" });
    const activityToggle = screen.getByRole("button", { name: /Recent activity/ });
    expect(zone.compareDocumentPosition(agentsHeading) & Node.DOCUMENT_POSITION_CONTAINS).toBe(0);
    expect(agentsHeading.compareDocumentPosition(zone) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(activityToggle.compareDocumentPosition(zone) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    // It is the last .danger-zone and no other danger-zone sections leaked in.
    expect(container.querySelectorAll(".danger-zone")).toHaveLength(1);
  });

  it("+ New agent matches the larger list-page button size (no btn-sm)", () => {
    render(<SquadCockpitPage />);
    const btn = screen.getByRole("button", { name: "+ New agent" });
    expect(btn).toHaveClass("btn");
    expect(btn).toHaveClass("btn-primary");
    expect(btn).not.toHaveClass("btn-sm");
  });
});

describe("SquadCockpitPage new-agent modal", () => {
  it("opens, submits through apiPost and refreshes the agent list", async () => {
    const user = userEvent.setup();
    render(<SquadCockpitPage />);
    await user.click(screen.getByRole("button", { name: "+ New agent" }));
    const dialog = screen.getByRole("dialog", { name: "New agent" });
    await user.click(within(dialog).getByRole("button", { name: "modal-submit" }));
    await vi.waitFor(() =>
      expect(env.apiPost).toHaveBeenCalledWith("/squads/sq1/agents", "tok", { name: "coder" }),
    );
    await vi.waitFor(() => expect(env.refresh).toHaveBeenCalled());
    expect(screen.queryByRole("dialog", { name: "New agent" })).not.toBeInTheDocument();
  });

  it("closes without submitting", async () => {
    const user = userEvent.setup();
    render(<SquadCockpitPage />);
    await user.click(screen.getByRole("button", { name: "+ New agent" }));
    await user.click(within(screen.getByRole("dialog", { name: "New agent" })).getByRole("button", { name: "modal-close" }));
    expect(screen.queryByRole("dialog", { name: "New agent" })).not.toBeInTheDocument();
    expect(env.apiPost).not.toHaveBeenCalled();
  });
});
