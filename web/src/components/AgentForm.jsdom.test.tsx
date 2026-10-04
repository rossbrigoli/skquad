// S-189: jsdom behaviour tests for AgentFormModal (previously 0%).
// Covers: create-mode gating (name + primary model required), edit-mode
// name lock, model loading/empty hints, storage toggle + size
// validation, submit payload shaping (idle default, storage off),
// prompt-validation blocking, and submit error surfacing.
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";

const promptState = vi.hoisted(() => ({
  result: null as null | {
    valid: boolean;
    scope: string;
    tokens: number;
    soft_warn: number;
    hard_cap: number;
    warnings?: string[];
    error?: { code: string; message: string };
  },
  validating: false,
}));

vi.mock("../lib/usePromptValidation", () => ({
  usePromptValidation: () => promptState,
}));

vi.mock("./PromptTemplatePicker", () => ({
  PromptTemplatePicker: () => <div data-testid="template-picker" />,
}));

import { AgentFormModal } from "./AgentForm";
import type { AIModel } from "../lib/aimodels";

const MODELS = [
  { id: "m1", provider: "anthropic", model_name: "claude-x" },
  { id: "m2", provider: "openai", model_name: "gpt-y" },
] as unknown as AIModel[];

beforeEach(() => {
  promptState.result = null;
  promptState.validating = false;
});

function renderCreate(onSubmit = vi.fn().mockResolvedValue(undefined)) {
  render(
    <AgentFormModal
      title="New Agent"
      submitLabel="Create"
      models={MODELS}
      onSubmit={onSubmit}
      onClose={() => undefined}
    />,
  );
  return onSubmit;
}

describe("AgentFormModal create mode", () => {
  it("blocks submit until the name is filled in", async () => {
    renderCreate();
    const submit = screen.getByRole("button", { name: "Create" });
    expect(submit).toBeDisabled();
    await userEvent.type(screen.getByPlaceholderText("e.g. coder-1"), "coder-1");
    expect(submit).toBeDisabled(); // still needs a model
  });

  it("requires a primary model on create and passes it through", async () => {
    const onSubmit = renderCreate();
    await userEvent.type(screen.getByPlaceholderText("e.g. coder-1"), "coder-1");
    const select = screen.getByLabelText(/Primary model/);
    expect(select).toHaveValue("");
    expect(screen.getByRole("button", { name: "Create" })).toBeDisabled();
    await userEvent.selectOptions(select, "m2");
    expect(screen.getByRole("button", { name: "Create" })).toBeEnabled();
    await userEvent.click(screen.getByRole("button", { name: "Create" }));
    await waitFor(() =>
      expect(onSubmit).toHaveBeenCalledWith(
        expect.objectContaining({ name: "coder-1", ai_model_id: "m2" }),
      ),
    );
  });

  it("shows the model loading hint", () => {
    render(
      <AgentFormModal
        title="New Agent"
        submitLabel="Create"
        modelsLoading
        onSubmit={vi.fn()}
        onClose={() => undefined}
      />,
    );
    expect(screen.getByText("loading models…")).toBeInTheDocument();
  });

  it("warns when the user has no granted models", () => {
    render(
      <AgentFormModal
        title="New Agent"
        submitLabel="Create"
        models={[]}
        modelsLoading={false}
        onSubmit={vi.fn()}
        onClose={() => undefined}
      />,
    );
    expect(screen.getByText(/no granted AI models/)).toBeInTheDocument();
  });

  it("shapes the submit payload: idle blank → 0, storage off → empty size", async () => {
    const onSubmit = renderCreate();
    await userEvent.type(screen.getByPlaceholderText("e.g. coder-1"), " agent-9 ");
    await userEvent.type(screen.getByPlaceholderText("e.g. implementer"), " reviewer ");
    await userEvent.selectOptions(screen.getByLabelText(/Primary model/), "m1");
    await userEvent.click(screen.getByRole("button", { name: "Create" }));
    await waitFor(() =>
      expect(onSubmit).toHaveBeenCalledWith({
        name: "agent-9",
        role: "reviewer",
        system_prompt: "",
        idle_timeout_sec: 0,
        storage_enabled: false,
        storage_size: "",
        ai_model_id: "m1",
      }),
    );
  });

  it("strips non-digits from the idle timeout input", async () => {
    renderCreate();
    const idle = screen.getByPlaceholderText("Platform default (15 min)");
    await userEvent.type(idle, "12ab34");
    expect(idle).toHaveValue("1234");
  });
});

describe("AgentFormModal storage", () => {
  it("toggles presets, validates the size and includes it in the payload", async () => {
    const onSubmit = renderCreate();
    await userEvent.type(screen.getByPlaceholderText("e.g. coder-1"), "a1");
    await userEvent.selectOptions(screen.getByLabelText(/Primary model/), "m1");
    const checkbox = screen.getByRole("checkbox");
    await userEvent.click(checkbox);
    const size = screen.getByLabelText("Storage size");
    // Default size is valid; type garbage → submit disabled + hint.
    await userEvent.clear(size);
    await userEvent.type(size, "banana");
    expect(screen.getByText(/Must be a positive quantity/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Create" })).toBeDisabled();
    // Preset fixes it.
    await userEvent.click(screen.getByRole("button", { name: "2Gi" }));
    expect(size).toHaveValue("2Gi");
    await userEvent.click(screen.getByRole("button", { name: "Create" }));
    await waitFor(() =>
      expect(onSubmit).toHaveBeenCalledWith(
        expect.objectContaining({ storage_enabled: true, storage_size: "2Gi" }),
      ),
    );
  });

  it("surfaces a submit failure as an inline error and stays open", async () => {
    const onSubmit = vi.fn().mockRejectedValue(new Error("name taken"));
    render(
      <AgentFormModal
        title="New Agent"
        submitLabel="Create"
        models={MODELS}
        onSubmit={onSubmit}
        onClose={() => undefined}
      />,
    );
    await userEvent.type(screen.getByPlaceholderText("e.g. coder-1"), "dup");
    await userEvent.selectOptions(screen.getByLabelText(/Primary model/), "m1");
    await userEvent.click(screen.getByRole("button", { name: "Create" }));
    expect(await screen.findByText("name taken")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Create" })).toBeEnabled();
  });
});

describe("AgentFormModal edit mode", () => {
  it("locks the name and hides the model picker + template picker", () => {
    render(
      <AgentFormModal
        initial={{ name: "old-agent", role: "worker", idle_timeout_sec: 300 }}
        title="Edit Agent"
        submitLabel="Save"
        onSubmit={vi.fn()}
        onClose={() => undefined}
      />,
    );
    const name = screen.getByDisplayValue("old-agent");
    expect(name).toHaveAttribute("readonly");
    expect(screen.getByText(/cannot be renamed after creation/)).toBeInTheDocument();
    expect(screen.queryByLabelText(/Primary model/)).not.toBeInTheDocument();
    expect(screen.queryByTestId("template-picker")).not.toBeInTheDocument();
  });

  it("blocks save while prompt validation reports invalid", async () => {
    promptState.result = {
      valid: false,
      scope: "agent",
      tokens: 9999,
      soft_warn: 800,
      hard_cap: 1200,
      error: { code: "hard_cap_exceeded", message: "prompt is over the hard cap" },
    };
    render(
      <AgentFormModal
        initial={{ name: "a1" }}
        title="Edit Agent"
        submitLabel="Save"
        onSubmit={vi.fn()}
        onClose={() => undefined}
      />,
    );
    expect(screen.getByRole("alert")).toHaveTextContent("prompt is over the hard cap");
    expect(screen.getByRole("button", { name: "Save" })).toBeDisabled();
  });

  it("renders prompt warnings when present", () => {
    promptState.result = {
      valid: true,
      scope: "agent",
      tokens: 900,
      soft_warn: 800,
      hard_cap: 1200,
      warnings: ["approaching the soft warning threshold"],
    };
    render(
      <AgentFormModal
        initial={{ name: "a1" }}
        title="Edit Agent"
        submitLabel="Save"
        onSubmit={vi.fn()}
        onClose={() => undefined}
      />,
    );
    expect(screen.getByText("approaching the soft warning threshold")).toBeInTheDocument();
  });
});
