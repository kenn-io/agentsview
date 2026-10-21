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
  it("aborts a stalled initial check after ten seconds", async () => {
    let signal: AbortSignal | undefined;
    const fetch = vi.fn(
      (_input: RequestInfo | URL, init?: RequestInit) =>
        new Promise<Response>((_resolve, reject) => {
          signal = init?.signal ?? undefined;
          signal?.addEventListener("abort", () => reject(signal?.reason), { once: true });
        }),
    );
    vi.stubGlobal("fetch", fetch);
    stop = setupTelemetryReporting();
    await vi.advanceTimersByTimeAsync(9_999);
    expect(signal?.aborted).toBe(false);
    await vi.advanceTimersByTimeAsync(1);
    expect(signal?.aborted).toBe(true);
  });
  it("clears the request deadline after a disabled receipt and cleanup", async () => {
    let signal: AbortSignal | undefined;
    const fetch = vi.fn(async (_input: RequestInfo | URL, init?: RequestInit) => {
      signal = init?.signal ?? undefined;
      return new Response('{"status":"disabled"}', { status: 202 });
    });
    vi.stubGlobal("fetch", fetch);
    stop = setupTelemetryReporting();
    await vi.advanceTimersByTimeAsync(0);
    expect(signal?.aborted).toBe(false);
    stop();
    await vi.advanceTimersByTimeAsync(10_000);
    expect(signal?.aborted).toBe(false);
  });
  it("holds an outcome while the open page waits for its initial receipt", async () => {
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
    expect(JSON.parse(fetch.mock.calls[0]![1]!.body as string)).toEqual({
      event: "app_opened",
      properties: { surface: "web" },
    });
    reply(new Response('{"status":"queued"}', { status: 202 }));
    await vi.advanceTimersByTimeAsync(0);
    expect(fetch).toHaveBeenCalledTimes(2);
    expect(JSON.parse(fetch.mock.lastCall![1]!.body as string)).toEqual({
      event: "error_shown",
      properties: { kind: "search" },
    });
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

  it("retries an unauthorized check on fresh setup after setting the token", async () => {
    const fetch = vi
      .fn<(input: RequestInfo | URL, init?: RequestInit) => Promise<Response>>()
      .mockResolvedValueOnce(new Response(null, { status: 401 }))
      .mockImplementation(async () => new Response('{"status":"queued"}', { status: 202 }));
    vi.stubGlobal("fetch", fetch);
    stop = setupTelemetryReporting();
    await vi.advanceTimersByTimeAsync(0);
    await vi.advanceTimersByTimeAsync(60_000);
    expect(fetch).toHaveBeenCalledOnce();
    stop();
    setAuthToken("signed-in");
    stop = setupTelemetryReporting();
    await vi.advanceTimersByTimeAsync(0);
    expect(fetch.mock.calls.map(([, init]) => JSON.parse(init!.body as string).event)).toEqual([
      "app_opened",
      "app_opened",
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

  it("drops old-server reports before a cross-tab server change is delivered", async () => {
    const now = vi.spyOn(performance, "now").mockReturnValue(0);
    vi.spyOn(document, "hidden", "get").mockReturnValue(false);
    const fetch = vi.fn<(input: RequestInfo | URL, init?: RequestInit) => Promise<Response>>(
      async () => new Response('{"status":"queued"}', { status: 202 }),
    );
    vi.stubGlobal("fetch", fetch);
    setServerUrl("https://a.example.org");
    setAuthToken("a-token");
    stop = setupTelemetryReporting();
    await vi.advanceTimersByTimeAsync(0);
    fetch.mockClear();

    localStorage.setItem(SERVER_URL_KEY, "https://b.example.org");
    setAuthToken("b-token");
    now.mockReturnValue(120_000);
    window.dispatchEvent(new Event("pagehide"));
    reportErrorShown("search");
    await vi.advanceTimersByTimeAsync(0);
    expect(fetch).not.toHaveBeenCalled();

    window.dispatchEvent(
      new StorageEvent("storage", { key: SERVER_URL_KEY, storageArea: localStorage }),
    );
    await vi.advanceTimersByTimeAsync(0);
    expect(
      fetch.mock.calls.map(([url, init]) => [
        url,
        new Headers(init?.headers).get("Authorization"),
        JSON.parse(init!.body as string),
      ]),
    ).toEqual([
      [
        "https://b.example.org/api/v1/telemetry/events",
        "Bearer b-token",
        {
          event: "app_opened",
          properties: { surface: "web" },
        },
      ],
    ]);
  });
});
