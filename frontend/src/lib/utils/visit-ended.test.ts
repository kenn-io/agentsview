import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { setServerUrl, SERVER_URL_KEY } from "../api/runtime.js";
import { setupTelemetryReporting } from "./telemetry.js";

describe("visit ended reporting", () => {
  let now: number;
  let hidden: boolean;
  let stop: (() => void) | undefined;
  let base: HTMLBaseElement;
  const fetch = vi.fn<(input: RequestInfo | URL, init?: RequestInit) => Promise<Response>>(
    async () => new Response('{"status":"queued"}', { status: 202 }),
  );
  const advance = (ms: number) => {
    now += ms;
    vi.advanceTimersByTime(ms);
  };
  const visibility = (value: boolean) => {
    hidden = value;
    document.dispatchEvent(new Event("visibilitychange"));
  };
  const buckets = async () => {
    await vi.advanceTimersByTimeAsync(0);
    return fetch.mock.calls
      .map(([, init]) => JSON.parse(init?.body as string))
      .filter((event) => event.event === "visit_ended")
      .map((event) => event.properties.duration_bucket);
  };
  const close = () => window.dispatchEvent(new Event("pagehide"));
  const reopen = () => window.dispatchEvent(new Event("pageshow"));
  const start = async () => {
    stop = setupTelemetryReporting();
    await vi.advanceTimersByTimeAsync(0);
  };

  beforeEach(() => {
    now = 0;
    hidden = false;
    fetch.mockClear();
    vi.stubGlobal("fetch", fetch);
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-10-02T12:00:00Z"));
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
  ])("reports %i visible milliseconds as %s once on close", async (ms, bucket) => {
    await start();
    advance(ms);
    close();
    close();
    expect(await buckets()).toEqual([bucket]);
    expect(fetch).toHaveBeenCalledWith(
      "/agentsview/api/v1/telemetry/events",
      expect.objectContaining({
        keepalive: true,
        body: JSON.stringify({
          event: "visit_ended",
          properties: { surface: "web", duration_bucket: bucket },
        }),
      }),
    );
  });

  it("sums twenty visible minutes across short tab switches", async () => {
    await start();
    for (let i = 0; i < 20; i++) {
      advance(60_000);
      visibility(true);
      advance(300_000);
      visibility(false);
    }
    expect(await buckets()).toEqual([]);
    close();
    expect(await buckets()).toEqual(["5_to_30m"]);
  });

  it("ends a visit after thirty hidden minutes and starts a fresh one on return", async () => {
    await start();
    advance(120_000);
    visibility(true);
    advance(1_799_999);
    expect(await buckets()).toEqual([]);
    advance(1);
    expect(await buckets()).toEqual(["1_to_5m"]);
    visibility(false);
    advance(30_000);
    close();
    expect(await buckets()).toEqual(["1_to_5m", "under_1m"]);
  });

  it("ends a visit on return when a suspended hidden timer never fired", async () => {
    await start();
    advance(120_000);
    visibility(true);
    now += 2_400_000;
    vi.setSystemTime(Date.now() + 2_400_000);
    expect(await buckets()).toEqual([]);
    visibility(false);
    advance(30_000);
    close();
    expect(await buckets()).toEqual(["1_to_5m", "under_1m"]);
  });

  it("sends nothing for hidden-only pages", async () => {
    hidden = true;
    await start();
    advance(2_400_000);
    close();
    expect(await buckets()).toEqual([]);
  });

  it("drops pending time and removes timers and listeners on cleanup", async () => {
    await start();
    advance(120_000);
    visibility(true);
    stop!();
    advance(1_800_000);
    visibility(false);
    reopen();
    advance(120_000);
    close();
    expect(await buckets()).toEqual([]);
  });
  it.each([
    ["initial", "queued"],
    ["initial", "disabled"],
    ["daily", "queued"],
    ["daily", "disabled"],
  ])("keeps visit time through a pending %s check with a %s receipt", async (check, status) => {
    if (check === "daily") {
      await start();
      advance(120_000);
    }
    let reply!: (response: Response) => void;
    fetch.mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          reply = resolve;
        }),
    );
    if (check === "initial") {
      stop = setupTelemetryReporting();
      advance(5_000);
    } else {
      vi.setSystemTime(new Date("2026-10-03T00:00:00Z"));
      window.dispatchEvent(new Event("focus"));
      advance(5_000);
    }
    expect(await buckets()).toEqual([]);
    reply(new Response(JSON.stringify({ status }), { status: 202 }));
    await vi.advanceTimersByTimeAsync(0);
    close();
    expect(await buckets()).toEqual(
      status === "queued" ? [check === "initial" ? "under_1m" : "1_to_5m"] : [],
    );
    if (status === "disabled") {
      visibility(true);
      expect(vi.getTimerCount()).toBe(0);
      visibility(false);
      advance(120_000);
      close();
      expect(await buckets()).toEqual([]);
    }
  });

  it.each([false, true])(
    "discards the old server's visit on replacement, cross-tab=%s",
    async (crossTab) => {
      await start();
      advance(120_000);
      if (crossTab) {
        localStorage.setItem(SERVER_URL_KEY, "https://example.org");
        window.dispatchEvent(
          new StorageEvent("storage", { key: SERVER_URL_KEY, storageArea: localStorage }),
        );
      } else setServerUrl("https://example.org");
      await vi.advanceTimersByTimeAsync(0);
      advance(30_000);
      close();
      expect(await buckets()).toEqual(["under_1m"]);
    },
  );
});
