// S-258: jsdom tests for InboxAttachments — the Inbox reading-view
// attachment block. Covers: empty/absent attachments render nothing,
// non-image files render name/size/type + a Download button, the
// download pulls bytes through the authenticated blob client at the
// proxy-corrected path and saves under the original filename, fetch
// failures degrade to an inline note (never a crash), and inline-safe
// images reuse AttachmentThumbs for previews.
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";

const env = vi.hoisted(() => ({
  blobImpl: async () => new Blob(["data"], { type: "application/pdf" }),
  calls: [] as string[],
}));

vi.mock("../lib/auth", () => ({
  useAuth: () => ({ token: "tok", mode: "token" }),
}));

vi.mock("../lib/api", () => {
  class ApiError extends Error {
    status: number;
    constructor(status: number, message: string) {
      super(message);
      this.name = "ApiError";
      this.status = status;
    }
  }
  return {
    ApiError,
    apiGetBlob: async (path: string) => {
      env.calls.push(path);
      return env.blobImpl();
    },
  };
});

import { InboxAttachments, inboxFileAttachments, inboxImageAttachments } from "./InboxAttachments";
import type { InboxAttachmentMeta } from "../lib/api";

const urlShim = URL as unknown as {
  createObjectURL: (b: unknown) => string;
  revokeObjectURL: (u: string) => void;
};
let createdUrls: string[] = [];
let clickedAnchors: HTMLAnchorElement[] = [];

beforeEach(() => {
  env.blobImpl = async () => new Blob(["data"], { type: "application/pdf" });
  env.calls = [];
  createdUrls = [];
  clickedAnchors = [];
  urlShim.createObjectURL = (blob: unknown) => {
    const url = `blob:mock-${createdUrls.length}`;
    createdUrls.push(url);
    void blob;
    return url;
  };
  urlShim.revokeObjectURL = () => undefined;
  vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(function (
    this: HTMLAnchorElement,
  ) {
    clickedAnchors.push(this);
  });
});

function att(overrides: Partial<InboxAttachmentMeta> = {}): InboxAttachmentMeta {
  return {
    id: "att-1",
    message_id: "msg-1",
    filename: "report.pdf",
    content_type: "application/pdf",
    size_bytes: 12345,
    url: "/api/v1/inbox/msg-1/attachments/att-1",
    ...overrides,
  };
}

describe("InboxAttachments", () => {
  it("renders nothing when there are no attachments", () => {
    const { container } = render(<InboxAttachments attachments={undefined} />);
    expect(container).toBeEmptyDOMElement();
    const empty = render(<InboxAttachments attachments={[]} />);
    expect(empty.container).toBeEmptyDOMElement();
  });

  it("renders name, size and type with a download button for non-image files", () => {
    render(<InboxAttachments attachments={[att()]} />);
    expect(screen.getByText("📎 report.pdf")).toBeInTheDocument();
    expect(screen.getByText(/12 KB · application\/pdf/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Download report.pdf" })).toBeInTheDocument();
  });

  it("downloads via the authenticated blob client at the proxy-corrected path", async () => {
    const user = userEvent.setup();
    render(<InboxAttachments attachments={[att()]} />);
    await user.click(screen.getByRole("button", { name: "Download report.pdf" }));
    await waitFor(() => expect(clickedAnchors).toHaveLength(1));
    // uploadFetchPath strips the /api/v1 prefix (keeping the leading /
    // relative to apiBaseUrl) so auth rides along — token bearer or
    // OIDC proxy — exactly like the chat image path.
    expect(env.calls).toEqual(["/inbox/msg-1/attachments/att-1"]);
    expect(clickedAnchors[0].getAttribute("download")).toBe("report.pdf");
    expect(clickedAnchors[0].href).toBe("blob:mock-0");
  });

  it("shows an inline error and stays usable when the download fails", async () => {
    env.blobImpl = async () => {
      throw new Error("boom");
    };
    const user = userEvent.setup();
    render(<InboxAttachments attachments={[att()]} />);
    await user.click(screen.getByRole("button", { name: "Download report.pdf" }));
    await waitFor(() =>
      expect(screen.getByText("Download failed — try again.")).toBeInTheDocument(),
    );
    expect(clickedAnchors).toHaveLength(0);
    // The row itself is still rendered — nothing was blanked out.
    expect(screen.getByText("📎 report.pdf")).toBeInTheDocument();
  });

  it("previews inline-safe images through AttachmentThumbs", async () => {
    env.blobImpl = async () => new Blob(["img"], { type: "image/png" });
    render(<InboxAttachments attachments={[att({ content_type: "image/png", filename: "shot.png" })]} />);
    const img = await screen.findByAltText("shot.png");
    expect(img).toHaveAttribute("src", "blob:mock-0");
    // Images preview via thumbs and do NOT appear in the download list.
    expect(screen.queryByRole("button", { name: "Download shot.png" })).not.toBeInTheDocument();
  });

  it("splits mixed attachments: images previewed, files downloadable", async () => {
    env.blobImpl = async () => new Blob(["x"], { type: "image/jpeg" });
    render(
      <InboxAttachments
        attachments={[
          att({ id: "a-img", content_type: "image/jpeg", filename: "photo.jpg" }),
          att({ id: "a-doc", content_type: "text/csv", filename: "data.csv" }),
        ]}
      />,
    );
    expect(await screen.findByAltText("photo.jpg")).toBeInTheDocument();
    expect(screen.getByText("📎 data.csv")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Download data.csv" })).toBeInTheDocument();
  });

  it("pure classifiers bucket by the backend inline-image set", () => {
    const png = att({ id: "1", content_type: "image/png" });
    const svg = att({ id: "2", content_type: "image/svg+xml" });
    const zip = att({ id: "3", content_type: "application/zip" });
    expect(inboxImageAttachments([png, svg, zip])).toEqual([png]);
    expect(inboxFileAttachments([png, svg, zip])).toEqual([svg, zip]);
    expect(inboxImageAttachments(undefined)).toEqual([]);
    expect(inboxFileAttachments(undefined)).toEqual([]);
  });
});
