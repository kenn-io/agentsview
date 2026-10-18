import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { ApiError } from "../api/runtime.js";
import { reportErrorShown, reportSyncFailed } from "./telemetry.js";
describe("feature outcomes", () => {
  afterEach(() => vi.unstubAllGlobals());
  it("reports a search error shown", () => {
    const fetch = vi.fn().mockResolvedValue(new Response(null, { status: 202 }));
    vi.stubGlobal("fetch", fetch);
    reportErrorShown("search");
    expect(JSON.parse(fetch.mock.calls[0]![1].body)).toEqual({
      event: "error_shown",
      properties: { kind: "search" },
    });
    expect(fetch).toHaveBeenCalledOnce();
  });
  it.each([
    [new ApiError(500, "detail"), "request"],
    [new ApiError(409, "detail", "claude_ai_sync_running"), "already_running"],
    [new ApiError(401, "detail", "claude_ai_auth_required"), "sign_in_required"],
    [new ApiError(409, "detail", "claude_ai_chrome_host_update_required"), "browser_extension"],
    [new ApiError(409, "detail", "claude_ai_chrome_host_required"), "browser_extension"],
    [Object.assign(new Error("detail"), { code: "sync_running" }), "already_running"],
    [new Error("detail"), "request"],
    [Object.assign(new Error("detail"), { code: 409 }), "request"],
    [new DOMException("timed out", "TimeoutError"), "request"],
    [{ code: "sync_running" }, "other"],
    [new ApiError(500, "detail", "unknown"), "other"],
    [new ApiError(500, "detail", "toString"), "other"],
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
});
