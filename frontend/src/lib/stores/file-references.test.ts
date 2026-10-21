import { describe, expect, it, vi } from "vite-plus/test";
import { SessionFileReferencesStore } from "./file-references.svelte.js";
import type { ServiceFileReferencesResult as Result } from "../api/generated/index.js";

import {
  fileMatch,
  fileOccurrence,
  fileReferencesResult as response,
} from "../test/file-evidence.js";
const occurrence = (state = "deferred") => fileOccurrence({ state });
function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((r) => {
    resolve = r;
  });
  return { promise, resolve };
}

describe("session file references", () => {
  it.each([false, true])(
    "looks up only new ordinals beside an overlapping window, in-flight=%s",
    async (inFlight) => {
      const pending = deferred<Result>();
      const fetch = vi.fn().mockResolvedValue(response());
      if (inFlight) fetch.mockReturnValueOnce(pending.promise);
      const store = new SessionFileReferencesStore(fetch);
      store.setSession("s1", "b1");
      const first = store.loadWindow(5, 9);
      if (inFlight) {
        await Promise.resolve();
        await Promise.resolve();
      } else await first;
      const rest = store.loadWindow(0, 14);
      pending.resolve(response());
      await Promise.all([first, rest]);
      expect(fetch.mock.calls.map((call) => [call[1].first_ordinal, call[1].last_ordinal])).toEqual(
        [
          [5, 9],
          [0, 4],
          [10, 14],
        ],
      );
      await store.loadWindow(2, 12);
      expect(fetch).toHaveBeenCalledTimes(3);
    },
  );

  it("coalesces visible windows and sends binding, intent and cancellation through the generated client", async () => {
    const fetch = vi.fn().mockResolvedValue(response());
    const store = new SessionFileReferencesStore(fetch);
    store.setSession("s1", "b1");
    await Promise.all([store.loadWindow(0, 5), store.loadWindow(4, 9), store.loadWindow(2, 6)]);
    expect(fetch).toHaveBeenCalledTimes(1);
    expect(fetch.mock.calls[0]).toEqual([
      { id: "s1" },
      { binding: "b1", first_ordinal: 0, last_ordinal: 9, offset: 0, limit: 20 },
      { signal: expect.any(AbortSignal), headers: { "X-AgentsView-Lookup-Intent": "docbank" } },
    ]);
    expect(store.occurrences.get("o1")?.state).toBe("deferred");
    await store.loadWindow(1, 8);
    expect(fetch).toHaveBeenCalledTimes(1);
  });
  it.each(["navigation", "binding", "dispose"])(
    "aborts %s and ignores late success",
    async (change) => {
      const pending = deferred<Result>();
      const fetch = vi.fn().mockReturnValue(pending.promise);
      const store = new SessionFileReferencesStore(fetch);
      store.setSession("s1", "b1");
      const loading = store.loadWindow(0, 9);
      await Promise.resolve();
      await Promise.resolve();
      if (change === "dispose") store.dispose();
      else store.setSession(change === "navigation" ? "s2" : "s1", "b2");
      expect(fetch.mock.calls[0]![2].signal.aborted).toBe(true);
      pending.resolve(response());
      await loading;
      expect(store.occurrences.size).toBe(0);
      expect(store.integration).toBe("");
    },
  );
  it("refreshes the deferred occurrence instead of repeating successful lookups", async () => {
    const fetch = vi
      .fn()
      .mockResolvedValueOnce(response())
      .mockResolvedValue(response({ occurrences: [occurrence("found")] }));
    const store = new SessionFileReferencesStore(fetch);
    store.setSession("s1", "b1");
    await store.loadWindow(0, 9);
    await store.checkRemaining();
    expect(fetch.mock.calls[1]![1]).toEqual({
      binding: "b1",
      occurrence_id: "o1",
      limit: 20,
      refresh: true,
    });
    expect(store.occurrences.get("o1")?.state).toBe("found");
  });
  it("paginates only the selected occurrence at the raw remote offset", async () => {
    const match = fileMatch({ name: "one" });
    const fetch = vi
      .fn()
      .mockResolvedValueOnce(
        response({
          occurrences: [
            {
              ...occurrence("found"),
              matches: [match],
              reference_total: 102,
              next_reference_offset: 100,
            },
          ],
        }),
      )
      .mockResolvedValue(
        response({
          occurrences: [
            {
              ...occurrence("found"),
              matches: [match, { ...match, node_id: 2 }],
              reference_total: 102,
            },
          ],
        }),
      );
    const store = new SessionFileReferencesStore(fetch);
    store.setSession("s1", "b1");
    await store.loadWindow(0, 9);
    await store.loadMore("o1");
    expect(fetch.mock.calls[1]![1]).toEqual({
      binding: "b1",
      occurrence_id: "o1",
      reference_offset: 100,
      limit: 20,
    });
    expect(store.occurrences.get("o1")?.matches).toHaveLength(2);
  });
  it("reaches later occurrences despite a persistent first-page failure", async () => {
    const firstPage = Array.from({ length: 20 }, (_, i) =>
      fileOccurrence({
        occurrence_id: `o${i}`,
        message_ordinal: i,
        state: i === 0 ? "unavailable" : "found",
      }),
    );
    const last = fileOccurrence({ occurrence_id: "o20", message_ordinal: 20, state: "found" });
    const fetch = vi.fn(async (_path, params) => {
      if (params.occurrence_id === "o0")
        return response({ total: 21, occurrences: [firstPage[0]!] });
      if (params.offset === 20) return response({ total: 21, occurrences: [last] });
      return response({ total: 21, next_offset: 20, occurrences: firstPage });
    });
    const store = new SessionFileReferencesStore(fetch);
    store.setSession("s1", "b1");
    await store.loadWindow(0, 20);
    await store.checkRemaining();
    expect(store.occurrences.get("o20")?.state).toBe("found");
    expect(fetch.mock.calls[1]![1]).toEqual({
      binding: "b1",
      first_ordinal: 0,
      last_ordinal: 20,
      offset: 20,
      limit: 20,
    });
    await store.checkRemaining();
    expect(fetch.mock.calls[2]![1].occurrence_id).toBe("o0");
    expect(store.canCheckRemaining).toBe(true);
  });
  it("preserves the continuation offset when an additional page is deferred", async () => {
    const match = fileMatch();
    const fetch = vi
      .fn()
      .mockResolvedValueOnce(
        response({
          occurrences: [
            fileOccurrence({
              state: "found",
              matches: [match],
              reference_total: 102,
              next_reference_offset: 100,
            }),
          ],
        }),
      )
      .mockResolvedValueOnce(response({ occurrences: [fileOccurrence({ state: "deferred" })] }))
      .mockResolvedValue(
        response({
          occurrences: [
            fileOccurrence({
              state: "found",
              matches: [{ ...match, node_id: 2, name: "second" }],
              reference_total: 102,
              next_reference_offset: null,
            }),
          ],
        }),
      );
    const store = new SessionFileReferencesStore(fetch);
    store.setSession("s1", "b1");
    await store.loadWindow(0, 9);
    await store.loadMore("o1");
    expect(store.occurrences.get("o1")?.next_reference_offset).toBe(100);
    await store.loadMore("o1");
    expect(fetch.mock.calls[2]![1].reference_offset).toBe(100);
    expect(store.occurrences.get("o1")?.matches).toHaveLength(2);
  });
  it("retries later windows despite a persistent failure in the first window", async () => {
    const first = fileOccurrence({
      occurrence_id: "first",
      message_ordinal: 0,
      state: "unavailable",
    });
    const later = fileOccurrence({
      occurrence_id: "later",
      message_ordinal: 10,
      state: "deferred",
    });
    const fetch = vi.fn(async (_path, params) => {
      if (params.occurrence_id === "later")
        return response({ occurrences: [{ ...later, state: "found" }] });
      if (params.occurrence_id === "first" || params.first_ordinal === 0)
        return response({ occurrences: [first] });
      return response({ occurrences: [later] });
    });
    const store = new SessionFileReferencesStore(fetch);
    store.setSession("s1", "b1");
    await store.loadWindow(0, 9);
    await store.loadWindow(10, 19);
    await store.checkRemaining();
    await store.checkRemaining();
    expect(fetch.mock.calls.slice(2).map((call) => call[1])).toEqual([
      { binding: "b1", occurrence_id: "first", limit: 20, refresh: true },
      { binding: "b1", occurrence_id: "later", limit: 20, refresh: true },
    ]);
    expect(store.occurrences.get("later")?.state).toBe("found");
    expect(store.occurrences.get("first")?.state).toBe("unavailable");
    await store.checkRemaining();
    expect(fetch.mock.calls[4]![1].occurrence_id).toBe("first");
    expect(store.canCheckRemaining).toBe(true);
  });
  it("refreshes unresolved occurrences one at a time and advances across deferred items", async () => {
    const first = fileOccurrence({ occurrence_id: "o1", message_ordinal: 0, state: "found" });
    const second = fileOccurrence({ occurrence_id: "o2", message_ordinal: 1, state: "deferred" });
    const third = fileOccurrence({ occurrence_id: "o3", message_ordinal: 2, state: "unavailable" });
    const fetch = vi
      .fn()
      .mockResolvedValueOnce(response({ total: 3, occurrences: [first, second, third] }))
      .mockResolvedValueOnce(response({ occurrences: [second] }))
      .mockResolvedValueOnce(response({ occurrences: [{ ...third, state: "found" }] }));
    const store = new SessionFileReferencesStore(fetch);
    store.setSession("s1", "b1");
    await store.loadWindow(0, 9);
    await store.checkRemaining();
    expect(fetch.mock.calls[1]![1]).toEqual({
      binding: "b1",
      occurrence_id: "o2",
      limit: 20,
      refresh: true,
    });
    await store.checkRemaining();
    expect(fetch.mock.calls[2]![1]).toEqual({
      binding: "b1",
      occurrence_id: "o3",
      limit: 20,
      refresh: true,
    });
    expect(store.canCheckRemaining).toBe(true);
  });
  it("does not poll outages, hides disabled remote state, and rejects a stale response binding", async () => {
    const fetch = vi.fn().mockRejectedValue(new Error("offline"));
    const store = new SessionFileReferencesStore(fetch);
    store.setSession("s1", "b1");
    await store.loadWindow(0, 9);
    expect(store.unavailable).toBe(true);
    expect(store.canCheckRemaining).toBe(true);
    expect(fetch).toHaveBeenCalledTimes(1);
    fetch.mockResolvedValue(response({ integration: "disabled" }));
    await store.checkRemaining();
    expect(store.integration).toBe("disabled");
    expect(store.unavailable).toBe(false);
    store.setSession("s1", "b2");
    await store.loadWindow(0, 9);
    expect(store.occurrences.size).toBe(0);
  });
});

it("queues newly visible ranges behind a held request without duplicate window work", async () => {
  const held = deferred<Result>();
  const fetch = vi.fn().mockReturnValueOnce(held.promise).mockResolvedValue(response());
  const store = new SessionFileReferencesStore(fetch);
  store.setSession("s1", "b1");
  const first = store.loadWindow(0, 9);
  await Promise.resolve();
  await Promise.resolve();
  const second = store.loadWindow(8, 19);
  const duplicate = store.loadWindow(10, 18);
  expect(fetch).toHaveBeenCalledTimes(1);
  held.resolve(response());
  await Promise.all([first, second, duplicate]);
  expect(fetch).toHaveBeenCalledTimes(2);
  expect(fetch.mock.calls[1]![1].last_ordinal).toBe(19);
});
it("serializes a new window behind a deliberate retry and clears on a stale archive response", async () => {
  const held = deferred<Result>();
  const fetch = vi
    .fn()
    .mockResolvedValueOnce(response())
    .mockReturnValueOnce(held.promise)
    .mockRejectedValue({ status: 409 });
  const store = new SessionFileReferencesStore(fetch);
  store.setSession("s1", "b1");
  await store.loadWindow(0, 9);
  const retry = store.checkRemaining();
  const next = store.loadWindow(20, 29);
  await Promise.resolve();
  expect(fetch).toHaveBeenCalledTimes(2);
  held.resolve(response());
  await Promise.all([retry, next]);
  expect(store.occurrences.size).toBe(0);
});
