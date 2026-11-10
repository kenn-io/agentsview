import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { ApiError, setServerUrl, setAuthToken, SERVER_URL_KEY } from "../api/runtime.js";
import {
  reportErrorShown,
  reportSyncFailed,
  reportTelemetry,
  setupTelemetryReporting,
} from "./telemetry.js";
describe("feature outcomes", () => {
  let stop: (() => void) | undefined;
  beforeEach(() => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-10-02T12:00:00Z"));
  });
  afterEach(() => {
    stop?.();
    vi.restoreAllMocks();
    localStorage.clear();
    vi.useRealTimers();
    vi.unstubAllGlobals();
  });
  it.each(["stalled", "completed"])(
    "bounds a %s request with a ten-second deadline",
    async (state) => {
      let signal: AbortSignal | undefined;
      const fetch = vi.fn((_input: RequestInfo | URL, init?: RequestInit) => {
        signal = init?.signal ?? undefined;
        return new Promise<Response>((resolve, reject) => {
          if (state === "completed") resolve(new Response('{"status":"queued"}', { status: 202 }));
          signal?.addEventListener("abort", () => reject(signal?.reason), { once: true });
        });
      });
      vi.stubGlobal("fetch", fetch);
      stop = setupTelemetryReporting();
      await vi.advanceTimersByTimeAsync(9_999);
      expect(signal?.aborted).toBe(false);
      await vi.advanceTimersByTimeAsync(1);
      expect(signal?.aborted).toBe(state === "stalled");
    },
  );
  it.each([202, 404])(
    "routes held feature outcomes through the HTTP %i receipt",
    async (status) => {
      let reply!: (response: Response) => void;
      const fetch = vi
        .fn<(input: RequestInfo | URL, init?: RequestInit) => Promise<Response>>()
        .mockImplementationOnce(
          () =>
            new Promise((resolve) => {
              reply = resolve;
            }),
        )
        .mockImplementation(async () => new Response('{"status":"queued"}', { status: 202 }));
      vi.stubGlobal("fetch", fetch);
      stop = setupTelemetryReporting();
      reportErrorShown("search");
      await vi.advanceTimersByTimeAsync(0);
      expect(fetch).toHaveBeenCalledOnce();
      reply(new Response(status === 202 ? '{"status":"queued"}' : null, { status }));
      await vi.advanceTimersByTimeAsync(status === 404 ? 60_000 : 0);
      expect(fetch.mock.calls.map(([, init]) => JSON.parse(init!.body as string))).toEqual([
        { event: "app_opened", properties: { surface: "web" } },
        ...(status === 202 ? [{ event: "error_shown", properties: { kind: "search" } }] : []),
      ]);
    },
  );
  it.each([
    [new ApiError(500, "detail"), "request"],
    [new ApiError(409, "detail", "claude_ai_sync_running"), "already_running"],
    [new ApiError(401, "detail", "claude_ai_auth_required"), "sign_in_required"],
    [new ApiError(409, "detail", "claude_ai_chrome_host_update_required"), "browser_extension"],
    [new ApiError(409, "detail", "claude_ai_chrome_host_required"), "browser_extension"],
    [new ApiError(409, "detail", "chrome_sync_running"), "already_running"],
    [new ApiError(401, "detail", "gemini_auth_required"), "sign_in_required"],
    [new ApiError(401, "detail", "grok_web_auth_required"), "sign_in_required"],
    [new ApiError(409, "detail", "chrome_host_required"), "browser_extension"],
    [Object.assign(new Error("detail"), { code: "sync_running" }), "already_running"],
    [new Error("detail"), "request"],
    [Object.assign(new Error("detail"), { code: 409 }), "request"],
    [new DOMException("timed out", "TimeoutError"), "request"],
    [{ code: "sync_running" }, "other"],
    [new ApiError(500, "detail", "unknown"), "other"],
    [new ApiError(500, "detail", "toString"), "other"],
  ])("reports only the fixed reason %s", async (error, reason) => {
    const fetch = vi.fn().mockResolvedValue(new Response('{"status":"queued"}', { status: 202 }));
    vi.stubGlobal("fetch", fetch);
    stop = setupTelemetryReporting();
    await vi.advanceTimersByTimeAsync(0);
    fetch.mockClear();
    reportSyncFailed(error);
    await vi.advanceTimersByTimeAsync(0);
    expect(JSON.parse(fetch.mock.calls[0]![1].body)).toEqual({
      event: "sync_failed",
      properties: { reason },
    });
    expect(fetch).toHaveBeenCalledOnce();
  });
  it("shares selected-server claims across reloads after an enabled receipt", async () => {
    const fetch = vi.fn<(input: RequestInfo | URL, init: RequestInit) => Promise<Response>>(
      async () => new Response('{"status":"queued"}', { status: 202 }),
    );
    vi.stubGlobal("fetch", fetch);
    for (let i = 0; i < 2; i++) {
      stop = setupTelemetryReporting();
      reportTelemetry("screen_viewed", { screen: "sessions" });
      await vi.advanceTimersByTimeAsync(0);
      reportTelemetry("screen_viewed", { screen: "usage" });
      window.dispatchEvent(new Event("focus"));
      await vi.advanceTimersByTimeAsync(0);
      stop();
    }
    expect(fetch.mock.calls.map(([, init]) => JSON.parse(init.body as string).event)).toEqual([
      "app_opened",
      "screen_viewed",
      "screen_viewed",
    ]);
  });

  it.each([false, true])(
    "replaces server claims and credentials after a server change, cross-tab=%s",
    async (crossTab) => {
      const now = vi.spyOn(performance, "now").mockReturnValue(0);
      vi.spyOn(document, "hidden", "get").mockReturnValue(false);
      const fetch = vi
        .fn()
        .mockImplementation(async () => new Response('{"status":"queued"}', { status: 202 }));
      vi.stubGlobal("fetch", fetch);
      stop = setupTelemetryReporting();
      reportTelemetry("screen_viewed", { screen: "usage" });
      await vi.advanceTimersByTimeAsync(0);
      now.mockReturnValue(120_000);
      fetch.mockClear();
      if (crossTab) {
        localStorage.setItem(SERVER_URL_KEY, "https://example.org");
        setAuthToken("remote-token");
        window.dispatchEvent(new Event("pagehide"));
        reportErrorShown("search");
        await vi.advanceTimersByTimeAsync(0);
        expect(fetch).not.toHaveBeenCalled();
        window.dispatchEvent(
          new StorageEvent("storage", { key: SERVER_URL_KEY, storageArea: localStorage }),
        );
      } else setServerUrl("https://example.org");
      setAuthToken("remote-token");
      await vi.advanceTimersByTimeAsync(0);
      expect(
        fetch.mock.calls.map(([url, init]) => [
          url,
          new Headers(init.headers).get("Authorization"),
          JSON.parse(init.body).event,
        ]),
      ).toEqual([
        ["https://example.org/api/v1/telemetry/events", "Bearer remote-token", "app_opened"],
        ["https://example.org/api/v1/telemetry/events", "Bearer remote-token", "screen_viewed"],
      ]);
      now.mockReturnValue(150_000);
      window.dispatchEvent(new Event("pagehide"));
      await vi.advanceTimersByTimeAsync(0);
      expect(
        fetch.mock.calls
          .map(([, init]) => JSON.parse(init.body))
          .filter((event) => event.event === "visit_ended"),
      ).toEqual([
        { event: "visit_ended", properties: { surface: "web", duration_bucket: "under_1m" } },
      ]);
    },
  );
});
