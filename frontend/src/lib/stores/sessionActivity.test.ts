import { describe, it, expect, vi, beforeEach } from "vite-plus/test";
import {
  findActiveBucketIndex,
  sessionActivity,
} from "./sessionActivity.svelte.js";
import { SessionsService } from "../api/generated/index";
import type { SessionActivityBucket } from "../api/types/session-activity.js";
import type { SessionActivityResponse } from "../api/types/session-activity.js";

vi.mock("../api/runtime.js", () => ({
  configureGeneratedClient: vi.fn(),
  callGenerated: vi.fn((request: () => Promise<unknown>) => request()),
}));

vi.mock("../api/generated/index", () => ({
  SessionsService: {
    getApiV1SessionsIdActivity: vi.fn(),
  },
}));

const sessionsService = SessionsService as unknown as {
  getApiV1SessionsIdActivity: ReturnType<typeof vi.fn>;
};

function bucket(
  start: string,
  end: string,
  firstOrdinal: number | null = 0,
): SessionActivityBucket {
  return {
    start_time: start,
    end_time: end,
    user_count: 1,
    assistant_count: 1,
    first_ordinal: firstOrdinal,
  };
}

function makeResponse(
  bucketCount: number,
): SessionActivityResponse {
  const buckets: SessionActivityBucket[] = [];
  for (let i = 0; i < bucketCount; i++) {
    buckets.push(
      bucket(
        `2026-03-26T${String(10 + i).padStart(2, "0")}:00:00Z`,
        `2026-03-26T${String(11 + i).padStart(2, "0")}:00:00Z`,
        i * 10,
      ),
    );
  }
  return {
    buckets,
    interval_seconds: 3600,
    total_messages: bucketCount * 5,
  };
}

function createDeferred<T>() {
  let resolve!: (value: T | PromiseLike<T>) => void;
  const promise = new Promise<T>((res) => {
    resolve = res;
  });
  return { promise, resolve };
}

describe("SessionActivityStore", () => {
  beforeEach(() => {
    sessionActivity.clear();
    vi.resetAllMocks();
  });

  it("ignores stale response after session switch", async () => {
    const { promise: s1Hang, resolve: resolveS1 } =
      createDeferred<SessionActivityResponse>();
    sessionsService.getApiV1SessionsIdActivity.mockReturnValueOnce(
      s1Hang,
    );

    // Start loading session 1 (hangs).
    const p1 = sessionActivity.load("s1");

    // Switch to session 2 before s1 resolves.
    sessionsService.getApiV1SessionsIdActivity.mockResolvedValueOnce(
      makeResponse(3),
    );
    const p2 = sessionActivity.load("s2");
    await p2;

    expect(sessionActivity.buckets.length).toBe(3);

    // Now s1 resolves — should be ignored.
    resolveS1(makeResponse(5));
    await p1;

    // Buckets should still be from s2, not s1.
    expect(sessionActivity.buckets.length).toBe(3);
  });

  it("does not retry after a failed fetch", async () => {
    sessionsService.getApiV1SessionsIdActivity.mockRejectedValueOnce(
      new Error("network error"),
    );
    await sessionActivity.load("s1");

    expect(sessionActivity.error).toBe("network error");
    expect(sessionActivity.loading).toBe(false);
    expect(sessionActivity.loaded).toBe(true);

    // A second load for the same session should not re-fetch
    // because the session is already marked as loaded (even
    // though it failed). The user must use reload() to retry.
    sessionsService.getApiV1SessionsIdActivity.mockResolvedValueOnce(
      makeResponse(2),
    );
    await sessionActivity.load("s1");

    // Still shows the error, did not auto-retry.
    expect(sessionActivity.error).toBe("network error");
    expect(
      sessionsService.getApiV1SessionsIdActivity,
    ).toHaveBeenCalledTimes(1);
  });

  it("invalidate discards in-flight load and forces refetch", async () => {
    const { promise: s1Hang, resolve: resolveS1 } =
      createDeferred<SessionActivityResponse>();
    sessionsService.getApiV1SessionsIdActivity.mockReturnValueOnce(
      s1Hang,
    );

    // Start a load that hangs.
    const p1 = sessionActivity.load("s1");

    // SSE fires while minimap is hidden — invalidate.
    sessionActivity.invalidate();

    // Stale response arrives — should be discarded.
    resolveS1(makeResponse(3));
    await p1;

    expect(sessionActivity.buckets.length).toBe(0);
    expect(sessionActivity.loaded).toBe(false);

    // Reopen triggers load — should refetch, not short-circuit.
    sessionsService.getApiV1SessionsIdActivity.mockResolvedValueOnce(
      makeResponse(5),
    );
    await sessionActivity.load("s1");

    expect(sessionActivity.buckets.length).toBe(5);
    expect(
      sessionsService.getApiV1SessionsIdActivity,
    ).toHaveBeenCalledTimes(2);
  });

  it("tracks loaded lifecycle", async () => {
    expect(sessionActivity.loaded).toBe(false);
    expect(sessionActivity.loading).toBe(false);

    // Successful load sets loaded=true.
    sessionsService.getApiV1SessionsIdActivity.mockResolvedValueOnce(
      makeResponse(2),
    );
    await sessionActivity.load("s1");
    expect(sessionActivity.loaded).toBe(true);
    expect(sessionActivity.loading).toBe(false);

    // clear() resets to pre-load state.
    sessionActivity.clear();
    expect(sessionActivity.loaded).toBe(false);
    expect(sessionActivity.loading).toBe(false);
  });

  it("sets loaded on fetch error", async () => {
    sessionsService.getApiV1SessionsIdActivity.mockRejectedValueOnce(
      new Error("network error"),
    );
    await sessionActivity.load("s1");
    expect(sessionActivity.loaded).toBe(true);
    expect(sessionActivity.error).toBe("network error");
  });

  it("clears active indicator when timestamp set to null", async () => {
    // This tests the store-level contract: setting
    // firstVisibleTimestamp to null clears the active bucket.
    // The component-level publishVisibleTimestamp() path that
    // sets this value is covered by the E2E test "active
    // indicator moves after reopen without scroll."
    sessionsService.getApiV1SessionsIdActivity.mockResolvedValueOnce(
      makeResponse(2),
    );
    await sessionActivity.load("s1");

    // Set a visible timestamp — indicator should be active.
    sessionActivity.firstVisibleTimestamp =
      "2026-03-26T10:05:00Z";
    expect(sessionActivity.activeBucketIndex).toBe(0);

    // Clear it (simulates publishVisibleTimestamp finding
    // no visible items with timestamps).
    sessionActivity.firstVisibleTimestamp = null;
    expect(sessionActivity.activeBucketIndex).toBeNull();
  });

  it("clears firstVisibleTimestamp on new load", async () => {
    sessionsService.getApiV1SessionsIdActivity.mockResolvedValue(
      makeResponse(2),
    );
    await sessionActivity.load("s1");

    sessionActivity.firstVisibleTimestamp =
      "2026-03-26T10:05:00Z";
    expect(sessionActivity.activeBucketIndex).toBe(0);

    // Loading a new session should clear it.
    await sessionActivity.load("s2");
    expect(sessionActivity.firstVisibleTimestamp).toBeNull();
  });
});

describe("findActiveBucketIndex", () => {
  const buckets: SessionActivityBucket[] = [
    bucket("2026-03-26T10:00:00Z", "2026-03-26T10:15:00Z", 0),
    bucket("2026-03-26T10:15:00Z", "2026-03-26T10:30:00Z", null),
    bucket("2026-03-26T10:30:00Z", "2026-03-26T10:45:00Z", 10),
  ];

  it("maps timestamp to correct bucket", () => {
    expect(
      findActiveBucketIndex(buckets, "2026-03-26T10:05:00Z"),
    ).toBe(0);
    expect(
      findActiveBucketIndex(buckets, "2026-03-26T10:35:00Z"),
    ).toBe(2);
  });

  it("returns null for null timestamp", () => {
    expect(findActiveBucketIndex(buckets, null)).toBeNull();
  });

  it("returns null for timestamp outside range", () => {
    expect(
      findActiveBucketIndex(buckets, "2026-03-26T09:00:00Z"),
    ).toBeNull();
    expect(
      findActiveBucketIndex(buckets, "2026-03-26T11:00:00Z"),
    ).toBeNull();
  });

  it("maps timestamp at bucket boundary to that bucket", () => {
    expect(
      findActiveBucketIndex(buckets, "2026-03-26T10:15:00Z"),
    ).toBe(1);
  });

  it("returns empty bucket index (for highlight, not click)", () => {
    expect(
      findActiveBucketIndex(buckets, "2026-03-26T10:20:00Z"),
    ).toBe(1);
  });
});
