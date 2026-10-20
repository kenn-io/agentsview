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
  it("aborts a stalled daily check after ten seconds", async () => {
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
  it("starts telemetry when the saved server address is malformed", () => {
    localStorage.setItem("agentsview-server-url", "http://example.org:abc");
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new TypeError("Failed to parse URL")));
    expect(() => {
      stop = setupTelemetryReporting();
    }).not.toThrow();
  });
  it("aborts a stalled receipt body after ten seconds and completes a later retry", async () => {
    let signal: AbortSignal | undefined;
    let body!: ReadableStreamDefaultController<Uint8Array>;
    const response = new Response(
      new ReadableStream<Uint8Array>({
        start(controller) {
          body = controller;
          controller.enqueue(new TextEncoder().encode('{"status":'));
        },
      }),
      { status: 202 },
    );
    const fetch = vi
      .fn<(input: RequestInfo | URL, init?: RequestInit) => Promise<Response>>()
      .mockImplementationOnce(async (_input, init) => {
        signal = init?.signal ?? undefined;
        signal?.addEventListener("abort", () => body.error(signal?.reason), { once: true });
        return response;
      })
      .mockImplementation(async () => new Response('{"status":"queued"}', { status: 202 }));
    vi.stubGlobal("fetch", fetch);
    stop = setupTelemetryReporting();
    reportErrorShown("search");
    await vi.advanceTimersByTimeAsync(9_999);
    expect(response.bodyUsed).toBe(true);
    expect(signal?.aborted).toBe(false);
    await vi.advanceTimersByTimeAsync(1);
    expect(signal?.aborted).toBe(true);
    await vi.advanceTimersByTimeAsync(1_000);
    expect(fetch.mock.calls.map(([, init]) => JSON.parse(init!.body as string))).toEqual([
      { event: "app_opened", properties: { surface: "web" } },
      { event: "app_opened", properties: { surface: "web" } },
      { event: "error_shown", properties: { kind: "search" } },
    ]);
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
  it.each(["queued", "disabled"])(
    "shares daily claims across reloads and rechecks the next day after a %s receipt",
    async (status) => {
      const fetch = vi.fn<(input: RequestInfo | URL, init: RequestInit) => Promise<Response>>(
        async () => new Response(JSON.stringify({ status }), { status: 202 }),
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
      const events = () =>
        fetch.mock.calls.map(([, init]) => JSON.parse(init.body as string).event);
      expect(events()).toEqual(
        status === "queued" ? ["app_opened", "screen_viewed", "screen_viewed"] : ["app_opened"],
      );
      stop = setupTelemetryReporting();
      await vi.advanceTimersByTimeAsync(0);
      fetch.mockClear();
      vi.setSystemTime(new Date("2026-10-03T00:00:00Z"));
      window.dispatchEvent(new Event("focus"));
      await vi.advanceTimersByTimeAsync(0);
      expect(events()).toEqual(["app_opened"]);
    },
  );

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
