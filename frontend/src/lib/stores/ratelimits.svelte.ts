import {
  RateLimitsService,
  type GetApiV1RateLimitsHistoryVendor,
  type GetApiV1RateLimitsHistoryWindow,
  type ServiceRateLimitWindow,
} from "../api/generated/index";
import { callGenerated, isAbortError } from "../api/runtime.js";
import { sessions } from "./sessions.svelte.js";

export type RateLimitWindow = ServiceRateLimitWindow;

// Bounds each card's history request to a point count sized for its own
// small sparkline chart, well under the /rate-limits/history endpoint's
// own (much larger) default -- see RateLimitCard's history fetch.
const RATE_LIMIT_HISTORY_MAX_POINTS = 200;

/**
 * Identifies one Usage-page rate-limit card. This must be the full
 * identity a card is grouped by (see LatestRateLimitSnapshots on the
 * backend) -- vendor, account id, machine, limit id, and window kind --
 * not just a subset. Two cards can share a limit id and window kind
 * while differing in machine (e.g. the same account synced from two
 * machines), and each needs its own history request and cache entry so
 * their charts do not merge. Codex snapshots carry no account identity
 * (accountId always "").
 *
 * planType is deliberately not part of this identity: Codex reports it
 * as a label that can flip between "pro" and empty for the same window
 * from one observation to the next (see docs/agents/storage.md), not a
 * stable identity component, so keying or filtering history by it would
 * split one window's history across two card identities, or defensively
 * drop half of it from the chart.
 */
export interface RateLimitCardIdentity {
  vendor: string;
  accountId: string;
  machine: string;
  limitId: string;
  windowKind: string;
}

function historyKey(identity: RateLimitCardIdentity): string {
  return [
    identity.vendor,
    identity.accountId,
    identity.machine,
    identity.limitId,
    identity.windowKind,
  ].join(" ");
}

/** Groups current snapshots by vendor, then by account (Codex carries no
 * account identity, so its rows group by machine instead), matching the
 * Usage page's vendor-then-account layout.
 *
 * `key` is the group's real identity for a keyed `{#each}` loop:
 * `accountId` alone is not unique for Codex, which is always "" there,
 * so two different Codex machines would otherwise render under the same
 * key. `key` includes `machine` precisely for that case.
 */
export interface RateLimitAccountGroup {
  vendor: string;
  accountId: string;
  accountLabel: string;
  machine: string;
  key: string;
  windows: RateLimitWindow[];
}

export interface RateLimitVendorGroup {
  vendor: string;
  accounts: RateLimitAccountGroup[];
}

function groupByVendorThenAccount(snapshots: RateLimitWindow[]): RateLimitVendorGroup[] {
  const vendorOrder: string[] = [];
  const byVendor = new Map<string, Map<string, RateLimitAccountGroup>>();

  for (const snapshot of snapshots) {
    const vendor = snapshot.vendor;
    if (!byVendor.has(vendor)) {
      byVendor.set(vendor, new Map());
      vendorOrder.push(vendor);
    }
    const accounts = byVendor.get(vendor)!;
    // Codex rows carry no account identity, so machine is the closest
    // grouping key it has; a future account-keyed vendor would group by
    // its real account id instead.
    const machine = snapshot.machine ?? "";
    const accountId = snapshot.accountId ?? "";
    const accountKey = accountId || machine;
    if (!accounts.has(accountKey)) {
      accounts.set(accountKey, {
        vendor,
        accountId,
        accountLabel: snapshot.accountLabel || machine,
        machine,
        key: `${vendor} ${accountKey}`,
        windows: [],
      });
    }
    accounts.get(accountKey)!.windows.push(snapshot);
  }

  return vendorOrder.map((vendor) => ({
    vendor,
    accounts: [...byVendor.get(vendor)!.values()],
  }));
}

/**
 * Rate-limit snapshots for the Usage page's "Rate limits" section
 * (Codex today; the table is vendor-keyed so another vendor can add rows
 * without a frontend change). Mirrors the shape of stores/usage.svelte.ts:
 * reactive state written by callGenerated-wrapped fetches, one
 * AbortController per in-flight request so a filter change cannot let a
 * stale response overwrite a newer one.
 */
class RateLimitsStore {
  current: RateLimitWindow[] = $state([]);
  history: Record<string, RateLimitWindow[]> = $state({});
  loading = $state(false);
  loaded = $state(false);
  error: string | null = $state(null);
  /**
   * Bumped after every successful fetchCurrent(). Cards watch this (see
   * RateLimitCard.svelte) to re-fetch their own history whenever current
   * snapshots refresh -- mount, the Usage page's manual refresh button, or
   * kit-ui RefreshControl's periodic timer -- so a chart does not go stale
   * while newer observations are already showing in the card above it.
   */
  refreshToken = $state(0);

  private currentAbort?: AbortController;
  private historyAbort = new Map<string, AbortController>();
  // Per-identity request generation: aborting the previous controller for
  // a key does not guarantee its in-flight promise never resolves (the
  // underlying request may not honor AbortSignal), so a late response
  // must still be recognized as superseded rather than overwriting a
  // newer one for the same card.
  private historyVersion = new Map<string, number>();
  private version = 0;

  /** True once the first fetch has completed with at least one snapshot. */
  get hasData(): boolean {
    return this.current.length > 0;
  }

  /** Current snapshots grouped by vendor, then by account, in the order
   * the Usage page renders them (vendors and accounts appear in the
   * order their first snapshot was returned by the API). */
  get groupedByVendor(): RateLimitVendorGroup[] {
    return groupByVendorThenAccount(this.current);
  }

  historyFor(identity: RateLimitCardIdentity): RateLimitWindow[] {
    return this.history[historyKey(identity)] ?? [];
  }

  async fetchCurrent(): Promise<void> {
    const v = ++this.version;
    this.currentAbort?.abort();
    const controller = new AbortController();
    this.currentAbort = controller;
    this.loading = true;
    try {
      const machine = sessions.filters.machine || undefined;
      // sessions.filters.agent is the shared, comma-separated agent
      // selection; forwarded as-is so the backend's agent-list match
      // (see db.RateLimitAgentMatchesVendor) naturally empties the
      // result -- hiding the section -- once a non-Codex-only selection
      // is active.
      const agent = sessions.filters.agent || undefined;
      const data = await callGenerated(
        (options) => RateLimitsService.getApiV1RateLimitsCurrent({ machine, agent }, options),
        controller.signal,
      );
      if (v !== this.version) return;
      this.current = data;
      this.error = null;
      this.loaded = true;
      this.refreshToken++;
    } catch (err) {
      if (isAbortError(err)) return;
      this.error = err instanceof Error ? err.message : String(err);
      this.loaded = true;
    } finally {
      if (v === this.version) this.loading = false;
    }
  }

  async fetchHistory(identity: RateLimitCardIdentity, since: string, until: string): Promise<void> {
    const key = historyKey(identity);
    this.historyAbort.get(key)?.abort();
    const controller = new AbortController();
    this.historyAbort.set(key, controller);
    const requestVersion = (this.historyVersion.get(key) ?? 0) + 1;
    this.historyVersion.set(key, requestVersion);
    try {
      const data = await callGenerated(
        (options) =>
          RateLimitsService.getApiV1RateLimitsHistory(
            {
              vendor: identity.vendor as GetApiV1RateLimitsHistoryVendor,
              account_id: identity.accountId || undefined,
              // The card's own machine, not the page's (possibly
              // "all machines") filter: history must stay scoped to
              // exactly the window this card renders.
              machine: identity.machine,
              limit_id: identity.limitId,
              window: identity.windowKind as GetApiV1RateLimitsHistoryWindow,
              since,
              until,
              // RateLimitHistoryChart renders a small sparkline-sized
              // chart, not a full-page one, so a much smaller point
              // budget than the endpoint's own default keeps a wide
              // date range from returning (and the chart from laying
              // out) far more points than the chart can usefully show.
              max_points: RATE_LIMIT_HISTORY_MAX_POINTS,
            },
            options,
          ),
        controller.signal,
      );
      // A superseded request (a newer fetchHistory for this same
      // identity started after this one) must not overwrite the newer
      // one's result just because it happened to resolve later.
      if (this.historyVersion.get(key) !== requestVersion) return;
      // The request above already scopes the response to this card's
      // exact vendor/account/machine/limit/window, so the whole result
      // is cached as-is -- including every plan_type label the window's
      // history carries, so the chart is never missing points because a
      // plan_type happened to differ between observations.
      this.history = { ...this.history, [key]: data };
    } catch (err) {
      if (isAbortError(err)) return;
      // Leave any previously loaded history in place; a failed history
      // fetch just leaves that card's chart empty rather than surfacing a
      // page-level error for a secondary panel.
    }
  }
}

export const rateLimits = new RateLimitsStore();
