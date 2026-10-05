// S-189: jsdom behaviour tests for PromptTemplatesPanel. The pure
// selection helpers stay real (importOriginal); only the four network
// functions are mocked, so these lock the interaction contract:
// select-all/indeterminate wiring, the bulk-delete confirm round-trip
// (ids sent, notice rendered, onChanged fired), create + edit modal
// submits, and error surfacing.
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";

const net = vi.hoisted(() => ({
  create: vi.fn(),
  update: vi.fn(),
  remove: vi.fn(),
  bulkDelete: vi.fn(),
}));

vi.mock("../lib/promptTemplates", async (importOriginal) => {
  const mod = await importOriginal<typeof import("../lib/promptTemplates")>();
  return {
    ...mod,
    createPromptTemplate: net.create,
    updatePromptTemplate: net.update,
    deletePromptTemplate: net.remove,
    bulkDeletePromptTemplates: net.bulkDelete,
  };
});

vi.mock("../lib/auth", () => ({
  useAuth: () => ({ token: "tok", mode: "token" }),
}));

import { PromptTemplatesPanel } from "./PromptTemplatesPanel";
import type { PromptTemplate } from "../lib/promptTemplates";

function tpl(id: string, name: string, overrides: Partial<PromptTemplate> = {}): PromptTemplate {
  return {
    id,
    name,
    description: `desc ${name}`,
    content: "content",
    applies_to: "agent",
    created_by: "ross",
    created_at: "2026-10-01T00:00:00Z",
    updated_at: "2026-10-01T00:00:00Z",
    ...overrides,
  };
}

const onChanged = vi.fn();

beforeEach(() => {
  net.create.mockReset().mockResolvedValue(tpl("new1", "New"));
  net.update.mockReset().mockResolvedValue(tpl("t1", "Edited"));
  net.remove.mockReset().mockResolvedValue(undefined);
  net.bulkDelete.mockReset().mockResolvedValue(2);
  onChanged.mockReset();
});

describe("list states", () => {
  it("shows the loading hint while fetching with no rows yet", () => {
    render(<PromptTemplatesPanel templates={[]} loading onChanged={onChanged} />);
    expect(screen.getByText("Loading templates…")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Delete selected/ })).not.toBeInTheDocument();
  });

  it("shows the empty state when there are no templates", () => {
    render(<PromptTemplatesPanel templates={[]} onChanged={onChanged} />);
    expect(screen.getByText("No prompt templates yet")).toBeInTheDocument();
    expect(screen.queryByLabelText("Select all templates")).not.toBeInTheDocument();
  });

  it("renders rows with name, description and applies-to label", () => {
    render(
      <PromptTemplatesPanel
        templates={[
          tpl("t1", "Reviewer", { applies_to: "both" }),
          tpl("t2", "Squad kickoff", { applies_to: "squad", description: "" }),
        ]}
        onChanged={onChanged}
      />,
    );
    expect(screen.getByText("Reviewer")).toBeInTheDocument();
    expect(screen.getByText("Squads + Agents")).toBeInTheDocument();
    // Blank description falls back to the em-dash placeholder.
    expect(screen.getByText("—")).toBeInTheDocument();
    expect(screen.getByText("Squad kickoff")).toBeInTheDocument();
  });
});

describe("selection + bulk delete", () => {
  const templates = [tpl("t1", "One"), tpl("t2", "Two"), tpl("t3", "Three")];

  it("select-all checks every row and enables the bulk delete", async () => {
    render(<PromptTemplatesPanel templates={templates} onChanged={onChanged} />);
    const bulkBtn = screen.getByRole("button", { name: "Delete 0 selected templates" });
    expect(bulkBtn).toBeDisabled();
    await userEvent.click(screen.getByLabelText("Select all templates"));
    expect(screen.getByRole("checkbox", { name: "Select template One" })).toBeChecked();
    expect(screen.getByRole("checkbox", { name: "Select template Three" })).toBeChecked();
    expect(screen.getByText("3 selected")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Delete 3 selected templates" })).toBeEnabled();
  });

  it("partial selection marks the header checkbox indeterminate", async () => {
    render(<PromptTemplatesPanel templates={templates} onChanged={onChanged} />);
    const header = screen.getByLabelText("Select all templates") as HTMLInputElement;
    await userEvent.click(screen.getByRole("checkbox", { name: "Select template Two" }));
    expect(header.indeterminate).toBe(true);
    expect(header.checked).toBe(false);
    // Selecting the rest clears indeterminate and checks the header.
    await userEvent.click(screen.getByRole("checkbox", { name: "Select template One" }));
    await userEvent.click(screen.getByRole("checkbox", { name: "Select template Three" }));
    expect(header.indeterminate).toBe(false);
    expect(header.checked).toBe(true);
  });

  it("unselect-all clears the selection", async () => {
    render(<PromptTemplatesPanel templates={templates} onChanged={onChanged} />);
    const header = screen.getByLabelText("Select all templates");
    await userEvent.click(header);
    await userEvent.click(header);
    expect(screen.getByRole("checkbox", { name: "Select template One" })).not.toBeChecked();
    expect(screen.getByRole("button", { name: "Delete 0 selected templates" })).toBeDisabled();
  });

  it("bulk delete confirms, sends the selected ids and fires onChanged", async () => {
    render(<PromptTemplatesPanel templates={templates} onChanged={onChanged} />);
    await userEvent.click(screen.getByRole("checkbox", { name: "Select template One" }));
    await userEvent.click(screen.getByRole("checkbox", { name: "Select template Two" }));
    await userEvent.click(screen.getByRole("button", { name: "Delete 2 selected templates" }));
    const dialog = screen.getByRole("dialog");
    expect(within(dialog).getByText(/permanently delete 2 prompt templates/)).toBeInTheDocument();
    await userEvent.click(within(dialog).getByRole("button", { name: "Delete 2" }));
    await vi.waitFor(() => expect(net.bulkDelete).toHaveBeenCalledWith("tok", ["t1", "t2"]));
    expect(screen.getByText("Deleted 2 templates.")).toBeInTheDocument();
    expect(onChanged).toHaveBeenCalledTimes(1);
    // Selection is cleared after the delete.
    expect(screen.getByRole("button", { name: "Delete 0 selected templates" })).toBeDisabled();
  });

  it("bulk delete singular wording for one template", async () => {
    render(<PromptTemplatesPanel templates={templates} onChanged={onChanged} />);
    await userEvent.click(screen.getByRole("checkbox", { name: "Select template One" }));
    await userEvent.click(screen.getByRole("button", { name: "Delete 1 selected templates" }));
    const dialog = screen.getByRole("dialog");
    expect(within(dialog).getByText(/permanently delete 1 prompt template\./)).toBeInTheDocument();
    net.bulkDelete.mockResolvedValue(1);
    await userEvent.click(within(dialog).getByRole("button", { name: "Delete 1" }));
    await vi.waitFor(() => expect(screen.getByText("Deleted 1 template.")).toBeInTheDocument());
  });

  it("bulk delete failure surfaces the error and keeps the dialog closed", async () => {
    net.bulkDelete.mockRejectedValue(new Error("403 forbidden"));
    render(<PromptTemplatesPanel templates={templates} onChanged={onChanged} />);
    await userEvent.click(screen.getByRole("checkbox", { name: "Select template One" }));
    await userEvent.click(screen.getByRole("button", { name: "Delete 1 selected templates" }));
    await userEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Delete 1" }));
    await vi.waitFor(() => expect(screen.getByText("403 forbidden")).toBeInTheDocument());
    expect(onChanged).not.toHaveBeenCalled();
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });

  it("cancelling the confirm dialog performs no delete", async () => {
    render(<PromptTemplatesPanel templates={templates} onChanged={onChanged} />);
    await userEvent.click(screen.getByRole("checkbox", { name: "Select template One" }));
    await userEvent.click(screen.getByRole("button", { name: "Delete 1 selected templates" }));
    await userEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Cancel" }));
    expect(net.bulkDelete).not.toHaveBeenCalled();
  });
});

describe("create + edit modals", () => {
  it("create modal blocks submit until name and content are filled, then posts the form", async () => {
    render(<PromptTemplatesPanel templates={[]} onChanged={onChanged} />);
    await userEvent.click(screen.getByRole("button", { name: "+ New template" }));
    const dialog = screen.getByRole("dialog");
    const submit = within(dialog).getByRole("button", { name: "Save template" });
    expect(submit).toBeDisabled();
    await userEvent.type(within(dialog).getByLabelText(/Name/), "Code Reviewer");
    expect(submit).toBeDisabled(); // content still empty
    await userEvent.type(within(dialog).getByLabelText(/Content/), "Review the diff");
    expect(submit).toBeEnabled();
    await userEvent.selectOptions(within(dialog).getByLabelText(/Applies to/), "squad");
    await userEvent.type(within(dialog).getByLabelText(/Description/), "cr desc");
    await userEvent.click(submit);
    await vi.waitFor(() =>
      expect(net.create).toHaveBeenCalledWith("tok", {
        name: "Code Reviewer",
        description: "cr desc",
        content: "Review the diff",
        applies_to: "squad",
      }),
    );
    expect(onChanged).toHaveBeenCalled();
    expect(screen.getByText("Template created.")).toBeInTheDocument();
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });

  it("create failure keeps the modal open with the error", async () => {
    net.create.mockRejectedValue(new Error("dup name"));
    render(<PromptTemplatesPanel templates={[]} onChanged={onChanged} />);
    await userEvent.click(screen.getByRole("button", { name: "+ New template" }));
    const dialog = screen.getByRole("dialog");
    await userEvent.type(within(dialog).getByLabelText(/Name/), "X");
    await userEvent.type(within(dialog).getByLabelText(/Content/), "Y");
    await userEvent.click(within(dialog).getByRole("button", { name: "Save template" }));
    await vi.waitFor(() => expect(within(dialog).getByText("dup name")).toBeInTheDocument());
    expect(screen.queryByText("Template created.")).not.toBeInTheDocument();
    expect(onChanged).not.toHaveBeenCalled();
  });

  it("edit modal prefills from the template and PATCHes by id", async () => {
    const t = tpl("t1", "Reviewer", { description: "d", content: "c", applies_to: "both" });
    render(<PromptTemplatesPanel templates={[t]} onChanged={onChanged} />);
    await userEvent.click(screen.getByRole("button", { name: "Edit" }));
    const dialog = screen.getByRole("dialog");
    expect(within(dialog).getByLabelText(/Name/)).toHaveValue("Reviewer");
    await userEvent.clear(within(dialog).getByLabelText(/Name/));
    await userEvent.type(within(dialog).getByLabelText(/Name/), "Senior Reviewer");
    await userEvent.click(within(dialog).getByRole("button", { name: "Save template" }));
    await vi.waitFor(() =>
      expect(net.update).toHaveBeenCalledWith("tok", "t1", {
        name: "Senior Reviewer",
        description: "d",
        content: "c",
        applies_to: "both",
      }),
    );
    expect(screen.getByText("Template saved.")).toBeInTheDocument();
    expect(onChanged).toHaveBeenCalled();
  });

  it("closing the create modal without saving fires nothing", async () => {
    render(<PromptTemplatesPanel templates={[]} onChanged={onChanged} />);
    await userEvent.click(screen.getByRole("button", { name: "+ New template" }));
    await userEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Cancel" }));
    expect(net.create).not.toHaveBeenCalled();
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });
});
