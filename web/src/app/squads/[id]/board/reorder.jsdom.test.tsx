// S-304: board column ordering — the vertical order within a column IS
// the pickup priority (control-plane ClaimNextTask takes the lowest
// position first). These tests cover the pure ordering helpers and the
// drag-and-drop reorder wiring on the Board page.
import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { orderedIdsFor, orderedTasksFor } from "./page";
import type { Task } from "../../../../lib/api";

vi.mock("next/link", () => ({
  default: ({ href, children, ...rest }: { href: string; children: React.ReactNode }) => (
    <a href={typeof href === "string" ? href : "#"} {...rest}>
      {children}
    </a>
  ),
}));

vi.mock("next/navigation", () => ({
  useParams: () => ({ id: "s1" }),
  useRouter: () => ({ push: vi.fn() }),
}));

const env = vi.hoisted(() => ({
  tasks: [] as unknown[],
  posts: [] as { path: string; body: unknown }[],
  postThrow: null as unknown,
}));

vi.mock("../../../../lib/useApi", () => ({
  useApi: (path: string) => {
    if (path === "/squads") return { data: [{ id: "s1", name: "Alpha", owner_id: "u1" }], loading: false, error: "", refresh: vi.fn() };
    if (path.includes("/board")) return { data: { tasks: env.tasks }, loading: false, error: "", refresh: vi.fn() };
    return { data: [], loading: false, error: "", refresh: vi.fn() };
  },
}));

vi.mock("../../../../lib/auth", () => ({
  useAuth: () => ({ token: "tok", mode: "token", user: { id: "u1" }, authed: true }),
}));

vi.mock("../../../../components/AuthGate", () => ({
  AuthGate: ({ children }: { children: React.ReactNode }) => <>{children}</>,
}));
vi.mock("../../../../components/AppShell", () => ({
  AppShell: ({ children }: { children: React.ReactNode }) => <>{children}</>,
}));

vi.mock("../../../../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../../../lib/api")>();
  return {
    ...actual,
    apiPost: vi.fn(async (path: string, _token: string, body: unknown) => {
      env.posts.push({ path, body });
      if (env.postThrow) throw env.postThrow;
      return {};
    }),
  };
});

import SquadBoardPage from "./page";

function task(id: string, status: string, position: number): Task {
  return {
    id,
    board_id: "b1",
    squad_id: "s1",
    title: `task ${id}`,
    status,
    position,
    updated_at: new Date().toISOString(),
  } as Task;
}

beforeEach(() => {
  env.tasks = [task("t1", "todo", 1), task("t2", "todo", 2), task("t3", "todo", 3)];
  env.posts = [];
  env.postThrow = null;
});

describe("ordering helpers (S-304)", () => {
  it("orderedIdsFor returns server order untouched", () => {
    const tasks = [task("a", "todo", 1), task("b", "todo", 2)];
    expect(orderedIdsFor("todo" as never, tasks)).toEqual(["a", "b"]);
  });

  it("orderedTasksFor falls back to server order with no optimistic order", () => {
    const tasks = [task("a", "todo", 1), task("b", "todo", 2)];
    expect(orderedTasksFor("todo" as never, tasks, {}).map((t) => t.id)).toEqual(["a", "b"]);
  });

  it("orderedTasksFor applies the optimistic order", () => {
    const tasks = [task("a", "todo", 1), task("b", "todo", 2), task("c", "todo", 3)];
    const out = orderedTasksFor("todo" as never, tasks, { todo: ["c", "a", "b"] });
    expect(out.map((t) => t.id)).toEqual(["c", "a", "b"]);
  });

  it("orderedTasksFor appends unknown ids at the end", () => {
    const tasks = [task("a", "todo", 1), task("b", "todo", 2), task("new", "todo", 3)];
    const out = orderedTasksFor("todo" as never, tasks, { todo: ["b", "a"] });
    expect(out.map((t) => t.id)).toEqual(["b", "a", "new"]);
  });
});

describe("drag-and-drop reorder (S-304)", () => {
  function makeDataTransfer() {
    const store: Record<string, string> = {};
    return {
      setData: (k: string, v: string) => {
        store[k] = v;
      },
      getData: (k: string) => store[k] ?? "",
      effectAllowed: "",
    };
  }

  it("dropping a card onto a lower card in the same column persists the new order", async () => {
    render(<SquadBoardPage />);
    const cards = screen.getAllByText(/^task t/);
    const dragged = cards[0]; // t1
    const target = cards[2]; // t3

    const dt = makeDataTransfer();
    fireEvent.dragStart(dragged.closest(".task-card")!, { dataTransfer: dt });
    fireEvent.dragOver(target.closest(".task-card")!, { dataTransfer: dt });
    dt.setData("text/skquad-task-target", "t3");
    fireEvent.drop(target.closest(".task-card")!, { dataTransfer: dt });

    await waitFor(() =>
      expect(env.posts.some((p) => p.path === "/squads/s1/board/columns/todo/order")).toBe(true),
    );
    const call = env.posts.find((p) => p.path === "/squads/s1/board/columns/todo/order")!;
    // t1 moves to t3's slot: order becomes t2, t3, t1.
    expect((call.body as { task_ids: string[] }).task_ids).toEqual(["t2", "t3", "t1"]);
  });

  it("dropping a card onto empty column space appends to the end", async () => {
    env.tasks = [task("t1", "todo", 1), task("t2", "todo", 2)];
    render(<SquadBoardPage />);
    const cards = screen.getAllByText(/^task t/);
    const dt = makeDataTransfer();
    fireEvent.dragStart(cards[0].closest(".task-card")!, { dataTransfer: dt });
    // Drop on the todo column body (below the cards) — no target id set.
    const todoCol = screen.getByText("task t1").closest(".board-column")!;
    const colBody = todoCol.querySelector(".board-col-body")!;
    fireEvent.drop(colBody, { dataTransfer: dt });

    await waitFor(() =>
      expect(env.posts.some((p) => p.path === "/squads/s1/board/columns/todo/order")).toBe(true),
    );
    const call = env.posts.find((p) => p.path === "/squads/s1/board/columns/todo/order")!;
    expect((call.body as { task_ids: string[] }).task_ids).toEqual(["t2", "t1"]);
  });

  it("cross-column drop still uses the move endpoint (not reorder)", async () => {
    env.tasks = [task("t1", "todo", 1), task("t2", "in-progress", 1)];
    render(<SquadBoardPage />);
    const todoCard = screen.getByText("task t1").closest(".task-card")!;
    const dt = makeDataTransfer();
    fireEvent.dragStart(todoCard, { dataTransfer: dt });
    const inProgBody = screen.getByText("task t2").closest(".board-column")!.querySelector(".board-col-body")!;
    fireEvent.drop(inProgBody, { dataTransfer: dt });

    await waitFor(() =>
      expect(env.posts.some((p) => p.path === "/tasks/t1/move")).toBe(true),
    );
    expect(env.posts.some((p) => p.path.includes("/order"))).toBe(false);
  });

  it("reorder failure surfaces an error notice", async () => {
    env.postThrow = new Error("reorder rejected");
    render(<SquadBoardPage />);
    const cards = screen.getAllByText(/^task t/);
    const dt = makeDataTransfer();
    fireEvent.dragStart(cards[0].closest(".task-card")!, { dataTransfer: dt });
    dt.setData("text/skquad-task-target", "t3");
    fireEvent.drop(cards[2].closest(".task-card")!, { dataTransfer: dt });

    await waitFor(() => expect(screen.getByText(/reorder rejected/)).toBeInTheDocument());
  });
});
