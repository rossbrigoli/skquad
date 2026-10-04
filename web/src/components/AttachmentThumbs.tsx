"use client";

// S-194: renders image attachments inside chat bubbles and task thread
// entries.
//
// S-226: the control-plane upload endpoint requires a bearer token, but a
// plain <img src> / <a href> navigation cannot send an Authorization
// header (and under OIDC the token never reaches the browser at all), so
// direct loads returned `{"error":{"code":"unauthorized"...}}`. We now
// fetch the bytes through the authenticated API client (bearer header in
// token mode, httpOnly-cookie + /proxy in OIDC mode) and render/open the
// image via an object URL.
import { useEffect, useState } from "react";
import { apiGetBlob } from "../lib/api";
import { useAuth } from "../lib/auth";
import { formatBytes, uploadFetchPath, type UploadRef } from "../lib/uploads";

// Object URLs are cached per upload id so re-renders and remounts of the
// chat/thread views don't refetch bytes. The cap bounds memory; evicted
// entries are revoked. Cache is page-scoped by design.
const BLOB_CACHE_LIMIT = 32;
const blobCache = new Map<string, string>();

function cacheBlobUrl(id: string, url: string): void {
  if (blobCache.has(id)) return;
  blobCache.set(id, url);
  if (blobCache.size > BLOB_CACHE_LIMIT) {
    const oldest = blobCache.keys().next();
    if (!oldest.done) {
      const evicted = blobCache.get(oldest.value);
      blobCache.delete(oldest.value);
      if (evicted) URL.revokeObjectURL(evicted);
    }
  }
}

export function AttachmentThumbs({ attachments }: { readonly attachments: UploadRef[] }) {
  const { token } = useAuth();
  const [srcs, setSrcs] = useState<Record<string, string>>({});
  const [failedIds, setFailedIds] = useState<Record<string, boolean>>({});
  // The effect dependency is a serialized (id, url) list, not the array
  // identity: parents rebuild `attachments` on every render and a naive
  // dep would refetch in a loop. Parsing the key inside the effect also
  // keeps the effect free of refs (react-hooks/refs).
  const fetchKey = attachments.map((a) => `${a.id}\u0000${a.url}`).join(",");

  useEffect(() => {
    if (fetchKey === "") return;
    const list = fetchKey.split(",").map((pair) => {
      const nul = pair.indexOf("\u0000");
      return { id: pair.slice(0, nul), url: pair.slice(nul + 1) };
    });
    let cancelled = false;
    (async () => {
      // Yield first so every setState below happens asynchronously —
      // never synchronously inside the effect body.
      await Promise.resolve();
      for (const att of list) {
        const cached = blobCache.get(att.id);
        if (cached) {
          if (!cancelled) setSrcs((prev) => (prev[att.id] === cached ? prev : { ...prev, [att.id]: cached }));
          continue;
        }
        try {
          const blob = await apiGetBlob(uploadFetchPath(att.url), token);
          const objUrl = URL.createObjectURL(blob);
          cacheBlobUrl(att.id, objUrl);
          if (cancelled) return;
          setSrcs((prev) => (prev[att.id] ? prev : { ...prev, [att.id]: objUrl }));
        } catch {
          if (!cancelled) setFailedIds((prev) => (prev[att.id] ? prev : { ...prev, [att.id]: true }));
        }
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [fetchKey, token]);

  if (attachments.length === 0) return null;
  return (
    <fieldset className="chat-attachments" style={{ border: "none", padding: 0, margin: 0, minWidth: 0 }} aria-label={`${attachments.length} attached image(s)`}>
      {attachments.map((att) => {
        const src = srcs[att.id];
        const title = `${att.filename} (${formatBytes(att.size_bytes)})`;
        if (failedIds[att.id]) {
          return (
            <span key={att.id} className="chat-attachment chat-attachment-error" title={title}>
              Could not load {att.filename}
            </span>
          );
        }
        return (
          <a
            key={att.id}
            className="chat-attachment"
            href={src ?? undefined}
            target="_blank"
            rel="noopener noreferrer"
            title={title}
          >
            {src ? (
              // eslint-disable-next-line @next/next/no-img-element
              <img src={src} alt={att.filename} />
            ) : (
              <span className="chat-attachment-loading" aria-label={`Loading ${att.filename}`}>
                Loading…
              </span>
            )}
            <span className="chat-attachment-name">{att.filename}</span>
          </a>
        );
      })}
    </fieldset>
  );
}
