import type {
  UsageComparison,
  UsageSummaryResponse,
  TopUsageSessionsResponse,
} from "../api/types/usage.js";
import { UsageService } from "../api/generated/index";
import {
  callGenerated,
  isAbortError,
} from "../api/runtime.js";
import { sessions } from "./sessions.svelte.js";
import { perf, type PerfEntryStatus } from "./perf.svelte.js";
import { daysAgo, today } from "../utils/dates.js";

type UsageParams = Parameters<typeof UsageService.getApiV1UsageSummary>[0];
type UsagePanel = "summary" | "comparison" | "topSessions";
type FetchResult = "ok" | "error" | "aborted";
type LoadedUsageSummary = {
  version: number;
  summary: UsageSummaryResponse;
  params: UsageParams;
};

export type GroupBy = "project" | "model" | "agent";
export type TimeSeriesView = "stacked-area" | "bars" | "lines";
export type AttributionView = "treemap" | "list" | "bars";

interface Toggles {
  timeSeries: { groupBy: GroupBy; view: TimeSeriesView };
  attribution: { groupBy: GroupBy; view: AttributionView };
}

const TOGGLES_KEY = "usage-toggles";

function defaultToggles(): Toggles {
  return {
    timeSeries: { groupBy: "project", view: "stacked-area" },
    attribution: { groupBy: "project", view: "treemap" },
  };
}

function isGroupBy(value: unknown): value is GroupBy {
  return value === "project" || value === "model" || value === "agent";
}

function loadToggles(): Toggles {
  try {
    const raw = localStorage.getItem(TOGGLES_KEY);
    if (raw) {
      const parsed = JSON.parse(raw) as Partial<Toggles>;
      const defaults = defaultToggles();
      // `Project | Model | Agent` selector is shared across usage
      // panels. Migrate legacy split state by choosing one value
      // and applying it to both widgets.
      const sharedGroupBy = isGroupBy(parsed.timeSeries?.groupBy)
        ? parsed.timeSeries.groupBy
        : isGroupBy(parsed.attribution?.groupBy)
          ? parsed.attribution.groupBy
          : defaults.timeSeries.groupBy;
      return {
        timeSeries: {
          groupBy: sharedGroupBy,
          view: parsed.timeSeries?.view ?? defaults.timeSeries.view,
        },
        attribution: {
          groupBy: sharedGroupBy,
          view: parsed.attribution?.view ?? defaults.attribution.view,
        },
      };
    }
  } catch {
    // Corrupted localStorage — fall back to defaults.
  }
  return defaultToggles();
}

function saveToggles(t: Toggles): void {
  try {
    localStorage.setItem(TOGGLES_KEY, JSON.stringify(t));
  } catch {
    // localStorage full or unavailable — silently skip.
  }
}

const DEFAULT_WINDOW_DAYS = 30;

// 100 years is well beyond any realistic session history and stays
// inside Date#setDate's safe range, so daysAgo(MAX_WINDOW_DAYS) always
// produces a valid YYYY-MM-DD string.
const MAX_WINDOW_DAYS = 36500;

const USAGE_FILTERS_KEY = "usage-filters";

export interface UsageFilterState {
  excludedProjects: string;
  excludedAgents: string;
  excludedModels: string;
  selectedModels: string;
}

function loadUsageFilters(): UsageFilterState {
  try {
    const raw = localStorage.getItem(USAGE_FILTERS_KEY);
    if (raw) {
      const saved = JSON.parse(raw) as Partial<UsageFilterState>;
      return {
        excludedProjects: saved.excludedProjects ?? "",
        excludedAgents: saved.excludedAgents ?? "",
        excludedModels: "",
        selectedModels: saved.selectedModels ?? "",
      };
    }
  } catch {
    // Corrupted localStorage — fall back to defaults.
  }
  return {
    excludedProjects: "",
    excludedAgents: "",
    excludedModels: "",
    selectedModels: "",
  };
}

function saveUsageFilters(f: UsageFilterState): void {
  try {
    const data: UsageFilterState = {
      excludedProjects: f.excludedProjects,
      excludedAgents: f.excludedAgents,
      excludedModels: f.excludedModels,
      selectedModels: f.selectedModels,
    };
    localStorage.setItem(USAGE_FILTERS_KEY, JSON.stringify(data));
  } catch {
    // localStorage full or unavailable — silently skip.
  }
}

function joinCsvParts(...parts: string[]): string {
  const out: string[] = [];
  const seen = new Set<string>();
  for (const part of parts) {
    for (const value of part.split(",")) {
      const trimmed = value.trim();
      if (!trimmed || seen.has(trimmed)) continue;
      seen.add(trimmed);
      out.push(trimmed);
    }
  }
  return out.join(",");
}

type Endpoint = "summary" | "topSessions";

class UsageStore {
  from: string = $state(daysAgo(DEFAULT_WINDOW_DAYS));
  to: string = $state(today());
  isPinned: boolean = $state(false);
  windowDays: number = $state(DEFAULT_WINDOW_DAYS);

  // Excluded project items and included model items
  // (comma-separated strings). Empty models = all models.
  // Initialized from localStorage to survive tab switches.
  excludedProjects: string = $state("");
  excludedAgents: string = $state("");
  excludedModels: string = $state("");
  selectedModels: string = $state("");

  constructor() {
    const saved = loadUsageFilters();
    this.excludedProjects = saved.excludedProjects;
    this.excludedAgents = saved.excludedAgents;
    this.excludedModels = saved.excludedModels;
    this.selectedModels = saved.selectedModels;
  }

  summary = $state<UsageSummaryResponse | null>(null);
  topSessions = $state<TopUsageSessionsResponse | null>(null);
  lastUpdatedAt: number | null = $state(null);
  hasNewData: boolean = $state(false);

  loading = $state({ summary: false, topSessions: false });
  querying = $state<Record<UsagePanel, boolean>>({
    summary: false,
    comparison: false,
    topSessions: false,
  });
  errors = $state<Record<Endpoint, string | null>>({
    summary: null,
    topSessions: null,
  });

  toggles: Toggles = $state(loadToggles());

  private versions: Record<Endpoint, number> = {
    summary: 0,
    topSessions: 0,
  };
  private fetchAllVersion = 0;
  private abortControllers: Partial<Record<UsagePanel, AbortController>> = {};

  private get timezone(): string {
    return Intl.DateTimeFormat().resolvedOptions().timeZone;
  }

  markNewData(): void {
    if (this.lastUpdatedAt === null) return;
    this.hasNewData = true;
  }

  private baseParams(): UsageParams {
    const sessionFilters = sessions.filters;
    const p: UsageParams = {
      from: this.from,
      to: this.to,
      timezone: this.timezone,
      project: sessionFilters.project || undefined,
      machine: sessionFilters.machine || undefined,
      agent: sessionFilters.agent || undefined,
      termination: sessionFilters.termination || undefined,
      minUserMessages:
        sessionFilters.minUserMessages > 0
          ? sessionFilters.minUserMessages
          : undefined,
      includeOneShot: sessionFilters.includeOneShot,
      includeAutomated:
        sessionFilters.includeAutomated || undefined,
      activeSince: sessionFilters.recentlyActive
        ? new Date(
            Date.now() - 24 * 60 * 60 * 1000,
          ).toISOString()
        : undefined,
    };
    if (
      sessionFilters.hideUnknownProject &&
      sessionFilters.project !== "unknown"
    ) {
      p.excludeProject = joinCsvParts(
        this.excludedProjects,
        "unknown",
      );
    } else if (this.excludedProjects) {
      p.excludeProject = this.excludedProjects;
    }
    if (this.selectedModels) {
      p.model = this.selectedModels;
    }
    return p;
  }

  applyDateRange(from: string, to: string) {
    this.isPinned = true;
    this.from = from;
    this.to = to;
  }

  applyRollingWindow(days: number) {
    this.windowDays = days;
    this.isPinned = false;
    this.rollDates();
  }

  setDateRange(from: string, to: string) {
    this.applyDateRange(from, to);
    this.fetchAll();
  }

  setRollingWindow(days: number) {
    this.applyRollingWindow(days);
    this.fetchAll();
  }

  // Toggle an item's exclusion. Clicking an included item
  // excludes it; clicking an excluded item re-includes it.
  toggleProject(name: string): void {
    this.excludedProjects = this.toggleCsv(
      this.excludedProjects, name,
    );
    this.fetchAll();
  }

  toggleAgent(name: string): void {
    this.excludedAgents = this.toggleCsv(
      this.excludedAgents, name,
    );
    this.fetchAll();
  }

  toggleModel(name: string): void {
    this.selectedModels = this.toggleCsv(
      this.selectedModels, name,
    );
    this.excludedModels = "";
    this.fetchAll();
  }

  private toggleCsv(csv: string, name: string): string {
    const current = csv ? csv.split(",") : [];
    const idx = current.indexOf(name);
    if (idx >= 0) {
      current.splice(idx, 1);
    } else {
      current.push(name);
    }
    return current.join(",");
  }

  // An item is "excluded" if it appears in the excluded CSV.
  // The UI shows a check for items NOT excluded (i.e., visible).
  isProjectExcluded(name: string): boolean {
    if (!this.excludedProjects) return false;
    return this.excludedProjects.split(",").includes(name);
  }

  isAgentExcluded(name: string): boolean {
    if (!this.excludedAgents) return false;
    return this.excludedAgents.split(",").includes(name);
  }

  isModelExcluded(name: string): boolean {
    if (!this.excludedModels) return false;
    return this.excludedModels.split(",").includes(name);
  }

  isModelSelected(name: string): boolean {
    if (!this.selectedModels) return false;
    return this.selectedModels.split(",").includes(name);
  }

  selectAllProjects(): void {
    this.excludedProjects = "";
    this.fetchAll();
  }

  deselectAllProjects(all: string[]): void {
    this.excludedProjects = all.join(",");
    this.fetchAll();
  }

  selectAllAgents(): void {
    this.excludedAgents = "";
    this.fetchAll();
  }

  deselectAllAgents(all: string[]): void {
    this.excludedAgents = all.join(",");
    this.fetchAll();
  }

  selectAllModels(): void {
    this.selectedModels = "";
    this.excludedModels = "";
    this.fetchAll();
  }

  deselectAllModels(_all: string[]): void {
    this.selectedModels = "";
    this.excludedModels = "";
    this.fetchAll();
  }

  clearFilters(): void {
    this.excludedProjects = "";
    this.excludedAgents = "";
    this.excludedModels = "";
    this.selectedModels = "";
    this.fetchAll();
  }

  get hasActiveFilters(): boolean {
    return this.excludedProjects !== "" || this.selectedModels !== "";
  }

  get isQuerying(): boolean {
    return Object.values(this.querying).some(Boolean);
  }

  setTimeSeriesGroupBy(g: GroupBy) {
    this.toggles.timeSeries.groupBy = g;
    this.toggles.attribution.groupBy = g;
    saveToggles(this.toggles);
  }

  setTimeSeriesView(v: TimeSeriesView) {
    this.toggles.timeSeries.view = v;
    saveToggles(this.toggles);
  }

  setAttributionGroupBy(g: GroupBy) {
    this.toggles.timeSeries.groupBy = g;
    this.toggles.attribution.groupBy = g;
    saveToggles(this.toggles);
  }

  setAttributionView(v: AttributionView) {
    this.toggles.attribution.view = v;
    saveToggles(this.toggles);
  }

  private rollDates(): void {
    if (this.isPinned) return;
    this.from = daysAgo(this.windowDays);
    this.to = today();
  }

  async fetchAll() {
    const fetchVersion = ++this.fetchAllVersion;
    this.invalidatePanel("topSessions");
    this.rollDates();
    saveUsageFilters(this);
    const params = this.baseParams();
    const summaryPromise = this.fetchSummary({
      loadComparison: false,
      params,
    });
    const topSessionsPromise = this.fetchTopSessions(params);
    const loadedSummary = await summaryPromise;
    if (fetchVersion !== this.fetchAllVersion || !loadedSummary) {
      await topSessionsPromise;
      return;
    }
    const [topSessionsResult, comparisonResult] = await Promise.all([
      topSessionsPromise,
      this.fetchComparison(
        loadedSummary.version,
        loadedSummary.summary,
        loadedSummary.params,
      ),
    ]);
    if (
      fetchVersion === this.fetchAllVersion &&
      topSessionsResult === "ok" &&
      comparisonResult === "ok"
    ) {
      this.markRefreshComplete();
    }
  }

  async fetchSummary(
    options: { loadComparison?: boolean; params?: UsageParams } = {},
  ): Promise<LoadedUsageSummary | null> {
    const loadComparison = options.loadComparison ?? true;
    const v = ++this.versions.summary;
    this.abortPanel("comparison");
    const signal = this.nextAbortSignal("summary");
    // Only show the skeleton when we don't already have data to
    // display. Refetches triggered by live events or filter changes
    // replace data in place instead of flashing to loading state.
    const isFirstLoad = this.summary === null;
    if (isFirstLoad) this.loading.summary = true;
    // Clear errors only on first load; on refetch, keep any prior
    // error state in place until we have a definitive result.
    if (isFirstLoad) this.errors.summary = null;
    const started = performance.now();
    let status: Extract<PerfEntryStatus, "ok" | "error" | "aborted"> = "ok";
    try {
      const params = options.params ?? this.baseParams();
      const data = await callGenerated(() =>
        UsageService.getApiV1UsageSummary(params),
        signal,
      ) as unknown as UsageSummaryResponse;
      if (this.versions.summary === v) {
        this.summary = data;
        this.errors.summary = null;
        const loaded = { version: v, summary: data, params };
        if (loadComparison) {
          void this.fetchComparison(v, data, params);
        }
        return loaded;
      }
      return null;
    } catch (e) {
      if (isAbortError(e)) {
        status = "aborted";
        return null;
      }
      status = "error";
      if (this.versions.summary === v) {
        // On refetch failure with cached data, swallow the error so
        // existing values stay visible instead of flipping to a "--"
        // error state. First-load failures still surface.
        if (this.summary === null) {
          this.errors.summary =
            e instanceof Error ? e.message : "Failed to load";
        } else {
          console.warn("usage.fetchSummary refetch failed:", e);
        }
      }
    } finally {
      perf.recordPanel({
        route: "usage",
        name: "summary",
        durationMs: performance.now() - started,
        status,
      });
      this.clearAbortSignal("summary", signal);
      if (this.versions.summary === v) {
        this.loading.summary = false;
      }
    }
    return null;
  }

  private async fetchComparison(
    summaryVersion: number,
    summary: UsageSummaryResponse,
    params: UsageParams,
  ): Promise<FetchResult> {
    if (this.versions.summary !== summaryVersion) return "aborted";
    const signal = this.nextAbortSignal("comparison");
    const started = performance.now();
    let status: Extract<PerfEntryStatus, "ok" | "error" | "aborted"> = "ok";
    try {
      const comparison = await callGenerated(() =>
        UsageService.getApiV1UsageComparison({
          ...params,
          currentCost: summary.totals.totalCost,
        }),
        signal,
      ) as unknown as UsageComparison;
      if (this.versions.summary === summaryVersion) {
        this.summary = { ...summary, comparison };
        return "ok";
      }
      return "aborted";
    } catch (e) {
      if (isAbortError(e)) {
        status = "aborted";
        return "aborted";
      }
      status = "error";
      if (this.versions.summary === summaryVersion) {
        console.warn("usage.fetchComparison failed:", e);
      }
      return "error";
    } finally {
      perf.recordPanel({
        route: "usage",
        name: "comparison",
        durationMs: performance.now() - started,
        status,
      });
      this.clearAbortSignal("comparison", signal);
    }
  }

  async fetchTopSessions(
    params: UsageParams | null = null,
  ): Promise<FetchResult> {
    const v = ++this.versions.topSessions;
    const signal = this.nextAbortSignal("topSessions");
    const isFirstLoad = this.topSessions === null;
    if (isFirstLoad) this.loading.topSessions = true;
    if (isFirstLoad) this.errors.topSessions = null;
    const started = performance.now();
    let status: Extract<PerfEntryStatus, "ok" | "error" | "aborted"> = "ok";
    try {
      const data = await callGenerated(() =>
        UsageService.getApiV1UsageTopSessions(
          params ?? this.baseParams(),
        ),
        signal,
      ) as unknown as TopUsageSessionsResponse;
      if (this.versions.topSessions === v) {
        this.topSessions = data;
        this.errors.topSessions = null;
        return "ok";
      }
      return "aborted";
    } catch (e) {
      if (isAbortError(e)) {
        status = "aborted";
        return "aborted";
      }
      status = "error";
      if (this.versions.topSessions === v) {
        if (this.topSessions === null) {
          this.errors.topSessions =
            e instanceof Error ? e.message : "Failed to load";
        } else {
          console.warn("usage.fetchTopSessions refetch failed:", e);
        }
      }
      return "error";
    } finally {
      perf.recordPanel({
        route: "usage",
        name: "topSessions",
        durationMs: performance.now() - started,
        status,
      });
      this.clearAbortSignal("topSessions", signal);
      if (this.versions.topSessions === v) {
        this.loading.topSessions = false;
      }
    }
  }

  private invalidatePanel(panel: Endpoint): void {
    this.versions[panel]++;
    this.abortPanel(panel);
  }

  private abortPanel(panel: UsagePanel): void {
    this.abortControllers[panel]?.abort();
    delete this.abortControllers[panel];
    this.querying[panel] = false;
  }

  private nextAbortSignal(panel: UsagePanel): AbortSignal {
    this.abortControllers[panel]?.abort();
    const controller = new AbortController();
    this.abortControllers[panel] = controller;
    this.querying[panel] = true;
    return controller.signal;
  }

  private clearAbortSignal(
    panel: UsagePanel,
    signal: AbortSignal,
  ): boolean {
    if (this.abortControllers[panel]?.signal === signal) {
      delete this.abortControllers[panel];
      this.querying[panel] = false;
      return true;
    }
    return false;
  }

  private markRefreshComplete(): void {
    this.lastUpdatedAt = Date.now();
    this.hasNewData = false;
  }
}

export const usage = new UsageStore();

export interface UsageUrlState {
  from: string;
  to: string;
  isPinned: boolean;
  windowDays: number;
  excludedProjects: string;
  excludedAgents: string;
  excludedModels: string;
  selectedModels: string;
}

export const USAGE_DEFAULT_WINDOW_DAYS = DEFAULT_WINDOW_DAYS;

export function parseWindowDays(raw: string | undefined): number | null {
  if (!raw) return null;
  const n = Number.parseInt(raw, 10);
  if (
    !Number.isFinite(n) ||
    n <= 0 ||
    n > MAX_WINDOW_DAYS ||
    String(n) !== raw
  ) {
    return null;
  }
  return n;
}

export function buildUsageUrlParams(
  state: UsageUrlState,
): Record<string, string> {
  const params: Record<string, string> = {};
  if (state.isPinned) {
    if (state.from) params["from"] = state.from;
    if (state.to) params["to"] = state.to;
  } else if (
    state.windowDays > 0 &&
    state.windowDays !== DEFAULT_WINDOW_DAYS
  ) {
    params["window_days"] = String(state.windowDays);
  }
  if (state.selectedModels) {
    params["model"] = state.selectedModels;
  }
  if (state.excludedProjects) {
    params["exclude_project"] = state.excludedProjects;
  }
  return params;
}

const CSV_MERGE_URL_KEYS = new Set(["exclude_project"]);
const SESSION_DATE_URL_KEYS = new Set([
  "date",
  "date_from",
  "date_to",
]);

export function mergeUsageAndSessionUrlParams(
  usageParams: Record<string, string>,
  sessionParams: Record<string, string>,
): Record<string, string> {
  const params = { ...usageParams };
  for (const [key, value] of Object.entries(sessionParams)) {
    if (SESSION_DATE_URL_KEYS.has(key)) continue;
    if (CSV_MERGE_URL_KEYS.has(key) && params[key]) {
      params[key] = joinCsvParts(params[key], value);
    } else {
      params[key] = value;
    }
  }
  return params;
}
