// S-194: image attachments (chat composer + task threads).
// Pure helpers — validation mirrors the control-plane rules client-side
// (the server always re-validates by sniffing bytes, never by extension).
import type { Message } from "./api";

export const MAX_UPLOAD_BYTES = 5 * 1024 * 1024; // 5 MiB, matches control-plane maxUploadBytes
export const MAX_MESSAGE_ATTACHMENTS = 8; // matches control-plane resolveUploadAttachments
export const ALLOWED_IMAGE_TYPES = ["image/png", "image/jpeg", "image/gif", "image/webp"] as const;

export type UploadRef = {
  id: string;
  squad_id?: string;
  filename: string;
  content_type: string;
  size_bytes: number;
  url: string;
};

/** Client-side pre-flight for a picked/pasted file. Returns an error
 *  message when the file cannot be attached, or null when it may be
 *  uploaded. (Server-side sniffing remains the real gate.) */
export function validateImageFile(file: { type?: string; size?: number; name?: string }): string | null {
  const size = typeof file.size === "number" ? file.size : 0;
  if (size <= 0) return "The file is empty.";
  if (size > MAX_UPLOAD_BYTES) return "Images must be 5 MB or smaller.";
  const type = (file.type ?? "").toLowerCase();
  if (!ALLOWED_IMAGE_TYPES.includes(type as (typeof ALLOWED_IMAGE_TYPES)[number])) {
    return "Only PNG, JPEG, GIF and WebP images can be attached.";
  }
  return null;
}

function refFromEntry(item: unknown): UploadRef | null {
  if (!item || typeof item !== "object" || Array.isArray(item)) {
    return null;
  }
  const entry = item as Record<string, unknown>;
  const id = typeof entry.id === "string" ? entry.id : "";
  const url = typeof entry.url === "string" ? entry.url : "";
  if (id === "" || url === "") {
    return null;
  }
  const ref: UploadRef = {
    id,
    filename: typeof entry.filename === "string" && entry.filename !== "" ? entry.filename : "image",
    content_type: typeof entry.content_type === "string" ? entry.content_type : "image",
    size_bytes: typeof entry.size_bytes === "number" ? entry.size_bytes : 0,
    url,
  };
  if (typeof entry.squad_id === "string") {
    ref.squad_id = entry.squad_id;
  }
  return ref;
}

/** Lenient parse of `payload.attachments` (normalized by the control
 *  plane since S-194) into renderable references. Malformed entries are
 *  skipped, never thrown. */
export function messageAttachments(msg: Message | { payload?: Record<string, unknown> | undefined }): UploadRef[] {
  const raw = msg.payload?.attachments;
  if (!Array.isArray(raw)) return [];
  const out: UploadRef[] = [];
  for (const item of raw) {
    const ref = refFromEntry(item);
    if (ref) out.push(ref);
  }
  return out;
}

/** S-226: turn a control-plane attachment URL (the `/api/v1/uploads/<id>`
 *  embedded in message payloads by the control plane) into a path that is
 *  fetched through `apiBaseUrl()` so it carries auth. In token mode the
 *  base is `/api/v1`, so the request is unchanged; in OIDC mode the base
 *  is `/proxy`, so the server-side forwarder attaches the bearer. A plain
 *  `<img src="/api/v1/...">` navigation can never send an Authorization
 *  header, which is exactly why direct loads 401. URLs that are not
 *  control-plane upload paths are returned unchanged. */
export function uploadFetchPath(url: string): string {
  const match = /^\/api\/v1\/(.+)$/.exec(url);
  return match ? `/${match[1]}` : url;
}

/** Human-readable byte size for attachment chips (e.g. "412 KB"). */
export function formatBytes(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes <= 0) return "0 B";
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${Math.round(bytes / 1024)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}

/** S-200 follow-up: pull image files out of a paste event's clipboard
 *  items (DataTransferItemList-shaped). Non-image items (plain text, etc.)
 *  are ignored so text pastes fall through untouched. Pure and
 *  ClipboardEvent-free so it can be unit-tested. */
export function extractPastedImages(
  items:
    | ArrayLike<{ kind: string; type: string; getAsFile(): File | null }>
    | null
    | undefined,
): File[] {
  if (!items) return [];
  const out: File[] = [];
  for (const item of Array.from(items)) {
    if (!item || item.kind !== "file") continue;
    if (!item.type.toLowerCase().startsWith("image/")) continue;
    const f = item.getAsFile();
    if (f) out.push(f);
  }
  return out;
}
