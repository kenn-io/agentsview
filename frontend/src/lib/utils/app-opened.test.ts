import { describe, it, expect, vi, beforeEach, afterEach } from "vite-plus/test";
import { setAuthToken, setServerUrl } from "../api/runtime.js";

describe("setupAppOpenedReporting", () => {
  let originalFetch: typeof globalThis.fetch;
  let fetchMock: ReturnType<typeof vi.fn>;
  let setupAppOpenedReporting: () => () => void;
  let cleanup: (() => void) | undefined;

  beforeEach(async () => {
    vi.resetModules();
    ({ setupAppOpenedReporting } = await import("./app-opened.js"));
    vi.useFakeTimers({ toFake: ["Date"] });
    setAuthToken("");
    setServerUrl("");
    originalFetch = globalThis.fetch;
    fetchMock = vi.fn().mockImplementation(
      async () =>
        new Response('{"status":"queued"}', {
          status: 202,
          headers: { "Content-Type": "application/json" },
        }),
    );
    globalThis.fetch = fetchMock as unknown as typeof globalThis.fetch;
  });

  afterEach(() => {
    cleanup?.();
    cleanup = undefined;
    globalThis.fetch = originalFetch;
    setAuthToken("");
    setServerUrl("");
    vi.useRealTimers();
    vi.restoreAllMocks();
  });

  function focus() {
    window.dispatchEvent(new Event("focus"));
  }

  function start(at: string) {
    vi.setSystemTime(new Date(at));
    cleanup = setupAppOpenedReporting();
  }

  it("posts app_opened once on load", () => {
    start("2026-10-02T09:00:00Z");

    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0]!;
    expect(url).toBe("/api/v1/telemetry/events");
    expect(init.method).toBe("POST");
    expect(init.body).toBe('{"event":"app_opened"}');
    expect(new Headers(init.headers).get("Content-Type")).toBe("application/json");
  });

  it("sends nothing more on focus the same UTC day", () => {
    start("2026-10-02T09:00:00Z");
    vi.setSystemTime(new Date("2026-10-02T20:00:00Z"));
    focus();
    focus();

    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it("sends once on the first focus of a later UTC day", () => {
    start("2026-10-02T09:00:00Z");
    vi.setSystemTime(new Date("2026-10-03T08:00:00Z"));
    focus();
    expect(fetchMock).toHaveBeenCalledTimes(2);

    focus();
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  it("counts a new day across the UTC midnight boundary", () => {
    start("2026-10-02T23:59:00Z");
    vi.setSystemTime(new Date("2026-10-03T00:01:00Z"));
    focus();

    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  it("uses the remote server URL and bearer token", () => {
    setServerUrl("http://remote.example:8080");
    setAuthToken("tok");
    start("2026-10-02T09:00:00Z");

    const [url, init] = fetchMock.mock.calls[0]!;
    expect(url).toBe("http://remote.example:8080/api/v1/telemetry/events");
    expect(new Headers(init.headers).get("Authorization")).toBe("Bearer tok");
  });

  it("swallows a failed post and still reports the next day", async () => {
    fetchMock.mockRejectedValueOnce(new Error("offline"));
    start("2026-10-02T09:00:00Z");
    // Vitest fails the run on an unhandled rejection, so settling the tick is the assertion.
    await expect(new Promise((resolve) => setTimeout(resolve, 0))).resolves.toBeUndefined();

    vi.setSystemTime(new Date("2026-10-03T08:00:00Z"));
    focus();
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  it("stops listening after cleanup", () => {
    start("2026-10-02T09:00:00Z");
    cleanup!();
    cleanup = undefined;
    vi.setSystemTime(new Date("2026-10-03T08:00:00Z"));
    focus();

    expect(fetchMock).toHaveBeenCalledTimes(1);
  });
});
