// S-158: prompt templates — admin-managed reusable starting points for
// squad and agent system prompts. Selecting a template COPIES its content
// into the draft prompt; templates are never live references.

import { apiDelete, apiDeleteWithBody, apiGet, apiPatch, apiPost } from "./api";

export type TemplateAppliesTo = "squad" | "agent" | "both";

export type PromptTemplate = {
  id: string;
  name: string;
  description: string;
  content: string;
  applies_to: TemplateAppliesTo;
  created_by: string;
  created_at: string;
  updated_at: string;
};

export type PromptTemplateFormValues = {
  name: string;
  description: string;
  content: string;
  applies_to: TemplateAppliesTo;
};

export async function listPromptTemplates(
  token: string,
  appliesTo?: "squad" | "agent",
): Promise<PromptTemplate[]> {
  const qs = appliesTo ? `?applies_to=${appliesTo}` : "";
  return apiGet<PromptTemplate[]>(`/prompt-templates${qs}`, token);
}

export function templateMatchesAppliesTo(t: PromptTemplate, wants: "squad" | "agent"): boolean {
  return t.applies_to === wants || t.applies_to === "both";
}

export function emptyTemplateForm(): PromptTemplateFormValues {
  return { name: "", description: "", content: "", applies_to: "agent" };
}

export function formFromTemplate(t: PromptTemplate): PromptTemplateFormValues {
  return {
    name: t.name,
    description: t.description ?? "",
    content: t.content,
    applies_to: t.applies_to,
  };
}

export function validateTemplateForm(v: PromptTemplateFormValues): string {
  if (!v.name.trim()) return "Name is required.";
  if (!v.content.trim()) return "Content is required.";
  return "";
}

export async function createPromptTemplate(
  token: string,
  values: PromptTemplateFormValues,
): Promise<PromptTemplate> {
  return apiPost<PromptTemplate>("/prompt-templates", token, {
    name: values.name.trim(),
    description: values.description.trim(),
    content: values.content,
    applies_to: values.applies_to,
  });
}

export async function updatePromptTemplate(
  token: string,
  id: string,
  values: PromptTemplateFormValues,
): Promise<PromptTemplate> {
  return apiPatch<PromptTemplate>(`/prompt-templates/${id}`, token, {
    name: values.name.trim(),
    description: values.description.trim(),
    content: values.content,
    applies_to: values.applies_to,
  });
}

export async function deletePromptTemplate(token: string, id: string): Promise<void> {
  return apiDelete(`/prompt-templates/${id}`, token);
}

// S-214: bulk delete. Returns the number of templates actually removed
// (unknown ids are skipped server-side).
export async function bulkDeletePromptTemplates(token: string, ids: string[]): Promise<number> {
  const res = await apiDeleteWithBody<{ deleted?: number }>("/prompt-templates/bulk", token, { ids });
  return res?.deleted ?? 0;
}

// --- S-214: selection helpers (pure, unit-tested) ---------------------

export function toggleTemplateSelection(
  selected: ReadonlySet<string>,
  id: string,
): Set<string> {
  const next = new Set(selected);
  if (next.has(id)) {
    next.delete(id);
  } else {
    next.add(id);
  }
  return next;
}

// S-189/S2301: two explicit methods instead of a boolean flag.
export function allTemplateIds(templates: readonly { id: string }[]): Set<string> {
  return new Set(templates.map((t) => t.id));
}

export function emptyTemplateSelection(): Set<string> {
  return new Set();
}

// pruneTemplateSelection drops selected ids that no longer exist in the
// template list (e.g. after a refresh or delete).
export function pruneTemplateSelection(
  selected: ReadonlySet<string>,
  templates: readonly { id: string }[],
): Set<string> {
  const live = new Set(templates.map((t) => t.id));
  const next = new Set<string>();
  for (const id of selected) {
    if (live.has(id)) next.add(id);
  }
  return next;
}
