"use client";

import { Agent, AgentPermission, ApiState, LLMProvider, Message, MeteringSummary, RegistryResource, ResourceType, Squad, Task, TaskStatus } from "../lib/api";

export const taskStatuses: TaskStatus[] = ["todo", "in-progress", "in-review", "done", "blocked"];

export const registryTypes: Array<{ type: Exclude<ResourceType, "llm_provider">; label: string; path: string }> = [
  { type: "skill", label: "Skills", path: "skills" },
  { type: "tool", label: "Tools", path: "tools" },
  { type: "api", label: "APIs", path: "apis" },
  { type: "knowledge_base", label: "Knowledge Bases", path: "knowledge-bases" },
  { type: "project_workspace", label: "Project Workspaces", path: "project-workspaces" },
];

export function StateNotice<T>({ state, empty }: { state: ApiState<T>; empty: string }) {
  const arrayData = Array.isArray(state.data) ? state.data : null;
  if (state.loading) {
    return <div className="notice compact">Loading</div>;
  }
  if (state.error) {
    return <div className="notice error compact">{state.error}</div>;
  }
  if (!state.data || (arrayData && arrayData.length === 0)) {
    return <div className="notice compact">{empty}</div>;
  }
  return null;
}

export function messageText(message: Message) {
  const payload = message.payload || {};
  if (typeof payload.message === "string") {
    return payload.message;
  }
  return JSON.stringify(payload);
}

export function resourceLabel(permission: AgentPermission, providers: LLMProvider[], resources: RegistryResource[]) {
  if (permission.resource_type === "llm_provider") {
    return providers.find((provider) => provider.id === permission.resource_id)?.name || permission.resource_id;
  }
  return resources.find((resource) => resource.id === permission.resource_id)?.name || permission.resource_id;
}

export function formatCost(summary: MeteringSummary | null) {
  if (!summary) {
    return "-";
  }
  const cost = summary.cost ?? 0;
  const currency = summary.currency || "USD";
  return `${currency} ${cost.toFixed(4)}`;
}

export type LeaseState = "running" | "stalled" | "idle";

// A task holds a lease while an agent runtime is actively working it. An
// expired lease means the worker stopped heartbeating without completing.
export function leaseState(task: Task): LeaseState {
  if (!task.execution_id || !task.lease_expires_at) {
    return "idle";
  }
  const expiry = Date.parse(task.lease_expires_at);
  // Go serialises a zero time.Time as 0001-01-01T00:00:00Z rather than omitting
  // it, and that string is truthy but parses to a large negative timestamp.
  // Anything before the epoch means "no lease", not "lease expired long ago".
  if (Number.isNaN(expiry) || expiry <= 0) {
    return "idle";
  }
  return expiry > Date.now() ? "running" : "stalled";
}

export function formatRelativeTime(value?: string): string {
  if (!value) {
    return "";
  }
  const timestamp = Date.parse(value);
  if (Number.isNaN(timestamp)) {
    return "";
  }
  const deltaSec = Math.round((timestamp - Date.now()) / 1000);
  const absSec = Math.abs(deltaSec);
  const [amount, unit]: [number, Intl.RelativeTimeFormatUnit] =
    absSec < 60 ? [deltaSec, "second"]
    : absSec < 3600 ? [Math.round(deltaSec / 60), "minute"]
    : absSec < 86400 ? [Math.round(deltaSec / 3600), "hour"]
    : [Math.round(deltaSec / 86400), "day"];
  return new Intl.RelativeTimeFormat(undefined, { numeric: "auto" }).format(amount, unit);
}

export function messageDeliveryNote(message: Message): string {
  const parts: string[] = [];
  if (typeof message.attempts === "number" && message.attempts > 0) {
    const max = message.max_attempts ? `/${message.max_attempts}` : "";
    parts.push(`attempt ${message.attempts}${max}`);
  }
  if (message.next_retry_at) {
    // A retry time already in the past means the queue is behind, not that a
    // retry happened — say it is due rather than reporting it as history.
    const due = Date.parse(message.next_retry_at);
    parts.push(
      !Number.isNaN(due) && due <= Date.now() ? "retry due" : `retry ${formatRelativeTime(message.next_retry_at)}`,
    );
  }
  if (message.expires_at) {
    parts.push(`expires ${formatRelativeTime(message.expires_at)}`);
  }
  return parts.join(" · ");
}

// A squad's LLM lives in its operating_model until the control plane grows a
// first-class field for it; every agent added to the squad inherits it.
export type SquadLLM = { provider_id: string; model: string };

export function squadLLM(squad: Pick<Squad, "operating_model"> | null | undefined): SquadLLM | null {
  const llm = asRecord(asRecord(squad?.operating_model)?.llm);
  const rawProvider = llm?.provider_id;
  const providerID = typeof rawProvider === "string" ? rawProvider.trim() : "";
  if (!providerID) {
    return null;
  }
  const rawModel = llm?.model;
  return { provider_id: providerID, model: typeof rawModel === "string" ? rawModel.trim() : "" };
}

// PATCHing a squad replaces operating_model wholesale, so the LLM is merged in
// rather than written alone and any other operating-model keys survive.
export function withSquadLLM(operatingModel: unknown, llm: SquadLLM | null): Record<string, unknown> {
  const next: Record<string, unknown> = { ...(asRecord(operatingModel) || {}) };
  if (llm && llm.provider_id) {
    next.llm = { provider_id: llm.provider_id, model: llm.model };
  } else {
    delete next.llm;
  }
  return next;
}

// Mirrors the control plane's allowedGatewayModels: a provider grants its
// default model plus every entry in its models list. The gateway rejects any
// other name, so these are the only models an agent can actually use.
export function providerModels(provider: LLMProvider | null | undefined): string[] {
  if (!provider) {
    return [];
  }
  const models: string[] = [];
  const add = (value: unknown) => {
    const model = typeof value === "string" ? value.trim() : "";
    if (model && !models.includes(model)) {
      models.push(model);
    }
  };
  add(provider.default_model);
  if (Array.isArray(provider.models)) {
    provider.models.forEach(add);
  }
  return models;
}

export type SquadLLMStatus =
  | { state: "unset" }
  | { state: "unknown"; llm: SquadLLM }
  | { state: "deprecated"; llm: SquadLLM; provider: LLMProvider }
  | { state: "no-models"; llm: SquadLLM; provider: LLMProvider }
  | { state: "ready"; llm: SquadLLM; provider: LLMProvider; models: string[] };

// The gateway skips inactive providers and refuses to provision a key with no
// models, so both are surfaced as distinct states rather than treated as ready.
export function resolveSquadLLM(squad: Pick<Squad, "operating_model"> | null | undefined, providers: LLMProvider[]): SquadLLMStatus {
  const llm = squadLLM(squad);
  if (!llm) {
    return { state: "unset" };
  }
  const provider = providers.find((item) => item.id === llm.provider_id);
  if (!provider) {
    return { state: "unknown", llm };
  }
  if (provider.status !== "active") {
    return { state: "deprecated", llm, provider };
  }
  const models = providerModels(provider);
  if (models.length === 0) {
    return { state: "no-models", llm, provider };
  }
  return { state: "ready", llm, provider, models };
}

export function pickModel(models: string[], ...preferred: Array<string | undefined>): string {
  for (const candidate of preferred) {
    const model = candidate?.trim();
    if (model && models.includes(model)) {
      return model;
    }
  }
  return models[0] || "";
}

// An agent is on the squad's LLM only when all three pieces the runtime and
// gateway need agree: its default provider, a gateway grant for that provider,
// and a model the provider actually serves.
export function agentUsesSquadLLM(
  agent: Pick<Agent, "default_provider_id" | "default_model">,
  permissions: Array<Pick<AgentPermission, "resource_type" | "resource_id">>,
  status: SquadLLMStatus,
): boolean {
  if (status.state !== "ready") {
    return false;
  }
  return agent.default_provider_id === status.llm.provider_id
    && status.models.includes(agent.default_model || "")
    && permissions.some((item) => item.resource_type === "llm_provider" && item.resource_id === status.llm.provider_id);
}

// LLM access is squad-managed, so applying it replaces any existing provider
// grant and leaves every non-LLM grant exactly as it was.
export function permissionsWithLLM(
  permissions: Array<Pick<AgentPermission, "resource_type" | "resource_id">>,
  providerID: string,
): Array<{ resource_type: ResourceType; resource_id: string }> {
  return [
    ...permissions
      .filter((item) => item.resource_type !== "llm_provider")
      .map((item) => ({ resource_type: item.resource_type, resource_id: item.resource_id })),
    { resource_type: "llm_provider", resource_id: providerID },
  ];
}

export function countLabel(count: number, noun: string): string {
  return `${count} ${count === 1 ? noun : `${noun}s`}`;
}

function asRecord(value: unknown): Record<string, unknown> | null {
  return value && typeof value === "object" && !Array.isArray(value) ? value as Record<string, unknown> : null;
}
