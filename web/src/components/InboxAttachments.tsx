"use client";

// S-258: attachment visibility + download in the Inbox reading view.
//
// The control plane already batch-enriches GET /inbox with attachment
// metadata (S-216 enrichInboxAttachments), so this component renders
// from data the list response carries — no extra metadata fetch. Bytes
// are pulled on demand only:
//
//  - Inline-safe raster images (the backend's safeInlineImage set) get
//    the existing AttachmentThumbs preview, reused as-is: it fetches
//    through the authenticated client (S-226) and opens full-size.
//  - Everything else renders as a name/size/type row with a Download
//    button. A plain <a href> can never carry the Authorization header
//    (and under OIDC the token never reaches the browser), so the
//    download goes through apiGetBlob and a transient object URL with
//    the `download` attribute — the server also sets
//    Content-Disposition: attachment for these types.
//
// Failures degrade to a subtle inline note on the affected row; the
// message itself always stays readable.
import { useState } from "react";
import { apiGetBlob, type InboxAttachmentMeta } from "../lib/api";
import { useAuth } from "../lib/auth";
import { formatBytes, uploadFetchPath } from "../lib/uploads";
import { AttachmentThumbs } from "./AttachmentThumbs";

// Mirrors the control plane's safeInlineImage (inbox_attachments.go):
// only these raster types are served inline; SVG and everything else is
// forced to download server-side, so we never try to preview it here.
const INLINE_IMAGE_TYPES = new Set([
  "image/png",
  "image/jpeg",
  "image/gif",
  "image/webp",
  "image/bmp",
]);

export function inboxImageAttachments(
  attachments: readonly InboxAttachmentMeta[] | undefined,
): InboxAttachmentMeta[] {
  return asArray(attachments).filter((a) => INLINE_IMAGE_TYPES.has(a.content_type));
}

export function inboxFileAttachments(
  attachments: readonly InboxAttachmentMeta[] | undefined,
): InboxAttachmentMeta[] {
  return asArray(attachments).filter((a) => !INLINE_IMAGE_TYPES.has(a.content_type));
}

function asArray(value: readonly InboxAttachmentMeta[] | undefined): InboxAttachmentMeta[] {
  return Array.isArray(value) ? value : [];
}

/** Save blob bytes under a filename via a transient anchor click. */
function triggerDownload(url: string, filename: string): void {
  const anchor = document.createElement("a");
  anchor.href = url;
  anchor.download = filename;
  document.body.appendChild(anchor);
  anchor.click();
  anchor.remove();
}

export function InboxAttachments({
  attachments,
}: {
  readonly attachments: readonly InboxAttachmentMeta[] | undefined;
}) {
  const { token } = useAuth();
  const [busyId, setBusyId] = useState("");
  const [failedId, setFailedId] = useState("");

  const files = inboxFileAttachments(attachments);
  const images = inboxImageAttachments(attachments);
  if (files.length === 0 && images.length === 0) return null;

  async function download(att: InboxAttachmentMeta) {
    setBusyId(att.id);
    setFailedId("");
    try {
      const blob = await apiGetBlob(uploadFetchPath(att.url), token);
      const url = URL.createObjectURL(blob);
      triggerDownload(url, att.filename);
      // Give the navigation time to start before revoking.
      setTimeout(() => URL.revokeObjectURL(url), 60_000);
    } catch {
      setFailedId(att.id);
    } finally {
      setBusyId("");
    }
  }

  return (
    <div className="inbox-attachments" aria-label="Attachments">
      {images.length > 0 ? <AttachmentThumbs attachments={images} /> : null}
      {files.length > 0 ? (
        <ul className="inbox-attachment-list">
          {files.map((att) => (
            <li key={att.id} className="inbox-attachment-item">
              <span className="inbox-attachment-name" title={att.filename}>
                📎 {att.filename}
              </span>
              <span className="inbox-attachment-size">
                {formatBytes(att.size_bytes)} · {att.content_type}
              </span>
              <button
                type="button"
                className="btn btn-small inbox-attachment-download"
                disabled={busyId === att.id}
                aria-label={`Download ${att.filename}`}
                onClick={() => {
                  void download(att);
                }}
              >
                {busyId === att.id ? "Downloading…" : "Download"}
              </button>
              {failedId === att.id ? (
                <span className="inbox-attachment-error">Download failed — try again.</span>
              ) : null}
            </li>
          ))}
        </ul>
      ) : null}
    </div>
  );
}
