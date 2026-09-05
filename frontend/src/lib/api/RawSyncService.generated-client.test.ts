import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { RawSyncService } from "./generated/index.js";
import { getRawSyncUploadStatus } from "./raw-sync.js";
import { setAuthToken } from "./runtime.js";

describe("RawSyncService generated client", () => {
  beforeEach(() => {
    localStorage.setItem("agentsview-server-url", "http://localhost");
  });

  afterEach(() => {
    localStorage.clear();
    vi.unstubAllGlobals();
  });

  it("sends the operation credential instead of the shared API token", async () => {
    const fetchMock = vi.fn(
      async (_input: RequestInfo | URL, _init?: RequestInit) => new Response(null, { status: 204 }),
    );
    vi.stubGlobal("fetch", fetchMock);
    setAuthToken("legacy-token");

    await RawSyncService.postApiV1RawSyncTokens(
      { scopes: ["raw:write"] },
      {
        headers: {
          Authorization: "Bearer device-credential",
          "X-AgentsView-Device-ID": "device-a",
        },
      },
    );

    expect(fetchMock).toHaveBeenCalledOnce();
    const [, init] = fetchMock.mock.calls[0]!;
    expect(new Headers(init?.headers).get("Authorization")).toBe("Bearer device-credential");
  });

  it("returns every authoritative raw upload status header", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(null, {
        status: 200,
        headers: {
          "Upload-Offset": "7",
          "Upload-Length": "11",
          "Upload-Complete": "false",
        },
      }),
    );
    vi.stubGlobal("fetch", fetchMock);

    const status = await getRawSyncUploadStatus(
      "upl_AQEBAQEBAQEBAQEBAQEBAQ",
      "Bearer upload-token",
    );

    expect(status).toEqual({ offset: 7, length: 11, complete: false });
    expect(fetchMock).toHaveBeenCalledOnce();
  });
});
