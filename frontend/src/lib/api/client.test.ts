import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import {
  triggerSync,
  listSessions,
  search,
  getAnalyticsSummary,
  getAnalyticsActivity,
  getAnalyticsHeatmap,
  getAnalyticsTopSessions,
  getTrendsTerms,
  watchEvents,
  WATCH_EVENTS_MAX_CONSECUTIVE_ERRORS,
  watchSession,
  WATCH_SESSION_MAX_CONSECUTIVE_ERRORS,
  ApiError,
} from "./client.js";
import type { SyncHandle } from "./client.js";
import type { SyncProgress } from "./types.js";

/**
 * Create a ReadableStream that yields the given chunks as
 * Uint8Array values, then closes.
 */
function makeSSEStream(
  chunks: string[],
): ReadableStream<Uint8Array> {
  const encoder = new TextEncoder();
  let i = 0;
  return new ReadableStream({
    pull(controller) {
      if (i < chunks.length) {
        controller.enqueue(encoder.encode(chunks[i]!));
        i++;
      } else {
        controller.close();
      }
    },
  });
}

function mockFetchWithStream(
  chunks: string[],
): void {
  const stream = makeSSEStream(chunks);
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue({
    ok: true,
    body: stream,
  }));
}

describe("triggerSync SSE parsing", () => {
  let activeHandles: SyncHandle[] = [];

  beforeEach(() => {
    vi.clearAllMocks();
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    for (const h of activeHandles) h.abort();
    activeHandles = [];
  });

  function startSync(
    chunks: string[],
  ): { handle: SyncHandle; progress: SyncProgress[] } {
    mockFetchWithStream(chunks);
    const progress: SyncProgress[] = [];
    const handle = triggerSync((p) => progress.push(p));
    activeHandles.push(handle);
    return { handle, progress };
  }

  it("should parse CRLF-terminated SSE frames", async () => {
    const { handle, progress } = startSync([
      "event: progress\r\ndata: {\"phase\":\"scanning\",\"projects_total\":1,\"projects_done\":0,\"sessions_total\":0,\"sessions_done\":0,\"messages_indexed\":0}\r\n\r\n",
      "event: done\r\ndata: {\"total_sessions\":5,\"synced\":3,\"skipped\":2,\"failed\":0}\r\n\r\n",
    ]);

    const stats = await handle.done;

    expect(progress.length).toBe(1);
    expect(progress[0]!.phase).toBe("scanning");
    expect(stats.total_sessions).toBe(5);
    expect(stats.synced).toBe(3);
  });

  it("should handle multi-line data: payloads", async () => {
    const { handle, progress } = startSync([
      'event: progress\ndata: {"phase":"scanning",\ndata: "projects_total":2,"projects_done":1,\ndata: "sessions_total":10,"sessions_done":5,"messages_indexed":50}\n\n',
      'event: done\ndata: {"total_sessions":10,"synced":5,"skipped":5,"failed":0}\n\n',
    ]);

    await handle.done;

    expect(progress.length).toBe(1);
    expect(progress[0]!.projects_total).toBe(2);
    expect(progress[0]!.sessions_done).toBe(5);
  });

  it("should process trailing frame on EOF", async () => {
    const { handle } = startSync([
      'event: done\ndata: {"total_sessions":1,"synced":1,"skipped":0,"failed":0}',
    ]);

    const stats = await handle.done;

    expect(stats.total_sessions).toBe(1);
    expect(stats.synced).toBe(1);
  });

  it("should trigger done once and stop processing after done", async () => {
    const { handle, progress } = startSync([
      'event: done\ndata: {"total_sessions":1,"synced":1,"skipped":0,"failed":0}\n\n',
      'event: progress\ndata: {"phase":"extra","projects_total":0,"projects_done":0,"sessions_total":0,"sessions_done":0,"messages_indexed":0}\n\n',
    ]);

    const stats = await handle.done;

    // Small delay to ensure no further processing happens
    await new Promise((r) => setTimeout(r, 50));

    expect(stats.total_sessions).toBe(1);
    expect(progress.length).toBe(0);
  });

  it("should handle data: without space after colon", async () => {
    const { handle } = startSync([
      'event: done\ndata:{"total_sessions":3,"synced":2,"skipped":1,"failed":0}\n\n',
    ]);

    const stats = await handle.done;

    expect(stats.total_sessions).toBe(3);
  });

  it("should reject for non-ok responses", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({
      ok: false,
      status: 500,
      body: null,
    }));

    const handle = triggerSync();
    activeHandles.push(handle);

    await expect(handle.done).rejects.toThrow("500");
  });

  it("should handle chunks split across frame boundaries", async () => {
    const { handle, progress } = startSync([
      'event: progress\ndata: {"phase":"scan',
      'ning","projects_total":1,"projects_done":0,"sessions_total":0,"sessions_done":0,"messages_indexed":0}\n\nevent: done\ndata: {"total_sessions":1,"synced":1,"skipped":0,"failed":0}\n\n',
    ]);

    await handle.done;

    expect(progress.length).toBe(1);
    expect(progress[0]!.phase).toBe("scanning");
  });
});

describe("deleteInsight", () => {
  let fetchSpy: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    fetchSpy = vi.fn();
    vi.stubGlobal("fetch", fetchSpy);
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("sends DELETE request to correct endpoint", async () => {
    fetchSpy.mockResolvedValue({ ok: true });
    const { deleteInsight } = await import("./client.js");
    await deleteInsight(42);

    expect(fetchSpy).toHaveBeenCalledWith(
      "/api/v1/insights/42",
      { method: "DELETE" },
    );
  });

  it("throws ApiError with status on non-ok response", async () => {
    fetchSpy.mockResolvedValue({
      ok: false,
      status: 404,
      text: () => Promise.resolve("not found"),
    });
    const { deleteInsight } = await import("./client.js");

    try {
      await deleteInsight(99);
      expect.unreachable("should have thrown");
    } catch (e) {
      expect(e).toBeInstanceOf(ApiError);
      expect((e as InstanceType<typeof ApiError>).status).toBe(404);
    }
  });

  it("throws ApiError with 500 status on server error", async () => {
    fetchSpy.mockResolvedValue({
      ok: false,
      status: 500,
      text: () => Promise.resolve("internal error"),
    });
    const { deleteInsight } = await import("./client.js");

    try {
      await deleteInsight(1);
      expect.unreachable("should have thrown");
    } catch (e) {
      expect(e).toBeInstanceOf(ApiError);
      expect((e as InstanceType<typeof ApiError>).status).toBe(500);
    }
  });
});

describe("fetchJSON error handling", () => {
  let fetchSpy: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    fetchSpy = vi.fn();
    vi.stubGlobal("fetch", fetchSpy);
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("throws ApiError with status on non-ok response", async () => {
    fetchSpy.mockResolvedValue({
      ok: false,
      status: 502,
      text: () => Promise.resolve("bad gateway"),
    });
    const { listInsights } = await import("./client.js");

    try {
      await listInsights();
      expect.unreachable("should have thrown");
    } catch (e) {
      expect(e).toBeInstanceOf(ApiError);
      expect((e as InstanceType<typeof ApiError>).status).toBe(502);
      expect((e as InstanceType<typeof ApiError>).message).toBe(
        "bad gateway",
      );
    }
  });

  it("falls back to 'API <status>' when body is empty", async () => {
    fetchSpy.mockResolvedValue({
      ok: false,
      status: 500,
      text: () => Promise.resolve(""),
    });
    const { listInsights } = await import("./client.js");

    try {
      await listInsights();
      expect.unreachable("should have thrown");
    } catch (e) {
      expect(e).toBeInstanceOf(ApiError);
      expect((e as InstanceType<typeof ApiError>).message).toBe(
        "API 500",
      );
    }
  });
});

describe("insights query serialization", () => {
  let fetchSpy: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    fetchSpy = vi.fn().mockResolvedValue({
      ok: true,
      json: () => Promise.resolve({}),
    });
    vi.stubGlobal("fetch", fetchSpy);
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  function lastUrl(): string {
    const call = fetchSpy.mock.calls[0] as [
      string,
      ...unknown[],
    ];
    return call[0];
  }

  it("lists insights with no filters", async () => {
    const { listInsights } = await import("./client.js");
    await listInsights();
    expect(lastUrl()).toBe("/api/v1/insights");
  });

  it("lists insights with type and project", async () => {
    const { listInsights } = await import("./client.js");
    await listInsights({
      type: "daily_activity",
      project: "my-app",
    });
    expect(lastUrl()).toBe(
      "/api/v1/insights?type=daily_activity&project=my-app",
    );
  });

  it("omits empty string filters", async () => {
    const { listInsights } = await import("./client.js");
    await listInsights({
      type: "",
      project: "",
    });
    expect(lastUrl()).toBe("/api/v1/insights");
  });

  it("gets single insight by id", async () => {
    const { getInsight } = await import("./client.js");
    await getInsight(42);
    expect(lastUrl()).toBe("/api/v1/insights/42");
  });
});

describe("generateInsight SSE parsing", () => {
  let activeHandles: { abort: () => void }[];

  beforeEach(() => {
    vi.clearAllMocks();
    activeHandles = [];
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    for (const h of activeHandles) h.abort();
    activeHandles = [];
  });

  function mockStream(chunks: string[]) {
    const stream = makeSSEStream(chunks);
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue({
        ok: true,
        body: stream,
      }),
    );
  }

  it("parses done event into Insight", async () => {
    const insight = {
      id: 1,
      type: "daily_activity",
      date_from: "2025-01-15",
      date_to: "2025-01-15",
      content: "# Report",
    };
    mockStream([
      `event: status\ndata: {"phase":"generating"}\n\n`,
      `event: done\ndata: ${JSON.stringify(insight)}\n\n`,
    ]);

    const { generateInsight } = await import("./client.js");
    const phases: string[] = [];
    const handle = generateInsight(
      {
        type: "daily_activity",
        date_from: "2025-01-15",
        date_to: "2025-01-15",
      },
      (p) => phases.push(p),
    );
    activeHandles.push(handle);

    const result = await handle.done;

    expect(result.id).toBe(1);
    expect(result.content).toBe("# Report");
    expect(phases).toContain("generating");
  });

  it("throws on error event", async () => {
    mockStream([
      `event: error\ndata: {"message":"CLI not found"}\n\n`,
    ]);

    const { generateInsight } = await import("./client.js");
    const handle = generateInsight({
      type: "daily_activity",
      date_from: "2025-01-15",
      date_to: "2025-01-15",
    });
    activeHandles.push(handle);

    await expect(handle.done).rejects.toThrow(
      "CLI not found",
    );
  });

  it("throws when stream ends without done", async () => {
    mockStream([
      `event: status\ndata: {"phase":"generating"}\n\n`,
    ]);

    const { generateInsight } = await import("./client.js");
    const handle = generateInsight({
      type: "daily_activity",
      date_from: "2025-01-15",
      date_to: "2025-01-15",
    });
    activeHandles.push(handle);

    await expect(handle.done).rejects.toThrow(
      "without done event",
    );
  });

  it("dispatches log events", async () => {
    const insight = {
      id: 2,
      type: "daily_activity",
      date_from: "2025-01-15",
      date_to: "2025-01-15",
      content: "# Report",
    };
    mockStream([
      `event: log\ndata: {"stream":"stdout","line":"{\\"type\\":\\"system\\"}"}\n\n`,
      `event: log\ndata: {"stream":"stderr","line":"rate limited"}\n\n`,
      `event: done\ndata: ${JSON.stringify(insight)}\n\n`,
    ]);

    const { generateInsight } = await import("./client.js");
    const logs: { stream: string; line: string }[] = [];
    const handle = generateInsight(
      {
        type: "daily_activity",
        date_from: "2025-01-15",
        date_to: "2025-01-15",
      },
      undefined,
      (event) => logs.push(event),
    );
    activeHandles.push(handle);

    await handle.done;
    expect(logs).toEqual([
      { stream: "stdout", line: "{\"type\":\"system\"}" },
      { stream: "stderr", line: "rate limited" },
    ]);
  });

  it("does not replay already processed SSE frames across chunks", async () => {
    const insight = {
      id: 3,
      type: "daily_activity",
      date_from: "2025-01-15",
      date_to: "2025-01-15",
      content: "# Report",
    };
    mockStream([
      `event: log\ndata: {"stream":"stdout","line":"first"}\n\n`,
      `event: log\ndata: {"stream":"stdout","line":"second"}\n\n`,
      `event: done\ndata: ${JSON.stringify(insight)}\n\n`,
    ]);

    const { generateInsight } = await import("./client.js");
    const logs: string[] = [];
    const handle = generateInsight(
      {
        type: "daily_activity",
        date_from: "2025-01-15",
        date_to: "2025-01-15",
      },
      undefined,
      (event) => logs.push(event.line),
    );
    activeHandles.push(handle);

    await handle.done;
    expect(logs).toEqual(["first", "second"]);
  });

  it("rejects for non-ok response", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue({
        ok: false,
        status: 500,
        body: null,
        text: () => Promise.resolve(""),
      }),
    );

    const { generateInsight } = await import("./client.js");
    const handle = generateInsight({
      type: "daily_activity",
      date_from: "2025-01-15",
      date_to: "2025-01-15",
    });
    activeHandles.push(handle);

    await expect(handle.done).rejects.toThrow("500");
  });

  it("surfaces backend error text on non-ok response", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue({
        ok: false,
        status: 501,
        text: () =>
          Promise.resolve(
            JSON.stringify({
              error:
                "insight generation is not available in read-only mode",
            }),
          ),
      }),
    );

    const { generateInsight } = await import("./client.js");
    const handle = generateInsight({
      type: "daily_activity",
      date_from: "2025-01-15",
      date_to: "2025-01-15",
    });
    activeHandles.push(handle);

    try {
      await handle.done;
      expect.unreachable("should have thrown");
    } catch (e) {
      expect(e).toBeInstanceOf(ApiError);
      expect((e as InstanceType<typeof ApiError>).status).toBe(501);
      expect((e as InstanceType<typeof ApiError>).message).toBe(
        "insight generation is not available in read-only mode",
      );
    }
  });
});

describe("query serialization", () => {
  let fetchSpy: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    fetchSpy = vi.fn().mockResolvedValue({
      ok: true,
      json: () => Promise.resolve({}),
    });
    vi.stubGlobal("fetch", fetchSpy);
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  function lastUrl(): string {
    const call = fetchSpy.mock.calls[0] as [string, ...unknown[]];
    return call[0];
  }

  describe("buildQuery edge cases via listSessions", () => {
    const cases: {
      name: string;
      params: Record<string, string | number | undefined>;
      expected: string;
    }[] = [
      {
        name: "omits undefined values",
        params: {
          project: undefined,
          machine: "m1",
        },
        expected: "/api/v1/sessions?machine=m1",
      },
      {
        name: "omits empty string values",
        params: { project: "", machine: "m1" },
        expected: "/api/v1/sessions?machine=m1",
      },
      {
        name: "includes numeric zero",
        params: { min_messages: 0 },
        expected: "/api/v1/sessions?min_messages=0",
      },
      {
        name: "includes positive numbers",
        params: { limit: 25, min_messages: 5 },
        expected:
          "/api/v1/sessions?limit=25&min_messages=5",
      },
      {
        name: "produces no query string when all empty",
        params: {
          project: "",
          machine: "",
          agent: "",
        },
        expected: "/api/v1/sessions",
      },
      {
        name: "produces no query string when all undefined",
        params: {
          project: undefined,
          machine: undefined,
        },
        expected: "/api/v1/sessions",
      },
      {
        name: "preserves comma-separated machine filters",
        params: {
          machine: "host-a,host-b,host-c",
        },
        expected:
          "/api/v1/sessions?machine=host-a%2Chost-b%2Chost-c",
      },
    ];

    for (const { name, params, expected } of cases) {
      it(name, async () => {
        await listSessions(params);
        expect(lastUrl()).toBe(expected);
      });
    }
  });

  describe("search query serialization", () => {
    it("includes query and non-empty params", async () => {
      await search("hello", { project: "proj1", limit: 10 });
      expect(lastUrl()).toBe(
        "/api/v1/search?q=hello&project=proj1&limit=10",
      );
    });

    it("omits empty project filter", async () => {
      await search("hello", { project: "" });
      expect(lastUrl()).toBe("/api/v1/search?q=hello");
    });

    it("includes sort param when provided", async () => {
      await search("hello", { sort: "recency" });
      expect(lastUrl()).toBe("/api/v1/search?q=hello&sort=recency");
    });

    it("omits sort param when not provided", async () => {
      await search("hello");
      expect(lastUrl()).toBe("/api/v1/search?q=hello");
    });

    it("rejects empty query string", () => {
      expect(() => search("")).toThrow(
        "search query must not be empty",
      );
      expect(fetchSpy).not.toHaveBeenCalled();
    });
  });

  describe("analytics query serialization", () => {
    it("omits empty string params from summary", async () => {
      await getAnalyticsSummary({
        from: "2024-01-01",
        project: "",
        machine: "",
      });
      expect(lastUrl()).toBe(
        "/api/v1/analytics/summary?from=2024-01-01",
      );
    });

    it("includes all non-empty analytics params", async () => {
      await getAnalyticsActivity({
        from: "2024-01-01",
        to: "2024-12-31",
        granularity: "week",
      });
      expect(lastUrl()).toBe(
        "/api/v1/analytics/activity" +
          "?from=2024-01-01&to=2024-12-31&granularity=week",
      );
    });

    it("omits empty metric from heatmap", async () => {
      await getAnalyticsHeatmap({
        from: "2024-01-01",
        metric: "" as "messages" | "sessions",
      });
      expect(lastUrl()).toBe(
        "/api/v1/analytics/heatmap?from=2024-01-01",
      );
    });

    it("omits empty metric from top-sessions", async () => {
      await getAnalyticsTopSessions({
        from: "2024-01-01",
        metric: "" as "messages" | "duration",
      });
      expect(lastUrl()).toBe(
        "/api/v1/analytics/top-sessions?from=2024-01-01",
      );
    });
  });

  describe("trends query serialization", () => {
    it("serializes trends repeated term params", async () => {
      fetchSpy.mockResolvedValue({
        ok: true,
        json: () => Promise.resolve({
          granularity: "week",
          from: "2024-06-01",
          to: "2024-06-30",
          message_count: 0,
          buckets: [],
          series: [],
        }),
      });

      await getTrendsTerms({
        from: "2024-06-01",
        to: "2024-06-30",
        timezone: "UTC",
        granularity: "week",
        terms: ["load bearing | load-bearing", "seam"],
      });

      const [path, query = ""] = lastUrl().split("?");
      expect(path).toBe("/api/v1/trends/terms");
      const params = new URLSearchParams(query);
      expect(params.get("from")).toBe("2024-06-01");
      expect(params.get("to")).toBe("2024-06-30");
      expect(params.get("timezone")).toBe("UTC");
      expect(params.get("granularity")).toBe("week");
      expect(params.getAll("term")).toEqual([
        "load bearing | load-bearing",
        "seam",
      ]);
    });
  });
});

describe("watchEvents", () => {
  class FakeEventSource {
    static instances: FakeEventSource[] = [];
    public url: string;
    public readyState = 1;
    private listeners: Record<string, ((ev: MessageEvent) => void)[]> = {};
    public onerror: ((ev: Event) => void) | null = null;
    public closed = false;

    constructor(url: string) {
      this.url = url;
      FakeEventSource.instances.push(this);
    }

    addEventListener(name: string, cb: (ev: MessageEvent) => void) {
      (this.listeners[name] ||= []).push(cb);
    }

    close() {
      this.closed = true;
    }

    // Fire an onerror event (the native API triggers via the property, not addEventListener).
    fireError() {
      if (this.onerror) this.onerror(new Event("error"));
    }

    // Fire an open event (successful (re)connect).
    fireOpen() {
      (this.listeners["open"] || []).forEach((cb) => cb(new Event("open") as MessageEvent));
    }

    // Fire a frame with a string body (caller controls JSON validity).
    fireRaw(name: string, data: string) {
      const payload = { data } as MessageEvent;
      (this.listeners[name] || []).forEach((cb) => cb(payload));
    }

    static reset() {
      FakeEventSource.instances = [];
    }
  }

  beforeEach(() => {
    FakeEventSource.reset();
    vi.stubGlobal("EventSource", FakeEventSource);
    localStorage.clear();
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    localStorage.clear();
  });

  it("opens /api/v1/events locally without a token", () => {
    watchEvents(() => {});
    expect(FakeEventSource.instances).toHaveLength(1);
    expect(FakeEventSource.instances[0]!.url).toBe("/api/v1/events");
  });

  it("appends ?token= when an auth token is set", () => {
    localStorage.setItem("agentsview-auth-token", "secret");
    watchEvents(() => {});
    expect(FakeEventSource.instances[0]!.url).toBe(
      "/api/v1/events?token=secret",
    );
  });

  it("invokes onEvent with parsed scope for valid data_changed frames", () => {
    const received: string[] = [];
    watchEvents((e) => received.push(e.scope));
    FakeEventSource.instances[0]!.fireRaw(
      "data_changed",
      JSON.stringify({ scope: "messages" }),
    );
    expect(received).toEqual(["messages"]);
  });

  it("falls back to { scope: 'sync' } for malformed payloads", () => {
    const received: string[] = [];
    watchEvents((e) => received.push(e.scope));
    FakeEventSource.instances[0]!.fireRaw(
      "data_changed",
      "not valid json",
    );
    expect(received).toEqual(["sync"]);
  });

  it("falls back to { scope: 'sync' } for parsed-but-invalid payloads", () => {
    const received: string[] = [];
    watchEvents((e) => received.push(e.scope));
    const es = FakeEventSource.instances[0]!;
    // Empty object — no scope field.
    es.fireRaw("data_changed", JSON.stringify({}));
    // Unknown scope value.
    es.fireRaw("data_changed", JSON.stringify({ scope: "bogus" }));
    // Non-object payloads (string, number, null).
    es.fireRaw("data_changed", JSON.stringify("messages"));
    es.fireRaw("data_changed", JSON.stringify(42));
    es.fireRaw("data_changed", JSON.stringify(null));
    expect(received).toEqual(["sync", "sync", "sync", "sync", "sync"]);
  });

  it("opens <server>/api/v1/events with server-scoped token in remote mode", () => {
    const server = "https://remote.example.com";
    localStorage.setItem("agentsview-server-url", server);
    localStorage.setItem(`agentsview-auth-token::${server}`, "remote-token");
    watchEvents(() => {});
    expect(FakeEventSource.instances[0]!.url).toBe(
      `${server}/api/v1/events?token=remote-token`,
    );
  });

  it("URL-encodes reserved characters in the token query parameter", () => {
    const rawToken = "a b&c?d=e/f+g";
    localStorage.setItem("agentsview-auth-token", rawToken);
    watchEvents(() => {});
    expect(FakeEventSource.instances[0]!.url).toBe(
      `/api/v1/events?token=${encodeURIComponent(rawToken)}`,
    );
  });

  it("closes the EventSource after N consecutive errors without a successful event", () => {
    watchEvents(() => {});
    const es = FakeEventSource.instances[0]!;
    for (let i = 0; i < WATCH_EVENTS_MAX_CONSECUTIVE_ERRORS - 1; i++) {
      es.fireError();
      expect(es.closed).toBe(false);
    }
    es.fireError();
    expect(es.closed).toBe(true);
  });

  it("resets the error counter on a successful (re)connect", () => {
    watchEvents(() => {});
    const es = FakeEventSource.instances[0]!;
    // Accumulate N-1 errors.
    for (let i = 0; i < WATCH_EVENTS_MAX_CONSECUTIVE_ERRORS - 1; i++) {
      es.fireError();
    }
    // A successful reconnect (open event) resets the counter.
    es.fireOpen();
    // Another N-1 errors should still not close.
    for (let i = 0; i < WATCH_EVENTS_MAX_CONSECUTIVE_ERRORS - 1; i++) {
      es.fireError();
    }
    expect(es.closed).toBe(false);
  });

  it("resets the error counter after a successful event delivery", () => {
    const received: string[] = [];
    watchEvents((e) => received.push(e.scope));
    const es = FakeEventSource.instances[0]!;
    // Accumulate N-1 errors, then a successful event resets the counter.
    for (let i = 0; i < WATCH_EVENTS_MAX_CONSECUTIVE_ERRORS - 1; i++) {
      es.fireError();
    }
    es.fireRaw("data_changed", JSON.stringify({ scope: "messages" }));
    expect(received).toEqual(["messages"]);
    // Another N-1 errors should still not close — counter is back at 0.
    for (let i = 0; i < WATCH_EVENTS_MAX_CONSECUTIVE_ERRORS - 1; i++) {
      es.fireError();
    }
    expect(es.closed).toBe(false);
  });
});

describe("watchSession", () => {
  class FakeEventSource {
    static instances: FakeEventSource[] = [];
    public url: string;
    public readyState = 1;
    private listeners: Record<string, ((ev: MessageEvent) => void)[]> = {};
    public onerror: ((ev: Event) => void) | null = null;
    public closed = false;

    constructor(url: string) {
      this.url = url;
      FakeEventSource.instances.push(this);
    }

    addEventListener(name: string, cb: (ev: MessageEvent) => void) {
      (this.listeners[name] ||= []).push(cb);
    }

    close() {
      this.closed = true;
    }

    fireError() {
      if (this.onerror) this.onerror(new Event("error"));
    }

    fireOpen() {
      (this.listeners["open"] || []).forEach((cb) =>
        cb(new Event("open") as MessageEvent),
      );
    }

    fireUpdate() {
      (this.listeners["session_updated"] || []).forEach((cb) =>
        cb(new MessageEvent("session_updated")),
      );
    }

    static reset() {
      FakeEventSource.instances = [];
    }
  }

  beforeEach(() => {
    FakeEventSource.reset();
    vi.stubGlobal("EventSource", FakeEventSource);
    localStorage.clear();
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    localStorage.clear();
  });

  it("closes the EventSource after N consecutive errors", () => {
    // Unknown session ids now return HTTP 404 per the Session API
    // contract. Without a retry cap the browser would hammer /watch
    // forever; this test locks in the circuit breaker instead.
    watchSession("abc", () => {});
    const es = FakeEventSource.instances[0]!;
    for (let i = 0; i < WATCH_SESSION_MAX_CONSECUTIVE_ERRORS - 1; i++) {
      es.fireError();
      expect(es.closed).toBe(false);
    }
    es.fireError();
    expect(es.closed).toBe(true);
  });

  it("resets the error counter on session_updated or open", () => {
    const seen: number[] = [];
    watchSession("abc", () => seen.push(1));
    const es = FakeEventSource.instances[0]!;

    for (let i = 0; i < WATCH_SESSION_MAX_CONSECUTIVE_ERRORS - 1; i++) {
      es.fireError();
    }
    es.fireUpdate(); // successful delivery resets counter
    expect(seen).toEqual([1]);

    for (let i = 0; i < WATCH_SESSION_MAX_CONSECUTIVE_ERRORS - 1; i++) {
      es.fireError();
    }
    es.fireOpen(); // successful (re)connect also resets
    for (let i = 0; i < WATCH_SESSION_MAX_CONSECUTIVE_ERRORS - 1; i++) {
      es.fireError();
    }
    expect(es.closed).toBe(false);
  });
});
