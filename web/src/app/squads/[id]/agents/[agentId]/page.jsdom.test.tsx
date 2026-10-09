// S-189: jsdom behaviour tests for the agent profile page (Talk-first
// layout). Heavy child panels (prompt editor, revisions, effective
// prompt, subagent thread, inbox panel, activity feed) are stubbed to
// their callback contracts — they own their own suites. Everything the
// page itself decides is tested for real: tab switching, metric-chip
// derivation, restart/reset/delete confirm flows and their API calls,
// the composer (send, wake-on-typing, stop, thinking level), the
// Configuration form (dirty gating, single PATCH payload, storage
// validation, LLM binding states), grants and identity actions.
import { fireEvent, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";

const env = vi.hoisted(() => ({
  agents: [] as unknown[],
  agentsLoading: false,
  tasks: [] as unknown[],
  metering: null as unknown,
  meteringMtd: null as unknown,
  meteringMtdLoading: false,
  meteringMtdError: null as string | null,
  perms: [] as unknown[],
  audit: [] as unknown[],
  chat: [] as unknown[],
  chatNull: false,
  boardNull: false,
  auditNull: false,
  permsNull: false,
  modelsNull: false,
  registryNull: false,
  registryError: "",
  resetEmpty: false,
  models: [] as unknown[],
  modelsError: "",
  registry: [] as unknown[],
  restartPods: { pods: 2 } as Record<string, number> | null,
  posts: [] as { path: string; body: unknown }[],
  patches: [] as { path: string; body: unknown }[],
  puts: [] as { path: string; body: unknown }[],
  deletes: [] as string[],
  postThrow: null as unknown,
  patchThrow: null as unknown,
  putError: null as unknown,
  refreshes: [] as string[],
  push: vi.fn(),
}));

vi.mock("next/link", () => ({
  default: ({ href, children }: { href: string; children: React.ReactNode }) => (
    <a href={href}>{children}</a>
  ),
}));

vi.mock("next/navigation", () => ({
  useParams: () => ({ id: "sq1", agentId: "ag1" }),
  useRouter: () => ({ push: env.push }),
}));

vi.mock("../../../../../components/AuthGate", () => ({
  AuthGate: ({ children }: { children: React.ReactNode }) => <>{children}</>,
}));
vi.mock("../../../../../components/AppShell", () => ({
  AppShell: ({ children }: { children: React.ReactNode }) => <>{children}</>,
}));
vi.mock("../../../../../components/ActivityFeed", () => ({
  ActivityFeed: ({ entries }: { entries: unknown[] }) => (
    <div data-testid="activity-feed">{`feed:${entries.length}`}</div>
  ),
}));
vi.mock("../../../../../components/EffectivePromptPanel", () => ({
  EffectivePromptPanel: () => <div>effective-panel</div>,
}));
vi.mock("../../../../../components/PromptRevisionsPanel", () => ({
  PromptRevisionsPanel: ({ onRestore }: { onRestore: (c: string) => void }) => (
    <button type="button" onClick={() => onRestore("restored prompt")}>
      stub-restore
    </button>
  ),
}));
vi.mock("../../../../../components/PromptTierEditor", () => ({
  PromptTierEditor: ({
    content,
    onChange,
  }: {
    content: string;
    onChange: (v: string) => void;
  }) => (
    <div data-testid="tier-editor">
      <span>{content}</span>
      <button type="button" onClick={() => onChange("tier edited")}>
        stub-tier-change
      </button>
    </div>
  ),
}));
vi.mock("../../../../../components/SubagentThreadPanel", () => ({
  SubagentThreadPanel: ({ onClose }: { onClose: () => void }) => (
    <div data-testid="subagent-panel">
      subagent-panel
      <button type="button" onClick={onClose}>
        close-subagent
      </button>
    </div>
  ),
}));
vi.mock("../../../../../components/AgentInboxPanel", () => ({
  AgentInboxPanel: ({ agentId }: { agentId: string }) => (
    <div data-testid="agent-inbox">{`agent-inbox:${agentId}`}</div>
  ),
}));
vi.mock("../../../../../components/MarkdownMessage", () => ({
  MarkdownMessage: ({ text }: { text: string }) => <div className="md">{text}</div>,
}));
// TG-4c: the real panel does its own HTTP; covered in its own jsdom test.
// Here we only assert the page mounts it for rest/api grant rows.
vi.mock("../../../../../components/RestAgentCredential", () => ({
  RestAgentCredential: ({ resourceId, agentId }: { resourceId: string; agentId: string }) => (
    <div data-testid="rest-agent-cred">{`cred:${resourceId}:${agentId}`}</div>
  ),
}));

vi.mock("../../../../../lib/auth", () => ({
  useAuth: () => ({
    token: "tok",
    mode: "token",
    user: { id: "me", name: "Me", role: "platform_admin" },
  }),
}));

vi.mock("../../../../../lib/useApi", () => ({
  useApi: (path: string) => {
    let data: unknown = null;
    let loading = false;
    let error: string | null = "";
    if (path === "/squads/sq1/agents") {
      data = env.agents;
      loading = env.agentsLoading;
    }
    if (path === "/squads/sq1/board") data = env.boardNull ? null : { tasks: env.tasks };
    if (path === "/agents/ag1/metering") data = env.metering;
    if (path.startsWith("/agents/ag1/metering?since=")) {
      data = env.meteringMtd;
      loading = env.meteringMtdLoading;
      error = env.meteringMtdError;
    }
    if (path === "/agents/ag1/permissions") data = env.permsNull ? null : env.perms;
    if (path === "/squads/sq1/audit?limit=50") data = env.auditNull ? null : env.audit;
    if (path === "/agents/ag1/chat") data = env.chatNull ? null : env.chat;
    if (path === "/models/me") {
      data = env.modelsNull ? null : env.models;
      error = env.modelsError;
    }
    if (path.startsWith("/registry/")) {
      data = env.registryNull ? null : env.registry;
      error = env.registryError;
    }
    const key = path;
    return {
      data,
      loading,
      error,
      refresh: () => env.refreshes.push(key),
    };
  },
}));

vi.mock("../../../../../lib/api", () => ({
  apiPost: async (path: string, _token: string, body: unknown) => {
    env.posts.push({ path, body });
    if (env.postThrow) throw env.postThrow;
    if (path.endsWith("/restart")) return env.restartPods ?? {};
    if (path.endsWith("/chat/reset")) return env.resetEmpty ? {} : { archived: 3 };
    return {};
  },
  apiPatch: async (path: string, _token: string, body: unknown) => {
    env.patches.push({ path, body });
    if (env.patchThrow) throw env.patchThrow;
    return {};
  },
  apiPut: async (path: string, _token: string, body: unknown) => {
    env.puts.push({ path, body });
    if (env.putError) throw env.putError;
    return {};
  },
  apiDelete: async (path: string) => {
    env.deletes.push(path);
  },
  apiUploadImage: async (_path: string, _token: string, file: File) => ({
    id: `up-${file.name}`,
    url: `/u/${file.name}`,
    filename: file.name,
  }),
  // AttachmentThumbs fetches bytes through the authed client and renders
  // object URLs; jsdom has no object-URL support, so both are stubbed.
  apiGetBlob: async () => new Blob(["img"], { type: "image/png" }),
}));

import AgentProfilePage from "./page";

// jsdom lacks usable object-URL APIs that AttachmentThumbs relies on —
// override unconditionally (jsdom's stub throws "Not implemented").
const urlShim = URL as unknown as {
  createObjectURL: (b: unknown) => string;
  revokeObjectURL: (u: string) => void;
};
urlShim.createObjectURL = () => "blob:mock";
urlShim.revokeObjectURL = () => undefined;

const future = new Date(Date.now() + 600_000).toISOString();
const past = new Date(Date.now() - 600_000).toISOString();

function agent(overrides: Record<string, unknown> = {}) {
  return {
    id: "ag1",
    squad_id: "sq1",
    name: "coder",
    role: "implementer",
    status: "busy",
    ai_model_id: "m1",
    fallback_ai_model_id: "m2",
    idle_timeout_sec: 600,
    storage_enabled: true,
    storage_size: "2Gi",
    ...overrides,
  };
}

function model(id: string, name: string, status = "active") {
  return {
    id,
    provider_id: "p1",
    display_name: name,
    model_name: name.toLowerCase(),
    context_window: 8192,
    supports_tools: true,
    supports_vision: false,
    pricing: null,
    long_context_threshold_tokens: 0,
    status,
  };
}

function chatMsg(id: string, from: "user" | "agent", text: string, extra: Record<string, unknown> = {}) {
  return {
    id,
    from_type: from,
    from_id: from === "user" ? "me" : "ag1",
    to_agent_id: "ag1",
    squad_id: "sq1",
    type: "chat",
    payload: { message: text },
    status: "sent",
    created_at: new Date(Date.now() - 1000).toISOString(),
    ...extra,
  };
}

beforeEach(() => {
  env.agents = [agent()];
  env.agentsLoading = false;
  env.tasks = [];
  env.metering = { cost: 5, currency: "USD" };
  env.meteringMtd = { cost: 1.25, currency: "USD" };
  env.meteringMtdLoading = false;
  env.meteringMtdError = null;
  env.perms = [];
  env.audit = [];
  env.chat = [];
  env.chatNull = false;
  env.boardNull = false;
  env.auditNull = false;
  env.permsNull = false;
  env.modelsNull = false;
  env.registryNull = false;
  env.registryError = "";
  env.resetEmpty = false;
  env.models = [model("m1", "Model One"), model("m2", "Model Two")];
  env.modelsError = "";
  env.restartPods = { pods: 2 };
  env.registry = [{ id: "sk1", type: "skill", name: "Skill One", status: "active" }];
  env.posts = [];
  env.patches = [];
  env.puts = [];
  env.deletes = [];
  env.postThrow = null;
  env.patchThrow = null;
  env.putError = null;
  env.refreshes = [];
  env.push = vi.fn();
});

function chip(label: string): HTMLElement {
  const el = screen.getByText(label);
  return (el.closest(".metric-chip") ?? el.parentElement) as HTMLElement;
}

async function openConfigTab() {
  await userEvent.click(screen.getByRole("button", { name: "Configuration" }));
}

describe("title + tabs", () => {
  it("renders the agent title, status chip, role meta and metric chips", () => {
    env.tasks = [
      { id: "t1", title: "live task", assignee_agent_id: "ag1", execution_id: "e1", lease_expires_at: future },
      { id: "t2", title: "stalled task", assignee_agent_id: "ag1", execution_id: "e2", lease_expires_at: past },
    ];
    env.perms = [
      { id: "g1", agent_id: "ag1", resource_type: "skill", resource_id: "sk1", created_at: new Date().toISOString() },
      { id: "g2", agent_id: "ag1", resource_type: "ai_provider", resource_id: "legacy", created_at: new Date().toISOString() },
    ];
    env.chat = [chatMsg("c1", "agent", "hi", { payload: { message: "hi", context_tokens: 12345 } })];
    render(<AgentProfilePage />);
    expect(screen.getByRole("heading", { name: "coder" })).toBeInTheDocument();
    // Agent status chip (busy → Running); the live task chip shares the label.
    expect(screen.getAllByText("Running").length).toBeGreaterThanOrEqual(2);
    expect(screen.getByText(/^implementer\s·/)).toBeInTheDocument();
    // ai_provider grants are hidden from the grants count.
    expect(chip("Grants")).toHaveTextContent("1");
    expect(chip("Current lease")).toHaveTextContent("1 task");
    expect(chip("Current lease").className).toContain("attention");
    expect(chip("Tasks")).toHaveTextContent("2");
    expect(chip("MTD spend")).toHaveTextContent("USD 1.2500");
    expect(chip("MTD spend")).toHaveTextContent("lifetime USD 5.0000");
    expect(chip("Workspace")).toHaveTextContent("2Gi");
    expect(chip("Context")).toHaveTextContent("12,345");
  });

  it("MTD metering error shows the restricted-view hint", () => {
    env.meteringMtd = null;
    env.meteringMtdError = "403";
    render(<AgentProfilePage />);
    expect(chip("MTD spend")).toHaveTextContent("owner and platform admins only");
  });

  it("no agent after load → not-found empty state", () => {
    env.agents = [];
    render(<AgentProfilePage />);
    expect(screen.getByText("Agent not found")).toBeInTheDocument();
  });

  it("switches to the message delivery tab and mounts the delivery panel", async () => {
    render(<AgentProfilePage />);
    await userEvent.click(screen.getByRole("button", { name: "Message delivery" }));
    expect(screen.getByTestId("agent-inbox")).toHaveTextContent("agent-inbox:ag1");
    expect(screen.queryByText(/Talk to coder/)).not.toBeInTheDocument();
  });
});

describe("restart flow", () => {
  it("confirms and posts the restart, showing the eviction note", async () => {
    render(<AgentProfilePage />);
    await userEvent.click(screen.getByRole("button", { name: "Restart agent" }));
    const dialog = screen.getByRole("dialog");
    await userEvent.click(within(dialog).getByRole("button", { name: "Restart agent" }));
    await vi.waitFor(() =>
      expect(env.posts.some((p) => p.path === "/agents/ag1/restart")).toBe(true),
    );
    expect(screen.getByText("Restarting agent (2 pod(s) evicted)")).toBeInTheDocument();
  });

  it("restart failure surfaces in the note", async () => {
    env.postThrow = new Error("restart exploded");
    render(<AgentProfilePage />);
    await userEvent.click(screen.getByRole("button", { name: "Restart agent" }));
    await userEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Restart agent" }));
    await vi.waitFor(() => expect(screen.getByText("restart exploded")).toBeInTheDocument());
  });

  it("cancel skips the restart", async () => {
    render(<AgentProfilePage />);
    await userEvent.click(screen.getByRole("button", { name: "Restart agent" }));
    await userEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Cancel" }));
    expect(env.posts).toHaveLength(0);
  });
});

describe("chat thread", () => {
  it("empty thread shows the listening state and disables reset", () => {
    render(<AgentProfilePage />);
    expect(screen.getByText("coder is listening")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Reset chat" })).toBeDisabled();
  });

  it("renders user and agent rows with markdown bodies", async () => {
    env.chat = [chatMsg("c1", "user", "do the thing"), chatMsg("c2", "agent", "done!")];
    render(<AgentProfilePage />);
    expect(await screen.findByText("done!")).toBeInTheDocument();
    expect(screen.getAllByText("You").length).toBeGreaterThan(0);
    expect(screen.getByText("do the thing")).toBeInTheDocument();
  });

  it("renders plain tool calls with args detail", async () => {
    env.chat = [
      chatMsg("c2", "agent", "working", {
        payload: {
          message: "working",
          tool_calls: [{ name: "exec", arguments: { cmd: "ls" }, ok: true, result: "file list" }],
        },
      }),
    ];
    render(<AgentProfilePage />);
    expect(await screen.getByText("🔧 exec")).toBeInTheDocument();
    expect(screen.getByText("ok")).toBeInTheDocument();
  });

  it("subagent chip opens the thread panel and closes", async () => {
    env.chat = [
      chatMsg("c2", "agent", "spawned", {
        payload: {
          message: "spawned",
          tool_calls: [
            {
              name: "spawn_subagent",
              arguments: {},
              ok: true,
              result: "child finished",
              subagent: {
                thread: [{ role: "assistant", content: "child step" }],
                turns: 1,
                steps: ["plan"],
              },
            },
          ],
        },
      }),
    ];
    render(<AgentProfilePage />);
    await userEvent.click(await screen.findByRole("button", { name: "Details" }));
    expect(screen.getByTestId("subagent-panel")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "close-subagent" }));
    expect(screen.queryByTestId("subagent-panel")).not.toBeInTheDocument();
  });

  it("sends a message through the composer and refreshes", async () => {
    render(<AgentProfilePage />);
    const box = screen.getByPlaceholderText("Message coder…");
    await userEvent.type(box, "hello agent");
    await userEvent.click(screen.getByRole("button", { name: "Send message" }));
    await vi.waitFor(() =>
      expect(
        env.posts.some(
          (p) => p.path === "/agents/ag1/chat" && JSON.stringify(p.body) === JSON.stringify({ message: "hello agent", attachments: [] }),
        ),
      ).toBe(true),
    );
    expect(env.refreshes).toContain("/agents/ag1/chat");
    expect(box).toHaveValue("");
  });

  it("first keystroke wakes a scaled-to-zero agent", async () => {
    render(<AgentProfilePage />);
    await userEvent.type(screen.getByPlaceholderText("Message coder…"), "t");
    await vi.waitFor(() =>
      expect(env.posts.some((p) => p.path === "/agents/ag1/wake")).toBe(true),
    );
  });

  it("pending agent turn locks the composer and offers stop", async () => {
    env.chat = [chatMsg("c1", "user", "waiting", { status: "queued" })];
    render(<AgentProfilePage />);
    expect(await screen.findByText("Combobulating…")).toBeInTheDocument();
    expect(screen.getByPlaceholderText("Waiting for coder to finish…")).toBeDisabled();
    await userEvent.click(screen.getByRole("button", { name: "Stop agent turn" }));
    await vi.waitFor(() =>
      expect(env.posts.some((p) => p.path === "/agents/ag1/chat/cancel")).toBe(true),
    );
    await vi.waitFor(() =>
      expect(screen.getByPlaceholderText("Message coder…")).toBeEnabled(),
    );
  });

  // S-262: the indicator must survive the first response chunk and stay
  // until the turn-complete signal (final non-interim reply), with the
  // composer locked the whole time.
  it("multi-message turn: interim reply renders but Combobulating stays and send is disabled", async () => {
    env.chat = [
      chatMsg("c1", "user", "big task", { status: "delivered" }),
      chatMsg("c2", "agent", "First I will check the logs", {
        payload: { message: "First I will check the logs", interim: true },
      }),
    ];
    render(<AgentProfilePage />);
    // The interim progress note is rendered…
    expect(await screen.findByText("First I will check the logs")).toBeInTheDocument();
    // …but the turn is not over: indicator still showing, composer locked.
    expect(screen.getByText("Combobulating…")).toBeInTheDocument();
    expect(screen.getByPlaceholderText("Waiting for coder to finish…")).toBeDisabled();
    // Send is replaced by the stop control while the turn is in flight.
    expect(screen.getByRole("button", { name: "Stop agent turn" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Send message" })).not.toBeInTheDocument();
  });

  it("turn completes on the final reply: indicator gone and send enabled", async () => {
    env.chat = [
      chatMsg("c1", "user", "big task", { status: "delivered" }),
      chatMsg("c2", "agent", "First I will check the logs", {
        payload: { message: "First I will check the logs", interim: true },
      }),
    ];
    const { rerender } = render(<AgentProfilePage />);
    await screen.findByText("Combobulating…");
    // Final (non-interim) reply lands → turn complete.
    env.chat = [
      ...env.chat,
      chatMsg("c3", "agent", "All done — summary here", {
        payload: { message: "All done — summary here" },
      }),
    ];
    rerender(<AgentProfilePage />);
    await vi.waitFor(() => {
      expect(screen.queryByText("Combobulating…")).not.toBeInTheDocument();
    });
    expect(screen.getByPlaceholderText("Message coder…")).toBeEnabled();
    expect(screen.getByRole("button", { name: "Send message" })).toBeInTheDocument();
  });

  it("turn_error closure ends the turn: indicator gone and send enabled", async () => {
    env.chat = [
      chatMsg("c1", "user", "big task", { status: "delivered" }),
      chatMsg("c2", "agent", "couldn't finish this turn", {
        payload: { message: "couldn't finish this turn", turn_error: true },
      }),
    ];
    render(<AgentProfilePage />);
    expect(await screen.findByText("couldn't finish this turn")).toBeInTheDocument();
    expect(screen.queryByText("Combobulating…")).not.toBeInTheDocument();
    expect(screen.getByPlaceholderText("Message coder…")).toBeEnabled();
  });

  it("reset chat archives via POST and shows the note", async () => {
    env.chat = [chatMsg("c1", "agent", "hi")];
    render(<AgentProfilePage />);
    await userEvent.click(screen.getByRole("button", { name: "Reset chat" }));
    await userEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Reset chat" }));
    await vi.waitFor(() =>
      expect(env.posts.some((p) => p.path === "/agents/ag1/chat/reset")).toBe(true),
    );
    expect(screen.getByText("Thread reset · 3 earlier message(s) archived to memory")).toBeInTheDocument();
  });

  it("thinking level saves immediately via PATCH", async () => {
    render(<AgentProfilePage />);
    await userEvent.selectOptions(screen.getByLabelText("Thinking level"), "high");
    await vi.waitFor(() =>
      expect(
        env.patches.some(
          (p) => p.path === "/agents/ag1" && JSON.stringify(p.body) === JSON.stringify({ thinking_level: "high" }),
        ),
      ).toBe(true),
    );
    expect(env.refreshes).toContain("/squads/sq1/agents");
  });

  it("composer model chip falls back to the bare id when the model is invisible", async () => {
    env.agents = [agent({ ai_model_id: "zzz-gone" })];
    render(<AgentProfilePage />);
    expect(await screen.findByText(/zzz-gone…/)).toBeInTheDocument();
  });
});

describe("configuration tab", () => {
  it("prefills basics and saves one trimmed PATCH for the whole form", async () => {
    render(<AgentProfilePage />);
    await openConfigTab();
    const save = screen.getByRole("button", { name: "Save configuration" });
    expect(save).toBeDisabled();
    const role = screen.getByDisplayValue("implementer");
    await userEvent.clear(role);
    await userEvent.type(role, "senior implementer");
    expect(save).toBeEnabled();
    await userEvent.click(save);
    await vi.waitFor(() => expect(env.patches).toHaveLength(1));
    expect(env.patches[0].path).toBe("/agents/ag1");
    expect(env.patches[0].body).toEqual({
      role: "senior implementer",
      system_prompt: "",
      idle_timeout_sec: 600,
      storage_enabled: true,
      storage_size: "2Gi",
      ai_model_id: "m1",
      fallback_ai_model_id: "m2",
    });
    expect(env.refreshes).toContain("/agents/ag1/permissions");
  });

  it("agent without a primary model blocks save with a reason", async () => {
    env.agents = [agent({ ai_model_id: "", fallback_ai_model_id: "" })];
    render(<AgentProfilePage />);
    await openConfigTab();
    expect(screen.getByRole("button", { name: "Save configuration" })).toBeDisabled();
    expect(screen.getByText("A primary model is required.")).toBeInTheDocument();
  });

  it("invalid storage size blocks save with a reason", async () => {
    render(<AgentProfilePage />);
    await openConfigTab();
    await userEvent.clear(screen.getByLabelText("Storage size"));
    await userEvent.type(screen.getByLabelText("Storage size"), "banana");
    expect(screen.getByRole("button", { name: "Save configuration" })).toBeDisabled();
    expect(screen.getByText("Storage size is invalid.")).toBeInTheDocument();
  });

  it("disabling durable storage clears the size in the payload", async () => {
    render(<AgentProfilePage />);
    await openConfigTab();
    await userEvent.click(screen.getByLabelText("Durable workspace storage"));
    await userEvent.click(screen.getByRole("button", { name: "Save configuration" }));
    await vi.waitFor(() => expect(env.patches).toHaveLength(1));
    expect(env.patches[0].body).toMatchObject({ storage_enabled: false, storage_size: "" });
  });

  it("no granted models shows the access notice", async () => {
    env.models = [];
    render(<AgentProfilePage />);
    await openConfigTab();
    expect(screen.getByText(/You have no granted AI Models/)).toBeInTheDocument();
  });

  it("stale binding renders the current-binding option and hint", async () => {
    env.agents = [agent({ ai_model_id: "gone-model" })];
    render(<AgentProfilePage />);
    await openConfigTab();
    expect(screen.getByText(/current binding \(gone-model…\)/)).toBeInTheDocument();
    expect(screen.getByText(/no longer granted\/active/)).toBeInTheDocument();
  });

  it("prompt restore feeds the form and save includes it", async () => {
    render(<AgentProfilePage />);
    await openConfigTab();
    await userEvent.click(screen.getByRole("button", { name: "stub-restore" }));
    await userEvent.click(screen.getByRole("button", { name: "Save configuration" }));
    await vi.waitFor(() => expect(env.patches).toHaveLength(1));
    expect(env.patches[0].body).toMatchObject({ system_prompt: "restored prompt" });
  });

  it("save failure surfaces the error alert", async () => {
    env.patchThrow = new Error("409 nope");
    render(<AgentProfilePage />);
    await openConfigTab();
    const role = screen.getByDisplayValue("implementer");
    await userEvent.clear(role);
    await userEvent.type(role, "changed");
    await userEvent.click(screen.getByRole("button", { name: "Save configuration" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("409 nope");
  });

  it("grants list revokes down to the remaining set", async () => {
    env.perms = [
      { id: "g1", agent_id: "ag1", resource_type: "skill", resource_id: "sk1", created_at: new Date().toISOString() },
      { id: "g2", agent_id: "ag1", resource_type: "tool", resource_id: "tl1", created_at: new Date().toISOString() },
    ];
    render(<AgentProfilePage />);
    await openConfigTab();
    const rows = screen.getAllByText("skill");
    expect(rows.length).toBeGreaterThan(0);
    const revokeButtons = screen.getAllByRole("button", { name: "Revoke" });
    await userEvent.click(revokeButtons[0]);
    await vi.waitFor(() => expect(env.puts).toHaveLength(1));
    expect(env.puts[0].path).toBe("/agents/ag1/permissions");
    // First revoke removes the skill grant, leaving the tool grant.
    expect(env.puts[0].body).toEqual([{ resource_type: "tool", resource_id: "tl1" }]);
    expect(env.refreshes).toContain("/agents/ag1/permissions");
  });

  it("empty grants show the empty state", async () => {
    render(<AgentProfilePage />);
    await openConfigTab();
    expect(screen.getByText("No resource grants")).toBeInTheDocument();
  });

  it("rest grant rows show narrowing label + per-agent credential and preserve constraints on revoke", async () => {
    env.perms = [
      {
        id: "g1",
        agent_id: "ag1",
        resource_type: "rest",
        resource_id: "r-gh",
        created_at: new Date().toISOString(),
        constraints: { methods: ["GET"], rate_per_min: 10 },
      },
      { id: "g2", agent_id: "ag1", resource_type: "tool", resource_id: "tl1", created_at: new Date().toISOString() },
    ];
    render(<AgentProfilePage />);
    await openConfigTab();
    // Branch: constraints label renders only when the grant narrows something.
    expect(screen.getByText(/narrowing: methods: GET/)).toBeInTheDocument();
    // Branch: rest grants mount the per-agent credential control.
    expect(screen.getByTestId("rest-agent-cred")).toHaveTextContent("cred:r-gh:ag1");

    // Revoke the rest grant → tool grant remains without constraints key.
    await userEvent.click(screen.getAllByRole("button", { name: "Revoke" })[0]);
    await vi.waitFor(() => expect(env.puts).toHaveLength(1));
    expect(env.puts[0].body).toEqual([{ resource_type: "tool", resource_id: "tl1" }]);

    // Revoke the tool grant → rest grant keeps its constraints through the PUT.
    await userEvent.click(screen.getAllByRole("button", { name: "Revoke" })[1]);
    await vi.waitFor(() => expect(env.puts).toHaveLength(2));
    expect(env.puts[1].body).toEqual([
      { resource_type: "rest", resource_id: "r-gh", constraints: { methods: ["GET"], rate_per_min: 10 } },
    ]);
  });

  it("provisions a missing identity", async () => {
    env.agents = [agent({ identity_id: undefined })];
    render(<AgentProfilePage />);
    await openConfigTab();
    await userEvent.click(screen.getByRole("button", { name: "Provision identity now" }));
    await vi.waitFor(() =>
      expect(env.posts.some((p) => p.path === "/agents/ag1/identity")).toBe(true),
    );
  });

  it("rotates an existing identity and surfaces failures", async () => {
    env.agents = [agent({ identity_id: "idn1" })];
    render(<AgentProfilePage />);
    await openConfigTab();
    await userEvent.click(screen.getByRole("button", { name: "Rotate identity" }));
    await vi.waitFor(() =>
      expect(env.posts.some((p) => p.path === "/agents/ag1/identity/rotate")).toBe(true),
    );
  });

  it("identity failure shows the error notice", async () => {
    env.agents = [agent({ identity_id: undefined })];
    // A non-Error throw exercises the fallback wording in errMsg().
    env.postThrow = "weird failure";
    render(<AgentProfilePage />);
    await openConfigTab();
    await userEvent.click(screen.getByRole("button", { name: "Provision identity now" }));
    expect(await screen.findByText("provision failed")).toBeInTheDocument();
  });

  it("delete requires the typed name, then removes and redirects", async () => {
    render(<AgentProfilePage />);
    await openConfigTab();
    await userEvent.click(screen.getByRole("button", { name: "Delete agent" }));
    const dialog = screen.getByRole("dialog");
    const confirm = within(dialog).getByRole("button", { name: "Delete" });
    expect(confirm).toBeDisabled();
    await userEvent.type(within(dialog).getByRole("textbox"), "coder");
    await userEvent.click(confirm);
    await vi.waitFor(() => expect(env.deletes).toEqual(["/agents/ag1"]));
    expect(env.push).toHaveBeenCalledWith("/squads/sq1/agents");
  });

  it("effective prompt preview opens the modal", async () => {
    render(<AgentProfilePage />);
    await openConfigTab();
    await userEvent.click(screen.getByRole("button", { name: "Preview effective prompt" }));
    expect(screen.getByText("effective-panel")).toBeInTheDocument();
  });
});

describe("grant modal", () => {
  it("grants a new resource by merging with the existing set", async () => {
    env.perms = [
      { id: "g1", agent_id: "ag1", resource_type: "skill", resource_id: "sk1", created_at: new Date().toISOString() },
    ];
    env.registry = [
      { id: "sk1", type: "skill", name: "Skill One", status: "active" },
      { id: "sk2", type: "skill", name: "Skill Two", status: "active" },
    ];
    render(<AgentProfilePage />);
    await openConfigTab();
    await userEvent.click(screen.getByRole("button", { name: "+ Grant access" }));
    const dialog = screen.getByRole("dialog");
    const resourceSelect = within(dialog).getByLabelText("Resource");
    // Already-granted sk1 is excluded from the options.
    expect(within(resourceSelect).getByRole("option", { name: "Skill Two" })).toBeInTheDocument();
    expect(within(resourceSelect).queryByRole("option", { name: "Skill One" })).not.toBeInTheDocument();
    await userEvent.selectOptions(resourceSelect, "sk2");
    await userEvent.click(within(dialog).getByRole("button", { name: "Grant" }));
    await vi.waitFor(() => expect(env.puts).toHaveLength(1));
    expect(env.puts[0].path).toBe("/agents/ag1/permissions");
    expect(env.puts[0].body).toEqual([
      { resource_type: "skill", resource_id: "sk1" },
      { resource_type: "skill", resource_id: "sk2" },
    ]);
    expect(env.refreshes).toContain("/agents/ag1/permissions");
  });
});

describe("branch coverage extras", () => {
  it("stalled-only agent: lease chip reads the stalled count", () => {
    env.tasks = [
      { id: "t9", title: "stalled only", assignee_agent_id: "ag1", execution_id: "e9", lease_expires_at: past },
    ];
    render(<AgentProfilePage />);
    expect(chip("Current lease")).toHaveTextContent("none");
    expect(chip("Current lease")).toHaveTextContent("1 stalled task(s)");
  });

  it("agent still loading renders placeholder title and disabled restart", () => {
    env.agents = [];
    env.agentsLoading = true;
    render(<AgentProfilePage />);
    expect(screen.getByRole("heading", { name: "Agent" })).toBeInTheDocument();
    expect(screen.getByText(/^no role set\s·/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Restart agent" })).toBeDisabled();
  });

  it("chat reset failure falls back to the generic note", async () => {
    env.chat = [chatMsg("c1", "agent", "hi")];
    env.postThrow = "weird non-error";
    render(<AgentProfilePage />);
    await userEvent.click(screen.getByRole("button", { name: "Reset chat" }));
    await userEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Reset chat" }));
    await vi.waitFor(() => expect(screen.getByText("reset failed")).toBeInTheDocument());
  });

  it("restart with an empty response reads zero pods", async () => {
    env.restartPods = null;
    render(<AgentProfilePage />);
    await userEvent.click(screen.getByRole("button", { name: "Restart agent" }));
    await userEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Restart agent" }));
    await vi.waitFor(() =>
      expect(screen.getByText("Restarting agent (0 pod(s) evicted)")).toBeInTheDocument(),
    );
  });

  it("message with no text and no attachments renders the (no text) placeholder", async () => {
    env.chat = [{ ...chatMsg("c1", "agent", "x"), payload: {} }];
    render(<AgentProfilePage />);
    expect(await screen.findByText("(no text)")).toBeInTheDocument();
  });

  it("attachment-only message renders thumbs instead of the placeholder", async () => {
    env.chat = [
      {
        ...chatMsg("c1", "agent", "x"),
        payload: { attachments: [{ id: "a1", url: "/u/a1", filename: "pic.png" }] },
      },
    ];
    render(<AgentProfilePage />);
    expect(await screen.findByAltText("pic.png")).toBeInTheDocument();
    expect(screen.queryByText("(no text)")).not.toBeInTheDocument();
  });

  it("cancelled user turn is flagged in the thread", async () => {
    env.chat = [chatMsg("c1", "user", "oops", { status: "cancelled" })];
    render(<AgentProfilePage />);
    expect(await screen.findByText("cancelled")).toBeInTheDocument();
  });

  it("failed tool call renders the failed state", async () => {
    env.chat = [
      chatMsg("c2", "agent", "trying", {
        payload: { message: "trying", tool_calls: [{ name: "exec", arguments: {}, ok: false }] },
      }),
    ];
    render(<AgentProfilePage />);
    expect(await screen.findByText("🔧 exec")).toBeInTheDocument();
    expect(screen.getByText("failed")).toBeInTheDocument();
  });

  it("duplicate tool calls both render with unique keys", async () => {
    const call = { name: "web_search", arguments: { q: "same" }, ok: true };
    env.chat = [
      chatMsg("c2", "agent", "searching", {
        payload: { message: "searching", tool_calls: [call, call] },
      }),
    ];
    render(<AgentProfilePage />);
    expect((await screen.findAllByText("🔧 web_search")).length).toBe(2);
  });

  it("subagent chip shows the final result text", async () => {
    env.chat = [
      chatMsg("c2", "agent", "spawned", {
        payload: {
          message: "spawned",
          tool_calls: [
            {
              name: "spawn_subagent",
              arguments: {},
              ok: true,
              result: "child final answer",
              subagent: { thread: [], turns: 2, steps: ["a", "b"] },
            },
          ],
        },
      }),
    ];
    render(<AgentProfilePage />);
    expect(await screen.findByText("child final answer")).toBeInTheDocument();
  });

  it("send failure surfaces the composer error", async () => {
    env.postThrow = new Error("send boom");
    render(<AgentProfilePage />);
    await userEvent.type(screen.getByPlaceholderText("Message coder…"), "hi");
    await userEvent.click(screen.getByRole("button", { name: "Send message" }));
    expect(await screen.findByText("send boom")).toBeInTheDocument();
  });

  it("picked image uploads, stages a chip, and rides the next send", async () => {
    const { container } = render(<AgentProfilePage />);
    const fileInput = container.querySelector('input[type="file"]') as HTMLInputElement;
    fireEvent.change(fileInput, {
      target: { files: [new File(["bytes"], "shot.png", { type: "image/png" })] },
    });
    expect(await screen.findByText("shot.png")).toBeInTheDocument();
    await userEvent.type(screen.getByPlaceholderText("Message coder…"), "see image");
    await userEvent.click(screen.getByRole("button", { name: "Send message" }));
    await vi.waitFor(() =>
      expect(
        env.posts.some(
          (p) =>
            p.path === "/agents/ag1/chat" &&
            JSON.stringify(p.body) ===
              JSON.stringify({ message: "see image", attachments: ["up-shot.png"] }),
        ),
      ).toBe(true),
    );
  });

  it("staged attachment can be removed before sending", async () => {
    const { container } = render(<AgentProfilePage />);
    const fileInput = container.querySelector('input[type="file"]') as HTMLInputElement;
    fireEvent.change(fileInput, {
      target: { files: [new File(["bytes"], "gone.png", { type: "image/png" })] },
    });
    await screen.findByText("gone.png");
    await userEvent.click(screen.getByRole("button", { name: "Remove gone.png" }));
    expect(screen.queryByText("gone.png")).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Send message" })).toBeDisabled();
  });

  it("non-image files are rejected in the composer", async () => {
    const { container } = render(<AgentProfilePage />);
    const fileInput = container.querySelector('input[type="file"]') as HTMLInputElement;
    fireEvent.change(fileInput, {
      target: { files: [new File(["text"], "notes.txt", { type: "text/plain" })] },
    });
    expect(
      await screen.findByText("Only PNG, JPEG, GIF and WebP images can be attached."),
    ).toBeInTheDocument();
  });

  it("models fetch error surfaces in the LLM binding fields", async () => {
    env.modelsError = "models down";
    render(<AgentProfilePage />);
    await openConfigTab();
    expect(screen.getByText("models down")).toBeInTheDocument();
  });

  it("single granted model shows the no-fallback hint", async () => {
    env.models = [model("m1", "Model One")];
    env.agents = [agent({ fallback_ai_model_id: "" })];
    render(<AgentProfilePage />);
    await openConfigTab();
    expect(screen.getByText("No other granted model available for fallback.")).toBeInTheDocument();
  });

  it("weak fallback triggers cost, capability and same-provider warnings", async () => {
    env.models = [
      { ...model("m1", "Model One"), pricing: { input_per_1m: 1 } },
      {
        ...model("m2", "Model Two"),
        supports_tools: false,
        context_window: 4096,
        pricing: { input_per_1m: 3 },
      },
    ];
    render(<AgentProfilePage />);
    await openConfigTab();
    expect(await screen.findByText(/failover costs more per token/)).toBeInTheDocument();
    expect(screen.getByText(/does not support tool calling/)).toBeInTheDocument();
    expect(screen.getByText(/no availability gain/)).toBeInTheDocument();
  });

  it("grant modal shows the empty-registry hint when nothing is registered", async () => {
    env.registry = [];
    render(<AgentProfilePage />);
    await openConfigTab();
    await userEvent.click(screen.getByRole("button", { name: "+ Grant access" }));
    const dialog = screen.getByRole("dialog");
    expect(within(dialog).getByText(/No skills registered/)).toBeInTheDocument();
  });

  it("grant failure shows the error in the modal", async () => {
    env.putError = new Error("grant denied");
    render(<AgentProfilePage />);
    await openConfigTab();
    await userEvent.click(screen.getByRole("button", { name: "+ Grant access" }));
    const dialog = screen.getByRole("dialog");
    await userEvent.selectOptions(within(dialog).getByLabelText("Resource"), "sk1");
    await userEvent.click(within(dialog).getByRole("button", { name: "Grant" }));
    expect(await within(dialog).findByText("grant denied")).toBeInTheDocument();
  });

  it("rotate failure shows the rotate-failed fallback", async () => {
    env.agents = [agent({ identity_id: "idn1" })];
    env.postThrow = "weird non-error";
    render(<AgentProfilePage />);
    await openConfigTab();
    await userEvent.click(screen.getByRole("button", { name: "Rotate identity" }));
    expect(await screen.findByText("rotate failed")).toBeInTheDocument();
  });

  it("storage presets set the size and clear on toggle-off", async () => {
    env.agents = [agent({ storage_enabled: false, storage_size: undefined })];
    render(<AgentProfilePage />);
    await openConfigTab();
    await userEvent.click(screen.getByLabelText("Durable workspace storage"));
    await userEvent.click(screen.getByRole("button", { name: "5Gi" }));
    expect(screen.getByLabelText("Storage size")).toHaveValue("5Gi");
    await userEvent.click(screen.getByRole("button", { name: "Save configuration" }));
    await vi.waitFor(() => expect(env.patches).toHaveLength(1));
    expect(env.patches[0].body).toMatchObject({ storage_enabled: true, storage_size: "5Gi" });
  });

  it("two-word agent name gets two-letter initials in the chat avatar", async () => {
    env.agents = [agent({ name: "Ada Lovelace" })];
    render(<AgentProfilePage />);
    expect(await screen.findByText("AL")).toBeInTheDocument();
  });
});

describe("branch coverage extras 2", () => {
  it("null board/perms/audit/chat/model payloads fall back to empty lists", async () => {
    env.boardNull = true;
    env.auditNull = true;
    env.permsNull = true;
    env.chatNull = true;
    env.modelsNull = true;
    render(<AgentProfilePage />);
    expect(screen.getByRole("heading", { name: "coder" })).toBeInTheDocument();
    expect(chip("Tasks")).toHaveTextContent("0");
    expect(chip("Grants")).toHaveTextContent("0");
    expect(chip("Context")).toHaveTextContent("—");
    await openConfigTab();
    // Null /models/me behaves like "no models granted".
    expect(screen.getByText(/You have no granted AI Models/)).toBeInTheDocument();
  });

  it("minimal agent: config defaults are empty and save sends zeros", async () => {
    env.agents = [{ id: "ag1", squad_id: "sq1", name: "sparse", status: "idle" }];
    render(<AgentProfilePage />);
    await openConfigTab();
    expect(screen.getByPlaceholderText("e.g. implementer")).toHaveValue("");
    expect(screen.getByPlaceholderText("Platform default (15 min)")).toHaveValue("");
    const save = screen.getByRole("button", { name: "Save configuration" });
    expect(save).toBeDisabled();
    await userEvent.type(screen.getByPlaceholderText("e.g. implementer"), "worker");
    // The UI requires a primary model before saving.
    expect(screen.getByText("A primary model is required.")).toBeInTheDocument();
    await userEvent.selectOptions(screen.getByLabelText("Primary model (required)"), "m1");
    await userEvent.click(save);
    await vi.waitFor(() => expect(env.patches).toHaveLength(1));
    expect(env.patches[0].body).toEqual({
      role: "worker",
      system_prompt: "",
      idle_timeout_sec: 0,
      storage_enabled: false,
      storage_size: "",
      ai_model_id: "m1",
      fallback_ai_model_id: "",
    });
  });

  it("choosing the fallback model as primary clears the fallback", async () => {
    render(<AgentProfilePage />);
    await openConfigTab();
    const primary = screen.getByLabelText("Primary model (required)");
    await userEvent.selectOptions(primary, "m2");
    const fallback = screen.getByLabelText("Fallback model (optional)");
    expect(fallback).toHaveValue("");
  });

  it("Enter in the composer sends the message", async () => {
    render(<AgentProfilePage />);
    const box = screen.getByPlaceholderText("Message coder…");
    await userEvent.type(box, "quick send{Enter}");
    await vi.waitFor(() =>
      expect(
        env.posts.some(
          (p) =>
            p.path === "/agents/ag1/chat" &&
            (p.body as { message: string }).message === "quick send",
        ),
      ).toBe(true),
    );
  });

  it("non-Error send failure shows the generic composer message", async () => {
    env.postThrow = "weird string failure";
    render(<AgentProfilePage />);
    await userEvent.type(screen.getByPlaceholderText("Message coder…"), "hi");
    await userEvent.click(screen.getByRole("button", { name: "Send message" }));
    expect(await screen.findByText("send failed")).toBeInTheDocument();
  });

  it("non-Error cancel failure shows the generic cancel message", async () => {
    env.chat = [chatMsg("c1", "user", "waiting", { status: "queued" })];
    render(<AgentProfilePage />);
    await screen.findByText("Combobulating…");
    // Wake/cancel both go through apiPost; let wake pass, fail the cancel.
    env.postThrow = "weird string failure";
    await userEvent.click(screen.getByRole("button", { name: "Stop agent turn" }));
    expect(await screen.findByText("cancel failed")).toBeInTheDocument();
  });

  it("duplicate upload ids are deduped in the staged chips", async () => {
    const { container } = render(<AgentProfilePage />);
    const fileInput = container.querySelector('input[type="file"]') as HTMLInputElement;
    fireEvent.change(fileInput, {
      target: {
        files: [
          new File(["a"], "dup.png", { type: "image/png" }),
          new File(["b"], "dup.png", { type: "image/png" }),
        ],
      },
    });
    await vi.waitFor(() => expect(screen.getAllByText("dup.png").length).toBe(1));
  });

  it("pasting an image stages it like the file picker", async () => {
    render(<AgentProfilePage />);
    const box = screen.getByPlaceholderText("Message coder…");
    const file = new File(["img"], "pasted.png", { type: "image/png" });
    fireEvent.paste(box, {
      clipboardData: {
        items: [{ kind: "file", type: "image/png", getAsFile: () => file }],
      },
    });
    expect(await screen.findByText("pasted.png")).toBeInTheDocument();
  });

  it("non-Error grant failure shows the generic grant message", async () => {
    env.putError = "weird string failure";
    render(<AgentProfilePage />);
    await openConfigTab();
    await userEvent.click(screen.getByRole("button", { name: "+ Grant access" }));
    const dialog = screen.getByRole("dialog");
    await userEvent.selectOptions(within(dialog).getByLabelText("Resource"), "sk1");
    await userEvent.click(within(dialog).getByRole("button", { name: "Grant" }));
    expect(await within(dialog).findByText("grant failed")).toBeInTheDocument();
  });

  it("registry fetch error surfaces in the grant modal", async () => {
    env.registryError = "registry down";
    render(<AgentProfilePage />);
    await openConfigTab();
    await userEvent.click(screen.getByRole("button", { name: "+ Grant access" }));
    const dialog = screen.getByRole("dialog");
    expect(within(dialog).getByText("registry down")).toBeInTheDocument();
  });

  it("MTD metering still loading shows the ellipsis value", () => {
    env.meteringMtd = null;
    env.meteringMtdLoading = true;
    render(<AgentProfilePage />);
    expect(chip("MTD spend")).toHaveTextContent("…");
  });

  it("failed subagent chip renders the failed state with its result", async () => {
    env.chat = [
      chatMsg("c2", "agent", "spawn failed", {
        payload: {
          message: "spawn failed",
          tool_calls: [
            {
              name: "spawn_subagent",
              arguments: {},
              ok: false,
              result: "child crashed",
              subagent: { thread: [], turns: 0, steps: ["x"] },
            },
          ],
        },
      }),
    ];
    render(<AgentProfilePage />);
    const chipEl = await screen.findByText("🤖 subagent");
    expect(chipEl.closest(".subagent-chip")).toHaveClass("failed");
    expect(screen.getByText("child crashed")).toBeInTheDocument();
  });

  it("blank agent name falls back to the ? avatar", async () => {
    env.agents = [agent({ name: "   " })];
    render(<AgentProfilePage />);
    expect(await screen.findByText("?")).toBeInTheDocument();
  });

  it("reset with an empty response reads zero archived messages", async () => {
    env.chat = [chatMsg("c1", "agent", "hi")];
    env.resetEmpty = true;
    render(<AgentProfilePage />);
    await userEvent.click(screen.getByRole("button", { name: "Reset chat" }));
    await userEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Reset chat" }));
    await vi.waitFor(() =>
      expect(
        screen.getByText("Thread reset · 0 earlier message(s) archived to memory"),
      ).toBeInTheDocument(),
    );
  });
});
