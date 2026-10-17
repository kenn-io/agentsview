import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { ApiError } from "../api/runtime.js";
import { reportSyncFailed } from "./telemetry.js";
describe("feature outcomes", () => {
  afterEach(() => vi.unstubAllGlobals());
  it.each([
    [new DOMException("Cancelled", "AbortError"), null],
    [new ApiError(500, "detail"), "request"],
    [new ApiError(409, "detail", "claude_ai_sync_running"), "already_running"],
    [new ApiError(401, "detail", "claude_ai_auth_required"), "sign_in_required"],
    [new ApiError(409, "detail", "claude_ai_chrome_host_update_required"), "browser_extension"],
    [Object.assign(new Error("detail"), { code: "sync_running" }), "already_running"],
    [Object.assign(new Error("detail"), { code: "incomplete" }), "partial"],
    [new Error("detail"), "request"],
    [new ApiError(500, "detail", "unknown"), "other"],
    [Object.assign(new Error("detail"), { code: "unknown" }), "other"],
  ])("reports only the fixed reason %s", (error, reason) => {
    const fetch = vi.fn().mockResolvedValue(new Response(null, { status: 202 }));
    vi.stubGlobal("fetch", fetch);
    reportSyncFailed(error);
    if (reason === null) {
      return expect(fetch).not.toHaveBeenCalled();
    }
    expect(JSON.parse(fetch.mock.calls[0]![1].body)).toEqual({
      event: "sync_failed",
      properties: { reason },
    });
    expect(fetch).toHaveBeenCalledOnce();
  });
});
