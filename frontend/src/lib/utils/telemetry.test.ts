import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { ApiError } from "../api/runtime.js";
import { reportSyncFailed } from "./telemetry.js";
import { sync } from "../stores/sync.svelte.js";
describe("feature outcomes", () => {
  afterEach(() => vi.unstubAllGlobals());
  it.each([
    [new ApiError(500, "detail"), "request"],
    [new ApiError(409, "detail", "claude_ai_sync_running"), "already_running"],
    [new ApiError(401, "detail", "claude_ai_auth_required"), "sign_in_required"],
    [new ApiError(409, "detail", "claude_ai_chrome_host_update_required"), "browser_extension"],
    [new ApiError(409, "detail", "claude_ai_chrome_host_required"), "browser_extension"],
    [Object.assign(new Error("detail"), { code: "sync_running" }), "already_running"],
    [Object.assign(new Error("detail"), { code: "incomplete" }), "partial"],
    [new Error("detail"), "request"],
    [new ApiError(500, "detail", "unknown"), "other"],
    [Object.assign(new Error("detail"), { code: "unknown" }), "other"],
  ])("reports only the fixed reason %s", (error, reason) => {
    const fetch = vi.fn().mockResolvedValue(new Response(null, { status: 202 }));
    vi.stubGlobal("fetch", fetch);
    reportSyncFailed(error);
    expect(JSON.parse(fetch.mock.calls[0]![1].body)).toEqual({
      event: "sync_failed",
      properties: { reason },
    });
    expect(fetch).toHaveBeenCalledOnce();
  });
  it("reports partial for each decoded worker failure, including a retry", async () => {
    const fetch = vi.fn().mockImplementation((url: string, _init: RequestInit) => {
      if (url.endsWith("/api/v1/resync"))
        return Promise.resolve(
          new Response('event: error\ndata: {"error":"worker failed","code":"incomplete"}\n\n', {
            headers: { "Content-Type": "text/event-stream" },
          }),
        );
      expect(url).toBe("/api/v1/telemetry/events");
      return Promise.resolve(new Response(null, { status: 202 }));
    });
    vi.stubGlobal("fetch", fetch);
    const onError = vi.fn();
    for (const count of [1, 2]) {
      expect(sync.triggerResync(undefined, onError)).toBe(true);
      await vi.waitFor(() => expect(onError).toHaveBeenCalledTimes(count));
    }
    expect(
      fetch.mock.calls
        .filter(([url]) => url.endsWith("/telemetry/events"))
        .map(([, init]) => JSON.parse(init.body)),
    ).toEqual([
      { event: "sync_failed", properties: { reason: "partial" } },
      { event: "sync_failed", properties: { reason: "partial" } },
    ]);
  });
});
