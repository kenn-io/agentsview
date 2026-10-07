import { beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { OutcomeTotalsStore } from "./outcomeTotals.svelte.js";
import { MetadataService } from "../api/generated/index.js";
import type { DbSessionStats, GetApiV1SessionStatsParams } from "../api/generated/index.js";

type FetchStats = typeof MetadataService.getApiV1SessionStats;

// The params of the one call the store made, so a test can assert the flags.
function calledWith(fetchStats: ReturnType<typeof vi.fn>): GetApiV1SessionStatsParams {
  const call = fetchStats.mock.calls[0] as [GetApiV1SessionStatsParams] | undefined;
  expect(call).toBeDefined();
  return (call as [GetApiV1SessionStatsParams])[0];
}

// The store is the only caller of the outcome flags the API has always
// accepted. These tests pin which flags it sends and what it keeps.

describe("OutcomeTotalsStore", () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

  it("asks for git outcomes and not for the GitHub lookups", async () => {
    const fetchStats = vi.fn(async () => ({
      generated_at: "2026-09-01T00:00:00Z",
      outcome_stats: {
        repos_active: 2,
        commits: 7,
        loc_added: 1,
        loc_removed: 0,
        files_changed: 3,
      },
    }));
    const store = new OutcomeTotalsStore(fetchStats as unknown as FetchStats);

    await store.load({ since: "2026-08-01", until: "2026-09-01" });

    expect(fetchStats).toHaveBeenCalledTimes(1);
    expect(calledWith(fetchStats)).toMatchObject({
      since: "2026-08-01",
      until: "2026-09-01",
      include_git_outcomes: true,
      include_github_outcomes: false,
    });
    expect(store.stats?.commits).toBe(7);
    expect(store.includePullRequests).toBe(false);
    expect(store.error).toBeNull();
    expect(store.loading).toBe(false);
  });

  it("asks for the GitHub lookups only when they were requested", async () => {
    const fetchStats = vi.fn(async () => ({
      generated_at: "2026-09-01T00:00:00Z",
      outcome_stats: {
        repos_active: 2,
        commits: 7,
        loc_added: 1,
        loc_removed: 0,
        files_changed: 3,
        prs_opened: 25,
      },
    }));
    const store = new OutcomeTotalsStore(fetchStats as unknown as FetchStats);

    await store.loadWithPullRequests({ since: "2026-08-01", until: "2026-09-01" });

    expect(calledWith(fetchStats)).toMatchObject({ include_github_outcomes: true });
    expect(store.includePullRequests).toBe(true);
    expect(store.stats?.prs_opened).toBe(25);
  });

  it("keeps the response's absent outcome block as absent, not as zeros", async () => {
    const fetchStats = vi.fn(async () => ({ generated_at: "2026-09-01T00:00:00Z" }));
    const store = new OutcomeTotalsStore(fetchStats as unknown as FetchStats);

    await store.load({ since: "2026-08-01", until: "2026-09-01" });

    expect(store.stats).toBeNull();
    expect(store.error).toBeNull();
  });

  it("reports a failure instead of leaving stale numbers on the page", async () => {
    const fetchStats = vi.fn(async () => {
      throw new Error("daemon unreachable");
    });
    const store = new OutcomeTotalsStore(fetchStats as unknown as FetchStats);

    await store.load({ since: "2026-08-01", until: "2026-09-01" });

    expect(store.error).toBe("daemon unreachable");
    expect(store.stats).toBeNull();
    expect(store.loading).toBe(false);
  });

  it.each([
    ["window", false],
    ["window", true],
    ["reset", false],
  ])(
    "cancels a replaced request on %s and ignores its late result, fails=%s",
    async (replacement, fails) => {
      const responses: Array<() => void> = [];
      const signals: AbortSignal[] = [];
      const fetchStats = vi.fn(
        (...[params, options]: Parameters<FetchStats>) =>
          new Promise<DbSessionStats>((resolve, reject) => {
            const signal = options?.signal as AbortSignal;
            signals.push(signal);
            if (fails) {
              signal.addEventListener(
                "abort",
                () => reject(new DOMException("aborted", "AbortError")),
                { once: true },
              );
            }
            responses.push(() =>
              resolve({
                generated_at: "2026-09-01T00:00:00Z",
                outcome_stats: {
                  repos_active: 1,
                  commits: params?.since === "2026-08-01" ? 1 : 2,
                  loc_added: 0,
                  loc_removed: 0,
                  files_changed: 0,
                },
              } as DbSessionStats),
            );
          }),
      );
      const store = new OutcomeTotalsStore(fetchStats as unknown as FetchStats);

      const first = store.load({ since: "2026-08-01", until: "2026-09-01" });
      expect(signals[0]?.aborted).toBe(false);
      if (replacement === "reset") {
        store.reset();
        expect(signals[0]?.aborted).toBe(true);
        if (!fails) (responses[0] as () => void)();
        await first;
        expect(store.stats).toBeNull();
        expect(store.error).toBeNull();
        expect(store.loading).toBe(false);
        expect(store.includePullRequests).toBe(false);
        return;
      }
      const window = { since: "2026-08-02", until: "2026-09-01" };
      const second = store.load(window);
      expect(signals[0]?.aborted).toBe(true);
      expect(signals[1]?.aborted).toBe(false);
      if (fails) await first;
      expect(store.loading).toBe(true);
      expect(store.error).toBeNull();
      // The second window answers first, then the stale first window answers.
      (responses[1] as () => void)();
      if (!fails) (responses[0] as () => void)();
      await Promise.all([first, second]);

      expect(store.stats?.commits).toBe(2);
      expect(store.error).toBeNull();
      expect(store.loading).toBe(false);
    },
  );

  it("clears previous totals and errors while a new window loads", async () => {
    let finish!: (response: DbSessionStats) => void;
    const fetchStats = vi.fn(
      () =>
        new Promise<DbSessionStats>((resolve) => {
          finish = resolve;
        }),
    );
    const store = new OutcomeTotalsStore(fetchStats);
    store.stats = { repos_active: 1, commits: 7, loc_added: 0, loc_removed: 0, files_changed: 1 };
    store.error = "previous request failed";

    const pending = store.load({ since: "2026-08-02", until: "2026-09-01" });

    expect(store.loading).toBe(true);
    expect(store.stats).toBeNull();
    expect(store.error).toBeNull();
    finish({ generated_at: "2026-09-01T00:00:00Z" } as DbSessionStats);
    await pending;
    expect(store.loading).toBe(false);
  });
});
