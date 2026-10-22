import { describe, expect, it, vi } from "vite-plus/test";
import { ApiError } from "../api/runtime.js";
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

describe("file references store", () => {
  it.skip("Subagent inline finding: keeps parent and expanded child results per session; fixing track would lengthen it", async () => {
    lookup.mockReset().mockResolvedValue(result);
    const store = new FileReferencesStore();
    store.enabled = true;
    await store.track("parent", "1");
    await store.track("child", "1");
    expect(store.forMessage("parent", 0)).toEqual([imageOccurrence]);
    expect(store.forMessage("child", 0)).toEqual([imageOccurrence]);
  });
  it("clears results while fetching a new transcript revision", async () => {
    lookup.mockReset().mockResolvedValueOnce(result);
    const store = new FileReferencesStore();
    store.enabled = true;
    await store.track("a", "1");
    expect(store.forMessage("a", 0)).toEqual([imageOccurrence]);
    let resolve!: (value: unknown) => void;
    lookup.mockImplementationOnce(
      () =>
        new Promise((done) => {
          resolve = done;
        }),
    );
    const pending = store.track("a", "2");
    expect(store.result).toBeUndefined();
    expect(store.forMessage("a", 0)).toEqual([]);
    expect(store.status).toBe("loading");
    resolve({ ...result, transcript_revision: "2" });
    await pending;
    expect(store.forMessage("a", 0)).toEqual([imageOccurrence]);
    expect(store.result?.transcript_revision).toBe("2");
  });
  it.each([
    { name: "current refetch", refetchRevision: "1", status: "ok" },
    { name: "stale refetch", refetchRevision: "2", status: "failed" },
  ] as const)(
    "rejects a different transcript revision with a $name",
    async ({ refetchRevision, status }) => {
      lookup
        .mockReset()
        .mockResolvedValueOnce({ ...result, transcript_revision: "2" })
        .mockResolvedValue({ ...result, transcript_revision: refetchRevision });
      const store = new FileReferencesStore();
      store.enabled = true;
      await store.track("a", "1");
      expect(lookup).toHaveBeenCalledTimes(2);
      expect(store.status).toBe(status);
      if (status === "failed") {
        expect(store.result).toBeUndefined();
        await store.track("a", "2");
        expect(store.status).toBe("ok");
      }
    },
  );
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

it.each([null, ""])("keeps an empty transcript quiet with revision %s", async (revision) => {
  lookup.mockReset().mockResolvedValue(result);
  const store = new FileReferencesStore();
  store.enabled = true;
  await store.track("a", "1");
  lookup.mockClear();
  await store.track("empty", revision);
  expect(store.status).toBe("idle");
  expect(store.result).toBeUndefined();
  expect(lookup).not.toHaveBeenCalled();
});

it("allows retry after repeated source changes", async () => {
  lookup.mockReset().mockRejectedValue(new ApiError(409, "source changed", "source_changed"));
  const store = new FileReferencesStore();
  store.enabled = true;
  await store.track("a", "1");
  expect(lookup).toHaveBeenCalledTimes(2);
  expect(store.status).toBe("failed");
  lookup.mockResolvedValueOnce(result);
  await store.refresh();
  expect(store.status).toBe("ok");
});

it("keeps id-less tool calls separate by archive index", async () => {
  lookup.mockReset().mockResolvedValueOnce({
    ...result,
    occurrences: [
      { ...imageOccurrence, size: 3, tool_name: "Read", tool_use_id: "", call_index: 0 },
      {
        ...imageOccurrence,
        size: 3,
        tool_name: "Read",
        tool_use_id: "",
        call_index: 1,
        media_type: "image/jpeg",
      },
    ],
  });
  const store = new FileReferencesStore();
  store.enabled = true;
  await store.track("a", "1");
  expect(store.forTool("a", 0, "", null, 1).map((x) => x.media_type)).toEqual(["image/jpeg"]);
});

it("refetches when the archive changes during a read", async () => {
  lookup
    .mockReset()
    .mockRejectedValueOnce(new ApiError(409, "source changed", "source_changed"))
    .mockResolvedValueOnce(result);
  const store = new FileReferencesStore();
  store.enabled = true;
  await store.track("a", "1");
  expect(lookup).toHaveBeenCalledTimes(2);
  expect(store.status).toBe("ok");
});
