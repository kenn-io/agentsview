import { describe, it, expect, vi, beforeEach, afterEach } from "vite-plus/test";
import { triggerSync, syncBrowser, connectClaudeAI } from "./client.js";
import type { SyncHandle } from "./client.js";
import { ApiError, setAuthToken, setServerUrl } from "./runtime.js";
import type { SyncProgress } from "./generated/index.js";
import * as telemetry from "../utils/telemetry.js";
import type { BrowserHost } from "./browserHost.js";

vi.mock("../utils/telemetry.js", () => ({ reportTelemetry: vi.fn() }));

describe("syncBrowser", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("uses the native host query and receives only counts without posting results", async () => {
    const fetch = vi
      .fn()
      .mockResolvedValue(
        new Response(
          'event: progress\ndata: {"imported":1,"updated":0,"skipped":0,"errors":0}\n\nevent: done\ndata: {"imported":1,"updated":0,"skipped":0,"errors":0}\n\n',
          { headers: { "Content-Type": "text/event-stream" } },
        ),
      );
    vi.stubGlobal("fetch", fetch);
    await expect(syncBrowser("claude-ai", undefined)).resolves.toMatchObject({ imported: 1 });
    expect(fetch).toHaveBeenCalledExactlyOnceWith(
      "/api/v1/import/claude-ai/sync?browser=chrome",
      expect.objectContaining({ method: "POST" }),
    );
  });

  it("syncs ChatGPT through Chrome even when the desktop app offers its browser", async () => {
    const host: BrowserHost = {
      connect: vi.fn(),
      close: vi.fn().mockResolvedValue(undefined),
      fetch: vi.fn(),
    };
    const fetch = vi.fn().mockResolvedValue(
      new Response('event: done\ndata: {"imported":2,"updated":1,"skipped":0,"errors":0}\n\n', {
        headers: { "Content-Type": "text/event-stream" },
      }),
    );
    vi.stubGlobal("fetch", fetch);
    await expect(syncBrowser("chatgpt", host)).resolves.toEqual({
      imported: 2,
      updated: 1,
      skipped: 0,
      errors: 0,
    });
    expect(fetch).toHaveBeenCalledExactlyOnceWith(
      "/api/v1/import/chatgpt/sync?browser=chrome",
      expect.objectContaining({ method: "POST" }),
    );
    expect(host.fetch).not.toHaveBeenCalled();
    expect(host.close).not.toHaveBeenCalled();
  });

  it.each(["browser_sync_auth_required"])("preserves recovery code %s", async (code) => {
    const host = { close: vi.fn().mockResolvedValue(undefined) } as unknown as BrowserHost;
    vi.stubGlobal(
      "fetch",
      vi
        .fn()
        .mockResolvedValue(
          new Response(
            `event: error\ndata: ${JSON.stringify({ error: "English recovery instruction", code })}\n\n`,
            { headers: { "Content-Type": "text/event-stream" } },
          ),
        ),
    );
    await expect(syncBrowser("claude-ai", host)).rejects.toMatchObject({
      message: "English recovery instruction",
      code,
    });
    expect(host.close).toHaveBeenCalledOnce();
  });

  it.each(["done", "error"])("preserves sync %s when closing the browser fails", async (event) => {
    const host = {
      close: vi.fn().mockRejectedValue(new Error("Close failed")),
    } as unknown as BrowserHost;
    const data =
      event === "done"
        ? { imported: 1, updated: 0, skipped: 0, errors: 0 }
        : { error: "Sign in required", code: "browser_sync_auth_required" };
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        new Response(`event: ${event}\ndata: ${JSON.stringify(data)}\n\n`, {
          headers: { "Content-Type": "text/event-stream" },
        }),
      ),
    );
    if (event === "done") {
      await expect(syncBrowser("claude-ai", host)).resolves.toEqual({
        imported: 1,
        updated: 0,
        skipped: 0,
        errors: 0,
      });
    } else {
      await expect(syncBrowser("claude-ai", host)).rejects.toMatchObject({
        message: "Sign in required",
        code: "browser_sync_auth_required",
      });
    }
    expect(host.close).toHaveBeenCalledOnce();
  });

  it("returns completed sync while browser cleanup is pending", async () => {
    let close!: () => void;
    const host = {
      close: vi.fn(
        () =>
          new Promise<void>((resolve) => {
            close = resolve;
          }),
      ),
    } as unknown as BrowserHost;
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        new Response('event: done\ndata: {"imported":1,"updated":0,"skipped":0,"errors":0}\n\n', {
          headers: { "Content-Type": "text/event-stream" },
        }),
      ),
    );
    await expect(syncBrowser("claude-ai", host)).resolves.toEqual({
      imported: 1,
      updated: 0,
      skipped: 0,
      errors: 0,
    });
    expect(host.close).toHaveBeenCalledOnce();
    close();
  });

  it("signs in only after the previous browser close finishes", async () => {
    let close!: () => void;
    const host = {
      connect: vi.fn().mockResolvedValue(undefined),
      close: vi.fn(
        () =>
          new Promise<void>((resolve) => {
            close = resolve;
          }),
      ),
    } as unknown as BrowserHost;
    vi.stubGlobal(
      "fetch",
      vi.fn(
        async () =>
          new Response('event: done\ndata: {"imported":0,"updated":0,"skipped":0,"errors":0}\n\n', {
            headers: { "Content-Type": "text/event-stream" },
          }),
      ),
    );
    await syncBrowser("claude-ai", host);
    const connecting = connectClaudeAI(host);
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(host.connect).not.toHaveBeenCalled();
    close();
    await connecting;
    expect(host.connect).toHaveBeenCalledOnce();
  });

  it("starts a new sync only after the previous browser close finishes", async () => {
    let close!: () => void;
    const host = {
      close: vi.fn(
        () =>
          new Promise<void>((resolve) => {
            close = resolve;
          }),
      ),
    } as unknown as BrowserHost;
    const fetch = vi.fn(
      async () =>
        new Response('event: done\ndata: {"imported":0,"updated":0,"skipped":0,"errors":0}\n\n', {
          headers: { "Content-Type": "text/event-stream" },
        }),
    );
    vi.stubGlobal("fetch", fetch);
    await syncBrowser("claude-ai", host);
    const next = syncBrowser("claude-ai", host);
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(fetch).toHaveBeenCalledOnce();
    close();
    await next;
    expect(fetch).toHaveBeenCalledTimes(2);
    close();
  });

  it("answers fetch events with browser status", async () => {
    const status = 429;
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
      close: vi.fn().mockResolvedValue(undefined),
      fetch: vi.fn().mockResolvedValue({ status, body: "browser response", retryAfter: "12" }),
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
      expect(url).toBe("/api/v1/import/claude-ai/sync/results/request-1");
      expect(JSON.parse(options.body as string)).toEqual({
        status: 429,
        body: "browser response",
        retry_after: "12",
      });
      expect(new Headers(options.headers).get("Content-Type")).toBe("application/json");
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
    expect(await syncBrowser("claude-ai", host, { onProgress: progress })).toEqual({
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
    expect(host.close).toHaveBeenCalledOnce();
  });

  it.each(["returned", "thrown"])("relays %s browser errors to the user", async (kind) => {
    let stream: ReadableStreamDefaultController<Uint8Array>;
    const encoder = new TextEncoder();
    const message = "TypeError: Failed to fetch";
    const host = {
      close: vi.fn().mockResolvedValue(undefined),
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
        expect(url).toBe("/api/v1/import/claude-ai/sync/results/failed");
        expect(JSON.parse(options.body as string)).toEqual({
          status: 0,
          body: "",
          error: expected,
        });
        stream.enqueue(
          encoder.encode(`event: error\ndata: ${JSON.stringify({ error: expected })}\n\n`),
        );
        stream.close();
        return new Response(null, { status: 204 });
      }),
    );
    await expect(syncBrowser("claude-ai", host)).rejects.toThrow(expected);
    expect(host.fetch).toHaveBeenCalledWith("/api/organizations/org/chat_conversations_v2");
    expect(host.close).toHaveBeenCalledOnce();
  });

  it("makes a retry wait for a stopped run's browser close", async () => {
    const events: string[] = [];
    const closes: (() => void)[] = [];
    const host = {
      close: vi.fn(() => {
        events.push("close");
        return new Promise<void>((resolve) => closes.push(resolve));
      }),
    } as unknown as BrowserHost;
    vi.stubGlobal(
      "fetch",
      vi.fn(async (_url: string, options: RequestInit) => {
        if (options.signal?.aborted) throw new DOMException("Aborted", "AbortError");
        events.push("sync");
        return new Response(
          'event: done\ndata: {"imported":0,"updated":0,"skipped":0,"errors":0}\n\n',
          {
            headers: { "Content-Type": "text/event-stream" },
          },
        );
      }),
    );
    const settle = () => new Promise((resolve) => setTimeout(resolve, 0));
    await syncBrowser("claude-ai", host);
    const stopped = new AbortController();
    const canceled = syncBrowser("claude-ai", host, undefined, stopped.signal);
    stopped.abort();
    const retry = syncBrowser("claude-ai", host);
    closes[0]!();
    await expect(canceled).rejects.toThrow();
    await settle();
    expect(events).toEqual(["sync", "close", "close"]);
    closes[1]!();
    await expect(retry).resolves.toMatchObject({ imported: 0 });
    expect(events).toEqual(["sync", "close", "close", "sync", "close"]);
    closes[2]!();
  });

  it("relays a late browser response to the server Sync started on", async () => {
    localStorage.setItem("agentsview-auth-token", "local-token");
    let respond!: (value: { status: number; body: string }) => void;
    const host = {
      close: vi.fn().mockResolvedValue(undefined),
      fetch: vi.fn(
        () =>
          new Promise((resolve) => {
            respond = resolve;
          }),
      ),
    } as unknown as BrowserHost;
    let stream!: ReadableStreamDefaultController<Uint8Array>;
    const encoder = new TextEncoder();
    const fetch = vi.fn(async (url: string, _options: RequestInit) => {
      if (url === "/api/v1/import/claude-ai/sync") {
        return new Response(
          new ReadableStream<Uint8Array>({
            start(controller) {
              stream = controller;
              stream.enqueue(
                encoder.encode('event: fetch\ndata: {"id":"late","path":"/chat"}\n\n'),
              );
            },
          }),
          { headers: { "Content-Type": "text/event-stream" } },
        );
      }
      stream.enqueue(
        encoder.encode('event: done\ndata: {"imported":1,"updated":0,"skipped":0,"errors":0}\n\n'),
      );
      stream.close();
      return new Response(null, { status: 204 });
    });
    vi.stubGlobal("fetch", fetch);
    try {
      const run = syncBrowser("claude-ai", host);
      await vi.waitFor(() => expect(host.fetch).toHaveBeenCalledWith("/chat"));
      setServerUrl("https://remote.example");
      setAuthToken("remote-token");
      respond({ status: 200, body: "chat contents" });
      await expect(run).resolves.toMatchObject({ imported: 1 });
      const [url, options] = fetch.mock.calls[1]!;
      expect(url).toBe("/api/v1/import/claude-ai/sync/results/late");
      expect(new Headers(options.headers).get("Authorization")).toBe("Bearer local-token");
    } finally {
      localStorage.clear();
    }
  });

  it("ends the stream when posting a result fails", async () => {
    let signal!: AbortSignal;
    const host = {
      close: vi.fn().mockResolvedValue(undefined),
      fetch: vi.fn().mockResolvedValue({ status: 200, body: "{}" }),
    } as unknown as BrowserHost;
    vi.stubGlobal(
      "fetch",
      vi
        .fn()
        .mockImplementationOnce(async (_url, options) => {
          signal = options.signal;
          return new Response(
            new ReadableStream({
              start(controller) {
                signal.addEventListener(
                  "abort",
                  () => controller.error(new DOMException("Aborted", "AbortError")),
                  { once: true },
                );
                controller.enqueue(
                  new TextEncoder().encode(
                    'event: fetch\ndata: {"id":"expired","path":"/api/organizations/org/chat_conversations_v2"}\n\n',
                  ),
                );
              },
            }),
            { headers: { "Content-Type": "text/event-stream" } },
          );
        })
        .mockResolvedValueOnce(new Response("expired", { status: 404 })),
    );
    await expect(syncBrowser("claude-ai", host)).rejects.toThrow("expired");
    expect(signal.aborted).toBe(true);
    expect(host.close).toHaveBeenCalledOnce();
  });

  it("closes the browser when Sync is cancelled", async () => {
    const controller = new AbortController();
    const host = { close: vi.fn().mockResolvedValue(undefined) } as unknown as BrowserHost;
    vi.stubGlobal(
      "fetch",
      vi.fn(async (_url, options: RequestInit) => {
        return new Response(
          new ReadableStream({
            start(stream) {
              options.signal!.addEventListener(
                "abort",
                () => stream.error(new DOMException("Aborted", "AbortError")),
                { once: true },
              );
              controller.abort();
            },
          }),
          { headers: { "Content-Type": "text/event-stream" } },
        );
      }),
    );
    await expect(syncBrowser("claude-ai", host, undefined, controller.signal)).rejects.toThrow(
      "Aborted",
    );
    expect(host.close).toHaveBeenCalledOnce();
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

  it.each(["sync_running", undefined])(
    "should reject when the stream reports an error event with code %s",
    async (code) => {
      const { handle } = startSync([
        `event: error\ndata: ${JSON.stringify({ error: "sync worker pass reported failed", code })}\n\n`,
      ]);

      await expect(handle.done).rejects.toMatchObject({
        message: "sync worker pass reported failed",
        code,
      });
    },
  );

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
    ["downloadFrictionDigestMarkdown", "friction_markdown"],
  ] as const)("%s reports export_run %s", async (fn, format) => {
    vi.spyOn(window, "open").mockReturnValue(null);
    const client = await import("./client.js");

    if (fn === "downloadExport") await client.downloadExport("session-1");
    else if (fn === "downloadInsightExport") await client.downloadInsightExport(7);
    else await client.downloadFrictionDigestMarkdown("2026-09-14");

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

describe("live event payloads", () => {
  it("refreshes the pinned server token on storage changes and page return", async () => {
    const postMessage = vi.fn();
    class Worker {
      port = { start() {}, postMessage, onmessage: null };
    }
    vi.stubGlobal("SharedWorker", Worker);
    localStorage.setItem("agentsview-server-url", "https://example.test");
    localStorage.setItem("agentsview-auth-token::https://example.test", "old-token");
    const { watchEvents } = await import("./client.js");
    const source = watchEvents(() => {});
    expect(postMessage).toHaveBeenLastCalledWith(expect.objectContaining({ token: "old-token" }));
    setAuthToken("same-tab-token");
    expect(postMessage).toHaveBeenLastCalledWith(
      expect.objectContaining({ token: "same-tab-token" }),
    );
    localStorage.setItem("agentsview-server-url", "https://other.example.test");
    localStorage.setItem("agentsview-auth-token::https://example.test", "new-token");
    window.dispatchEvent(new Event("storage"));
    expect(postMessage).toHaveBeenLastCalledWith(expect.objectContaining({ token: "new-token" }));
    window.dispatchEvent(new Event("pagehide"));
    localStorage.setItem("agentsview-auth-token::https://example.test", "latest-token");
    window.dispatchEvent(new Event("storage"));
    expect(postMessage).toHaveBeenLastCalledWith("leave");
    window.dispatchEvent(new Event("pageshow"));
    expect(postMessage).toHaveBeenLastCalledWith(
      expect.objectContaining({ token: "latest-token" }),
    );
    source.close();
    const count = postMessage.mock.calls.length;
    window.dispatchEvent(new Event("storage"));
    expect(postMessage).toHaveBeenCalledTimes(count);
    localStorage.clear();
    vi.unstubAllGlobals();
  });

  it("delivers known scopes and refreshes for malformed or unknown data", async () => {
    let port!: MessagePort;
    class Worker {
      port = { start() {}, postMessage() {}, onmessage: null } as unknown as MessagePort;
      constructor() {
        port = this.port;
      }
    }
    vi.stubGlobal("SharedWorker", Worker);
    const { watchEvents } = await import("./client.js");
    const received: string[] = [];
    const source = watchEvents((event) => received.push(event.scope));
    for (const data of [
      '{"scope":"messages"}',
      '{"scope":"sessions"}',
      "invalid",
      "{}",
      "null",
      '{"scope":"other"}',
    ]) {
      port.onmessage?.(new MessageEvent("message", { data: { type: "data_changed", data } }));
    }
    expect(received).toEqual(["messages", "sessions", "sync", "sync", "sync", "sync"]);
    source.close();
    vi.unstubAllGlobals();
  });
});
