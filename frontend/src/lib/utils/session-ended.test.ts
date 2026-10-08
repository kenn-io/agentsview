import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { SERVER_URL_KEY, setAuthToken, setServerUrl } from "../api/runtime.js";
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
    [299_999, "1_to_5m"],
    [300_000, "5_to_30m"],
    [1_800_000, "5_to_30m"],
    [1_800_001, "over_30m"],
  ])("reports %i visible milliseconds as %s once on close", (ms, bucket) => {
    const timeout = vi.spyOn(AbortSignal, "timeout");
    stop = setupSessionEndedReporting();
    advance(Number(ms));
    close();
    close();
    expect(buckets()).toEqual([bucket]);
    expect(fetch).toHaveBeenCalledWith(
      expect.anything(),
      expect.objectContaining({
        keepalive: true,
        signal: expect.any(AbortSignal),
        body: JSON.stringify({
          event: "session_ended",
          properties: { surface: "web", duration_bucket: bucket },
        }),
      }),
    );
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
    ["round trip", "", "https://example.com/remote", "/agentsview/api/v1/telemetry/events"],
    [
      "round trip",
      "https://example.com/remote",
      "",
      "https://example.com/remote/api/v1/telemetry/events",
    ],
    [
      "another tab round trip",
      "https://example.com/remote",
      "",
      "https://example.com/remote/api/v1/telemetry/events",
    ],
    [
      "clear and restore",
      "https://example.com/remote",
      "",
      "https://example.com/remote/api/v1/telemetry/events",
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
        if (action === "reset" || action === "clear and restore") {
          localStorage.clear();
          if (action === "clear and restore") {
            localStorage.setItem(SERVER_URL_KEY, from);
            window.dispatchEvent(new StorageEvent("storage", { storageArea: localStorage }));
          }
        } else if (action.startsWith("another tab")) {
          if (to) localStorage.setItem(SERVER_URL_KEY, to);
          else localStorage.removeItem(SERVER_URL_KEY);
          if (action === "another tab round trip") {
            if (from) localStorage.setItem(SERVER_URL_KEY, from);
            else localStorage.removeItem(SERVER_URL_KEY);
          }
          window.dispatchEvent(
            new StorageEvent("storage", {
              key: SERVER_URL_KEY,
              oldValue: from || null,
              newValue: to || null,
              storageArea: localStorage,
            }),
          );
          if (action === "another tab round trip") {
            window.dispatchEvent(
              new StorageEvent("storage", {
                key: SERVER_URL_KEY,
                oldValue: to || null,
                newValue: from || null,
                storageArea: localStorage,
              }),
            );
          }
        } else {
          setServerUrl(to);
          if (action === "round trip") setServerUrl(from);
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

  it.each(["local clear", "unrelated key", "session storage"])(
    "keeps a visit across %s",
    (action) => {
      stop = setupSessionEndedReporting();
      advance(120_000);
      if (action === "local clear") localStorage.clear();
      window.dispatchEvent(
        new StorageEvent("storage", {
          key:
            action === "local clear"
              ? null
              : action === "unrelated key"
                ? "other-key"
                : SERVER_URL_KEY,
          newValue: action === "local clear" ? null : "https://example.com/remote",
          storageArea: action === "session storage" ? sessionStorage : localStorage,
        }),
      );
      close();
      expect(buckets()).toEqual(["1_to_5m"]);
    },
  );

  it.each(["hidden expiry", "cached page", "initially hidden"])(
    "captures the server at the next visible start after %s",
    (action) => {
      hidden = action === "initially hidden";
      stop = setupSessionEndedReporting();
      if (!hidden) {
        advance(120_000);
        if (action === "hidden expiry") {
          visibility(true);
          advance(1_800_000);
        } else close();
      }
      setServerUrl("https://example.com/remote");
      setAuthToken("current-token");
      visibility(false);
      window.dispatchEvent(new Event("pageshow"));
      advance(30_000);
      close();
      expect(buckets()).toEqual(
        action === "initially hidden" ? ["under_1m"] : ["1_to_5m", "under_1m"],
      );
      expect(fetch.mock.lastCall?.[0]).toBe("https://example.com/remote/api/v1/telemetry/events");
      expect(new Headers(fetch.mock.lastCall?.[1]?.headers).get("Authorization")).toBe(
        "Bearer current-token",
      );
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
