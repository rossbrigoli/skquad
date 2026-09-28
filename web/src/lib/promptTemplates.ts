// S-158: prompt templates — admin-managed reusable starting points for
// squad and agent system prompts. Selecting a template COPIES its content
// into the draft prompt; templates are never live references.

import { apiDelete, apiPatch, apiPost } from "./api";
import { apiGet } from "./api";

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
