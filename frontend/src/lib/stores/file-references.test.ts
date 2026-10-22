import { describe, expect, it, vi } from "vite-plus/test";
import { FileReferencesStore } from "./file-references.svelte.js";
import { imageOccurrence } from "../components/content/docbankTestHelpers.js";
const lookup = vi.hoisted(() => vi.fn());
vi.mock("../api/generated/sessions/sessions.js", () => ({
  getApiV1SessionsByIdFileReferences: lookup,
}));
const result = {
  integration: "enabled" as const,
  transcript_revision: "1",
  occurrences: [imageOccurrence],
};
const message = (content: string, ordinal = 0) => ({ ordinal, content, tool_calls: [] });

describe("file references store", () => {
  it("keeps unchanged messages while hiding changed messages during revalidation", async () => {
    lookup.mockReset().mockResolvedValueOnce({
      ...result,
      occurrences: [imageOccurrence, { ...imageOccurrence, message_ordinal: 1 }],
    });
    const store = new FileReferencesStore();
    store.enabled = true;
    await store.track("a", "1", [message("image"), message("old", 1)]);
    let resolve!: (value: unknown) => void;
    lookup.mockImplementationOnce(
      () =>
        new Promise((done) => {
          resolve = done;
        }),
    );
    const pending = store.track("a", "2", [
      message("image"),
      message("changed", 1),
      message("appended", 2),
    ]);
    expect(store.forMessage("a", 0)).toHaveLength(1);
    expect(store.forMessage("a", 1)).toEqual([]);
    expect(store.forMessage("a", 2)).toEqual([]);
    resolve({
      ...result,
      transcript_revision: "2",
      occurrences: [{ ...imageOccurrence, message_ordinal: 1 }],
    });
    await pending;
    expect(store.forMessage("a", 0)).toEqual([]);
    expect(store.forMessage("a", 1)).toHaveLength(1);
  });
  it("hides rows when tool result content changes", async () => {
    lookup.mockReset().mockResolvedValueOnce({
      ...result,
      occurrences: [{ ...imageOccurrence, tool_use_id: "read" }],
    });
    const store = new FileReferencesStore();
    store.enabled = true;
    await store.track("a", "1", [
      {
        ...message("image"),
        tool_calls: [
          { tool_name: "Read", category: "read", tool_use_id: "read", result_content: "old" },
        ],
      },
    ]);
    lookup.mockImplementationOnce(() => new Promise(() => {}));
    void store.track("a", "2", [
      {
        ...message("image"),
        tool_calls: [
          { tool_name: "Read", category: "read", tool_use_id: "read", result_content: "new" },
        ],
      },
    ]);
    expect(store.forTool("a", 0, "read", null)).toEqual([]);
    store.reset();
  });
  it("rejects lookup results from a different transcript revision", async () => {
    lookup.mockReset().mockResolvedValueOnce({ ...result, transcript_revision: "2" });
    const store = new FileReferencesStore();
    store.enabled = true;
    await store.track("a", "1", [message("old")]);
    expect(store.forMessage("a", 0)).toEqual([]);
    expect(store.status).toBe("failed");
  });
  it("ignores superseded requests and resets on session changes", async () => {
    let resolve!: (value: unknown) => void;
    lookup.mockReset().mockImplementationOnce(
      () =>
        new Promise((done) => {
          resolve = done;
        }),
    );
    const store = new FileReferencesStore();
    store.enabled = true;
    const old = store.track("a", "1");
    const signal = lookup.mock.calls[0]![1]!.signal;
    lookup.mockResolvedValueOnce({ ...result, transcript_revision: "2" });
    await store.track("a", "2");
    expect(signal.aborted).toBe(true);
    resolve(result);
    await old;
    expect(store.result?.transcript_revision).toBe("2");
    lookup.mockResolvedValueOnce(result);
    const next = store.track("b", "1");
    expect(store.result).toBeUndefined();
    await next;
    expect(store.forMessage("a", 0)).toEqual([]);
  });
  it("retries failures", async () => {
    lookup.mockReset().mockRejectedValueOnce(new Error("offline"));
    const store = new FileReferencesStore();
    store.enabled = true;
    await store.track("a", "1");
    expect(store.status).toBe("failed");
    lookup.mockResolvedValueOnce(result);
    await store.refresh();
    expect(store.status).toBe("ok");
  });
  it("makes no request when disabled", async () => {
    lookup.mockReset();
    const store = new FileReferencesStore();
    await store.track("a", "1");
    expect(lookup).not.toHaveBeenCalled();
  });
});
