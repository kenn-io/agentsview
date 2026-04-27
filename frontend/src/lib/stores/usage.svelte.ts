import type {
  UsageSummaryResponse,
  TopUsageSessionsResponse,
  UsageParams,
} from "../api/types/usage.js";
import {
  getUsageSummary,
  getUsageTopSessions,
} from "../api/client.js";

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

function localDateStr(d: Date): string {
  const y = d.getFullYear();
  const m = String(d.getMonth() + 1).padStart(2, "0");
  const day = String(d.getDate()).padStart(2, "0");
  return `${y}-${m}-${day}`;
}

function daysAgo(n: number): string {
  const d = new Date();
  d.setDate(d.getDate() - n);
  return localDateStr(d);
}

function today(): string {
  return localDateStr(new Date());
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
}

function loadUsageFilters(): UsageFilterState {
  try {
    const raw = localStorage.getItem(USAGE_FILTERS_KEY);
    if (raw) {
      const saved = JSON.parse(raw) as Partial<UsageFilterState>;
      return {
        excludedProjects: saved.excludedProjects ?? "",
        excludedAgents: saved.excludedAgents ?? "",
        excludedModels: saved.excludedModels ?? "",
      };
    }
  } catch {
    // Corrupted localStorage — fall back to defaults.
  }
  return { excludedProjects: "", excludedAgents: "", excludedModels: "" };
}

function saveUsageFilters(f: UsageFilterState): void {
  try {
    const data: UsageFilterState = {
      excludedProjects: f.excludedProjects,
      excludedAgents: f.excludedAgents,
      excludedModels: f.excludedModels,
    };
    localStorage.setItem(USAGE_FILTERS_KEY, JSON.stringify(data));
  } catch {
    // localStorage full or unavailable — silently skip.
  }
}

type Endpoint = "summary" | "topSessions";

class UsageStore {
  from: string = $state(daysAgo(DEFAULT_WINDOW_DAYS));
  to: string = $state(today());
  isPinned: boolean = $state(false);
  windowDays: number = $state(DEFAULT_WINDOW_DAYS);

  // Excluded items (comma-separated strings). Default is
  // empty = nothing excluded = show all. The UI shows all items
  // as checked; clicking one unchecks it (excludes it).
  // Sent directly to the backend as exclude_project / exclude_agent
  // / exclude_model query params (NOT IN filtering).
  // Initialized from localStorage to survive tab switches.
  excludedProjects: string = $state("");
  excludedAgents: string = $state("");
  excludedModels: string = $state("");

  constructor() {
    const saved = loadUsageFilters();
    this.excludedProjects = saved.excludedProjects;
    this.excludedAgents = saved.excludedAgents;
    this.excludedModels = saved.excludedModels;
  }

  summary = $state<UsageSummaryResponse | null>(null);
  topSessions = $state<TopUsageSessionsResponse | null>(null);

  loading = $state({ summary: false, topSessions: false });
  errors = $state<Record<Endpoint, string | null>>({
    summary: null,
    topSessions: null,
  });

  toggles: Toggles = $state(loadToggles());

  private versions: Record<Endpoint, number> = {
    summary: 0,
    topSessions: 0,
  };

  private get timezone(): string {
    return Intl.DateTimeFormat().resolvedOptions().timeZone;
  }

  private baseParams(): UsageParams {
    const p: UsageParams = {
      from: this.from,
      to: this.to,
      timezone: this.timezone,
    };
    if (this.excludedProjects) {
      p.exclude_project = this.excludedProjects;
    }
    if (this.excludedAgents) {
      p.exclude_agent = this.excludedAgents;
    }
    if (this.excludedModels) {
      p.exclude_model = this.excludedModels;
    }
    return p;
  }

  setDateRange(from: string, to: string) {
    this.isPinned = true;
    this.from = from;
    this.to = to;
    this.fetchAll();
  }

  setRollingWindow(days: number) {
    this.windowDays = days;
    this.isPinned = false;
    this.rollDates();
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
    this.excludedModels = this.toggleCsv(
      this.excludedModels, name,
    );
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
    this.excludedModels = "";
    this.fetchAll();
  }

  deselectAllModels(all: string[]): void {
    this.excludedModels = all.join(",");
    this.fetchAll();
  }

  clearFilters(): void {
    this.excludedProjects = "";
    this.excludedAgents = "";
    this.excludedModels = "";
    this.fetchAll();
  }

  get hasActiveFilters(): boolean {
    return (
      this.excludedProjects !== "" ||
      this.excludedAgents !== "" ||
      this.excludedModels !== ""
    );
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
    this.rollDates();
    saveUsageFilters(this);
    await Promise.all([
      this.fetchSummary(),
      this.fetchTopSessions(),
    ]);
  }

  async fetchSummary() {
    const v = ++this.versions.summary;
    // Only show the skeleton when we don't already have data to
    // display. Refetches triggered by live events or filter changes
    // replace data in place instead of flashing to loading state.
    const isFirstLoad = this.summary === null;
    if (isFirstLoad) this.loading.summary = true;
    // Clear errors only on first load; on refetch, keep any prior
    // error state in place until we have a definitive result.
    if (isFirstLoad) this.errors.summary = null;
    try {
      const data = await getUsageSummary(this.baseParams());
      if (this.versions.summary === v) {
        this.summary = data;
        this.errors.summary = null;
      }
    } catch (e) {
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
      if (this.versions.summary === v) {
        this.loading.summary = false;
      }
    }
  }

  async fetchTopSessions() {
    const v = ++this.versions.topSessions;
    const isFirstLoad = this.topSessions === null;
    if (isFirstLoad) this.loading.topSessions = true;
    if (isFirstLoad) this.errors.topSessions = null;
    try {
      const data = await getUsageTopSessions(this.baseParams());
      if (this.versions.topSessions === v) {
        this.topSessions = data;
        this.errors.topSessions = null;
      }
    } catch (e) {
      if (this.versions.topSessions === v) {
        if (this.topSessions === null) {
          this.errors.topSessions =
            e instanceof Error ? e.message : "Failed to load";
        } else {
          console.warn("usage.fetchTopSessions refetch failed:", e);
        }
      }
    } finally {
      if (this.versions.topSessions === v) {
        this.loading.topSessions = false;
      }
    }
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
  if (state.excludedProjects) {
    params["exclude_project"] = state.excludedProjects;
  }
  if (state.excludedAgents) {
    params["exclude_agent"] = state.excludedAgents;
  }
  if (state.excludedModels) {
    params["exclude_model"] = state.excludedModels;
  }
  return params;
}
