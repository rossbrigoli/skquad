// S-189: jsdom behaviour tests for the Inbox page. The pure grouping
// and display libs (inboxGroups/notifications/format) run for real; the
// transport (apiGet/apiDelete) and the Attention provider are mocked,
// so these lock: fetch query construction per filter, the read-on-open
// round-trip through the provider, bulk mark-read/delete wiring, the
// admin user-scoped filter, and the empty/error/loading states.
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";

const env = vi.hoisted(() => ({
  role: "member" as string,
  messages: [] as unknown[],
  users: [] as unknown[],
  inboxError: null as Error | null,
  getCalls: [] as string[],
  deleteCalls: [] as string[],
  deleteError: null as Error | null,
  agentNames: { ag1: "coder", ag2: "tester" } as Record<string, string>,
  markRead: vi.fn(),
}));

vi.mock("next/link", () => ({
  default: ({ href, children }: { href: string; children: React.ReactNode }) => (
    <a href={href}>{children}</a>
  ),
}));

vi.mock("../../components/AuthGate", () => ({
  AuthGate: ({ children }: { children: React.ReactNode }) => <>{children}</>,
}));

vi.mock("../../components/AppShell", () => ({
  AppShell: ({ children }: { children: React.ReactNode }) => <>{children}</>,
}));

vi.mock("../../lib/auth", () => ({
  useAuth: () => ({
    token: "tok",
    mode: "token",
    user: { id: "me", email: "me@x", name: "Me", role: env.role },
    authed: true,
  }),
}));

vi.mock("../../lib/useAttention", () => ({
  useAttention: () => ({
    agentName: (id?: string) => (id ? env.agentNames[id] : undefined),
    markRead: env.markRead,
  }),
}));

vi.mock("../../lib/api", () => ({
  apiGet: async (path: string) => {
    env.getCalls.push(path);
    if (path === "/users") return env.users;
    return [];
  },
  // S-239: the inbox list now goes through apiGetWithTotal so the pager
  // can see the server's X-Total-Count. The mock mirrors the transport:
  // it honours limit/offset over the seeded messages and reports the
  // full seeded count as the total.
  apiGetWithTotal: async (path: string) => {
    env.getCalls.push(path);
    if (path.startsWith("/inbox")) {
      if (env.inboxError) throw env.inboxError;
      const params = new URLSearchParams(path.split("?")[1] ?? "");
      const limit = Number.parseInt(params.get("limit") ?? "100", 10);
      const offset = Number.parseInt(params.get("offset") ?? "0", 10);
      return { items: env.messages.slice(offset, offset + limit), total: env.messages.length };
    }
    return { items: [], total: 0 };
  },
  apiDelete: async (path: string) => {
    if (env.deleteError) throw env.deleteError;
    env.deleteCalls.push(path);
    // S-239: deletes really remove the seeded rows so later refetches
    // report an honest X-Total-Count.
    const deletedId = path.split("/").pop();
    env.messages = env.messages.filter((m) => (m as { id: string }).id !== deletedId);
  },
}));

import InboxPage from "./page";
import type { InboxMessage } from "../../lib/api";

const now = () => new Date().toISOString();

function msg(id: string, overrides: Partial<InboxMessage> = {}): InboxMessage {
  return {
    id,
    squad_id: "sq1",
    from_agent_id: "ag1",
    kind: "agent_message",
    message: `plain ${id}`,
    subject: `Subject ${id}`,
    body: `Body of message ${id}`,
    created_at: now(),
    ...overrides,
  };
}

beforeEach(() => {
  env.role = "member";
  env.messages = [];
  env.users = [];
  env.inboxError = null;
  env.getCalls = [];
  env.deleteCalls = [];
  env.deleteError = null;
  env.markRead = vi.fn().mockResolvedValue(undefined);
});

describe("list states", () => {
  it("shows the loading empty-state before the first response", () => {
    render(<InboxPage />);
    expect(screen.getByText("Loading your inbox…")).toBeInTheDocument();
  });

  it("shows the empty inbox state when no messages", async () => {
    render(<InboxPage />);
    expect(await screen.findByText("Your inbox is empty")).toBeInTheDocument();
  });

  it("surfaces a fetch failure as an error notice", async () => {
    env.inboxError = new Error("api down");
    render(<InboxPage />);
    expect(await screen.findByText("api down")).toBeInTheDocument();
  });
});

describe("list rendering", () => {
  it("titles with unread count, resolves senders and shows previews", async () => {
    env.messages = [
      msg("m1"),
      msg("m2", { read_at: now(), from_agent_id: "ag2" }),
      msg("m3", { from_agent_id: undefined }),
    ];
    render(<InboxPage />);
    expect(await screen.findByRole("heading", { name: "Inbox — 2 new" })).toBeInTheDocument();
    expect(screen.getByText("coder")).toBeInTheDocument();
    expect(screen.getByText("tester")).toBeInTheDocument();
    expect(screen.getByText("System")).toBeInTheDocument();
    expect(screen.getByText("Subject m1")).toBeInTheDocument();
    expect(screen.getByText(/Body of message m1/)).toBeInTheDocument();
    // Column headers present.
    expect(screen.getByText("From")).toBeInTheDocument();
    expect(screen.getByText("Received")).toBeInTheDocument();
  });

  it("plain title when everything is read", async () => {
    env.messages = [msg("m1", { read_at: now() })];
    render(<InboxPage />);
    expect(await screen.findByRole("heading", { name: "Inbox" })).toBeInTheDocument();
  });

  it("requests the scoped query with limit and unread flag", async () => {
    env.messages = [msg("m1")];
    render(<InboxPage />);
    await screen.findByRole("heading", { name: "Inbox — 1 new" });
    expect(env.getCalls).toContain("/inbox?limit=25");
    await userEvent.selectOptions(screen.getByLabelText("Show"), "Unread only");
    await vi.waitFor(() => expect(env.getCalls).toContain("/inbox?unread=true&limit=25"));
  });
});

describe("reading a message", () => {
  it("opens the detail view and marks the message read via the provider", async () => {
    env.messages = [msg("m1")];
    render(<InboxPage />);
    await screen.findByRole("heading", { name: "Inbox — 1 new" });
    await userEvent.click(screen.getByRole("button", { name: "Open message: Subject m1" }));
    expect(await screen.findByText("Body of message m1")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Back to inbox" })).toBeInTheDocument();
    await vi.waitFor(() => expect(env.markRead).toHaveBeenCalledWith("m1"));
    // Optimistic flip: title drops the "new" count.
    await vi.waitFor(() =>
      expect(screen.getByRole("heading", { name: "Inbox" })).toBeInTheDocument(),
    );
  });

  it("opening an already-read message does not re-mark it", async () => {
    env.messages = [msg("m1", { read_at: now() })];
    render(<InboxPage />);
    await userEvent.click(await screen.findByRole("button", { name: "Open message: Subject m1" }));
    await screen.findByText("Body of message m1");
    expect(env.markRead).not.toHaveBeenCalled();
  });

  it("provider markRead failures are swallowed (UI stays open)", async () => {
    env.markRead = vi.fn().mockRejectedValue(new Error("offline"));
    env.messages = [msg("m1")];
    render(<InboxPage />);
    await userEvent.click(await screen.findByRole("button", { name: "Open message: Subject m1" }));
    expect(await screen.findByText("Body of message m1")).toBeInTheDocument();
    await vi.waitFor(() => expect(env.markRead).toHaveBeenCalled());
    expect(screen.queryByText("offline")).not.toBeInTheDocument();
  });

  it("detail links to the task when task_id is present", async () => {
    env.messages = [msg("m1", { task_id: "t7" })];
    render(<InboxPage />);
    await userEvent.click(await screen.findByRole("button", { name: "Open message: Subject m1" }));
    expect(await screen.findByRole("link", { name: "Open task →" })).toHaveAttribute(
      "href",
      "/squads/sq1/tasks/t7",
    );
  });

  it("back arrow returns to the list", async () => {
    env.messages = [msg("m1")];
    render(<InboxPage />);
    await userEvent.click(await screen.findByRole("button", { name: "Open message: Subject m1" }));
    await screen.findByText("Body of message m1");
    await userEvent.click(screen.getByRole("button", { name: "Back to inbox" }));
    expect(screen.getByRole("button", { name: "Open message: Subject m1" })).toBeInTheDocument();
  });
});

describe("single delete", () => {
  it("deletes from the reading view after confirmation", async () => {
    env.messages = [msg("m1"), msg("m2")];
    render(<InboxPage />);
    await userEvent.click(await screen.findByRole("button", { name: "Open message: Subject m1" }));
    await userEvent.click(screen.getByRole("button", { name: "Delete message" }));
    const dialog = screen.getByRole("dialog");
    await userEvent.click(within(dialog).getByRole("button", { name: "Delete" }));
    await vi.waitFor(() => expect(env.deleteCalls).toEqual(["/inbox/m1"]));
    // Row is gone; list shows the remaining message.
    await vi.waitFor(() =>
      expect(screen.queryByRole("button", { name: "Open message: Subject m1" })).not.toBeInTheDocument(),
    );
    expect(screen.getByRole("button", { name: "Open message: Subject m2" })).toBeInTheDocument();
  });

  it("cancel skips the delete", async () => {
    env.messages = [msg("m1")];
    render(<InboxPage />);
    await userEvent.click(await screen.findByRole("button", { name: "Open message: Subject m1" }));
    await userEvent.click(screen.getByRole("button", { name: "Delete message" }));
    await userEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Cancel" }));
    expect(env.deleteCalls).toHaveLength(0);
  });
});

describe("bulk actions", () => {
  it("bulk mark-read only touches unread selected messages", async () => {
    env.messages = [msg("m1"), msg("m2"), msg("m3", { read_at: now() })];
    render(<InboxPage />);
    await screen.findByRole("heading", { name: "Inbox — 2 new" });
    await userEvent.click(screen.getByRole("checkbox", { name: "Select message: Subject m1" }));
    await userEvent.click(screen.getByRole("checkbox", { name: "Select message: Subject m3" }));
    expect(screen.getByText("2 selected")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Mark 2 selected as read" }));
    await vi.waitFor(() => expect(env.markRead).toHaveBeenCalledTimes(1));
    expect(env.markRead).toHaveBeenCalledWith("m1");
    expect(env.markRead).not.toHaveBeenCalledWith("m3");
    // Selection cleared after the bulk action.
    expect(screen.getByRole("button", { name: "Mark 0 selected as read" })).toBeDisabled();
  });

  it("bulk delete removes every selected message after confirmation", async () => {
    env.messages = [msg("m1"), msg("m2"), msg("m3")];
    render(<InboxPage />);
    await userEvent.click(await screen.findByRole("checkbox", { name: "Select all visible messages" }));
    expect(screen.getByText("3 selected")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Delete 3 selected messages" }));
    const dialog = screen.getByRole("dialog");
    expect(within(dialog).getByText(/deleting these 3 messages is permanent/)).toBeInTheDocument();
    await userEvent.click(within(dialog).getByRole("button", { name: "Delete" }));
    await vi.waitFor(() =>
      expect(env.deleteCalls.sort()).toEqual(["/inbox/m1", "/inbox/m2", "/inbox/m3"]),
    );
    expect(await screen.findByText("Your inbox is empty")).toBeInTheDocument();
  });

  it("bulk delete failure surfaces the error", async () => {
    env.messages = [msg("m1")];
    env.deleteError = new Error("403 nope");
    render(<InboxPage />);
    await userEvent.click(await screen.findByRole("checkbox", { name: "Select all visible messages" }));
    await userEvent.click(screen.getByRole("button", { name: "Delete 1 selected messages" }));
    await userEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Delete" }));
    expect(await screen.findByText("403 nope")).toBeInTheDocument();
  });

  it("bulk buttons are disabled with no selection", async () => {
    env.messages = [msg("m1")];
    render(<InboxPage />);
    await screen.findByRole("heading", { name: "Inbox — 1 new" });
    expect(screen.getByRole("button", { name: "Mark 0 selected as read" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Delete 0 selected messages" })).toBeDisabled();
  });

  it("changing the unread filter clears the selection", async () => {
    env.messages = [msg("m1"), msg("m2")];
    render(<InboxPage />);
    await userEvent.click(await screen.findByRole("checkbox", { name: "Select message: Subject m1" }));
    expect(screen.getByText("1 selected")).toBeInTheDocument();
    await userEvent.selectOptions(screen.getByLabelText("Show"), "Unread only");
    expect(screen.getByRole("button", { name: "Mark 0 selected as read" })).toBeDisabled();
  });
});

describe("admin user filter", () => {
  it("non-admins never see the user filter or the /users fetch", async () => {
    env.messages = [msg("m1")];
    render(<InboxPage />);
    await screen.findByRole("heading", { name: "Inbox — 1 new" });
    expect(screen.queryByLabelText("User")).not.toBeInTheDocument();
    expect(env.getCalls).not.toContain("/users");
  });

  it("admins can scope the inbox to another user", async () => {
    env.role = "platform_admin";
    env.users = [
      { id: "me", email: "me@x", name: "Me", role: "platform_admin" },
      { id: "u2", email: "other@x", name: "Other Human", role: "member" },
    ];
    env.messages = [msg("m1")];
    render(<InboxPage />);
    await screen.findByRole("heading", { name: "Inbox — 1 new" });
    expect(env.getCalls).toContain("/users");
    const userSelect = screen.getByLabelText("User");
    // The admin's own id is filtered out of the options.
    expect(within(userSelect).getByRole("option", { name: "My inbox" })).toBeInTheDocument();
    expect(within(userSelect).getByRole("option", { name: "Other Human" })).toBeInTheDocument();
    expect(within(userSelect).queryByRole("option", { name: "Me" })).not.toBeInTheDocument();
    await userEvent.selectOptions(userSelect, "u2");
    await vi.waitFor(() => expect(env.getCalls).toContain("/inbox?user_id=u2&limit=25"));
    // Switching users also exits any open reading view back to the list.
    expect(screen.getByRole("checkbox", { name: "Select all visible messages" })).toBeInTheDocument();
  });
});

describe("paging (S-239)", () => {
  function manyMsgs(n: number): InboxMessage[] {
    return Array.from({ length: n }, (_, i) => msg(`m${i}`));
  }

  it("defaults to 25 items per page with no offset on the first page", async () => {
    env.messages = [msg("m1")];
    render(<InboxPage />);
    await screen.findByText("Subject m1");
    expect(env.getCalls.filter((c) => c.startsWith("/inbox"))).toEqual(["/inbox?limit=25"]);
    const sizeSelect = screen.getByLabelText("Items per page") as HTMLSelectElement;
    expect(sizeSelect.value).toBe("25");
  });

  it("shows the count summary for the current window", async () => {
    env.messages = [msg("m1"), msg("m2"), msg("m3", { read_at: now() })];
    render(<InboxPage />);
    expect(await screen.findByText(/Showing 1–3 of 3/)).toBeInTheDocument();
  });

  it("changing the items-per-page selector re-fetches and resets to page 1", async () => {
    env.messages = manyMsgs(30);
    render(<InboxPage />);
    await screen.findByText("Subject m0");
    await userEvent.click(screen.getByRole("button", { name: "Next page" }));
    await vi.waitFor(() => expect(env.getCalls).toContain("/inbox?limit=25&offset=25"));
    await userEvent.selectOptions(screen.getByLabelText("Items per page"), "10");
    await vi.waitFor(() => expect(env.getCalls).toContain("/inbox?limit=10"));
    // Reset to page 1: the newest inbox call carries no offset.
    const lastInbox = env.getCalls.filter((c) => c.startsWith("/inbox")).at(-1);
    expect(lastInbox).toBe("/inbox?limit=10");
    expect(screen.getByText("Page 1 of 3")).toBeInTheDocument();
  });

  it("navigates forward and back through pages", async () => {
    env.messages = manyMsgs(60);
    render(<InboxPage />);
    await screen.findByText("Subject m0");
    expect(screen.getByRole("button", { name: "Previous page" })).toBeDisabled();
    expect(screen.getByText("Page 1 of 3")).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "Next page" }));
    await vi.waitFor(() => expect(env.getCalls).toContain("/inbox?limit=25&offset=25"));
    expect(screen.getByText("Page 2 of 3")).toBeInTheDocument();
    expect(screen.getByText(/Showing 26–50 of 60/)).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "Previous page" }));
    await vi.waitFor(() =>
      expect(env.getCalls.filter((c) => c.startsWith("/inbox")).at(-1)).toBe("/inbox?limit=25"),
    );
    expect(screen.getByText("Page 1 of 3")).toBeInTheDocument();
  });

  it("hides the pager when there are no messages", async () => {
    render(<InboxPage />);
    await screen.findByText("Your inbox is empty");
    expect(screen.queryByRole("button", { name: "Next page" })).not.toBeInTheDocument();
  });

  it("changing a filter jumps back to the first page", async () => {
    env.messages = manyMsgs(30);
    render(<InboxPage />);
    await screen.findByText("Subject m0");
    await userEvent.click(screen.getByRole("button", { name: "Next page" }));
    await vi.waitFor(() => expect(env.getCalls).toContain("/inbox?limit=25&offset=25"));
    await userEvent.selectOptions(screen.getByLabelText("Show"), "Unread only");
    await vi.waitFor(() =>
      expect(env.getCalls.filter((c) => c.startsWith("/inbox")).at(-1)).toBe(
        "/inbox?unread=true&limit=25",
      ),
    );
    expect(screen.getByText("Page 1 of 2")).toBeInTheDocument();
  });

  it("bulk delete on the last page steps back when the page empties", async () => {
    env.messages = manyMsgs(26); // page 2 holds exactly one row
    render(<InboxPage />);
    await screen.findByText("Subject m0");
    await userEvent.click(screen.getByRole("button", { name: "Next page" }));
    await vi.waitFor(() => expect(env.getCalls).toContain("/inbox?limit=25&offset=25"));
    expect(screen.getByText("Page 2 of 2")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("checkbox", { name: "Select all visible messages" }));
    await userEvent.click(screen.getByRole("button", { name: "Delete 1 selected messages" }));
    await userEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Delete" }));
    await vi.waitFor(() => expect(env.deleteCalls).toHaveLength(1));
    // Total drops to 25 → back to a single page.
    expect(await screen.findByText("Page 1 of 1")).toBeInTheDocument();
  });
});

describe("envelope read-state icons (S-234)", () => {
  function statusSvg(container: HTMLElement, subject: string): SVGSVGElement {
    const row = screen.getByText(subject).closest(".inbox-row") as HTMLElement;
    return row.querySelector(".inbox-status svg") as SVGSVGElement;
  }

  it("unread rows get the closed filled envelope, read rows the open envelope", async () => {
    env.messages = [msg("u1"), msg("r1", { read_at: now() })];
    const { container } = render(<InboxPage />);
    await screen.findByText("Subject u1");

    const unreadSvg = statusSvg(container, "Subject u1");
    const readSvg = statusSvg(container, "Subject r1");
    expect(unreadSvg).not.toBeNull();
    expect(readSvg).not.toBeNull();

    // Unread: closed envelope, filled with the even-odd flap crease.
    expect(unreadSvg.getAttribute("fill")).toBe("currentColor");
    expect(unreadSvg.querySelector("path")?.getAttribute("d")).toContain("M2 6a2 2 0 0 1 2-2h16");

    // Read: OPEN envelope (flap raised), plain stroke outline.
    expect(readSvg.getAttribute("fill")).toBe("none");
    expect(readSvg.querySelector("path")?.getAttribute("d")).toContain("M21.2 8.4");

    // The two markers are visually distinct glyphs.
    expect(readSvg.innerHTML).not.toBe(unreadSvg.innerHTML);
  });

  it("bulk mark-read flips the row icon from closed to open", async () => {
    env.messages = [msg("m1")];
    const { container } = render(<InboxPage />);
    await screen.findByText("Subject m1");
    expect(statusSvg(container, "Subject m1").getAttribute("fill")).toBe("currentColor");

    await userEvent.click(screen.getByRole("checkbox", { name: "Select message: Subject m1" }));
    await userEvent.click(screen.getByRole("button", { name: "Mark 1 selected as read" }));

    await vi.waitFor(() => {
      expect(statusSvg(container, "Subject m1").getAttribute("fill")).toBe("none");
    });
    expect(statusSvg(container, "Subject m1").querySelector("path")?.getAttribute("d")).toContain(
      "M21.2 8.4",
    );
  });
});
