import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { OpenAPI } from "./generated/core/OpenAPI";
import { RawSyncService } from "./generated/services/RawSyncService";

describe("RawSyncService authentication", () => {
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
});
