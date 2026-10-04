// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { downloadFrictionDigestMarkdown } from "./client.js";

const storage = {
  getItem: vi.fn().mockReturnValue(""),
  setItem: vi.fn(),
  removeItem: vi.fn(),
  clear: vi.fn(),
};

describe("downloadFrictionDigestMarkdown", () => {
  beforeEach(() => {
    vi.stubGlobal("localStorage", storage);
    storage.getItem.mockReturnValue("");
    document.head.innerHTML = "";
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it("opens the Markdown route directly on a local connection", async () => {
    const open = vi.spyOn(window, "open").mockReturnValue(null);
    await downloadFrictionDigestMarkdown("2026-09-21");
    expect(open).toHaveBeenCalledWith("/api/v1/friction/digests/2026-09-21/md", "_blank");
  });

  it("fetches with the bearer token and names the file after the date on a remote connection", async () => {
    storage.getItem.mockImplementation((key: string) =>
      key === "agentsview-server-url"
        ? "https://remote.example.test"
        : key.includes("token")
          ? "test-token"
          : "",
    );
    const fetchMock = vi.fn().mockResolvedValue(
      new Response("# Friction Log — 2026-09-21\n", {
        status: 200,
        headers: { "Content-Type": "text/markdown" },
      }),
    );
    vi.stubGlobal("fetch", fetchMock);
    vi.stubGlobal(
      "URL",
      Object.assign(URL, {
        createObjectURL: vi.fn().mockReturnValue("blob:friction"),
        revokeObjectURL: vi.fn(),
      }),
    );
    let downloadName = "";
    vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(
      function (this: HTMLAnchorElement) {
        downloadName = this.download;
      },
    );

    await downloadFrictionDigestMarkdown("2026-09-21");

    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(String(fetchMock.mock.calls[0]![0])).toContain("/api/v1/friction/digests/2026-09-21/md");
    expect(downloadName).toBe("friction-log-2026-09-21.md");
  });
});
