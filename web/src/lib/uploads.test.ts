// S-194: attachment helper tests — client-side validation, payload
// parsing, and byte formatting.
import { describe, expect, it } from "vitest";
import type { Message } from "./api";
import {
  ALLOWED_IMAGE_TYPES,
  MAX_MESSAGE_ATTACHMENTS,
  MAX_UPLOAD_BYTES,
  formatBytes,
  messageAttachments,
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
