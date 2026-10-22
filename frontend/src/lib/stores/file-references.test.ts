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
    lookup.mockResolvedValueOnce({ ...result, source: "unavailable" });
    await store.track("a", "3");
    resolve(result);
    await old;
    expect(store.result?.source).toBe("unavailable");
    await store.track("a", "3");
    expect(lookup).toHaveBeenCalledTimes(3);
    expect(lookup).toHaveBeenLastCalledWith({ id: "a" });
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
    expect(lookup).toHaveBeenLastCalledWith({ id: "b" });
  });
  it("makes no request when disabled", async () => {
    lookup.mockReset();
    const store = new FileReferencesStore();
    await store.track("a", "1");
    expect(lookup).not.toHaveBeenCalled();
  });
});
