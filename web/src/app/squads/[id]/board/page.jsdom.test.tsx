// S-235: New Task dialog tests — the dialog gets the `wider` sizing
// (~50% bigger, matching New Squad/New Agent) and an "Attach image"
// control that reuses the S-194 upload flow: upload into the squad,
// then post the attachment onto the new task's thread. The control is
// locked until an assignee is picked (thread messages need a recipient).
import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { TaskCreateModal } from "./page";
import * as api from "../../../../lib/api";

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

vi.mock("../../../../lib/useApi", () => ({
  useApi: () => ({ data: null, loading: false, error: "", refresh: () => undefined }),
}));

vi.mock("../../../../lib/auth", () => ({
  useAuth: () => ({ token: "tok", mode: "token", user: null, authed: true }),
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
    apiPost: vi.fn(async () => ({ id: "t9" })),
    apiUploadImage: vi.fn(async () => ({
      id: "up1",
      filename: "shot.png",
      content_type: "image/png",
      size_bytes: 4,
      url: "/api/v1/uploads/up1",
    })),
  };
});

function renderModal() {
  return render(
    <TaskCreateModal
      columnLabel="Backlog"
      agents={[{ id: "a1", squad_id: "s1", name: "Bob" } as api.Agent]}
      squadId="s1"
      token="tok"
      targetStatus="backlog"
      onClose={() => undefined}
      onCreated={vi.fn()}
    />,
  );
}

beforeEach(() => {
  vi.mocked(api.apiPost).mockClear();
  vi.mocked(api.apiUploadImage).mockClear();
});

describe("dialog sizing (requirement 6)", () => {
  it("uses the wider modal variant (~50% bigger)", () => {
    renderModal();
    const card = document.querySelector(".modal-card");
    expect(card?.className).toContain("modal-wider");
  });
});

describe("attach image (requirement 6)", () => {
  it("is disabled until an assignee is selected", () => {
    renderModal();
    const attach = screen.getByRole("button", { name: /Attach image/ });
    expect(attach).toBeDisabled();
    fireEvent.change(screen.getByLabelText(/Assignee/), { target: { value: "a1" } });
    expect(screen.getByRole("button", { name: /Attach image/ })).toBeEnabled();
  });

  it("uploads the picked file and posts it to the new task's thread on submit", async () => {
    renderModal();
    fireEvent.change(screen.getByLabelText(/Assignee/), { target: { value: "a1" } });
    fireEvent.change(document.querySelector('input[type="file"]')!, {
      target: { files: [new File(["data"], "shot.png", { type: "image/png" })] },
    });
    expect(screen.getByText("shot.png")).toBeInTheDocument();

    fireEvent.change(screen.getByLabelText(/Title/), { target: { value: "Broken header" } });
    fireEvent.click(screen.getByRole("button", { name: "Create task" }));

    await waitFor(() => expect(vi.mocked(api.apiUploadImage)).toHaveBeenCalled());
    const posts = vi.mocked(api.apiPost).mock.calls.map((c) => c[0]);
    expect(posts).toContain("/squads/s1/board/tasks");
    expect(posts).toContain("/tasks/t9/messages");
    const messageCall = vi.mocked(api.apiPost).mock.calls.find((c) => c[0] === "/tasks/t9/messages");
    expect((messageCall?.[2] as { attachments: string[] }).attachments).toEqual(["up1"]);
  });

  it("skips the attachment flow when no file was picked", async () => {
    renderModal();
    fireEvent.change(screen.getByLabelText(/Title/), { target: { value: "Plain task" } });
    fireEvent.click(screen.getByRole("button", { name: "Create task" }));
    await waitFor(() =>
      expect(vi.mocked(api.apiPost).mock.calls.some((c) => c[0] === "/squads/s1/board/tasks")).toBe(true),
    );
    expect(vi.mocked(api.apiUploadImage)).not.toHaveBeenCalled();
  });

  it("rejects non-image files client-side", () => {
    renderModal();
    fireEvent.change(screen.getByLabelText(/Assignee/), { target: { value: "a1" } });
    fireEvent.change(document.querySelector('input[type="file"]')!, {
      target: { files: [new File(["data"], "notes.txt", { type: "text/plain" })] },
    });
    expect(screen.getByText(/Only PNG, JPEG, GIF and WebP/)).toBeInTheDocument();
    expect(screen.queryByText("notes.txt")).not.toBeInTheDocument();
  });
});
