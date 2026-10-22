import { describe, expect, it, vi } from "vite-plus/test";
import { FileReferencesStore } from "./file-references.svelte.js";
const lookup = vi.hoisted(() => vi.fn());
vi.mock("../api/generated/sessions/sessions.js", () => ({
  getApiV1SessionsByIdFileReferences: lookup,
}));
const result = { integration: "enabled" as const, occurrences: [] };

describe("file references store", () => {
  it("refetches revisions while retaining the last result and ignores older requests", async () => {
    lookup.mockReset().mockResolvedValueOnce(result);
    const store = new FileReferencesStore();
    store.enabled = true;
    await store.track("a", "1");
    let resolve!: (value: unknown) => void;
    lookup.mockImplementationOnce(
      () =>
        new Promise((done) => {
          resolve = done;
        }),
    );
    const old = store.track("a", "2");
    expect(store.result).toEqual(result);
    expect(store.status).toBe("loading");
    const latest = { ...result, lookup_failed: true };
    lookup.mockResolvedValueOnce(latest);
    await store.track("a", "3");
    resolve(result);
    await old;
    expect(store.result).toEqual(latest);
    await store.track("a", "3");
    expect(lookup).toHaveBeenCalledTimes(3);
    expect(lookup).toHaveBeenLastCalledWith({ id: "a" }, { signal: expect.any(AbortSignal) });
  });
  it("retries failures and clears results when switching sessions", async () => {
    lookup.mockReset().mockRejectedValueOnce(new Error("offline"));
    const store = new FileReferencesStore();
    store.enabled = true;
    await store.track("a", "1");
    expect(store.status).toBe("failed");
    lookup.mockResolvedValueOnce(result);
    await store.refresh();
    expect(store.status).toBe("ok");
    lookup.mockResolvedValueOnce(result);
    const next = store.track("b", "1");
    expect(store.result).toBeUndefined();
    await next;
    expect(store.forMessage("a", 0)).toEqual([]);
    expect(lookup).toHaveBeenLastCalledWith({ id: "b" }, { signal: expect.any(AbortSignal) });
  });
  it("makes no request when disabled", async () => {
    lookup.mockReset();
    const store = new FileReferencesStore();
    await store.track("a", "1");
    expect(lookup).not.toHaveBeenCalled();
  });
});

it("cancels superseded requests", async () => {
  lookup.mockReset().mockImplementationOnce((_params, { signal }) =>
    new Promise((_resolve, reject) => signal.addEventListener("abort", () => reject(new Error("aborted")))),
  ).mockResolvedValue(result);
  const store = new FileReferencesStore();
  store.enabled = true;
  const pending = store.track("a", "1");
  const options = lookup.mock.calls[0]![1]!;
  expect(options?.signal).toBeInstanceOf(AbortSignal);
  await store.track("a", "2");
  await pending;
  expect(store.status).toBe("ok");
  expect(options.signal.aborted).toBe(true);
  expect(lookup.mock.calls[1]![1]!.signal.aborted).toBe(false);
});
