// S-132: known LLM provider API kinds. The `value` is the provider name
// the LiteLLM gateway understands (custom_llm_provider); the `label` is
// the human-facing API name shown in the dropdown. Editing a provider whose
// stored kind is not in the curated list keeps it selectable as "(current)"
// so the dropdown never silently rewrites an exotic-but-working value.

export type ProviderKindOption = { readonly value: string; readonly label: string };

export const PROVIDER_KINDS: readonly ProviderKindOption[] = [
  { value: "openai", label: "OpenAI Completions" },
  { value: "anthropic", label: "Anthropic Messages" },
  { value: "gemini", label: "Google Gemini" },
  { value: "ollama_chat", label: "Ollama Chat" },
  { value: "ollama", label: "Ollama Completions" },
  { value: "azure", label: "Azure OpenAI" },
];

export function kindOptionsFor(current?: string | null): ProviderKindOption[] {
  const options = [...PROVIDER_KINDS];
  const trimmed = (current ?? "").trim();
  if (trimmed && !options.some((option) => option.value === trimmed)) {
    options.push({ value: trimmed, label: `${trimmed} (current)` });
  }
  return options;
}
