import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { SERVER_URL_KEY, setAuthToken, setServerUrl } from "../api/runtime.js";
import { setupVisitEndedReporting } from "./visit-ended.js";

const REMOTE = "https://example.com/remote";
const OTHER_REMOTE = "https://example.org/remote";
const LOCAL_EVENTS_URL = "/agentsview/api/v1/telemetry/events";

function eventsUrl(server: string): string {
  return server ? `${server}/api/v1/telemetry/events` : LOCAL_EVENTS_URL;
}

function storeServer(server: string): void {
  if (server) localStorage.setItem(SERVER_URL_KEY, server);
  else localStorage.removeItem(SERVER_URL_KEY);
}

function storageEventFromOtherTab(key: string | null, newValue: string | null): void {
  window.dispatchEvent(new StorageEvent("storage", { key, newValue, storageArea: localStorage }));
}

describe("visit ended reporting", () => {
  let now: number;
  let hidden: boolean;
  let stop: (() => void) | undefined;
  let base: HTMLBaseElement;
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
  const reopen = () => window.dispatchEvent(new Event("pageshow"));
  const lastAuthorization = () =>
    new Headers(fetch.mock.lastCall?.[1]?.headers).get("Authorization");

  beforeEach(() => {
    now = 0;
    hidden = false;
    fetch.mockClear();
    vi.stubGlobal("fetch", fetch);
    vi.useFakeTimers();
    vi.spyOn(performance, "now").mockImplementation(() => now);
    vi.spyOn(document, "hidden", "get").mockImplementation(() => hidden);
    base = document.createElement("base");
    base.href = `${window.location.origin}/agentsview/`;
    document.head.append(base);
  });
  afterEach(() => {
    stop?.();
    base.remove();
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
    [1_799_999, "5_to_30m"],
    [1_800_000, "over_30m"],
  ])("reports %i visible milliseconds as %s once on close", (ms, bucket) => {
    const timeout = vi.spyOn(AbortSignal, "timeout");
    stop = setupVisitEndedReporting();
    advance(ms);
    close();
    close();
    expect(buckets()).toEqual([bucket]);
    expect(fetch).toHaveBeenCalledWith(
      LOCAL_EVENTS_URL,
      expect.objectContaining({
        keepalive: true,
        signal: expect.any(AbortSignal),
        body: JSON.stringify({
          event: "visit_ended",
          properties: { surface: "web", duration_bucket: bucket },
        }),
      }),
    );
    expect(timeout).toHaveBeenCalledWith(10_000);
  });

  it("sums twenty visible minutes across short tab switches", () => {
    stop = setupVisitEndedReporting();
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
    { change: "connecting in this tab", from: "", to: REMOTE, run: () => setServerUrl(REMOTE) },
    { change: "disconnecting in this tab", from: REMOTE, to: "", run: () => setServerUrl("") },
    {
      change: "a round trip to remote in this tab",
      from: "",
      to: "",
      run: () => {
        setServerUrl(REMOTE);
        setServerUrl("");
      },
    },
    {
      change: "a round trip to local in this tab",
      from: REMOTE,
      to: REMOTE,
      run: () => {
        setServerUrl("");
        setServerUrl(REMOTE);
      },
    },
    {
      change: "a switch in another tab",
      from: REMOTE,
      to: OTHER_REMOTE,
      run: () => {
        storeServer(OTHER_REMOTE);
        storageEventFromOtherTab(SERVER_URL_KEY, OTHER_REMOTE);
      },
    },
    {
      change: "a round trip in another tab",
      from: REMOTE,
      to: REMOTE,
      run: () => {
        storageEventFromOtherTab(SERVER_URL_KEY, null);
        storageEventFromOtherTab(SERVER_URL_KEY, REMOTE);
      },
    },
    {
      change: "a clear and restore in another tab",
      from: REMOTE,
      to: REMOTE,
      run: () => {
        storageEventFromOtherTab(null, null);
        storageEventFromOtherTab(SERVER_URL_KEY, REMOTE);
      },
    },
    {
      change: "a clear with no storage event",
      from: REMOTE,
      to: "",
      run: () => localStorage.clear(),
    },
  ])(
    "discards a visit across $change and reports the next visit to the current server",
    ({ from, to, run }) => {
      setServerUrl(from);
      setAuthToken("old-token");
      stop = setupVisitEndedReporting();
      advance(120_000);
      run();
      close();
      expect(fetch).not.toHaveBeenCalled();

      reopen();
      setAuthToken("current-token");
      advance(30_000);
      close();
      expect(buckets()).toEqual(["under_1m"]);
      expect(fetch.mock.lastCall?.[0]).toBe(eventsUrl(to));
      expect(lastAuthorization()).toBe("Bearer current-token");
    },
  );

  it.each([
    { change: "a clear while local", run: () => storageEventFromOtherTab(null, null) },
    { change: "an unrelated key", run: () => storageEventFromOtherTab("other-key", REMOTE) },
    {
      change: "a session storage change",
      run: () =>
        window.dispatchEvent(
          new StorageEvent("storage", {
            key: SERVER_URL_KEY,
            newValue: REMOTE,
            storageArea: sessionStorage,
          }),
        ),
    },
    { change: "reselecting the same server", run: () => setServerUrl("") },
  ])("keeps a visit across $change", ({ run }) => {
    stop = setupVisitEndedReporting();
    advance(120_000);
    run();
    close();
    expect(buckets()).toEqual(["1_to_5m"]);
  });

  it.each([
    {
      start: "after hidden expiry",
      startHidden: false,
      endFirstVisit: () => {
        advance(120_000);
        visibility(true);
        advance(1_800_000);
      },
      expected: ["1_to_5m", "under_1m"],
    },
    {
      start: "after a cached page returns",
      startHidden: false,
      endFirstVisit: () => {
        advance(120_000);
        close();
      },
      expected: ["1_to_5m", "under_1m"],
    },
    {
      start: "for an initially hidden page",
      startHidden: true,
      endFirstVisit: () => {},
      expected: ["under_1m"],
    },
  ])(
    "captures the server at the next visible start $start",
    ({ startHidden, endFirstVisit, expected }) => {
      hidden = startHidden;
      stop = setupVisitEndedReporting();
      endFirstVisit();
      setServerUrl(REMOTE);
      setAuthToken("current-token");
      visibility(false);
      reopen();
      advance(30_000);
      close();
      expect(buckets()).toEqual(expected);
      expect(fetch.mock.lastCall?.[0]).toBe(eventsUrl(REMOTE));
      expect(lastAuthorization()).toBe("Bearer current-token");
    },
  );

  it("ends a visit after thirty hidden minutes and starts a fresh one on return", () => {
    stop = setupVisitEndedReporting();
    advance(120_000);
    visibility(true);
    advance(1_799_999);
    expect(buckets()).toEqual([]);
    advance(1);
    expect(buckets()).toEqual(["1_to_5m"]);
    visibility(false);
    advance(30_000);
    close();
    expect(buckets()).toEqual(["1_to_5m", "under_1m"]);
  });

  it("ends a visit on return when a suspended hidden timer never fired", () => {
    stop = setupVisitEndedReporting();
    advance(120_000);
    visibility(true);
    now += 2_400_000;
    vi.setSystemTime(Date.now() + 2_400_000);
    expect(buckets()).toEqual([]);
    visibility(false);
    advance(30_000);
    close();
    expect(buckets()).toEqual(["1_to_5m", "under_1m"]);
    expect(fetch.mock.calls[0]![1]?.signal).not.toBe(fetch.mock.calls[1]![1]?.signal);
  });

  it("sends nothing for hidden-only pages", () => {
    hidden = true;
    stop = setupVisitEndedReporting();
    advance(2_400_000);
    close();
    expect(buckets()).toEqual([]);
  });

  it("drops pending time and removes timers and listeners on cleanup", () => {
    stop = setupVisitEndedReporting();
    advance(120_000);
    visibility(true);
    stop();
    advance(1_800_000);
    visibility(false);
    reopen();
    advance(120_000);
    close();
    expect(buckets()).toEqual([]);
  });
});
