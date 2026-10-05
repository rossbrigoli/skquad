// S-235: task detail page behaviour tests.
// Covers: actor names resolved from the backend display fields (with
// fallbacks), collapsible Thread / Status timeline sections (expanded by
// default, session-persisted), "Talk to <agent>" button in the lease
// row, and the "Back to board" link at the top.
import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import TaskDetailPage from "./page";

const { apiData } = vi.hoisted(() => ({ apiData: {} as Record<string, unknown> }));

vi.mock("next/link", () => ({
  default: ({ href, children, ...rest }: { href: string; children: React.ReactNode }) => (
    <a href={typeof href === "string" ? href : "#"} {...rest}>
      {children}
    </a>
  ),
}));

vi.mock("next/navigation", () => ({
  useParams: () => ({ id: "s1", tid: "t1" }),
  useRouter: () => ({ push: vi.fn() }),
}));

vi.mock("../../../../../lib/useApi", () => ({
  useApi: (path: string) => ({
    data: apiData[path] ?? null,
    loading: false,
    error: "",
    refresh: () => undefined,
  }),
}));

vi.mock("../../../../../lib/auth", () => ({
  useAuth: () => ({ token: "tok", mode: "token", user: null, authed: true }),
}));

// Chrome components are irrelevant here — pass children straight through.
vi.mock("../../../../../components/AuthGate", () => ({
  AuthGate: ({ children }: { children: React.ReactNode }) => <>{children}</>,
}));
vi.mock("../../../../../components/AppShell", () => ({
  AppShell: ({ children }: { children: React.ReactNode }) => <>{children}</>,
}));

const ROSS_ID = "11111111-1111-4111-8111-111111111111";
const BOB_ID = "22222222-2222-4222-8222-222222222222";
const GHOST_AGENT_ID = "33333333-3333-4333-8333-333333333333";

function seed() {
  apiData["/tasks/t1"] = {
    id: "t1",
    board_id: "b1",
    squad_id: "s1",
    title: "Fix the login bug",
    status: "in-progress",
    assignee_agent_id: BOB_ID,
    updated_at: new Date().toISOString(),
  };
  apiData["/squads/s1/agents"] = [
    { id: BOB_ID, squad_id: "s1", name: "Bob" },
    { id: GHOST_AGENT_ID, squad_id: "s1", name: "Ghost" },
  ];
  apiData["/tasks/t1/messages"] = [
    {
      id: "m1",
      from_type: "user",
      from_id: ROSS_ID,
      from_display: "Ross",
      to_agent_id: BOB_ID,
      squad_id: "s1",
      type: "consult",
      payload: { message: "can you look at this?" },
      status: "delivered",
      created_at: new Date().toISOString(),
    },
    {
      id: "m2",
      from_type: "agent",
      from_id: BOB_ID,
      from_display: "Bob",
      to_agent_id: "",
      squad_id: "s1",
      type: "reply",
      payload: { message: "on it" },
      status: "delivered",
      created_at: new Date().toISOString(),
    },
    {
      // Legacy row without the S-235 display fields — fallbacks apply.
      id: "m3",
      from_type: "user",
      from_id: ROSS_ID,
      to_agent_id: BOB_ID,
      squad_id: "s1",
      type: "consult",
      payload: { message: "legacy human" },
      status: "delivered",
      created_at: new Date().toISOString(),
    },
    {
      id: "m4",
      from_type: "agent",
      from_id: GHOST_AGENT_ID,
      to_agent_id: "",
      squad_id: "s1",
      type: "reply",
      payload: { message: "legacy agent" },
      status: "delivered",
      created_at: new Date().toISOString(),
    },
  ];
  apiData["/squads/s1/audit?limit=50"] = [
    {
      id: "e1",
      actor_type: "agent",
      actor_id: BOB_ID,
      actor_display: "Bob",
      action: "task.start",
      resource_type: "task",
      resource_id: "t1",
      timestamp: new Date().toISOString(),
    },
    {
      id: "e2",
      actor_type: "user",
      actor_id: ROSS_ID,
      actor_display: "Ross",
      action: "task.move",
      resource_type: "task",
      resource_id: "t1",
      timestamp: new Date().toISOString(),
    },
  ];
}

beforeEach(() => {
  window.sessionStorage.clear();
  seed();
});

describe("actor names (requirements 1 + 2)", () => {
  it("thread shows 'agent Bob' and 'Ross', never GUIDs", () => {
    render(<TaskDetailPage />);
    expect(screen.getByText("agent Bob")).toBeInTheDocument();
    expect(screen.getByText("Ross")).toBeInTheDocument();
    expect(document.body.textContent).not.toContain("22222222-2222");
    expect(document.body.textContent).not.toContain(ROSS_ID);
  });

  it("legacy thread rows fall back to 'you' / short agent id", () => {
    const { container } = render(<TaskDetailPage />);
    const rows = Array.from(container.querySelectorAll(".entity-row"));
    const humanRow = rows.find((r) => r.textContent?.includes("legacy human"));
    const agentRow = rows.find((r) => r.textContent?.includes("legacy agent"));
    expect(humanRow?.textContent).toContain("you");
    expect(agentRow?.textContent).toContain(`agent ${GHOST_AGENT_ID.slice(0, 8)}`);
  });

  it("status timeline names the actor ('agent Bob', 'Ross')", () => {
    render(<TaskDetailPage />);
    const timeline = screen.getByText("Status timeline").closest("section");
    expect(timeline?.textContent).toContain("agent Bob");
    expect(timeline?.textContent).toContain("Ross");
    expect(timeline?.textContent).not.toContain(BOB_ID);
  });
});

describe("collapsible sections (requirement 4)", () => {
  it("Thread and Status timeline start expanded", () => {
    render(<TaskDetailPage />);
    const toggles = screen
      .getAllByRole("button")
      .filter((b) => b.className.includes("collapsible-toggle"));
    expect(toggles.length).toBe(2);
    for (const toggle of toggles) {
      expect(toggle).toHaveAttribute("aria-expanded", "true");
    }
  });

  it("collapsing the thread hides messages and persists to sessionStorage", () => {
    render(<TaskDetailPage />);
    const threadToggle = screen
      .getAllByRole("button")
      .find((b) => b.className.includes("collapsible-toggle") && b.textContent?.includes("Thread"));
    fireEvent.click(threadToggle!);
    expect(threadToggle).toHaveAttribute("aria-expanded", "false");
    expect(screen.queryByText("can you look at this?")).not.toBeInTheDocument();
    expect(window.sessionStorage.getItem("skquad:collapsible:task-thread-t1")).toBe("0");
  });
});

describe("agent chat button (requirement 5)", () => {
  it("lives in the lease/assignee row and is labelled 'Talk to Bob'", () => {
    render(<TaskDetailPage />);
    const talk = screen.getByRole("link", { name: "Talk to Bob" });
    expect(talk).toHaveAttribute("href", `/squads/s1/agents/${BOB_ID}`);
    // Same row as the assignee/lease meta text (shared flex parent).
    const meta = talk.parentElement;
    expect(meta?.textContent).toContain("assigned to");
    expect(meta?.textContent).toContain("updated");
    // The old bottom-of-thread button is gone.
    expect(screen.queryByRole("link", { name: /Agent Chat/ })).not.toBeInTheDocument();
  });
});

describe("back to board (requirement 3)", () => {
  it("sits at the top, above the Actions section", () => {
    render(<TaskDetailPage />);
    const back = screen.getByRole("link", { name: /Back to board/ });
    const actions = screen.getByRole("heading", { name: "Actions" });
    // back link precedes the Actions heading in document order.
    expect(back.compareDocumentPosition(actions) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });
});
