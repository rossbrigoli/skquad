export type ApiUser = {
  id: string;
  email: string;
  name: string;
  role: string;
};

export type Squad = {
  id: string;
  name: string;
  mission?: string;
  // S-PROMPT WP4: layer-3 squad prompt (mission stays as the short
  // listing summary; the two coexist per resolved decision Q4).
  prompt?: string;
  owner_id?: string;
  namespace?: string;
  status?: string;
  operating_model?: unknown;
  created_at?: string;
};

// S-242: minimal task summary embedded on agent payloads (dashboard +
// squad agent lists). Optional by contract — the UI must degrade
// gracefully when the backend has not populated them yet.
export type AgentTaskBrief = {
  id: string;
  ref?: string;
  title?: string;
  status?: string;
};

export type Agent = {
  id: string;
  squad_id: string;
  name: string;
  role?: string;
  system_prompt?: string;
  identity_id?: string;
  // AI model binding (ADR-0010 D4, WP7). Bound via the agent page's
  // LLM section; PATCH /agents/{id} with "" clears a slot.
  ai_model_id?: string;
  fallback_ai_model_id?: string;
  idle_timeout_sec?: number;
  // S-178: per-agent reasoning effort ("low"|"medium"|"high"; unset =
  // provider default). Set from the composer's thinking-level selector.
  thinking_level?: string;
  // Durable workspace PVC (S-138). storageClass is platform-admin only
  // and intentionally absent from the tenant-facing surface.
  storage_enabled?: boolean;
  storage_size?: string;
  status?: string;
  // S-242: AI model name + current/last task shown on the reusable
  // agent tile. Optional: absent payloads simply hide those lines.
  model?: string;
  current_task?: AgentTaskBrief | null;
  last_task?: AgentTaskBrief | null;
  created_at?: string;
  updated_at?: string;
};

export type AgentIdentity = {
  id: string;
  agent_id: string;
  credential_ref: string;
  virtual_key_ref?: string;
  created_by: string;
  created_at?: string;
  rotated_at?: string;
};

export type TaskStatus = "backlog" | "todo" | "in-progress" | "in-review" | "done" | "blocked";

export type Task = {
  id: string;
  board_id: string;
  squad_id: string;
  title: string;
  description?: string;
  status: TaskStatus;
  assignee_agent_id?: string;
  created_by_type?: string;
  created_by_id?: string;
  position?: number;
  // S-184: per-squad sequential display reference (rendered "T-<n>").
  task_number?: number;
  created_at?: string;
  updated_at?: string;
  execution_id?: string;
  worker_id?: string;
  fencing_token?: string;
  lease_expires_at?: string;
  // S-181: final outcome text of the latest done/blocked transition.
  result?: string;
  result_status?: string;
  result_at?: string;
};

export type BoardPayload = {
  board: {
    id: string;
    squad_id: string;
    created_at?: string;
  };
  tasks: Task[];
};

export type Message = {
  id: string;
  from_type: string;
  from_id: string;
  // S-235: sender display name resolved server-side at read time
  // (current agent name / user first name, email local-part fallback).
  // Absent when unresolvable — clients fall back (see actorDisplay).
  from_display?: string;
  to_agent_id: string;
  squad_id: string;
  type: string;
  payload?: {
    message?: string;
    [key: string]: unknown;
  };
  status: string;
  correlation_id?: string;
  attempts?: number;
  max_attempts?: number;
  next_retry_at?: string;
  expires_at?: string;
  terminal_reason?: string;
  created_at?: string;
  delivered_at?: string;
};

export type InboxMessage = {
  id: string;
  squad_id: string;
  user_id?: string;
  from_agent_id?: string;
  task_id?: string;
  // S-193: "agent_message" = content an agent delivered via send_inbox
  // at the human's request. task_completed/action_required stay
  // system-emitted.
  kind: "task_completed" | "action_required" | "agent_message";
  message: string;
  // S-181: email-style richer payload; optional for older messages.
  subject?: string;
  body?: string;
  read_at?: string;
  created_at: string;
  // S-258: attachment metadata (S-216 backend). GET /inbox already
  // batch-enriches each message via enrichInboxAttachments, so the UI
  // needs no per-message fetch to render the attachment list; bytes are
  // only pulled on demand (preview/download) from the serve endpoint.
  attachments?: InboxAttachmentMeta[];
};

// S-258: one inbox attachment as serialized by the control plane
// (control-plane/internal/httpapi inboxAttachmentMeta). Shape-compatible
// with uploads.UploadRef so AttachmentThumbs can render image previews.
export type InboxAttachmentMeta = {
  id: string;
  message_id: string;
  squad_id?: string;
  filename: string;
  content_type: string;
  size_bytes: number;
  sha256?: string;
  url: string;
  created_at?: string;
};

// S-193: recipient-scoped "something went wrong" alerts for the bell.
// S-203 WP4 adds the budget types (user-level: squad_id is "").
export type NotificationType =
  | "task_failed"
  | "task_stuck"
  | "agent_died"
  | "task_blocked"
  | "budget_warning"
  | "budget_stopped";
export type NotificationSeverity = "info" | "warning" | "error";

export type AppNotification = {
  id: string;
  user_id: string;
  squad_id: string;
  task_id?: string;
  agent_id?: string;
  type: NotificationType;
  severity: NotificationSeverity;
  message: string;
  read_at?: string;
  created_at: string;
};

export type ResourceType = "ai_provider" | "skill" | "tool" | "api" | "knowledge_base" | "project_workspace" | "git";

export type AIProvider = {
  id: string;
  name: string;
  kind: string;
  base_url: string;
  // S-155: the key itself is write-only (pasted into `api_key` on
  // create/update, stored as a Kubernetes Secret). Reads expose only
  // the masked tail and whether a key exists.
  api_key_masked?: string;
  has_api_key?: boolean;
  // WP8 (0014): legacy default_model/models removed — model config lives
  // on AIModel rows (ADR-0010). S-128: pricing lives only on AIModel.
  status: string;
  registered_by?: string;
  created_at?: string;
};

export type RegistryResource = {
  id: string;
  type: ResourceType;
  name: string;
  description?: string;
  endpoint?: string;
  auth_ref?: string;
  manifest?: unknown;
  status: string;
  registered_by?: string;
  created_at?: string;
  // TG-2 typed egress fields (rest/web/mcp/git). Secret material is
  // never present — credentials live in managed K8s Secrets (TG-4).
  endpoint_config?: unknown;
  policy_ceiling?: unknown;
  risk_tier?: string;
  egress_class?: string;
};

export type AgentPermission = {
  id: string;
  agent_id: string;
  resource_type: ResourceType;
  resource_id: string;
  granted_by?: string;
  created_at?: string;
  // TG-2: grant-level constraints (subset of the resource ceiling).
  constraints?: unknown;
};

export type AccessGrant = {
  id: string;
  squad_id: string;
  grantee_type: "user" | "agent";
  grantee_id: string;
  permissions: string;
  granted_by?: string;
  created_at?: string;
};

export type MeteringSummary = {
  input_tokens?: number;
  output_tokens?: number;
  cost?: number;
  currency?: string;
};

export type AuditEntry = {
  id: string;
  actor_type: string;
  actor_id: string;
  // S-235: actor display name resolved server-side at read time.
  // Absent for system actors and unresolvable ids.
  actor_display?: string;
  action: string;
  resource_type: string;
  resource_id: string;
  squad_id?: string;
  // TG-9: raw audit metadata (JSON object as sent by the control plane;
  // may also arrive as a pre-encoded string in fixtures). See lib/audit.ts
  // parseAuditMetadata for normalisation.
  metadata?: unknown;
  timestamp?: string;
};

export type ApiState<T> = {
  data: T | null;
  loading: boolean;
  error: string;
};

export function apiBaseUrl(): string {
  if (apiBaseOverride) return apiBaseOverride.replace(/\/$/, "");
  const configured = process.env.NEXT_PUBLIC_SKQUAD_API_BASE_URL || "/api/v1";
  return configured.replace(/\/$/, "");
}

// OIDC mode routes all API calls through the server-side /proxy (UIv2-13).
let apiBaseOverride: string | null = null;
export function setApiBaseOverride(base: string | null): void {
  apiBaseOverride = base;
}

export class ApiError extends Error {
  status: number;
  // Parsed JSON body when available — carries the S-103 in-use usage list.
  body: unknown;

  constructor(status: number, message: string, body?: unknown) {
    super(message);
    this.status = status;
    this.body = body;
  }
}

export async function apiGet<T>(path: string, token: string): Promise<T> {
  return apiRequest<T>(path, token, { method: "GET" });
}

// S-239: GET for paged listings — returns the parsed body plus the
// server's X-Total-Count header (total items matching the filter,
// independent of the current page). total is 0 when the header is
// absent (older servers), so callers degrade gracefully.
export async function apiGetWithTotal<T>(
  path: string,
  token: string,
): Promise<{ items: T; total: number }> {
  const headers: Record<string, string> = { Accept: "application/json" };
  if (token.trim() !== "") {
    headers.Authorization = `Bearer ${token.trim()}`;
  }
  const response = await fetch(`${apiBaseUrl()}${path}`, {
    method: "GET",
    headers,
    credentials: "same-origin",
    cache: "no-store",
  });
  if (!response.ok) {
    let message = response.statusText;
    let body: unknown = undefined;
    try {
      body = await response.json();
      const parsed = body as { error?: { message?: unknown }; message?: unknown } | null;
      message = extractErrorMessage(parsed) || message;
    } catch {
      // Keep the HTTP status text when the body is not JSON.
    }
    throw new ApiError(response.status, message, body);
  }
  const total = Number.parseInt(response.headers.get("X-Total-Count") ?? "0", 10);
  const items = response.status === 204 ? (undefined as T) : ((await response.json()) as T);
  return { items, total: Number.isFinite(total) ? total : 0 };
}

export async function apiPost<T>(path: string, token: string, body: unknown, opts?: { timeoutMs?: number }): Promise<T> {
  return apiRequest<T>(path, token, { method: "POST", body, timeoutMs: opts?.timeoutMs });
}

export async function apiPatch<T>(path: string, token: string, body: unknown): Promise<T> {
  return apiRequest<T>(path, token, { method: "PATCH", body });
}

export async function apiPut<T>(path: string, token: string, body: unknown): Promise<T> {
  return apiRequest<T>(path, token, { method: "PUT", body });
}

export async function apiDelete(path: string, token: string): Promise<void> {
  await apiRequest<void>(path, token, { method: "DELETE" });
}

// S-214: DELETE with a JSON body — used by bulk-delete routes.
export async function apiDeleteWithBody<T>(path: string, token: string, body: unknown): Promise<T> {
  return apiRequest<T>(path, token, { method: "DELETE", body });
}

// S-226: GET binary content (image attachments) with the same auth
// semantics as apiRequest. The browser cannot attach an Authorization
// header to <img src> / link navigations, so attachment viewers fetch
// the bytes here and render them via URL.createObjectURL instead.
export async function apiGetBlob(path: string, token: string): Promise<Blob> {
  const headers: Record<string, string> = { Accept: "image/*" };
  if (token.trim() !== "") {
    headers.Authorization = `Bearer ${token.trim()}`;
  }
  const response = await fetch(`${apiBaseUrl()}${path}`, {
    method: "GET",
    headers,
    credentials: "same-origin",
    cache: "no-store",
  });
  if (!response.ok) {
    let message = response.statusText;
    try {
      const parsed = (await response.json()) as { error?: { message?: unknown } } | null;
      if (typeof parsed?.error?.message === "string" && parsed.error.message !== "") message = parsed.error.message;
    } catch {
      // Keep the HTTP status text when the body is not JSON.
    }
    throw new ApiError(response.status, message);
  }
  return await response.blob();
}

// S-194: multipart image upload for the chat composer and task threads.
// Kept separate from apiRequest because the body is FormData (the JSON
// Content-Type header must NOT be set — the browser adds the boundary).
export async function apiUploadImage(path: string, token: string, file: File, query = ""): Promise<import("./uploads").UploadRef> {
  const form = new FormData();
  form.append("file", file);
  const headers: Record<string, string> = { Accept: "application/json" };
  if (token.trim() !== "") {
    headers.Authorization = `Bearer ${token.trim()}`;
  }
  const response = await fetch(`${apiBaseUrl()}${path}${query}`, {
    method: "POST",
    headers,
    body: form,
    credentials: "same-origin",
    cache: "no-store",
  });
  if (!response.ok) {
    let message = response.statusText;
    try {
      const parsed = (await response.json()) as { error?: { message?: unknown } } | null;
      if (typeof parsed?.error?.message === "string" && parsed.error.message !== "") message = parsed.error.message;
    } catch {
      // Keep the HTTP status text when the body is not JSON.
    }
    throw new ApiError(response.status, message);
  }
  return (await response.json()) as import("./uploads").UploadRef;
}

function extractErrorMessage(
  parsed: { error?: { message?: unknown }; message?: unknown } | null,
): string {
  if (typeof parsed?.error?.message === "string") {
    return parsed.error.message;
  }
  if (typeof parsed?.message === "string") {
    return parsed.message;
  }
  return "";
}

async function apiRequest<T>(path: string, token: string, options: { method: string; body?: unknown; timeoutMs?: number }): Promise<T> {
  const headers: Record<string, string> = {
    Accept: "application/json",
  };
  if (options.body !== undefined) {
    headers["Content-Type"] = "application/json";
  }
  if (token.trim() !== "") {
    headers.Authorization = `Bearer ${token.trim()}`;
  }

  const response = await fetch(`${apiBaseUrl()}${path}`, {
    method: options.method,
    headers,
    body: options.body === undefined ? undefined : JSON.stringify(options.body),
    credentials: "same-origin",
    cache: "no-store",
    // S-180 follow-up: callers can bound the request client-side; the
    // abort surfaces as a TimeoutError/AbortError the test logic maps
    // to the "timeout" reason instead of a generic failure.
    signal:
      options.timeoutMs && options.timeoutMs > 0
        ? AbortSignal.timeout(options.timeoutMs)
        : undefined,
  });
  if (!response.ok) {
    let message = response.statusText;
    let body: unknown = undefined;
    try {
      body = await response.json();
      const parsed = body as { error?: { message?: unknown }; message?: unknown } | null;
      const parsedMessage = extractErrorMessage(parsed);
      message = parsedMessage || message;
    } catch {
      // Keep the HTTP status text when the body is not JSON.
    }
    throw new ApiError(response.status, message, body);
  }
  if (response.status === 204) {
    return undefined as T;
  }
  return response.json() as Promise<T>;
}
