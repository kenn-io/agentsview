import { describe, expect, it, vi } from "vite-plus/test";
import { FileReferencesStore } from "./file-references.svelte.js";
const lookup = vi.hoisted(() => vi.fn());
vi.mock("../api/generated/sessions/sessions.js", () => ({ getApiV1SessionsByIdFileReferences: lookup }));

describe("file references store", () => {
  it("shares in-flight and settled lookups and refreshes on request", async () => {
    lookup.mockReset();
    let resolve!: (value: unknown) => void;
    lookup.mockImplementationOnce(() => new Promise((done) => { resolve = done; }));
    const store = new FileReferencesStore();
    const first = store.load("session-a");
    expect(store.load("session-a")).toBe(first);
    expect(lookup).toHaveBeenCalledExactlyOnceWith({ id: "session-a" });
    resolve({ integration: "enabled", occurrences: [] });
    await first;
    await store.load("session-a");
    expect(lookup).toHaveBeenCalledTimes(1);
    expect(store.entries.get("session-a")?.result?.integration).toBe("enabled");
    lookup.mockRejectedValueOnce(new Error("offline"));
    await store.refresh("session-a");
    expect(store.entries.get("session-a")?.failed).toBe(true);
    expect(lookup).toHaveBeenCalledTimes(2);
  });
});
