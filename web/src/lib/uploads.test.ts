// S-194: attachment helper tests — client-side validation, payload
// parsing, and byte formatting.
import { describe, expect, it } from "vitest";
import type { Message } from "./api";
import {
  ALLOWED_IMAGE_TYPES,
  MAX_MESSAGE_ATTACHMENTS,
  MAX_UPLOAD_BYTES,
  formatBytes,
  extractPastedImages,
  messageAttachments,
  uploadFetchPath,
  validateImageFile,
} from "./uploads";

describe("validateImageFile", () => {
  it("accepts the four supported image types", () => {
    for (const type of ALLOWED_IMAGE_TYPES) {
      expect(validateImageFile({ type, size: 1024, name: "x" })).toBeNull();
    }
  });

  it("rejects empty files", () => {
    expect(validateImageFile({ type: "image/png", size: 0 })).toMatch(/empty/i);
  });

  it("rejects files over 5 MB", () => {
    expect(validateImageFile({ type: "image/png", size: MAX_UPLOAD_BYTES + 1 })).toMatch(/5 MB/i);
  });

  it("accepts exactly 5 MB", () => {
    expect(validateImageFile({ type: "image/png", size: MAX_UPLOAD_BYTES })).toBeNull();
  });

  it("rejects non-image types", () => {
    expect(validateImageFile({ type: "application/pdf", size: 100 })).toMatch(/PNG|JPEG|GIF|WebP/);
    expect(validateImageFile({ size: 100 })).toMatch(/PNG|JPEG|GIF|WebP/);
  });
});

describe("messageAttachments", () => {
  it("parses normalized attachments", () => {
    const msg = {
      payload: {
        attachments: [
          {
            id: "abc",
            filename: "bug.png",
            content_type: "image/png",
            size_bytes: 1234,
            url: "/api/v1/uploads/abc",
          },
        ],
      },
    } as unknown as Message;
    const atts = messageAttachments(msg);
    expect(atts).toHaveLength(1);
    expect(atts[0].id).toBe("abc");
    expect(atts[0].url).toBe("/api/v1/uploads/abc");
    expect(atts[0].filename).toBe("bug.png");
  });

  it("skips malformed entries without throwing", () => {
    const msg = {
      payload: {
        attachments: [null, 42, "junk", { id: "x" }, { url: "/u" }, { id: "", url: "/u" }],
      },
    } as unknown as Message;
    expect(messageAttachments(msg)).toHaveLength(0);
  });

  it("returns empty for missing payload", () => {
    expect(messageAttachments({} as Message)).toEqual([]);
    expect(messageAttachments({ payload: {} } as Message)).toEqual([]);
    expect(messageAttachments({ payload: { attachments: "nope" } } as unknown as Message)).toEqual([]);
  });

  it("defaults missing filename to 'image'", () => {
    const msg = {
      payload: { attachments: [{ id: "a", url: "/u/a", content_type: "image/gif" }] },
    } as unknown as Message;
    expect(messageAttachments(msg)[0].filename).toBe("image");
  });
});

describe("formatBytes", () => {
  it("formats human-readable sizes", () => {
    expect(formatBytes(0)).toBe("0 B");
    expect(formatBytes(-5)).toBe("0 B");
    expect(formatBytes(999)).toBe("999 B");
    expect(formatBytes(2048)).toBe("2 KB");
    expect(formatBytes(1.5 * 1024 * 1024)).toBe("1.5 MB");
  });
});

describe("limits", () => {
  it("mirror the control-plane constants", () => {
    expect(MAX_UPLOAD_BYTES).toBe(5 * 1024 * 1024);
    expect(MAX_MESSAGE_ATTACHMENTS).toBe(8);
  });
});

// S-200 follow-up: the chat composer's paste handler was missing; these
// cover the clipboard extraction the onPaste handler relies on.
describe("extractPastedImages", () => {
  const mkItem = (kind: string, type: string, file: File | null = null) => ({
    kind,
    type,
    getAsFile: () => file,
  });

  it("returns empty for missing/empty item lists", () => {
    expect(extractPastedImages(undefined)).toEqual([]);
    expect(extractPastedImages(null)).toEqual([]);
    expect(extractPastedImages([])).toEqual([]);
  });

  it("extracts image files and ignores text items", () => {
    const png = new File([new Uint8Array([1, 2, 3])], "paste.png", { type: "image/png" });
    const items = [
      mkItem("string", "text/plain"),
      mkItem("file", "image/png", png),
    ];
    const out = extractPastedImages(items);
    expect(out).toHaveLength(1);
    expect(out[0]).toBe(png);
  });

  it("ignores non-image files and null getAsFile results", () => {
    const items = [
      mkItem("file", "application/pdf", new File(["x"], "a.pdf", { type: "application/pdf" })),
      mkItem("file", "image/png", null),
    ];
    expect(extractPastedImages(items)).toEqual([]);
  });

  it("handles multiple pasted images", () => {
    const a = new File(["a"], "a.png", { type: "image/png" });
    const b = new File(["b"], "b.webp", { type: "image/webp" });
    const out = extractPastedImages([
      mkItem("file", "image/png", a),
      mkItem("file", "image/webp", b),
    ]);
    expect(out).toEqual([a, b]);
  });
});

// S-226: attachment URLs must be rebased onto the active API base so the
// fetch carries auth (bearer in token mode, /proxy in OIDC mode).
describe("uploadFetchPath", () => {
  it("strips the /api/v1 prefix so apiBaseUrl() can re-add the authed base", () => {
    expect(uploadFetchPath("/api/v1/uploads/6f9d2a4e-0000-4000-8000-abcdefabcdef")).toBe(
      "/uploads/6f9d2a4e-0000-4000-8000-abcdefabcdef",
    );
  });

  it("keeps query strings intact", () => {
    expect(uploadFetchPath("/api/v1/uploads/abc?download=1")).toBe("/uploads/abc?download=1");
  });

  it("returns non-control-plane URLs unchanged", () => {
    expect(uploadFetchPath("/uploads/abc")).toBe("/uploads/abc");
    expect(uploadFetchPath("https://cdn.example.test/img.png")).toBe("https://cdn.example.test/img.png");
    expect(uploadFetchPath("")).toBe("");
  });

  it("does not mangle paths that merely start with /api/v1x", () => {
    expect(uploadFetchPath("/api/v1x/uploads/abc")).toBe("/api/v1x/uploads/abc");
  });
});
