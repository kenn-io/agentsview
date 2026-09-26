import { beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { OutcomeTotalsStore } from "./outcomeTotals.svelte.js";
import type { DbSessionStats, GetApiV1SessionStatsParams } from "../api/generated/index.js";

type FetchStats = (params: GetApiV1SessionStatsParams) => Promise<DbSessionStats>;

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

  it("ignores a slow earlier response once a later window was requested", async () => {
    const responses: Array<() => void> = [];
    const fetchStats = vi.fn(
      (params: GetApiV1SessionStatsParams) =>
        new Promise<DbSessionStats>((resolve) => {
          responses.push(() =>
            resolve({
              generated_at: "2026-09-01T00:00:00Z",
              outcome_stats: {
                repos_active: 1,
                commits: params.since === "2026-08-01" ? 1 : 2,
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
    const second = store.load({ since: "2026-08-02", until: "2026-09-01" });
    // The second window answers first, then the stale first window answers.
    (responses[1] as () => void)();
    (responses[0] as () => void)();
    await Promise.all([first, second]);

    expect(store.stats?.commits).toBe(2);
  });
});
