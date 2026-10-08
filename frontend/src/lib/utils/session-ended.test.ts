import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { setAuthToken, setServerUrl } from "../api/runtime.js";
import { setupSessionEndedReporting } from "./session-ended.js";

describe("session ended reporting", () => {
  let now: number;
  let hidden: boolean;
  let stop: (() => void) | undefined;
  const fetch = vi.fn<(input: RequestInfo | URL, init?: RequestInit) => Promise<Response>>(
    async () => new Response(null, { status: 202 }),
  );
  const advance = (ms: number) => {
    now += ms;
    vi.advanceTimersByTime(ms);
  };
  const visibility = (value: boolean) => {
    hidden = value;
    document.dispatchEvent(new Event("visibilitychange"));
  };
  const buckets = () =>
    fetch.mock.calls.map(([, init]) => JSON.parse(init?.body as string).properties.duration_bucket);
  const close = () => window.dispatchEvent(new Event("pagehide"));

  beforeEach(() => {
    now = 0;
    hidden = false;
    fetch.mockClear();
    vi.stubGlobal("fetch", fetch);
    vi.useFakeTimers();
    vi.spyOn(performance, "now").mockImplementation(() => now);
    vi.spyOn(document, "hidden", "get").mockImplementation(() => hidden);
  });
  afterEach(() => {
    stop?.();
    vi.restoreAllMocks();
    vi.useRealTimers();
    vi.unstubAllGlobals();
    localStorage.clear();
  });

  it.each([
    [59_999, "under_1m"],
    [60_000, "1_to_5m"],
    [120_000, "1_to_5m"],
    [299_999, "1_to_5m"],
    [300_000, "5_to_30m"],
    [1_800_000, "5_to_30m"],
    [1_800_001, "over_30m"],
  ])("reports %i visible milliseconds as %s once on close", (ms, bucket) => {
    setServerUrl("https://example.com/agentsview");
    setAuthToken("test-token");
    const timeout = vi.spyOn(AbortSignal, "timeout");
    stop = setupSessionEndedReporting();
    advance(Number(ms));
    close();
    close();
    expect(buckets()).toEqual([bucket]);
    expect(fetch).toHaveBeenCalledWith(
      "https://example.com/agentsview/api/v1/telemetry/events",
      expect.objectContaining({
        method: "POST",
        keepalive: true,
        signal: expect.any(AbortSignal),
        body: JSON.stringify({
          event: "session_ended",
          properties: { surface: "web", duration_bucket: bucket },
        }),
      }),
    );
    const init = fetch.mock.calls[0]![1];
    expect(new Headers(init?.headers).get("Authorization")).toBe("Bearer test-token");
    expect(new Headers(init?.headers).get("Content-Type")).toBe("application/json");
    expect(timeout).toHaveBeenCalledWith(10_000);
  });

  it("sums twenty visible minutes across short tab switches", () => {
    stop = setupSessionEndedReporting();
    for (let i = 0; i < 20; i++) {
      advance(60_000);
      visibility(true);
      advance(300_000);
      visibility(false);
    }
    expect(buckets()).toEqual([]);
    close();
    expect(buckets()).toEqual(["5_to_30m"]);
  });

  it.each([
    [
      "connect",
      "",
      "https://example.com/remote",
      "https://example.com/remote/api/v1/telemetry/events",
    ],
    ["disconnect", "https://example.com/remote", "", "/agentsview/api/v1/telemetry/events"],
    ["reset", "https://example.com/remote", "", "/agentsview/api/v1/telemetry/events"],
    [
      "another tab",
      "https://example.com/remote",
      "https://example.org/remote",
      "https://example.org/remote/api/v1/telemetry/events",
    ],
  ])(
    "discards a visit across %s and reports the next visit to the current server",
    (action, from, to, url) => {
      const base = document.createElement("base");
      base.href = `${window.location.origin}/agentsview/`;
      document.head.append(base);
      try {
        setServerUrl(from);
        setAuthToken("old-token");
        stop = setupSessionEndedReporting();
        advance(120_000);
        if (action === "reset") {
          localStorage.clear();
        } else if (action === "another tab") {
          localStorage.setItem("agentsview-server-url", to);
          window.dispatchEvent(
            new StorageEvent("storage", {
              key: "agentsview-server-url",
              oldValue: from,
              newValue: to,
            }),
          );
        } else {
          setServerUrl(to);
        }
        if (action === "reset") {
          visibility(true);
          advance(1_800_000);
        }
        close();
        expect(fetch).not.toHaveBeenCalled();
        visibility(false);
        window.dispatchEvent(new Event("pageshow"));
        setAuthToken("current-token");
        advance(30_000);
        close();
        expect(buckets()).toEqual(["under_1m"]);
        expect(fetch.mock.calls[0]![0]).toBe(url);
        expect(new Headers(fetch.mock.calls[0]![1]?.headers).get("Authorization")).toBe(
          "Bearer current-token",
        );
      } finally {
        base.remove();
      }
    },
  );

  it.each(["timer boundary", "suspended timer"])(
    "ends a hidden visit and starts a fresh visit on return: %s",
    (scenario) => {
      stop = setupSessionEndedReporting();
      advance(120_000);
      visibility(true);
      if (scenario === "timer boundary") {
        advance(1_799_999);
        expect(buckets()).toEqual([]);
        advance(1);
        expect(buckets()).toEqual(["1_to_5m"]);
      } else {
        now += 2_400_000;
        vi.setSystemTime(Date.now() + 2_400_000);
        expect(buckets()).toEqual([]);
      }
      visibility(false);
      advance(30_000);
      close();
      expect(buckets()).toEqual(["1_to_5m", "under_1m"]);
      expect(fetch.mock.calls[0]![1]?.signal).not.toBe(fetch.mock.calls[1]![1]?.signal);
    },
  );

  it("sends nothing for hidden-only pages", () => {
    hidden = true;
    stop = setupSessionEndedReporting();
    advance(2_400_000);
    close();
    expect(buckets()).toEqual([]);
  });

  it("starts a new visit when a cached page is restored", () => {
    stop = setupSessionEndedReporting();
    advance(120_000);
    close();
    window.dispatchEvent(new Event("pageshow"));
    advance(30_000);
    close();
    expect(buckets()).toEqual(["1_to_5m", "under_1m"]);
  });

  it("drops pending time and removes timers and listeners on cleanup", () => {
    stop = setupSessionEndedReporting();
    advance(120_000);
    visibility(true);
    stop();
    advance(1_800_000);
    visibility(false);
    window.dispatchEvent(new Event("pageshow"));
    advance(120_000);
    close();
    expect(buckets()).toEqual([]);
  });
});
