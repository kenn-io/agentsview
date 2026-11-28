import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { ApiError } from "../api/runtime.js";
import { setLocale } from "../paraglide/runtime.js";

const syncBrowser = vi.hoisted(() => vi.fn());
vi.mock("../api/client.js", () => ({ syncBrowser }));
vi.mock("../utils/telemetry.js", () => ({ reportSyncFailed: vi.fn() }));

import { browserSync } from "./browserSync.svelte.js";

const stats = { imported: 4, updated: 1, skipped: 2, errors: 0 };

beforeEach(() => {
  void setLocale("en", { reload: false });
});
afterEach(() => {
  browserSync.stop();
  browserSync.dismiss();
  vi.resetAllMocks();
});

describe("browserSync", () => {
  it("keeps the platform of a finished run with its result", async () => {
    syncBrowser.mockResolvedValueOnce(stats);
    const onimported = vi.fn();
    await browserSync.start("chatgpt", undefined, onimported);
    expect(syncBrowser).toHaveBeenCalledExactlyOnceWith(
      "chatgpt",
      undefined,
      expect.objectContaining({ onProgress: expect.any(Function) }),
      expect.any(AbortSignal),
    );
    expect(browserSync.provider).toBe("chatgpt");
    expect(browserSync.running).toBe(false);
    expect(browserSync.result).toEqual(stats);
    expect(onimported).toHaveBeenCalledOnce();
  });

  it.each([
    ["chrome_host_required", { flag: null, error: null, refreshed: true }],
    [
      "browser_sync_auth_required",
      { flag: "browser_sync_auth_required", error: null, refreshed: true },
    ],
    [
      "chrome_host_update_required",
      { flag: "chrome_host_update_required", error: null, refreshed: true },
    ],
    [
      "chrome_sync_running",
      { flag: null, error: "Chrome Sync is already running.", refreshed: false },
    ],
    ["browser_sync_format", { flag: null, error: "Server detail", refreshed: true }],
  ])("maps the %s failure code", async (code, want) => {
    syncBrowser.mockRejectedValueOnce(new ApiError(409, "Server detail", code));
    const onimported = vi.fn();
    await browserSync.start("chatgpt", undefined, onimported);
    expect(browserSync.provider).toBe("chatgpt");
    expect(browserSync.flag).toBe(want.flag);
    expect(browserSync.error).toBe(want.error);
    expect(onimported).toHaveBeenCalledTimes(want.refreshed ? 1 : 0);
  });

  it("ignores a second start while a run is active", async () => {
    syncBrowser.mockReturnValueOnce(new Promise(() => {}));
    void browserSync.start("claude-ai", undefined, () => {});
    await browserSync.start("chatgpt", undefined, () => {});
    expect(syncBrowser).toHaveBeenCalledOnce();
    expect(browserSync.provider).toBe("claude-ai");
  });
});
