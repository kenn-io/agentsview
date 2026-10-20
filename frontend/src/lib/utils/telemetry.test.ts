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
    localStorage.clear();
    vi.useRealTimers();
    vi.unstubAllGlobals();
  });
  it("reports a search error shown", async () => {
    const fetch = vi.fn().mockResolvedValue(new Response('{"status":"queued"}', { status: 202 }));
    vi.stubGlobal("fetch", fetch);
    stop = setupTelemetryReporting();
    await vi.advanceTimersByTimeAsync(0);
    fetch.mockClear();
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
  ])("reports only the fixed reason %s", async (error, reason) => {
    const fetch = vi.fn().mockResolvedValue(new Response('{"status":"queued"}', { status: 202 }));
    vi.stubGlobal("fetch", fetch);
    stop = setupTelemetryReporting();
    await vi.advanceTimersByTimeAsync(0);
    fetch.mockClear();
    reportSyncFailed(error);
    expect(JSON.parse(fetch.mock.calls[0]![1].body)).toEqual({
      event: "sync_failed",
      properties: { reason },
    });
    expect(fetch).toHaveBeenCalledOnce();
  });
  it("shares daily open and screen claims across reloads and stays silent after a disabled receipt", async () => {
    const fetch = vi
      .fn()
      .mockImplementation(async () => new Response('{"status":"disabled"}', { status: 202 }));
    vi.stubGlobal("fetch", fetch);
    stop = setupTelemetryReporting();
    await vi.advanceTimersByTimeAsync(0);
    reportTelemetry("screen_viewed", { screen: "sessions" });
    reportErrorShown("search");
    window.dispatchEvent(new Event("pagehide"));
    stop();
    stop = setupTelemetryReporting();
    window.dispatchEvent(new Event("focus"));
    await vi.advanceTimersByTimeAsync(0);
    expect(fetch).toHaveBeenCalledTimes(1);
    vi.setSystemTime(new Date("2026-10-03T00:00:00Z"));
    window.dispatchEvent(new Event("focus"));
    await vi.advanceTimersByTimeAsync(0);
    expect(fetch).toHaveBeenCalledTimes(2);
  });

  it("reports each screen and app open once daily through focus and reloads", async () => {
    const fetch = vi
      .fn()
      .mockImplementation(async () => new Response('{"status":"queued"}', { status: 202 }));
    vi.stubGlobal("fetch", fetch);
    for (let i = 0; i < 3; i++) {
      stop = setupTelemetryReporting();
      reportTelemetry("screen_viewed", { screen: "sessions" });
      await vi.advanceTimersByTimeAsync(0);
      reportTelemetry("screen_viewed", { screen: "usage" });
      window.dispatchEvent(new Event("focus"));
      await vi.advanceTimersByTimeAsync(0);
      stop();
    }
    expect(fetch.mock.calls.map(([, init]) => JSON.parse(init.body).event)).toEqual([
      "app_opened",
      "screen_viewed",
      "screen_viewed",
    ]);
  });

  it("holds outcomes across midnight until the new day's enabled receipt", async () => {
    let reply!: (response: Response) => void;
    const fetch = vi
      .fn()
      .mockImplementation(async () => new Response('{"status":"queued"}', { status: 202 }));
    fetch.mockImplementationOnce(
      () =>
        new Promise<Response>((resolve) => {
          reply = resolve;
        }),
    );
    vi.stubGlobal("fetch", fetch);
    stop = setupTelemetryReporting();
    reportErrorShown("search");
    vi.setSystemTime(new Date("2026-10-03T00:00:00Z"));
    const firstReply = reply;
    fetch.mockImplementationOnce(
      () =>
        new Promise<Response>((resolve) => {
          reply = resolve;
        }),
    );
    firstReply(new Response('{"status":"queued"}', { status: 202 }));
    await vi.advanceTimersByTimeAsync(0);
    expect(fetch.mock.calls.map(([, init]) => JSON.parse(init.body).event)).toEqual([
      "app_opened",
      "app_opened",
    ]);
    reply(new Response('{"status":"queued"}', { status: 202 }));
    await vi.advanceTimersByTimeAsync(0);
    expect(fetch.mock.calls.map(([, init]) => JSON.parse(init.body).event)).toEqual([
      "app_opened",
      "app_opened",
      "error_shown",
    ]);
  });

  it("retries an unauthorized check after authentication and reload", async () => {
    const fetch = vi
      .fn()
      .mockImplementation(async () => new Response('{"status":"queued"}', { status: 202 }));
    fetch.mockResolvedValueOnce(new Response(null, { status: 401 }));
    vi.stubGlobal("fetch", fetch);
    stop = setupTelemetryReporting();
    await vi.advanceTimersByTimeAsync(0);
    stop();
    setAuthToken("signed-in");
    stop = setupTelemetryReporting();
    await vi.advanceTimersByTimeAsync(0);
    expect(fetch).toHaveBeenCalledTimes(2);
    expect(new Headers(fetch.mock.lastCall?.[1].headers).get("Authorization")).toBe(
      "Bearer signed-in",
    );
  });

  it("uses the bound server's latest token without restarting the reporter", async () => {
    const fetch = vi.fn(async () => new Response('{"status":"queued"}', { status: 202 }));
    vi.stubGlobal("fetch", fetch);
    setServerUrl("https://example.org");
    setAuthToken("first-token");
    stop = setupTelemetryReporting();
    await vi.advanceTimersByTimeAsync(0);
    setAuthToken("updated-token");
    reportErrorShown("search");
    expect(fetch).toHaveBeenLastCalledWith(
      "https://example.org/api/v1/telemetry/events",
      expect.objectContaining({
        headers: expect.objectContaining({ Authorization: "Bearer updated-token" }),
      }),
    );
  });

  it.each([false, true])(
    "replaces server claims and credentials after a server change, cross-tab=%s",
    async (crossTab) => {
      const fetch = vi
        .fn()
        .mockImplementation(async () => new Response('{"status":"queued"}', { status: 202 }));
      vi.stubGlobal("fetch", fetch);
      stop = setupTelemetryReporting();
      reportTelemetry("screen_viewed", { screen: "usage" });
      await vi.advanceTimersByTimeAsync(0);
      fetch.mockClear();
      if (crossTab) {
        localStorage.setItem(SERVER_URL_KEY, "https://example.org");
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
      setServerUrl("");
      stop();
      await vi.advanceTimersByTimeAsync(0);
      expect(fetch).toHaveBeenCalledTimes(2);
    },
  );
});
