import { beforeEach, describe, expect, it, vi } from "vitest";

// S-182: logic-layer tests for the prompt-templates module (S-158).
// The api module is mocked so the async wrappers can be asserted without a
// live control plane; the pure helpers are tested directly.

const api = vi.hoisted(() => ({
  apiGet: vi.fn(),
  apiPost: vi.fn(),
  apiPatch: vi.fn(),
  apiDelete: vi.fn(),
  apiDeleteWithBody: vi.fn(),
}));

vi.mock("./api", () => api);

import {
  bulkDeletePromptTemplates,
  createPromptTemplate,
  deletePromptTemplate,
  emptyTemplateForm,
  formFromTemplate,
  listPromptTemplates,
  pruneTemplateSelection,
  allTemplateIds,
  emptyTemplateSelection,
  templateMatchesAppliesTo,
  toggleTemplateSelection,
  updatePromptTemplate,
  validateTemplateForm,
  type PromptTemplate,
} from "./promptTemplates";

function template(overrides: Partial<PromptTemplate> = {}): PromptTemplate {
  return {
    id: "tpl-1",
    name: "Code reviewer",
    description: "Reviews PRs politely",
    content: "You review code.",
    applies_to: "agent",
    created_by: "ross",
    created_at: "2026-09-30T00:00:00Z",
    updated_at: "2026-09-30T00:00:00Z",
    ...overrides,
  };
}

beforeEach(() => {
  vi.clearAllMocks();
});

describe("templateMatchesAppliesTo", () => {
  it("matches exact applies_to", () => {
    expect(templateMatchesAppliesTo(template({ applies_to: "agent" }), "agent")).toBe(true);
    expect(templateMatchesAppliesTo(template({ applies_to: "squad" }), "squad")).toBe(true);
  });

  it("both matches squad and agent", () => {
    const both = template({ applies_to: "both" });
    expect(templateMatchesAppliesTo(both, "squad")).toBe(true);
    expect(templateMatchesAppliesTo(both, "agent")).toBe(true);
  });

  it("does not cross-match", () => {
    expect(templateMatchesAppliesTo(template({ applies_to: "agent" }), "squad")).toBe(false);
    expect(templateMatchesAppliesTo(template({ applies_to: "squad" }), "agent")).toBe(false);
  });
});

describe("form helpers", () => {
  it("emptyTemplateForm defaults to agent", () => {
    expect(emptyTemplateForm()).toEqual({
      name: "",
      description: "",
      content: "",
      applies_to: "agent",
    });
  });

  it("formFromTemplate copies the editable fields", () => {
    const t = template({ name: "N", description: "D", content: "C", applies_to: "squad" });
    expect(formFromTemplate(t)).toEqual({ name: "N", description: "D", content: "C", applies_to: "squad" });
  });

  it("formFromTemplate normalizes a missing description", () => {
    const t = template({ description: undefined as unknown as string });
    expect(formFromTemplate(t).description).toBe("");
  });
});

describe("validateTemplateForm", () => {
  it("requires a name", () => {
    expect(validateTemplateForm({ ...emptyTemplateForm(), name: "  ", content: "x" })).toBe(
      "Name is required.",
    );
  });

  it("requires content", () => {
    expect(validateTemplateForm({ ...emptyTemplateForm(), name: "ok", content: " " })).toBe(
      "Content is required.",
    );
  });

  it("accepts a filled form", () => {
    expect(validateTemplateForm({ ...emptyTemplateForm(), name: "ok", content: "body" })).toBe("");
  });
});

describe("api wrappers", () => {
  it("listPromptTemplates without filter hits the bare path", async () => {
    api.apiGet.mockResolvedValue([template()]);
    const out = await listPromptTemplates("tok");
    expect(api.apiGet).toHaveBeenCalledWith("/prompt-templates", "tok");
    expect(out).toHaveLength(1);
  });

  it("listPromptTemplates passes the applies_to filter", async () => {
    api.apiGet.mockResolvedValue([]);
    await listPromptTemplates("tok", "squad");
    expect(api.apiGet).toHaveBeenCalledWith("/prompt-templates?applies_to=squad", "tok");
  });

  it("createPromptTemplate trims name/description but keeps content verbatim", async () => {
    api.apiPost.mockResolvedValue(template());
    await createPromptTemplate("tok", {
      name: "  Padded  ",
      description: "  desc  ",
      content: "  keep me  ",
      applies_to: "agent",
    });
    expect(api.apiPost).toHaveBeenCalledWith(
      "/prompt-templates",
      "tok",
      { name: "Padded", description: "desc", content: "  keep me  ", applies_to: "agent" },
    );
  });

  it("updatePromptTemplate patches the templated path", async () => {
    api.apiPatch.mockResolvedValue(template({ id: "tpl-9" }));
    const out = await updatePromptTemplate("tok", "tpl-9", {
      name: "Renamed",
      description: "d",
      content: "c",
      applies_to: "both",
    });
    expect(api.apiPatch).toHaveBeenCalledWith(
      "/prompt-templates/tpl-9",
      "tok",
      { name: "Renamed", description: "d", content: "c", applies_to: "both" },
    );
    expect(out.id).toBe("tpl-9");
  });

  it("deletePromptTemplate hits the delete endpoint", async () => {
    api.apiDelete.mockResolvedValue(undefined);
    await deletePromptTemplate("tok", "tpl-3");
    expect(api.apiDelete).toHaveBeenCalledWith("/prompt-templates/tpl-3", "tok");
  });
});

// S-214: bulk delete wrapper + selection helpers.
describe("bulkDeletePromptTemplates (S-214)", () => {
  it("DELETEs /prompt-templates/bulk with the id list and returns the deleted count", async () => {
    api.apiDeleteWithBody.mockResolvedValue({ deleted: 2 });
    const deleted = await bulkDeletePromptTemplates("tok", ["a", "b", "c"]);
    expect(api.apiDeleteWithBody).toHaveBeenCalledWith("/prompt-templates/bulk", "tok", {
      ids: ["a", "b", "c"],
    });
    expect(deleted).toBe(2);
  });

  it("returns 0 when the server reports no rows deleted", async () => {
    api.apiDeleteWithBody.mockResolvedValue({});
    expect(await bulkDeletePromptTemplates("tok", ["gone"])).toBe(0);
  });
});

describe("selection helpers (S-214)", () => {
  it("toggleTemplateSelection adds and removes ids", () => {
    const empty = new Set<string>();
    const added = toggleTemplateSelection(empty, "x");
    expect(added.has("x")).toBe(true);
    const removed = toggleTemplateSelection(added, "x");
    expect(removed.has("x")).toBe(false);
    // Originals are never mutated.
    expect(empty.size).toBe(0);
    expect(added.size).toBe(1);
  });

  it("allTemplateIds returns every id; emptyTemplateSelection returns none", () => {
    const tpls = [{ id: "a" }, { id: "b" }];
    expect([...allTemplateIds(tpls).values()].sort()).toEqual(["a", "b"]);
    expect(emptyTemplateSelection().size).toBe(0);
  });

  it("pruneTemplateSelection drops ids missing from the live list", () => {
    const selected = new Set(["a", "b", "ghost"]);
    const pruned = pruneTemplateSelection(selected, [{ id: "a" }, { id: "b" }]);
    expect([...pruned].sort()).toEqual(["a", "b"]);
  });
});
