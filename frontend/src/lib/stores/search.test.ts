import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import {
  ApiError as GeneratedApiError,
  CancelablePromise,
  SearchService,
} from "../api/generated/index.js";
import type { SearchResponse } from "../api/types.js";
import {
  SEARCH_MODE_STORAGE_KEY,
  createSearchStore,
  type SearchMode,
} from "./search.svelte.js";

vi.mock("../api/generated/index.js", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/generated/index.js")>();
  return {
    ...actual,
    SearchService: {
      getApiV1Search: vi.fn(),
      getApiV1SearchContent: vi.fn(),
    },
  };
});

const searchService = vi.mocked(SearchService);
const DEBOUNCE_MS = 300;

function memoryStorage(initial: Record<string, string> = {}) {
  const values = new Map(Object.entries(initial));
  return {
    getItem: vi.fn((key: string) => values.get(key) ?? null),
    setItem: vi.fn((key: string, value: string) => values.set(key, value)),
  };
}

function fullTextResponse(query: string, count = 1): SearchResponse {
  return {
    query,
    count,
    next: 0,
    results: Array.from({ length: count }, (_, index) => ({
      session_id: `full-${index}`,
      project: "alpha",
      agent: "codex",
      name: `Result ${index}`,
      ordinal: index + 2,
      session_ended_at: `2026-07-0${index + 1}T12:00:00Z`,
      snippet: `before <mark>result ${index}</mark> after`,
      rank: index + 0.25,
    })),
  };
}

function contentMatch(sessionId: string, ordinal: number, score: number) {
  return {
    session_id: sessionId,
    project: "semantic-project",
    agent: "claude",
    ordinal,
    timestamp: `2026-06-${String(ordinal).padStart(2, "0")}T10:00:00Z`,
    snippet: `semantic snippet ${ordinal}`,
    score,
  };
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (error: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

function generatedApiError(status: number, detail: string): GeneratedApiError {
  return new GeneratedApiError(
    {} as never,
    {
      url: "/api/v1/search/content",
      ok: false,
      status,
      statusText: status === 501 ? "Not Implemented" : "Service Unavailable",
      body: { error: detail },
    },
    `request failed with ${status}`,
  );
}

async function runDebounce() {
  await vi.advanceTimersByTimeAsync(DEBOUNCE_MS);
  await Promise.resolve();
}

async function flushMicrotasks(ticks = 5) {
  for (let index = 0; index < ticks; index += 1) await Promise.resolve();
}

describe("SearchStore", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    vi.resetAllMocks();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it.each([
    [undefined, "fulltext"],
    ["not-a-mode", "fulltext"],
    ["semantic", "semantic"],
    ["hybrid", "hybrid"],
  ] as const)("loads persisted mode %s as %s", (stored, expected) => {
    const storage = memoryStorage(
      stored === undefined ? {} : { [SEARCH_MODE_STORAGE_KEY]: stored },
    );

    expect(createSearchStore(storage).mode).toBe(expected);
  });

  it("defensively handles unavailable storage", () => {
    const throwingStorage = {
      getItem: vi.fn(() => {
        throw new DOMException("denied", "SecurityError");
      }),
      setItem: vi.fn(() => {
        throw new DOMException("denied", "SecurityError");
      }),
    };
    const store = createSearchStore(throwingStorage);

    expect(store.mode).toBe("fulltext");
    expect(() => store.setMode("semantic")).not.toThrow();
    expect(store.mode).toBe("semantic");
  });

  it("persists mode and clear preserves mode and sort", () => {
    const storage = memoryStorage();
    const store = createSearchStore(storage);

    store.setMode("hybrid");
    store.setSort("recency");
    store.clear();

    expect(storage.setItem).toHaveBeenCalledWith(
      SEARCH_MODE_STORAGE_KEY,
      "hybrid",
    );
    expect(store.mode).toBe("hybrid");
    expect(store.sort).toBe("recency");
  });

  it("calls full-text search with the exact request and normalizes its result", async () => {
    const store = createSearchStore(memoryStorage());
    searchService.getApiV1Search.mockResolvedValueOnce(fullTextResponse("needle"));

    store.search("needle", "alpha");
    await runDebounce();

    expect(searchService.getApiV1Search).toHaveBeenCalledWith({
      q: "needle",
      project: "alpha",
      limit: 30,
      sort: "relevance",
    });
    expect(searchService.getApiV1SearchContent).not.toHaveBeenCalled();
    expect(store.results).toEqual([
      {
        session_id: "full-0",
        project: "alpha",
        agent: "codex",
        name: "Result 0",
        ordinal: 2,
        timestamp: "2026-07-01T12:00:00Z",
        snippet: "before <mark>result 0</mark> after",
        rank: 0.25,
        snippetFormat: "highlighted-html",
      },
    ]);
  });

  it.each(["semantic", "hybrid"] as const)(
    "calls content search with exact %s request and trimmed pattern",
    async (mode: SearchMode) => {
      const store = createSearchStore(memoryStorage());
      store.setMode(mode);
      searchService.getApiV1SearchContent.mockResolvedValueOnce({
        matches: [contentMatch("semantic-1", 4, 0.9)],
      });

      store.search("  padded query  ", "semantic-project");
      await runDebounce();

      expect(searchService.getApiV1SearchContent).toHaveBeenCalledWith({
        pattern: "padded query",
        mode,
        project: "semantic-project",
        limit: 120,
        xAgentsViewSearchIntent: "semantic",
        includeOneShot: true,
        includeAutomated: true,
      });
      expect(searchService.getApiV1Search).not.toHaveBeenCalled();
      expect(store.results[0]).toEqual({
        session_id: "semantic-1",
        project: "semantic-project",
        agent: "claude",
        ordinal: 4,
        timestamp: "2026-06-04T10:00:00Z",
        snippet: "semantic snippet 4",
        rank: 0.9,
        snippetFormat: "plain-text",
      });
    },
  );

  it("deduplicates ranked content results in response order and truncates to 30", async () => {
    const store = createSearchStore(
      memoryStorage({ [SEARCH_MODE_STORAGE_KEY]: "semantic" }),
    );
    const matches = [
      contentMatch("first", 1, 0.1),
      contentMatch("duplicate", 2, 0.99),
      contentMatch("duplicate", 3, 1),
      ...Array.from({ length: 35 }, (_, index) =>
        contentMatch(`session-${index}`, index + 4, 100 - index),
      ),
    ];
    searchService.getApiV1SearchContent.mockResolvedValueOnce({ matches });

    store.search("ranked");
    await runDebounce();

    expect(store.results).toHaveLength(30);
    expect(store.results.map((result) => result.session_id).slice(0, 4)).toEqual([
      "first",
      "duplicate",
      "session-0",
      "session-1",
    ]);
    expect(store.results.find((result) => result.session_id === "duplicate")?.ordinal)
      .toBe(2);
    expect(store.results.at(-1)?.session_id).toBe("session-27");
  });

  it.each([
    [501, "Semantic search is unavailable: run agentsview embeddings build", "semantic-unavailable"],
    [503, "Embedding provider is temporarily unavailable; retry shortly", "generic"],
  ] as const)(
    "preserves actionable backend detail for HTTP %i",
    async (status, detail, kind) => {
      const store = createSearchStore(
        memoryStorage({ [SEARCH_MODE_STORAGE_KEY]: "semantic" }),
      );
      searchService.getApiV1SearchContent.mockRejectedValueOnce(
        generatedApiError(status, detail),
      );

      store.search("failure");
      await runDebounce();

      expect(store.error).toEqual({ detail, kind });
      expect(store.results).toEqual([]);
      expect(store.isSearching).toBe(false);
    },
  );

  it("keeps a full-text 501 generic: setup guidance only applies to semantic modes", async () => {
    const store = createSearchStore(memoryStorage());
    searchService.getApiV1Search.mockRejectedValueOnce(
      generatedApiError(501, "not implemented"),
    );

    store.search("fulltext failure");
    await runDebounce();

    expect(store.error).toEqual({ detail: "not implemented", kind: "generic" });
  });

  it("clears an earlier error when a new request starts and on clear", async () => {
    const store = createSearchStore(memoryStorage());
    searchService.getApiV1Search.mockRejectedValueOnce(new Error("network offline"));
    store.search("first");
    await runDebounce();
    expect(store.error).toEqual({
      detail: "network offline",
      kind: "generic",
    });

    const pending = deferred<SearchResponse>();
    searchService.getApiV1Search.mockReturnValueOnce(pending.promise as never);
    store.search("second");
    await runDebounce();
    expect(store.error).toBeNull();
    expect(store.isSearching).toBe(true);

    store.clear();
    expect(store.error).toBeNull();
    expect(store.isSearching).toBe(false);
  });

  it("mode changes cancel queued work and immediately rerun the active query", async () => {
    const storage = memoryStorage();
    const store = createSearchStore(storage);
    store.search("  rerun me  ", "alpha");
    searchService.getApiV1SearchContent.mockResolvedValueOnce({ matches: [] });

    store.setMode("semantic");
    await flushMicrotasks();
    await vi.advanceTimersByTimeAsync(DEBOUNCE_MS + 10);

    expect(searchService.getApiV1Search).not.toHaveBeenCalled();
    expect(searchService.getApiV1SearchContent).toHaveBeenCalledOnce();
    expect(searchService.getApiV1SearchContent).toHaveBeenCalledWith({
      pattern: "rerun me",
      mode: "semantic",
      project: "alpha",
      limit: 120,
      xAgentsViewSearchIntent: "semantic",
      includeOneShot: true,
      includeAutomated: true,
    });
  });

  it("retry immediately reruns the active semantic query exactly once", async () => {
    const store = createSearchStore(
      memoryStorage({ [SEARCH_MODE_STORAGE_KEY]: "semantic" }),
    );
    searchService.getApiV1SearchContent.mockRejectedValueOnce(
      generatedApiError(503, "Embedding provider is temporarily unavailable"),
    );
    store.search("  try again  ", "alpha");
    await runDebounce();
    expect(store.error).not.toBeNull();

    vi.clearAllMocks();
    searchService.getApiV1SearchContent.mockResolvedValueOnce({ matches: [] });
    store.retry();
    await flushMicrotasks();
    await vi.advanceTimersByTimeAsync(DEBOUNCE_MS + 10);

    expect(searchService.getApiV1SearchContent).toHaveBeenCalledOnce();
    expect(searchService.getApiV1SearchContent).toHaveBeenCalledWith({
      pattern: "try again",
      mode: "semantic",
      project: "alpha",
      limit: 120,
      xAgentsViewSearchIntent: "semantic",
      includeOneShot: true,
      includeAutomated: true,
    });
    expect(searchService.getApiV1Search).not.toHaveBeenCalled();
  });

  it.each(["semantic", "hybrid"] as const)(
    "classifies a %s request timeout for friendly presentation",
    async (mode) => {
      const store = createSearchStore(
        memoryStorage({ [SEARCH_MODE_STORAGE_KEY]: mode }),
      );
      searchService.getApiV1SearchContent.mockRejectedValueOnce(
        generatedApiError(503, "request timed out"),
      );

      store.search("slow first query");
      await runDebounce();

      expect(store.error).toEqual({
        detail: "request timed out",
        kind: "timeout",
      });
    },
  );

  it("keeps unknown failures detail-less for localized presentation", async () => {
    const store = createSearchStore(memoryStorage());
    searchService.getApiV1Search.mockRejectedValueOnce({ reason: "offline" });

    store.search("unknown failure");
    await runDebounce();

    expect(store.error).toEqual({ detail: null, kind: "generic" });
    expect(store.isSearching).toBe(false);
  });

  it("stale requests cannot overwrite newer results, loading, or error", async () => {
    const store = createSearchStore(memoryStorage());
    const stale = deferred<SearchResponse>();
    searchService.getApiV1Search.mockReturnValueOnce(stale.promise as never);
    store.search("stale");
    await runDebounce();

    searchService.getApiV1SearchContent.mockResolvedValueOnce({
      matches: [contentMatch("newer", 8, 0.8)],
    });
    store.setMode("semantic");
    await flushMicrotasks();

    stale.reject(new Error("late stale failure"));
    await flushMicrotasks();

    expect(store.results.map((result) => result.session_id)).toEqual(["newer"]);
    expect(store.error).toBeNull();
    expect(store.isSearching).toBe(false);
  });

  it("clear cancels a generated request so it cannot write later", async () => {
    const store = createSearchStore(memoryStorage());
    let completeRequest!: (value: SearchResponse) => void;
    let wasCancelled = false;
    const request = new CancelablePromise<SearchResponse>((resolve, _reject, onCancel) => {
      completeRequest = resolve;
      onCancel(() => {
        wasCancelled = true;
      });
    });
    searchService.getApiV1Search.mockReturnValueOnce(request as never);

    store.search("cancel me");
    await runDebounce();
    store.clear();
    completeRequest(fullTextResponse("cancel me", 2));
    await flushMicrotasks();

    expect(wasCancelled).toBe(true);
    expect(store.results).toEqual([]);
    expect(store.query).toBe("");
    expect(store.isSearching).toBe(false);
  });
});
