// S-194: renders image attachments inside chat bubbles and task thread
// entries. Thumbnails link to the full-size image (same authenticated
// URL; the browser session carries the auth).
import type { UploadRef } from "../lib/uploads";
import { formatBytes } from "../lib/uploads";

export function AttachmentThumbs({ attachments }: { attachments: UploadRef[] }) {
  if (attachments.length === 0) return null;
  return (
    <div className="chat-attachments" role="group" aria-label={`${attachments.length} attached image(s)`}>
      {attachments.map((att) => (
        <a
          key={att.id}
          className="chat-attachment"
          href={att.url}
          target="_blank"
          rel="noopener noreferrer"
          title={`${att.filename} (${formatBytes(att.size_bytes)})`}
        >
          {/* eslint-disable-next-line @next/next/no-img-element */}
          <img src={att.url} alt={att.filename} loading="lazy" />
          <span className="chat-attachment-name">{att.filename}</span>
        </a>
      ))}
    </div>
  );
}
