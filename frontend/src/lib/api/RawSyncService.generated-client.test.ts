import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { OpenAPI } from "./generated/core/OpenAPI";
import { RawSyncService } from "./generated/services/RawSyncService";

describe("RawSyncService generated client", () => {
  afterEach(() => {
    OpenAPI.TOKEN = undefined;
    vi.unstubAllGlobals();
  });

  it("sends the operation credential instead of the shared API token", async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response(null, { status: 204 }));
    vi.stubGlobal("fetch", fetchMock);
    OpenAPI.TOKEN = "legacy-token";

    await RawSyncService.postApiV1RawSyncTokens({
      requestBody: { scopes: ["raw:write"] },
      authorization: "Bearer device-credential",
      xAgentsViewDeviceId: "device-a",
    });

    expect(fetchMock).toHaveBeenCalledOnce();
    const request = fetchMock.mock.calls[0]?.[1] as RequestInit;
    expect(new Headers(request.headers).get("Authorization")).toBe("Bearer device-credential");
  });

  it("returns every authoritative raw upload status header", async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response(null, {
      status: 200,
      headers: {
        "Upload-Offset": "7",
        "Upload-Length": "11",
        "Upload-Complete": "false",
      },
    }));
    vi.stubGlobal("fetch", fetchMock);

    const status = await RawSyncService.headApiV1RawSyncUploadsUploadId({
      uploadId: "upl_AQEBAQEBAQEBAQEBAQEBAQ",
      authorization: "Bearer upload-token",
    });

    expect(status).toEqual({ offset: 7, length: 11, complete: false });
    expect(fetchMock).toHaveBeenCalledOnce();
  });
});
