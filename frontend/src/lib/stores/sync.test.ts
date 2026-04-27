import {
  describe,
  it,
  expect,
  vi,
  beforeEach,
} from "vitest";
import { commitsDisagree, sync } from "./sync.svelte.js";
import * as api from "../api/client.js";
import type { SyncStats, UpdateCheck } from "../api/types.js";

vi.mock("../api/client.js", () => ({
  triggerSync: vi.fn(),
  triggerResync: vi.fn(),
  getSyncStatus: vi.fn(),
  getStats: vi.fn(),
  getVersion: vi.fn(),
  watchSession: vi.fn(),
  checkForUpdate: vi.fn(),
}));

const MOCK_STATS: SyncStats = {
  synced: 5,
  skipped: 3,
  failed: 0,
  total_sessions: 8,
};

function mockResyncSuccess(): void {
  vi.mocked(api.triggerResync).mockReturnValue({
    abort: vi.fn(),
    done: Promise.resolve(MOCK_STATS),
  });
  vi.mocked(api.getStats).mockResolvedValue({
    session_count: 8,
    message_count: 100,
    project_count: 3,
    machine_count: 1,
    earliest_session: null,
  });
  // triggerResync schedules loadStatus() as a side effect.
  // loadStatus() reads getSyncStatus and unconditionally sets
  // lastSyncStats from the response, so mirror MOCK_STATS here so
  // the post-resync state matches what the resync just produced.
  vi.mocked(api.getSyncStatus).mockResolvedValue({
    last_sync: "2024-01-01T00:00:00Z",
    stats: MOCK_STATS,
  });
}

function mockResyncFailure(error: Error): void {
  vi.mocked(api.triggerResync).mockReturnValue({
    abort: vi.fn(),
    done: Promise.reject(error),
  });
}

describe("commitsDisagree", () => {
  it.each([
    // Unknown / undefined handling
    { expected: false, hash1: "unknown", hash2: "unknown", scenario: "both are unknown" },
    { expected: false, hash1: "unknown", hash2: "abc1234", scenario: "frontend is unknown" },
    { expected: false, hash1: "abc1234", hash2: "unknown", scenario: "server is unknown" },
    { expected: false, hash1: undefined, hash2: "abc1234", scenario: "first hash is undefined" },
    { expected: false, hash1: "abc1234", hash2: undefined, scenario: "second hash is undefined" },
    { expected: false, hash1: undefined, hash2: undefined, scenario: "both hashes are undefined" },

    // Empty strings
    { expected: false, hash1: "", hash2: "abc1234", scenario: "first hash is empty" },
    { expected: false, hash1: "abc1234", hash2: "", scenario: "second hash is empty" },
    { expected: false, hash1: "", hash2: "", scenario: "both hashes are empty" },

    // Matches
    { expected: false, hash1: "abc1234", hash2: "abc1234", scenario: "identical short hashes" },
    { expected: false, hash1: "abc1234", hash2: "abc1234def5678", scenario: "short matches full SHA prefix" },
    { expected: false, hash1: "abc1234aaaaaaaaaaaa", hash2: "abc1234aaaaaaaaaaaa", scenario: "identical full SHAs" },
    { expected: false, hash1: "abc12", hash2: "abc1234def5678", scenario: "short abbreviation matching prefix" },

    // Mismatches
    { expected: true, hash1: "abc1234", hash2: "def5678", scenario: "different hashes" },
    { expected: true, hash1: "abc1234aaaaaaaaaaaa", hash2: "def5678bbbbbbbbbbb", scenario: "full SHAs differ" },
    { expected: true, hash1: "abc1234aaaaaaaaaaaa", hash2: "abc1234bbbbbbbbbbb", scenario: "full SHAs share 7-char prefix" },
    { expected: true, hash1: "xyz99", hash2: "abc1234def5678", scenario: "short abbreviation not matching" },
  ])(
    "returns $expected when $scenario",
    ({ expected, hash1, hash2 }) => {
      expect(commitsDisagree(hash1, hash2)).toBe(expected);
    },
  );
});

describe("SyncStore.loadStats", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    const s = sync as unknown as Record<string, unknown>;
    s.stats = null;
  });

  it("discards stale response when a newer request exists", async () => {
    const older = {
      session_count: 10,
      message_count: 50,
      project_count: 2,
      machine_count: 1,
      earliest_session: null,
    };
    const newer = {
      session_count: 5,
      message_count: 30,
      project_count: 1,
      machine_count: 1,
      earliest_session: null,
    };

    let resolveOlder!: (v: typeof older) => void;
    let resolveNewer!: (v: typeof newer) => void;

    vi.mocked(api.getStats)
      .mockReturnValueOnce(
        new Promise((r) => {
          resolveOlder = r;
        }),
      )
      .mockReturnValueOnce(
        new Promise((r) => {
          resolveNewer = r;
        }),
      );

    // Start first request (include one-shot).
    const p1 = sync.loadStats({ include_one_shot: true });
    // Start second request (exclude one-shot) before first resolves.
    const p2 = sync.loadStats({});

    // Resolve newer first, then older.
    resolveNewer(newer);
    await p2;
    expect(sync.stats).toEqual(newer);

    // Now resolve the older request — it should be discarded.
    resolveOlder(older);
    await p1;
    expect(sync.stats).toEqual(newer);
  });
});

describe("SyncStore.triggerResync", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    // Reset singleton state between tests.
    const s = sync as unknown as Record<string, unknown>;
    s.syncing = false;
    s.progress = null;
  });

  it("returns false when already syncing", () => {
    mockResyncSuccess();
    const first = sync.triggerResync();
    expect(first).toBe(true);
    expect(sync.syncing).toBe(true);

    const second = sync.triggerResync();
    expect(second).toBe(false);
  });

  it("calls onError on stream failure", async () => {
    const error = new Error("stream failed");
    mockResyncFailure(error);

    const onError = vi.fn();
    sync.triggerResync(undefined, onError);

    await vi.waitFor(() => {
      expect(onError).toHaveBeenCalledWith(error);
    });
    expect(sync.syncing).toBe(false);
  });

  it("resets syncing on non-Error rejection", async () => {
    vi.mocked(api.triggerResync).mockReturnValue({
      abort: vi.fn(),
      done: Promise.reject("string error"),
    });

    const onError = vi.fn();
    sync.triggerResync(undefined, onError);

    await vi.waitFor(() => {
      expect(onError).toHaveBeenCalled();
    });
    expect(onError).toHaveBeenCalledWith(
      expect.objectContaining({ message: "Sync failed" }),
    );
    expect(sync.syncing).toBe(false);
  });

  it("allows retry after error", async () => {
    mockResyncFailure(new Error("fail"));
    const onError = vi.fn();
    sync.triggerResync(undefined, onError);

    await vi.waitFor(() => {
      expect(onError).toHaveBeenCalled();
    });

    // Retry should succeed
    mockResyncSuccess();
    const onComplete = vi.fn();
    const started = sync.triggerResync(onComplete);
    expect(started).toBe(true);

    await vi.waitFor(() => {
      expect(onComplete).toHaveBeenCalled();
    });
    expect(sync.syncing).toBe(false);
  });

  it("calls onComplete on success", async () => {
    mockResyncSuccess();
    const onComplete = vi.fn();
    sync.triggerResync(onComplete);

    await vi.waitFor(() => {
      expect(onComplete).toHaveBeenCalled();
    });
    expect(sync.syncing).toBe(false);
    expect(sync.lastSyncStats).toEqual(MOCK_STATS);
  });
});

describe("SyncStore.checkForUpdate", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    const s = sync as unknown as Record<string, unknown>;
    s.updateAvailable = false;
    s.latestVersion = null;
  });

  it("skips API call when isDesktop is true", async () => {
    const s = sync as unknown as Record<string, unknown>;
    // Temporarily override isDesktop
    const original = s.isDesktop;
    Object.defineProperty(sync, "isDesktop", {
      value: true,
      writable: true,
      configurable: true,
    });

    await sync.checkForUpdate();

    expect(api.checkForUpdate).not.toHaveBeenCalled();
    expect(sync.updateAvailable).toBe(false);

    Object.defineProperty(sync, "isDesktop", {
      value: original,
      writable: true,
      configurable: true,
    });
  });

  it("calls API and sets state when not desktop", async () => {
    const mockResult: UpdateCheck = {
      update_available: true,
      current_version: "v0.9.0",
      latest_version: "v1.0.0",
    };
    vi.mocked(api.checkForUpdate).mockResolvedValue(
      mockResult,
    );

    const s = sync as unknown as Record<string, unknown>;
    const original = s.isDesktop;
    Object.defineProperty(sync, "isDesktop", {
      value: false,
      writable: true,
      configurable: true,
    });

    await sync.checkForUpdate();

    expect(api.checkForUpdate).toHaveBeenCalled();
    expect(sync.updateAvailable).toBe(true);
    expect(sync.latestVersion).toBe("v1.0.0");

    Object.defineProperty(sync, "isDesktop", {
      value: original,
      writable: true,
      configurable: true,
    });
  });
});
