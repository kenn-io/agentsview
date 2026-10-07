import { EventSource, type EventSourceInit } from "eventsource";
import { describe, it, expect, vi, beforeEach, afterEach } from "vite-plus/test";
import {
  triggerSync,
  watchEvents,
  WATCH_EVENTS_MAX_CONSECUTIVE_ERRORS,
  watchSession,
  syncClaudeAI,
  WATCH_SESSION_MAX_CONSECUTIVE_ERRORS,
} from "./client.js";
import type { SyncHandle } from "./client.js";
import { ApiError } from "./runtime.js";
import type { SyncProgress } from "./generated/index.js";
import * as telemetry from "../utils/telemetry.js";
import type { BrowserHost } from "./browserHost.js";

vi.mock("../utils/telemetry.js", () => ({ reportTelemetry: vi.fn() }));

describe("syncClaudeAI browser relay", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("answers fetch events with the browser body and retry header", async () => {
    let stream: ReadableStreamDefaultController<Uint8Array>;
    const encoder = new TextEncoder();
    const response = new Response(
      new ReadableStream<Uint8Array>({
        start(controller) {
          stream = controller;
        },
      }),
      { headers: { "Content-Type": "text/event-stream" } },
    );
    const host: BrowserHost = {
      connect: vi.fn(),
      disconnect: vi.fn(),
      fetch: vi.fn().mockResolvedValue({ status: 429, body: "browser response", retryAfter: "12" }),
    };
    const fetch = vi.fn(async (url: string, options: RequestInit) => {
      if (url === "/api/v1/import/claude-ai/sync") {
        expect(options.body).toBeUndefined();
        stream.enqueue(
          encoder.encode(
            'event: fetch\ndata: {"id":"request-1","path":"/api/organizations/org/chat_conversations_v2?limit=50&offset=0"}\n\n',
          ),
        );
        return response;
      }
      expect(url).toBe("/api/v1/import/claude-ai/sync/results/request-1?status=429");
      expect(await (options.body as Blob).text()).toBe("browser response");
      expect(new Headers(options.headers).get("Retry-After")).toBe("12");
      stream.enqueue(
        encoder.encode(
          'event: progress\ndata: {"imported":1,"updated":0,"skipped":0,"errors":0}\n\nevent: done\ndata: {"imported":1,"updated":0,"skipped":0,"errors":0}\n\n',
        ),
      );
      stream.close();
      return new Response(null, { status: 204 });
    });
    vi.stubGlobal("fetch", fetch);
    const progress = vi.fn();
    expect(await syncClaudeAI(host, { onProgress: progress })).toEqual({
      imported: 1,
      updated: 0,
      skipped: 0,
      errors: 0,
    });
    expect(host.fetch).toHaveBeenCalledWith(
      "/api/organizations/org/chat_conversations_v2?limit=50&offset=0",
    );
    expect(fetch).toHaveBeenCalledTimes(2);
    expect(progress).toHaveBeenCalledWith({ imported: 1, updated: 0, skipped: 0, errors: 0 });
  });

  it.each(["returned", "thrown"])("relays %s browser errors to the user", async (kind) => {
    let stream: ReadableStreamDefaultController<Uint8Array>;
    const encoder = new TextEncoder();
    const message = "TypeError: Failed to fetch";
    const host = {
      fetch:
        kind === "returned"
          ? vi.fn().mockResolvedValue({ status: 0, body: "", error: message })
          : vi.fn().mockRejectedValue(new Error(message)),
    } as unknown as BrowserHost;
    const expected = kind === "returned" ? message : "Error: " + message;
    vi.stubGlobal(
      "fetch",
      vi.fn(async (url: string, options: RequestInit) => {
        if (url === "/api/v1/import/claude-ai/sync") {
          return new Response(
            new ReadableStream<Uint8Array>({
              start(controller) {
                stream = controller;
                stream.enqueue(
                  encoder.encode(
                    'event: fetch\ndata: {"id":"failed","path":"/api/organizations/org/chat_conversations_v2"}\n\n',
                  ),
                );
              },
            }),
            { headers: { "Content-Type": "text/event-stream" } },
          );
        }
        expect(url).toBe("/api/v1/import/claude-ai/sync/results/failed?status=0");
        expect(await (options.body as Blob).text()).toBe(expected);
        stream.enqueue(
          encoder.encode(`event: error\ndata: ${JSON.stringify({ error: expected })}\n\n`),
        );
        stream.close();
        return new Response(null, { status: 204 });
      }),
    );
    await expect(syncClaudeAI(host)).rejects.toThrow(expected);
    expect(host.fetch).toHaveBeenCalledWith("/api/organizations/org/chat_conversations_v2");
  });

  it("ends the stream when posting a result fails", async () => {
    const cancelled = vi.fn();
    const host = {
      fetch: vi.fn().mockResolvedValue({ status: 200, body: "{}" }),
    } as unknown as BrowserHost;
    vi.stubGlobal(
      "fetch",
      vi
        .fn()
        .mockResolvedValueOnce(
          new Response(
            new ReadableStream({
              start(controller) {
                controller.enqueue(
                  new TextEncoder().encode(
                    'event: fetch\ndata: {"id":"expired","path":"/api/organizations/org/chat_conversations_v2"}\n\n',
                  ),
                );
              },
              cancel: cancelled,
            }),
            { headers: { "Content-Type": "text/event-stream" } },
          ),
        )
        .mockResolvedValueOnce(new Response("expired", { status: 404 })),
    );
    await expect(syncClaudeAI(host)).rejects.toThrow("expired");
    expect(cancelled).toHaveBeenCalledOnce();
  });
});

/**
 * Create a ReadableStream that yields the given chunks as
 * Uint8Array values, then closes.
 */
function makeSSEStream(chunks: string[]): ReadableStream<Uint8Array> {
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

function mockFetchWithStream(chunks: string[]): void {
  const stream = makeSSEStream(chunks);
  vi.stubGlobal(
    "fetch",
    vi.fn().mockResolvedValue({
      ok: true,
      body: stream,
    }),
  );
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

  function startSync(chunks: string[]): { handle: SyncHandle; progress: SyncProgress[] } {
    mockFetchWithStream(chunks);
    const progress: SyncProgress[] = [];
    const handle = triggerSync((p) => progress.push(p));
    activeHandles.push(handle);
    return { handle, progress };
  }

  it("should parse CRLF-terminated SSE frames", async () => {
    const { handle, progress } = startSync([
      'event: progress\r\ndata: {"phase":"scanning","projects_total":1,"projects_done":0,"sessions_total":0,"sessions_done":0,"messages_indexed":0}\r\n\r\n',
      'event: done\r\ndata: {"total_sessions":5,"synced":3,"skipped":2,"failed":0}\r\n\r\n',
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

  it("should reject when the stream reports an error event", async () => {
    const { handle } = startSync([
      'event: error\ndata: {"error":"sync worker pass reported failed"}\n\n',
    ]);

    await expect(handle.done).rejects.toThrow("sync worker pass reported failed");
  });

  it("should reject for non-ok responses", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(null, { status: 500 })));

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
    mockStream([`event: error\ndata: {"message":"CLI not found"}\n\n`]);

    const { generateInsight } = await import("./client.js");
    const handle = generateInsight({
      type: "daily_activity",
      date_from: "2025-01-15",
      date_to: "2025-01-15",
    });
    activeHandles.push(handle);

    await expect(handle.done).rejects.toThrow("CLI not found");
  });

  it("throws when stream ends without done", async () => {
    mockStream([`event: status\ndata: {"phase":"generating"}\n\n`]);

    const { generateInsight } = await import("./client.js");
    const handle = generateInsight({
      type: "daily_activity",
      date_from: "2025-01-15",
      date_to: "2025-01-15",
    });
    activeHandles.push(handle);

    await expect(handle.done).rejects.toThrow("without done event");
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
      { stream: "stdout", line: '{"type":"system"}' },
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
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(null, { status: 500 })));

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
              error: "insight generation is not available in read-only mode",
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

vi.mock("eventsource", () => ({ EventSource: vi.fn() }));

describe("watchEvents", () => {
  class FakeEventSource {
    static instances: FakeEventSource[] = [];
    public url: string;
    public readyState = 1;
    private listeners: Record<string, ((ev: MessageEvent) => void)[]> = {};
    public onerror: ((ev: Event) => void) | null = null;
    public closed = false;

    constructor(
      url: URL,
      public options: EventSourceInit,
    ) {
      this.url = url.toString();
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
    vi.mocked(EventSource).mockImplementation(function (url, options) {
      return new FakeEventSource(new URL(url), options!) as unknown as EventSource;
    });
    localStorage.clear();
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    localStorage.clear();
  });

  it("opens /api/v1/events locally without a token", () => {
    watchEvents(() => {});
    expect(FakeEventSource.instances).toHaveLength(1);
    expect(FakeEventSource.instances[0]!.url).toBe(`${window.location.origin}/api/v1/events`);
  });

  it("sends authentication through the generated request", async () => {
    localStorage.setItem("agentsview-auth-token", "secret");
    watchEvents(() => {});
    const source = FakeEventSource.instances[0]!;
    expect(source.url).toBe(`${window.location.origin}/api/v1/events`);
    const fetchMock = vi
      .fn()
      .mockResolvedValue(new Response(null, { headers: { "Content-Type": "text/event-stream" } }));
    vi.stubGlobal("fetch", fetchMock);
    await source.options.fetch!(source.url, {
      signal: new AbortController().signal,
      headers: { Accept: "text/event-stream" },
      mode: "cors",
      cache: "no-store",
      redirect: "follow",
    });
    expect(fetchMock).toHaveBeenCalledWith(
      "/api/v1/events",
      expect.objectContaining({ method: "GET" }),
    );
    expect(new Headers(fetchMock.mock.calls[0]![1].headers).get("Authorization")).toBe(
      "Bearer secret",
    );
  });

  it("invokes onEvent with parsed scope for valid data_changed frames", () => {
    const received: string[] = [];
    watchEvents((e) => received.push(e.scope));
    FakeEventSource.instances[0]!.fireRaw("data_changed", JSON.stringify({ scope: "messages" }));
    expect(received).toEqual(["messages"]);
  });

  it("falls back to { scope: 'sync' } for malformed payloads", () => {
    const received: string[] = [];
    watchEvents((e) => received.push(e.scope));
    FakeEventSource.instances[0]!.fireRaw("data_changed", "not valid json");
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
    expect(FakeEventSource.instances[0]!.url).toBe(`${server}/api/v1/events`);
  });

  it("keeps token characters out of the stream URL", () => {
    const rawToken = "a b&c?d=e/f+g";
    localStorage.setItem("agentsview-auth-token", rawToken);
    watchEvents(() => {});
    expect(FakeEventSource.instances[0]!.url).toBe(`${window.location.origin}/api/v1/events`);
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

    constructor(
      url: URL,
      public options: EventSourceInit,
    ) {
      this.url = url.toString();
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
      (this.listeners["open"] || []).forEach((cb) => cb(new Event("open") as MessageEvent));
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
    vi.mocked(EventSource).mockImplementation(function (url, options) {
      return new FakeEventSource(new URL(url), options!) as unknown as EventSource;
    });
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

  it("encodes the session ID as one URL path segment", () => {
    watchSession("deepseek-harness:child%7E/%25?#", () => {});

    expect(FakeEventSource.instances[0]?.url).toBe(
      `${window.location.origin}/api/v1/sessions/deepseek-harness%3Achild%257E%2F%2525%3F%23/watch`,
    );
  });
});

describe("core action telemetry", () => {
  beforeEach(() => {
    vi.mocked(telemetry.reportTelemetry).mockClear();
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it.each([
    ["downloadExport", "html"],
    ["downloadInsightExport", "insight_html"],
  ] as const)("%s reports export_run %s", async (fn, format) => {
    vi.spyOn(window, "open").mockReturnValue(null);
    const client = await import("./client.js");

    await (fn === "downloadExport"
      ? client.downloadExport("session-1")
      : client.downloadInsightExport(7));

    expect(telemetry.reportTelemetry).toHaveBeenCalledExactlyOnceWith("export_run", { format });
  });

  it("generateInsight reports insight_generated only after the insight is done", async () => {
    const req = {
      type: "llm_canned",
      kind: "prompt_maturity_review",
      date_from: "2025-01-15",
      date_to: "2025-01-15",
    } as const;
    mockFetchWithStream(['event: error\ndata: {"message":"boom"}\n\n']);
    const { generateInsight } = await import("./client.js");
    await expect(generateInsight(req).done).rejects.toThrow("boom");
    expect(telemetry.reportTelemetry).not.toHaveBeenCalled();

    mockFetchWithStream([`event: done\ndata: ${JSON.stringify({ id: 1 })}\n\n`]);
    await generateInsight(req).done;
    expect(telemetry.reportTelemetry).toHaveBeenCalledExactlyOnceWith("insight_generated", {
      kind: "prompt_maturity_review",
    });

    mockFetchWithStream([
      `event: done\ndata: ${JSON.stringify({ id: 1, cache_status: "hit" })}\n\n`,
    ]);
    await generateInsight(req).done;
    expect(telemetry.reportTelemetry).toHaveBeenCalledTimes(1);
  });
});
