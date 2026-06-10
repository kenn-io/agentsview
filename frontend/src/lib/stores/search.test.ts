import { describe, it, expect, vi, beforeEach, afterEach } from "vite-plus/test";
import { searchStore } from "./search.svelte.js";
import { SearchService } from "../api/generated/index";
import type { SearchResponse } from "../api/types.js";

vi.mock("../api/runtime.js", () => ({
  configureGeneratedClient: vi.fn(),
  callGenerated: vi.fn((request: () => Promise<unknown>) => request()),
  isAbortError: (err: unknown) => {
    if (err instanceof DOMException && err.name === "AbortError") {
      return true;
    }
    if (err === null || typeof err !== "object") {
      return false;
    }
    const candidate = err as {
      isCancelled?: unknown;
      name?: unknown;
    };
    return candidate.isCancelled === true ||
      candidate.name === "CancelError";
  },
  withAbort: async <T>(promise: Promise<T> & { cancel?: () => void }, signal?: AbortSignal) => {
    if (signal) {
      if (signal.aborted) {
        promise.cancel?.();
      } else {
        signal.addEventListener("abort", () => promise.cancel?.(), {
          once: true,
        });
      }
    }
    return promise;
  },
}));

vi.mock("../api/generated/index", () => ({
  SearchService: {
    getApiV1Search: vi.fn(),
  },
}));

const searchService = SearchService as unknown as {
  getApiV1Search: ReturnType<typeof vi.fn>;
};

function createDeferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((r) => {
    resolve = r;
  });
  return { promise, resolve };
}

function makeSearchResponse(
  query: string,
  count: number,
): SearchResponse {
  return {
    query,
    results: Array.from({ length: count }, (_, i) => ({
      session_id: `s${i}`,
      project: "proj",
      agent: "claude",
      name: "session name",
      ordinal: i,
      session_ended_at: new Date().toISOString(),
      snippet: `result ${i}`,
      rank: i,
    })),
    count,
    next: 0,
  };
}

function generatedCancelError(): Error & { isCancelled: true } {
  const err = new Error("Request aborted") as Error & {
    isCancelled: true;
  };
  err.name = "CancelError";
  err.isCancelled = true;
  return err;
}

function cancelableNever<T>(): Promise<T> & { cancel: () => void } {
  let reject!: (reason?: unknown) => void;
  const promise = new Promise<T>((_resolve, r) => {
    reject = r;
  }) as Promise<T> & { cancel: () => void };
  promise.cancel = () => reject(generatedCancelError());
  return promise;
}

/** Flush multiple microtask ticks for async chains + reactivity. */
async function flushMicrotasks(ticks = 4) {
  for (let i = 0; i < ticks; i++) {
    await Promise.resolve();
  }
}

const DEBOUNCE_MS = 300;

describe("SearchStore", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    searchStore.clear();
    searchStore.resetSort();
    vi.clearAllMocks();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it("should abort stale in-flight search when a new one starts", async () => {
    // First search: slow, will be aborted
    searchService.getApiV1Search.mockReturnValueOnce(cancelableNever());

    // Second search: resolves immediately
    searchService.getApiV1Search.mockResolvedValueOnce(
      makeSearchResponse("world", 2),
    );

    // Trigger first search
    searchStore.search("hello");
    vi.advanceTimersByTime(DEBOUNCE_MS);

    // Trigger second search (aborts first)
    searchStore.search("world");
    vi.advanceTimersByTime(DEBOUNCE_MS);

    // Wait for all async work
    await vi.runAllTimersAsync();
    await Promise.resolve();

    // Results should be from the second search
    expect(searchStore.results.length).toBe(2);
    expect(searchStore.isSearching).toBe(false);
  });

  it("should abort in-flight search on clear()", async () => {
    searchService.getApiV1Search.mockReturnValueOnce(cancelableNever());

    // Trigger search
    searchStore.search("hello");
    vi.advanceTimersByTime(DEBOUNCE_MS);

    // Clear while search is in-flight
    searchStore.clear();
    await Promise.resolve();

    // Results should remain empty after clear
    expect(searchStore.results.length).toBe(0);
    expect(searchStore.query).toBe("");
    expect(searchStore.isSearching).toBe(false);
  });

  it("should debounce rapid queries and only fire the last one", async () => {
    searchService.getApiV1Search.mockResolvedValueOnce(
      makeSearchResponse("final", 3),
    );

    // Type several queries within debounce window
    searchStore.search("f");
    vi.advanceTimersByTime(100);
    searchStore.search("fi");
    vi.advanceTimersByTime(100);
    searchStore.search("final");
    vi.advanceTimersByTime(DEBOUNCE_MS);

    await vi.runAllTimersAsync();
    await Promise.resolve();

    // Only one API call should have been made (for "final")
    expect(searchService.getApiV1Search).toHaveBeenCalledTimes(1);
    expect(searchService.getApiV1Search).toHaveBeenCalledWith(
      expect.objectContaining({ q: "final", limit: 30 }),
    );
    expect(searchStore.results.length).toBe(3);
  });

  it("should clear results immediately for empty query", () => {
    // Manually set some results first
    searchStore.search("test");
    // Now clear via empty query
    searchStore.search("");

    expect(searchStore.results.length).toBe(0);
    expect(searchStore.isSearching).toBe(false);
    // No API call should be made for empty query
    vi.advanceTimersByTime(DEBOUNCE_MS);
    expect(searchService.getApiV1Search).not.toHaveBeenCalled();
  });

  it("should keep isSearching true while a newer search is pending", async () => {
    // First search: aborted when second starts
    searchService.getApiV1Search.mockReturnValueOnce(cancelableNever());

    // Second search: hangs until resolved
    const secondReq = createDeferred<SearchResponse>();
    searchService.getApiV1Search.mockReturnValueOnce(secondReq.promise);

    // Trigger first search
    searchStore.search("first");
    vi.advanceTimersByTime(DEBOUNCE_MS);

    // Trigger second search (aborts first)
    searchStore.search("second");
    vi.advanceTimersByTime(DEBOUNCE_MS);
    await Promise.resolve();

    // isSearching should be true (second search is in-flight)
    expect(searchStore.isSearching).toBe(true);

    // Resolve second search
    secondReq.resolve(makeSearchResponse("second", 2));
    await flushMicrotasks();

    expect(searchStore.isSearching).toBe(false);
    expect(searchStore.results.length).toBe(2);
  });

  it("should keep previous results when generated cancellation aborts stale search", async () => {
    searchService.getApiV1Search.mockResolvedValueOnce(
      makeSearchResponse("stable", 3),
    );

    searchStore.search("stable");
    vi.advanceTimersByTime(DEBOUNCE_MS);
    await vi.runAllTimersAsync();
    await flushMicrotasks();

    expect(searchStore.results.length).toBe(3);

    searchService.getApiV1Search.mockReturnValueOnce(cancelableNever());

    searchStore.search("slow");
    vi.advanceTimersByTime(DEBOUNCE_MS);
    await flushMicrotasks();

    searchStore.search("next");
    await flushMicrotasks();

    expect(searchStore.results.length).toBe(3);
  });

  it("should discard results from request that resolves during debounce window", async () => {
    const firstReq = createDeferred<SearchResponse>();

    // First search: resolves after query changes but before debounce fires
    const firstPromise = firstReq.promise as Promise<SearchResponse> & {
      cancel: () => void;
    };
    firstPromise.cancel = () => undefined;
    searchService.getApiV1Search.mockReturnValueOnce(firstPromise);

    // Second search: resolves immediately
    searchService.getApiV1Search.mockResolvedValueOnce(
      makeSearchResponse("beta", 2),
    );

    // Fire first search
    searchStore.search("alpha");
    vi.advanceTimersByTime(DEBOUNCE_MS);

    // Query changes to "beta" — this aborts the first request
    // immediately, before the debounce fires
    searchStore.search("beta");

    // First request tries to resolve during the debounce window
    // but its signal is already aborted
    firstReq.resolve(makeSearchResponse("alpha", 5));
    await Promise.resolve();

    // Alpha results must not appear
    expect(searchStore.results.length).toBe(0);

    // Now let the debounce fire for "beta"
    vi.advanceTimersByTime(DEBOUNCE_MS);
    await vi.runAllTimersAsync();
    await Promise.resolve();

    // Results should be from "beta"
    expect(searchStore.results.length).toBe(2);
    expect(searchStore.isSearching).toBe(false);
  });

  it("should call generated search with query params", async () => {
    searchService.getApiV1Search.mockResolvedValueOnce(
      makeSearchResponse("test", 1),
    );

    searchStore.search("test");
    vi.advanceTimersByTime(DEBOUNCE_MS);

    await vi.runAllTimersAsync();
    await Promise.resolve();

    expect(searchService.getApiV1Search).toHaveBeenCalledWith(
      { q: "test", project: undefined, limit: 30, sort: "relevance" },
    );
  });

  it("sort defaults to relevance", () => {
    expect(searchStore.sort).toBe("relevance");
  });

  it("setSort updates sort state", () => {
    searchStore.setSort("recency");
    expect(searchStore.sort).toBe("recency");
    searchStore.setSort("relevance");
    expect(searchStore.sort).toBe("relevance");
  });

  it("setSort re-runs search when query is active", async () => {
    searchService.getApiV1Search
      .mockResolvedValueOnce(makeSearchResponse("hello", 2))
      .mockResolvedValueOnce(makeSearchResponse("hello", 1));

    // Run first search
    searchStore.search("hello");
    vi.advanceTimersByTime(DEBOUNCE_MS);
    await vi.runAllTimersAsync();
    await Promise.resolve();

    expect(searchStore.results.length).toBe(2);

    // Switch sort — should trigger a new search immediately
    searchStore.setSort("recency");
    await vi.runAllTimersAsync();
    await Promise.resolve();

    expect(searchService.getApiV1Search).toHaveBeenCalledTimes(2);
    expect(searchService.getApiV1Search).toHaveBeenLastCalledWith(
      expect.objectContaining({ q: "hello", sort: "recency" }),
    );
    expect(searchStore.results.length).toBe(1);
  });

  it("setSort does nothing when no query is active", () => {
    searchStore.clear();
    searchStore.setSort("recency");
    expect(searchService.getApiV1Search).not.toHaveBeenCalled();
  });

  it("clear() does not reset sort (sort persists within a palette session)", () => {
    searchStore.setSort("recency");
    expect(searchStore.sort).toBe("recency");
    searchStore.clear();
    expect(searchStore.sort).toBe("recency");
  });

  it("resetSort() resets sort to relevance", () => {
    searchStore.setSort("recency");
    expect(searchStore.sort).toBe("recency");
    searchStore.resetSort();
    expect(searchStore.sort).toBe("relevance");
  });

  it("setSort cancels pending debounced search before running", async () => {
    searchService.getApiV1Search.mockResolvedValue(makeSearchResponse("hello", 1));

    // Start a search but don't let the debounce fire yet
    searchStore.search("hello");
    // Immediately switch sort — should cancel the queued debounce
    searchStore.setSort("recency");
    // Advance timers past debounce window
    vi.advanceTimersByTime(DEBOUNCE_MS + 100);
    await vi.runAllTimersAsync();
    await Promise.resolve();

    // Only the immediate setSort call should have fired, not the queued debounce
    expect(searchService.getApiV1Search).toHaveBeenCalledTimes(1);
    expect(searchService.getApiV1Search).toHaveBeenCalledWith(
      expect.objectContaining({ q: "hello", sort: "recency" }),
    );
  });
});
