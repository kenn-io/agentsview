import type { AgentInfo, ProjectInfo } from "../api/types.js";
import type { Report } from "../api/types/activity.js";
import { ActivityService, MetadataService } from "../api/generated/index";
import { configureGeneratedClient } from "../api/runtime.js";
import { sync } from "./sync.svelte.js";
import { router } from "./router.svelte.js";
import { localDateStr, rollingRange } from "../utils/dates.js";

export { localDateStr };

type Preset = "day" | "week" | "month" | "custom";

const PRESETS: ReadonlySet<string> = new Set<Preset>([
  "day",
  "week",
  "month",
  "custom",
]);

export type Automation = "all" | "interactive" | "automated";

const AUTOMATIONS: ReadonlySet<string> = new Set<Automation>([
  "all",
  "interactive",
  "automated",
]);

function parseWindowDays(raw: string | undefined): number | null {
  if (!raw) return null;
  const n = Number.parseInt(raw, 10);
  if (!Number.isInteger(n) || n <= 0 || String(n) !== raw) {
    return null;
  }
  return n;
}

/**
 * Translate a date-only custom `to` value (YYYY-MM-DD, local zone) to the
 * exclusive end of a half-open range: local 00:00 of the day AFTER `to`, as an
 * RFC3339 instant. The caller passes only a non-empty value.
 */
function customToInstant(to: string): string {
  const end = new Date(to + "T00:00:00");
  end.setDate(end.getDate() + 1);
  return end.toISOString();
}

class ActivityStore {
  preset = $state<Preset>("day");
  date: string = $state(localDateStr(new Date()));
  from = $state("");
  to = $state("");
  rollingWindowDays: number | null = $state(null);
  bucket = $state("");
  project: string = $state("");
  agent: string = $state("");
  machine: string = $state("");
  automation: Automation = $state("all");
  report: Report | null = $state(null);
  loading = $state(false);
  error: string | null = $state(null);
  // Epoch ms of the last successful report fetch, powering the "Updated Xm ago"
  // refresh label. null until the first load completes.
  lastUpdatedAt: number | null = $state(null);
  // Set when an SSE event arrives after the first load, signalling that newer
  // data exists. Mirrors the analytics/usage stores: marking is cheap, and the
  // actual refetch is left to the manual refresh button and the periodic
  // scheduler so a session actively writing files does not thrash the report
  // aggregation on every event.
  hasNewData: boolean = $state(false);

  // Filter-option lists for the activity controls. Loaded with full
  // inclusion (one-shot + automated) so every project/agent/machine
  // that can appear in the always-inclusive activity report is also
  // selectable here — unlike the sidebar's lists, which honor the
  // sidebar include toggles.
  projects: ProjectInfo[] = $state([]);
  agents: AgentInfo[] = $state([]);
  machines: string[] = $state([]);

  private loadVersion = 0;
  #filterOptionsLoaded = false;
  #filterOptionsPromise: Promise<void> | null = null;
  #filterOptionsVersion = 0;
  #attached = 0;

  /** Browser-resolved IANA timezone; not a user control. */
  get timezone(): string {
    return Intl.DateTimeFormat().resolvedOptions().timeZone;
  }

  /**
   * Flag that newer data exists (an SSE event arrived) without refetching, so a
   * session actively writing files does not thrash the report aggregation on
   * every event. No-op before the first successful load, matching the
   * analytics/usage stores. The report refreshes on the manual refresh button,
   * the periodic scheduler, or a range/filter change.
   */
  markNewData(): void {
    if (this.lastUpdatedAt === null) return;
    this.hasNewData = true;
  }

  private materializeRollingWindow(): boolean {
    if (this.preset !== "custom" || this.rollingWindowDays === null) {
      return false;
    }
    const range = rollingRange(this.rollingWindowDays);
    if (this.from === range.from && this.to === range.to) {
      return false;
    }
    this.from = range.from;
    this.to = range.to;
    return true;
  }

  async load({ background = false }: { background?: boolean } = {}) {
    const v = ++this.loadVersion;
    if (this.materializeRollingWindow()) {
      this.writeUrl();
    }
    // A custom range needs both bounds. With only one present (a cleared date
    // input or a partial deep link) the backend rejects the request, so hold
    // the current view until both are set instead of flashing an error.
    if (this.preset === "custom" && (!this.from || !this.to)) {
      this.loading = false;
      return;
    }
    this.loading = true;
    this.error = null;
    configureGeneratedClient();
    try {
      // Custom date-only inputs (YYYY-MM-DD, browser local zone) become
      // half-open local instants: from = local 00:00 of `from`, to = local
      // 00:00 of the day after `to`. Non-custom presets rely on preset+date, so
      // send no range. An empty input maps to undefined so selecting "custom"
      // before picking dates never builds an Invalid Date. A malformed
      // hand-edited date throws here, inside the try, so load() still resets
      // loading rather than leaving it stuck true.
      const fromParam =
        this.preset === "custom" && this.from
          ? new Date(this.from + "T00:00:00").toISOString()
          : undefined;
      const toParam =
        this.preset === "custom" && this.to
          ? customToInstant(this.to)
          : undefined;
      const res = await ActivityService.getApiV1ActivityReport({
        preset: this.preset,
        date: this.date,
        from: fromParam,
        to: toParam,
        timezone: this.timezone,
        // The store keeps bucket as a free-form override string (populated by
        // the Task 4 control); the generated client narrows it to the server's
        // accepted set. An out-of-set value is rejected server-side.
        bucket: (this.bucket || undefined) as
          | "5m"
          | "15m"
          | "1h"
          | "1d"
          | "1w"
          | undefined,
        project: this.project || undefined,
        agent: this.agent || undefined,
        machine: this.machine || undefined,
        automation: this.automation,
      });
      if (v !== this.loadVersion) return;
      this.report = res as unknown as Report;
      this.lastUpdatedAt = Date.now();
      this.hasNewData = false;
      this.loading = false;
    } catch (e) {
      if (v !== this.loadVersion) return;
      this.loading = false;
      // A failed background refresh keeps the last good report on screen so a
      // transient blip never blanks the report-first dashboard; the growing
      // "Updated Xm ago" label signals the staleness. With no report yet (a
      // first load still failing, or a refresh firing before one lands) there
      // is nothing to preserve, so fall through and surface the error rather
      // than leave a misleading empty state. First loads and range/filter
      // changes are always foreground and clear on error.
      if (background && this.report !== null) return;
      this.report = null;
      this.error =
        e instanceof Error ? e.message : "Failed to load activity report";
    }
  }

  /**
   * Populate the activity filter dropdowns, including one-shot and automated
   * sessions so the controls cover everything the always-inclusive activity
   * report can surface. The result is cached after a fully successful load
   * and refreshed when a sync/import completes (via invalidateFilterOptions),
   * mirroring the sessions store. A concurrent call shares the in-flight
   * request. A transient failure leaves the cache un-loaded so the next call
   * retries; lists that did succeed keep their values in the meantime.
   */
  async loadFilterOptions() {
    if (this.#filterOptionsLoaded) return;
    if (this.#filterOptionsPromise) return this.#filterOptionsPromise;
    const ver = this.#filterOptionsVersion;
    const opts = { includeOneShot: true, includeAutomated: true };
    this.#filterOptionsPromise = (async () => {
      configureGeneratedClient();
      let ok = true;
      try {
        const res = (await MetadataService.getApiV1Projects(
          opts,
        )) as unknown as { projects: ProjectInfo[] };
        if (ver === this.#filterOptionsVersion) this.projects = res.projects;
      } catch {
        ok = false; // keep the current list; retry on the next call
      }
      try {
        const res = (await MetadataService.getApiV1Agents(
          opts,
        )) as unknown as { agents: AgentInfo[] };
        if (ver === this.#filterOptionsVersion) this.agents = res.agents;
      } catch {
        ok = false;
      }
      try {
        const res = (await MetadataService.getApiV1Machines(
          opts,
        )) as unknown as { machines: string[] };
        if (ver === this.#filterOptionsVersion) this.machines = res.machines;
      } catch {
        ok = false;
      }
      if (ver === this.#filterOptionsVersion) {
        // Cache only a fully successful load so a transient failure is
        // retried rather than frozen as a permanent empty list.
        this.#filterOptionsLoaded = ok;
        this.#filterOptionsPromise = null;
      }
    })();
    return this.#filterOptionsPromise;
  }

  /**
   * Drop the activity filter-option cache so the next loadFilterOptions()
   * refetches. Invoked when a sync/import completes, since newly imported
   * sessions can introduce projects/agents/machines the activity report shows.
   */
  invalidateFilterOptions() {
    this.#filterOptionsVersion++;
    this.#filterOptionsLoaded = false;
    this.#filterOptionsPromise = null;
  }

  /** Whether an ActivityPage is currently mounted and showing the controls. */
  get attached(): boolean {
    return this.#attached > 0;
  }

  /**
   * Register a mounted ActivityPage so a completed sync can refresh the filter
   * options while they are on screen. Returns a detach callback for the
   * component's onMount cleanup.
   */
  attach(): () => void {
    this.#attached++;
    // Make state URL-canonical on mount, then keep it in sync with browser
    // back/forward. router's own popstate handler runs first (registered at
    // module load) and has already refreshed router.params synchronously, so
    // reading it here observes the navigated-to URL.
    this.hydrateFromUrl(router.params);
    const onPop = () => {
      this.hydrateFromUrl(router.params);
      void this.load();
    };
    window.addEventListener("popstate", onPop);
    let detached = false;
    return () => {
      if (detached) return;
      detached = true;
      window.removeEventListener("popstate", onPop);
      this.#attached = Math.max(0, this.#attached - 1);
    };
  }

  /**
   * Replace range/preset/filter state from URL query params. `preset` defaults
   * to "day" (and falls back to "day" for any unknown value); `date` defaults
   * to today's local YYYY-MM-DD; `automation` defaults to "all" (and falls back
   * to "all" for any unknown value). The remaining filters default to empty.
   * This is the single hydration path, run on mount and on popstate.
   */
  hydrateFromUrl(params: Record<string, string>) {
    const windowDays = parseWindowDays(params.window_days);
    if (windowDays !== null) {
      const range = rollingRange(windowDays);
      this.preset = "custom";
      this.date = range.to;
      this.from = range.from;
      this.to = range.to;
      this.rollingWindowDays = windowDays;
    } else {
      this.preset = PRESETS.has(params.preset ?? "")
        ? (params.preset as Preset)
        : "day";
      this.date = params.date || localDateStr(new Date());
      this.from = params.from ?? "";
      this.to = params.to ?? "";
      this.rollingWindowDays = null;
    }
    this.bucket = params.bucket ?? "";
    this.project = params.project ?? "";
    this.agent = params.agent ?? "";
    this.machine = params.machine ?? "";
    this.automation = AUTOMATIONS.has(params.automation ?? "")
      ? (params.automation as Automation)
      : "all";
  }

  /**
   * Write the current range/preset/filter state to the URL through the router's
   * single replaceState path. `preset` is always included; `date` is included
   * for day/week/month when non-empty; `from`/`to` only for the custom preset;
   * bucket/project/agent/machine only when non-empty; `automation` only when not
   * the "all" default. Empty filters and preset-irrelevant fields are omitted so
   * URLs stay minimal and deep-linkable.
   */
  writeUrl() {
    const p: Record<string, string> = { preset: this.preset };
    if (this.preset === "custom") {
      if (this.from) p.from = this.from;
      if (this.to) p.to = this.to;
      if (this.rollingWindowDays !== null) {
        p.window_days = String(this.rollingWindowDays);
      }
    } else {
      if (this.date) p.date = this.date;
    }
    if (this.bucket) p.bucket = this.bucket;
    if (this.project) p.project = this.project;
    if (this.agent) p.agent = this.agent;
    if (this.machine) p.machine = this.machine;
    if (this.automation !== "all") p.automation = this.automation;
    router.replaceParams(p);
  }

  setPreset(p: Preset) {
    this.preset = p;
    if (p !== "custom") {
      this.rollingWindowDays = null;
    }
    if (p === "custom") {
      // Seed a 1-day range from the current anchor so selecting Custom shows a
      // valid range immediately instead of erroring on empty from/to bounds.
      if (!this.from) this.from = this.date;
      if (!this.to) this.to = this.date;
    }
    this.writeUrl();
  }

  setDate(date: string) {
    this.date = date;
    if (this.preset !== "custom") {
      this.rollingWindowDays = null;
    }
    this.writeUrl();
  }

  setFrom(d: string) {
    this.from = d;
    this.rollingWindowDays = null;
    this.writeUrl();
  }

  setTo(d: string) {
    this.to = d;
    this.rollingWindowDays = null;
    this.writeUrl();
  }

  setCustomRange(
    from: string,
    to: string,
    rollingWindowDays: number | null = null,
  ) {
    this.preset = "custom";
    this.date = from;
    this.from = from;
    this.to = to;
    this.rollingWindowDays = rollingWindowDays;
    this.writeUrl();
  }

  /**
   * Advance the anchor `date` by one preset unit: one day for `day`, seven days
   * for `week`, one calendar month for `month` (clamped to a valid day). No-op
   * for `custom`, which is driven by explicit from/to inputs instead.
   */
  step(direction: -1 | 1) {
    if (this.preset === "custom") return;
    const d = new Date(this.date + "T00:00:00");
    if (this.preset === "week") {
      d.setDate(d.getDate() + 7 * direction);
    } else if (this.preset === "month") {
      // Advance one calendar month, clamping the day to the target month's last
      // day so e.g. Jan 31 -> Feb 28 instead of overflowing into March.
      const target = new Date(d.getFullYear(), d.getMonth() + direction, 1);
      const lastDay = new Date(
        target.getFullYear(),
        target.getMonth() + 1,
        0,
      ).getDate();
      target.setDate(Math.min(d.getDate(), lastDay));
      d.setTime(target.getTime());
    } else {
      d.setDate(d.getDate() + direction);
    }
    this.date = localDateStr(d);
    this.writeUrl();
  }

  setProject(project: string) {
    this.project = project;
    this.writeUrl();
  }

  setAgent(agent: string) {
    this.agent = agent;
    this.writeUrl();
  }

  setMachine(machine: string) {
    this.machine = machine;
    this.writeUrl();
  }

  setAutomation(automation: Automation) {
    this.automation = automation;
    this.writeUrl();
  }
}

export const activity = new ActivityStore();

// Refresh the activity filter options after any sync/import, mirroring the
// sessions store, so newly imported projects/agents/machines appear in the
// activity controls without a full page reload. Only refetch when an
// ActivityPage is mounted; otherwise the invalidated cache is picked up lazily
// by the next mount's loadFilterOptions(). The report itself is deliberately
// not refetched here: that is driven by the manual refresh button and the
// periodic scheduler in ActivityPage, so a session actively writing files does
// not thrash the report aggregation on every sync. The eager option refetch
// also recovers the controls when a sync lands mid-initial-load and the version
// bump discards that in-flight response.
sync.onSyncComplete(() => {
  activity.invalidateFilterOptions();
  if (activity.attached) void activity.loadFilterOptions();
});
