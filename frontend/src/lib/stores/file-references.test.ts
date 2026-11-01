import { describe, expect, it, vi } from "vite-plus/test";
import { ApiError } from "../api/runtime.js";
import { FileReferencesStore, MIN_REFRESH_INTERVAL_MS } from "./file-references.svelte.js";
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
  it("keeps results on screen and spaces refreshes of a growing transcript", async () => {
    vi.useFakeTimers();
    try {
      lookup.mockReset().mockResolvedValueOnce(result);
      const store = new FileReferencesStore();
      store.enabled = true;
      await store.track("a", "1");
      expect(lookup).toHaveBeenCalledTimes(1);

      lookup.mockResolvedValue({ ...result, transcript_revision: "3" });
      await store.track("a", "2");
      await store.track("a", "3");
      expect(lookup).toHaveBeenCalledTimes(1);
      expect(store.forMessage("a", 0)).toEqual([imageOccurrence]);
      expect(store.status).toBe("ok");

      // One refresh for the latest revision once the interval has passed.
      await vi.advanceTimersByTimeAsync(MIN_REFRESH_INTERVAL_MS);
      expect(lookup).toHaveBeenCalledTimes(2);
      expect(store.result?.transcript_revision).toBe("3");
      expect(store.forMessage("a", 0)).toEqual([imageOccurrence]);

      // Leaving the session cancels a scheduled refresh.
      await store.track("a", "4");
      store.reset();
      await vi.advanceTimersByTimeAsync(MIN_REFRESH_INTERVAL_MS);
      expect(lookup).toHaveBeenCalledTimes(2);
    } finally {
      vi.useRealTimers();
    }
  });
  it.each([
    { name: "current refetch", refetchRevision: "1", status: "ok", sourceChanged: false },
    { name: "source_changed error", refetchRevision: "1", status: "ok", sourceChanged: true },
    { name: "stale refetch", refetchRevision: "2", status: "failed", sourceChanged: false },
  ] as const)(
    "refetches stale file references with a $name",
    async ({ refetchRevision, status, sourceChanged }) => {
      lookup.mockReset();
      if (sourceChanged) {
        lookup.mockRejectedValueOnce(new ApiError(409, "source changed", "source_changed"));
      } else {
        lookup.mockResolvedValueOnce({ ...result, transcript_revision: "2" });
      }
      lookup.mockResolvedValue({ ...result, transcript_revision: refetchRevision });
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
    expect(store.forMessage("b", 0)).toEqual([imageOccurrence]);
  });
  it.each([
    { name: "offline", error: new Error("offline"), requests: 1 },
    {
      name: "repeated source changes",
      error: new ApiError(409, "source changed", "source_changed"),
      requests: 2,
    },
  ])("retries failures after $name", async ({ error, requests }) => {
    lookup.mockReset().mockRejectedValue(error);
    const store = new FileReferencesStore();
    store.enabled = true;
    await store.track("a", "1");
    expect(store.status).toBe("failed");
    expect(lookup).toHaveBeenCalledTimes(requests);
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

it.each([
  [0, "image/png", "image/jpeg"],
  [1, "image/webp", "image/gif"],
] as const)("scopes reused tool IDs to message %s", async (ordinal, outputType, eventType) => {
  lookup.mockReset().mockResolvedValueOnce({
    ...result,
    occurrences: [
      {
        ...imageOccurrence,
        size: 3,
        tool_name: "Read",
        call_index: 0,
        tool_use_id: "reused",
        media_type: "image/png",
      },
      {
        ...imageOccurrence,
        size: 3,
        tool_name: "Read",
        call_index: 0,
        tool_use_id: "reused",
        event_index: 0,
        media_type: "image/jpeg",
      },
      {
        ...imageOccurrence,
        size: 3,
        tool_name: "Read",
        call_index: 0,
        message_ordinal: 1,
        tool_use_id: "reused",
        media_type: "image/webp",
      },
      {
        ...imageOccurrence,
        size: 3,
        tool_name: "Read",
        call_index: 0,
        message_ordinal: 1,
        tool_use_id: "reused",
        event_index: 0,
        media_type: "image/gif",
      },
    ],
  });
  const store = new FileReferencesStore();
  store.enabled = true;
  await store.track("a", "1");
  expect(store.forTool("a", ordinal, "reused", null).map((x) => x.media_type)).toEqual([
    outputType,
  ]);
  expect(store.forTool("a", ordinal, "reused", 0).map((x) => x.media_type)).toEqual([eventType]);
});

it("keeps empty tool IDs separate from null message IDs", async () => {
  lookup.mockReset().mockResolvedValueOnce({
    ...result,
    occurrences: [
      { ...imageOccurrence, media_type: "image/jpeg" },
      { ...imageOccurrence, tool_use_id: "", call_index: 0, media_type: "image/png" },
      {
        ...imageOccurrence,
        tool_use_id: "",
        call_index: 0,
        event_index: 0,
        media_type: "image/gif",
      },
    ],
  });
  const store = new FileReferencesStore();
  store.enabled = true;
  await store.track("a", "1");
  expect(store.forMessage("a", 0).map((x) => x.media_type)).toEqual(["image/jpeg"]);
  expect(store.forTool("a", 0, "", null).map((x) => x.media_type)).toEqual(["image/png"]);
  expect(store.forTool("a", 0, "", 0).map((x) => x.media_type)).toEqual(["image/gif"]);
});
