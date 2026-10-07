import { MetadataService } from "../api/generated/index.js";
import type { DbStatsOutcomeStats, GetApiV1SessionStatsParams } from "../api/generated/index.js";

/** The window the totals are read for. */
export interface OutcomeWindow {
  since: string;
  until: string;
  timezone?: string;
  agent?: string;
  includeProject?: string[];
  excludeProject?: string[];
  includeOneShot?: boolean;
  includeAutomated?: boolean;
}

type FetchStats = typeof MetadataService.getApiV1SessionStats;

/**
 * Reads the git and GitHub outcome totals for a window.
 *
 * The API has always accepted `include_git_outcomes` and
 * `include_github_outcomes`; nothing in the UI asked for them, so commits and
 * pull requests were reachable from the command line only.
 *
 * The two flags are kept apart on purpose. The git side reads local history.
 * The GitHub side shells out to `gh` once per discovered repository and can
 * take minutes, so it is requested only when the reader asks for it.
 */
export class OutcomeTotalsStore {
  stats = $state<DbStatsOutcomeStats | null>(null);
  loading = $state(false);
  error = $state<string | null>(null);
  includePullRequests = $state(false);

  /** Cancels replaced reads; only the newest request may apply its result. */
  private abortController: AbortController | null = null;
  private requestSeq = 0;

  constructor(private readonly fetchStats: FetchStats = MetadataService.getApiV1SessionStats) {}

  /** Reads the git totals for the window, without the GitHub lookups. */
  async load(window: OutcomeWindow): Promise<void> {
    await this.read(window, false);
  }

  /** Reads the window again, with the pull-request lookups included. */
  async loadWithPullRequests(window: OutcomeWindow): Promise<void> {
    await this.read(window, true);
  }

  reset(): void {
    this.requestSeq += 1;
    this.abortController?.abort();
    this.abortController = null;
    this.stats = null;
    this.error = null;
    this.loading = false;
    this.includePullRequests = false;
  }

  private async read(window: OutcomeWindow, withPullRequests: boolean): Promise<void> {
    const seq = ++this.requestSeq;
    this.abortController?.abort();
    const controller = new AbortController();
    this.abortController = controller;
    this.stats = null;
    this.error = null;
    this.loading = true;
    this.includePullRequests = withPullRequests;
    try {
      const params: GetApiV1SessionStatsParams = {
        since: window.since,
        until: window.until,
        timezone: window.timezone,
        agent: window.agent,
        include_project: window.includeProject,
        exclude_project: window.excludeProject,
        include_one_shot: window.includeOneShot,
        include_automated: window.includeAutomated,
        include_git_outcomes: true,
        include_github_outcomes: withPullRequests,
      };
      const response = await this.fetchStats(params, { signal: controller.signal });
      if (seq !== this.requestSeq) return;
      // An absent block means the window had no repository to read, which is
      // not the same as a window with zero commits.
      this.stats = response.outcome_stats ?? null;
      this.error = null;
    } catch (cause) {
      if (seq !== this.requestSeq) return;
      this.stats = null;
      this.error = cause instanceof Error ? cause.message : String(cause);
    } finally {
      if (seq === this.requestSeq) this.loading = false;
    }
  }
}

export const outcomeTotals = new OutcomeTotalsStore();
