import { m } from "../i18n/index.js";
import { queryStepFrom, type QueryStep } from "../utils/refresh.js";
import { LiveQuery } from "../utils/liveQuery.svelte.js";
import type { UsagePairwiseDimension } from "../api/types/usage.js";
import {
  UsageService,
  type DbTopSessionEntry,
  type ServiceUsagePairwiseComparisonResponse,
  type UsageSummaryResponse,
} from "../api/generated/index";
import { ApiError, isAbortError, responseTimingOf } from "../api/runtime.js";
import { sessions } from "./sessions.svelte.js";
import { perf, type PerfEntryStatus } from "./perf.svelte.js";
import { rollingRange, today } from "../utils/dates.js";
import { ALL_TOKEN_TYPES, canonicalTokenTypes, type UsageTokenType } from "./usageTokenTypes.js";

type UsageParams = NonNullable<Parameters<typeof UsageService.getApiV1UsageSummary>[0]>;
type UsagePairwiseParams = Parameters<typeof UsageService.getApiV1UsagePairwiseComparison>[0];
type UsagePanel = "summary" | "comparison" | "pairwise" | "topSessions" | "zoom";
// Steps of a full refresh in execution order; the breakdown follows it. The
// window summary is the second summary request made while a time range is
// selected on the chart.
type UsageStep = UsagePanel | "contextSummary" | "attributionSummary" | "referenceSummary";
const USAGE_STEP_ORDER: readonly UsageStep[] = [
  "summary",
  "contextSummary",
  "attributionSummary",
  "referenceSummary",
  "topSessions",
  "zoom",
  "comparison",
  "pairwise",
];
type FetchResult = "ok" | "error" | "aborted";
type FetchAllOptions = {
  preserveTimeRange?: boolean;
  refreshTimeSeriesContext?: boolean;
};
type LoadedUsageSummary = {
  version: number;
  summary: UsageSummaryResponse;
  params: UsageParams;
  projectScopeRecovered: boolean;
};
export type UsagePairwiseSide = "left" | "right";
export interface UsagePairwiseSideSelection {
  dimension: UsagePairwiseDimension;
  value: string;
}
export interface UsagePairwiseSelection {
  left: UsagePairwiseSideSelection;
  right: UsagePairwiseSideSelection;
}

export interface UsageProjectFilterItem {
  id: string;
  name: string;
  count?: number;
}

export type GroupBy = "project" | "model" | "agent";
export type TimeSeriesView = "smooth" | "lines" | "bars";
export type AttributionView = "treemap" | "list" | "bars";

interface Toggles {
  timeSeries: { groupBy: GroupBy; view: TimeSeriesView };
  attribution: { groupBy: GroupBy; view: AttributionView };
}

const TOGGLES_KEY = "usage-toggles";

function defaultToggles(): Toggles {
  return {
    timeSeries: { groupBy: "project", view: "smooth" },
    attribution: { groupBy: "project", view: "treemap" },
  };
}

function isGroupBy(value: unknown): value is GroupBy {
  return value === "project" || value === "model" || value === "agent";
}

function isTimeSeriesView(value: unknown): value is TimeSeriesView {
  return value === "smooth" || value === "lines" || value === "bars";
}

function isUnknownProjectKeyError(error: unknown): boolean {
  return error instanceof ApiError && error.status === 400 && error.code === "unknown_project_key";
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
          view: isTimeSeriesView(parsed.timeSeries?.view)
            ? parsed.timeSeries.view
            : defaults.timeSeries.view,
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
// inside Date#setDate's safe range, so rollingRange(MAX_WINDOW_DAYS)
// always produces valid YYYY-MM-DD strings.
const MAX_WINDOW_DAYS = 36500;

const USAGE_FILTERS_KEY = "usage-filters";

export interface UsageFilterState {
  excludedProjects: string;
  excludedProjectKeys?: string;
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
        excludedProjectKeys: "",
        excludedAgents: saved.excludedAgents ?? "",
        excludedModels: saved.excludedModels ?? "",
      };
    }
  } catch {
    // Corrupted localStorage — fall back to defaults.
  }
  return {
    excludedProjects: "",
    excludedProjectKeys: "",
    excludedAgents: "",
    excludedModels: "",
  };
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

type Endpoint = "summary" | "pairwise" | "topSessions" | "zoom";

function emptyPairwiseSelection(): UsagePairwiseSelection {
  return {
    left: { dimension: "model", value: "" },
    right: { dimension: "model", value: "" },
  };
}

function samePairwiseSelection(
  left: UsagePairwiseSelection,
  right: UsagePairwiseSelection,
): boolean {
  return (
    left.left.dimension === right.left.dimension &&
    left.left.value === right.left.value &&
    left.right.dimension === right.right.dimension &&
    left.right.value === right.right.value
  );
}

export type UsageMode = "cost" | "token";

function selectionKey(params: UsageParams): string {
  return [params.project_key, params.model, params.agent].join("|");
}

function summaryForDateRange(
  summary: UsageSummaryResponse,
  from: string,
  to: string,
): UsageSummaryResponse {
  const daily = summary.daily.filter((day) => day.date >= from && day.date <= to);
  const projectTotals = new Map<string, UsageSummaryResponse["projectTotals"][number]>();
  const modelTotals = new Map<string, UsageSummaryResponse["modelTotals"][number]>();
  const agentTotals = new Map<string, UsageSummaryResponse["agentTotals"][number]>();
  let inputTokens = 0;
  let outputTokens = 0;
  let cacheCreationTokens = 0;
  let cacheReadTokens = 0;
  let totalMicrodollars = 0;

  for (const day of daily) {
    inputTokens += day.inputTokens;
    outputTokens += day.outputTokens;
    cacheCreationTokens += day.cacheCreationTokens;
    cacheReadTokens += day.cacheReadTokens;
    totalMicrodollars += day.totalCost.microdollars;

    for (const item of day.projectBreakdowns ?? []) {
      const total = projectTotals.get(item.project_key) ?? {
        project_key: item.project_key,
        project: item.project,
        inputTokens: 0,
        outputTokens: 0,
        cacheCreationTokens: 0,
        cacheReadTokens: 0,
        cost: { microdollars: 0 },
      };
      total.inputTokens += item.inputTokens;
      total.outputTokens += item.outputTokens;
      total.cacheCreationTokens += item.cacheCreationTokens;
      total.cacheReadTokens += item.cacheReadTokens;
      total.cost.microdollars += item.cost.microdollars;
      projectTotals.set(item.project_key, total);
    }

    for (const item of day.modelBreakdowns ?? []) {
      const total = modelTotals.get(item.modelName) ?? {
        model: item.modelName,
        inputTokens: 0,
        outputTokens: 0,
        cacheCreationTokens: 0,
        cacheReadTokens: 0,
        cost: { microdollars: 0 },
      };
      total.inputTokens += item.inputTokens;
      total.outputTokens += item.outputTokens;
      total.cacheCreationTokens += item.cacheCreationTokens;
      total.cacheReadTokens += item.cacheReadTokens;
      total.cost.microdollars += item.cost.microdollars;
      modelTotals.set(item.modelName, total);
    }

    for (const item of day.agentBreakdowns ?? []) {
      const total = agentTotals.get(item.agent) ?? {
        agent: item.agent,
        inputTokens: 0,
        outputTokens: 0,
        cacheCreationTokens: 0,
        cacheReadTokens: 0,
        cost: { microdollars: 0 },
      };
      total.inputTokens += item.inputTokens;
      total.outputTokens += item.outputTokens;
      total.cacheCreationTokens += item.cacheCreationTokens;
      total.cacheReadTokens += item.cacheReadTokens;
      total.cost.microdollars += item.cost.microdollars;
      agentTotals.set(item.agent, total);
    }
  }

  const byCost = <T extends { cost: { microdollars: number } }>(a: T, b: T) =>
    b.cost.microdollars - a.cost.microdollars;
  const cacheHitDenominator = cacheReadTokens + inputTokens;

  return {
    ...summary,
    from,
    to,
    daily,
    totals: {
      inputTokens,
      outputTokens,
      cacheCreationTokens,
      cacheReadTokens,
      totalCost: { microdollars: totalMicrodollars },
      // Daily entries carry no per-day savings, so a derived range cannot
      // recompute them; the UI does not read this field for derived ranges.
      cacheSavings: { microdollars: 0 },
    },
    projectTotals: [...projectTotals.values()].sort(
      (a, b) => byCost(a, b) || a.project_key.localeCompare(b.project_key),
    ),
    modelTotals: [...modelTotals.values()].sort(
      (a, b) => byCost(a, b) || a.model.localeCompare(b.model),
    ),
    agentTotals: [...agentTotals.values()].sort(
      (a, b) => byCost(a, b) || a.agent.localeCompare(b.agent),
    ),
    cacheStats: {
      cacheReadTokens,
      cacheCreationTokens,
      uncachedInputTokens: inputTokens,
      outputTokens,
      hitRate: cacheHitDenominator > 0 ? cacheReadTokens / cacheHitDenominator : 0,
      savingsVsUncached: { microdollars: 0 },
    },
    unsupportedUsage: undefined,
    comparison: undefined,
  };
}

class UsageStore {
  from: string = $state(rollingRange(DEFAULT_WINDOW_DAYS).from);
  to: string = $state(today());
  isPinned: boolean = $state(false);
  windowDays: number = $state(DEFAULT_WINDOW_DAYS);
  mode: UsageMode = $state("cost");
  selectedTokenTypes: UsageTokenType[] = $state([...ALL_TOKEN_TYPES]);
  selectedTimeRange: { from: string; to: string } | null = $state(null);

  // Picker exclusions persist across tab switches.
  excludedProjects: string = $state("");
  excludedProjectKeys: string = $state("");
  excludedAgents: string = $state("");
  excludedModels: string = $state("");
  knownProjects: UsageProjectFilterItem[] = $state([]);

  constructor() {
    const saved = loadUsageFilters();
    this.excludedProjects = saved.excludedProjects;
    this.excludedProjectKeys = saved.excludedProjectKeys ?? "";
    this.excludedAgents = saved.excludedAgents;
    this.excludedModels = saved.excludedModels;
  }

  summary = $state<UsageSummaryResponse | null>(null);
  attributionSummary = $state<UsageSummaryResponse | null>(null);
  selectedProjectKey = $state("");
  selectedModel = $state("");
  // The selected-dimension-free summary over the full unbrushed window.
  referenceSummary = $state<UsageSummaryResponse | null>(null);
  private timeSeriesContextSummary = $state<UsageSummaryResponse | null>(null);
  // Which selection the shown summary covers, so a failed reselect reports instead of showing old totals.
  private summarySelection: string | null = null;
  // A full-window refresh is in flight, so a brush must refetch the window instead of slicing old data.
  private fullWindowPending = false;
  isTimeRangeSummaryProvisional = $state(false);
  pairwiseComparison = $state<ServiceUsagePairwiseComparisonResponse | null>(null);
  pairwiseSelection = $state<UsagePairwiseSelection>(emptyPairwiseSelection());
  private zoomed = $state(false);

  get zoomedProject(): { key: string; label: string } | null {
    return this.zoomed && this.selectedProjectKey ? { key: this.selectedProjectKey, label: this.focusLabel } : null;
  }
  get focusLabel(): string {
    return this.knownProjects.find((project) => project.id === this.selectedProjectKey)?.name ?? this.selectedProjectKey;
  }
  zoomRows = $state<DbTopSessionEntry[] | null>(null);
  topSessions = $state<DbTopSessionEntry[] | null>(null);
  lastUpdatedAt: number | null = $state(null);
  // Wall-clock ms of the most recent full refresh, request start to data
  // applied, shown next to the refresh label. null until the first load.
  lastQueryDurationMs: number | null = $state(null);
  // Per-panel timings behind lastQueryDurationMs, in execution order.
  lastQuerySteps: QueryStep[] = $state([]);
  // Latest successful timing per panel, collected while a full refresh runs
  // and snapshotted into lastQuerySteps when it completes. Offsets are
  // relative to refreshStartedAt.
  private stepTimings = new Map<UsageStep, QueryStep>();
  private refreshStartedAt = 0;
  // The full refresh running now, drawn live by the refresh control.
  readonly liveQuery = new LiveQuery();
  hasNewData: boolean = $state(false);

  loading = $state({
    summary: false,
    pairwise: false,
    topSessions: false,
    zoom: false,
  });
  querying = $state<Record<UsagePanel, boolean>>({
    summary: false,
    comparison: false,
    pairwise: false,
    topSessions: false,
    zoom: false,
  });
  errors = $state<Record<Endpoint, string | null>>({
    summary: null,
    pairwise: null,
    topSessions: null,
    zoom: null,
  });

  toggles: Toggles = $state(loadToggles());

  private versions: Record<Endpoint, number> = {
    summary: 0,
    pairwise: 0,
    topSessions: 0,
    zoom: 0,
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
    const range = this.selectedTimeRange ?? {
      from: this.from,
      to: this.to,
    };
    const p: UsageParams = {
      from: range.from,
      to: range.to,
      timezone: this.timezone,
      project: sessionFilters.project || undefined,
      machine: sessionFilters.machine || undefined,
      agent: sessionFilters.agent || undefined,
      termination: sessionFilters.termination || undefined,
      min_user_messages:
        sessionFilters.minUserMessages > 0 ? sessionFilters.minUserMessages : undefined,
      include_one_shot: sessionFilters.includeOneShot,
      include_automated: sessionFilters.includeAutomated || undefined,
      active_since: sessionFilters.recentlyActive
        ? new Date(Date.now() - 24 * 60 * 60 * 1000).toISOString()
        : undefined,
    };
    if (sessionFilters.hideUnknownProject && sessionFilters.project !== "unknown") {
      p.exclude_project = joinCsvParts(this.excludedProjects, "unknown");
    } else if (this.excludedProjects) {
      p.exclude_project = this.excludedProjects;
    }
    if (this.excludedProjectKeys) {
      p.exclude_project_key = this.excludedProjectKeys;
    }
    if (this.excludedAgents) {
      p.exclude_agent = this.excludedAgents;
    }
    if (this.excludedModels) {
      p.exclude_model = this.excludedModels;
    }
    if (this.selectedProjectKey) p.project_key = this.selectedProjectKey;
    if (this.selectedModel) p.model = this.selectedModel;
    return p;
  }

  private attributionParams(): UsageParams | undefined {
    const by = this.toggles.attribution.groupBy;
    if (!this.hasSelection(by)) return undefined;
    const params = this.baseParams();
    if (by === "project") delete params.project_key;
    else if (by === "model") delete params.model;
    else delete params.agent;
    return params;
  }

  get timeSeriesSummary(): UsageSummaryResponse | null {
    return this.timeSeriesContextSummary ?? this.summary;
  }

  // Chart colors and legend rank from this, so selecting never recolors or reorders series.
  get colorSummary(): UsageSummaryResponse | null {
    return (this.hasSelection(this.toggles.attribution.groupBy) && this.referenceSummary) || this.timeSeriesSummary;
  }

  get pairwiseModelOptions(): string[] {
    return (this.summary?.modelTotals ?? []).map((entry) => entry.model);
  }

  get pairwiseProjectOptions(): string[] {
    return (this.summary?.projectTotals ?? []).map((entry) => entry.project_key);
  }

  pairwiseProjectLabel(key: string): string {
    return this.summary?.projectTotals.find((entry) => entry.project_key === key)?.project ?? "";
  }

  mergeKnownProjects(
    projects: Array<{ project_key: string; project: string }>,
    counts: Record<string, number>,
  ): void {
    if (projects.length === 0) return;
    const byKey = new Map(this.knownProjects.map((project) => [project.id, project]));
    let changed = false;
    for (const project of projects) {
      if (!project.project_key || !project.project) continue;
      const existing = byKey.get(project.project_key);
      const count = counts[project.project_key];
      if (!existing || existing.name !== project.project || existing.count !== count) {
        byKey.set(project.project_key, {
          id: project.project_key,
          name: project.project,
          count,
        });
        changed = true;
      }
    }
    if (changed) {
      this.knownProjects = [...byKey.values()].sort(
        (a, b) => a.name.localeCompare(b.name) || a.id.localeCompare(b.id),
      );
    }
  }

  private pairwiseOptionsFor(dimension: UsagePairwiseDimension): string[] {
    return dimension === "project" ? this.pairwiseProjectOptions : this.pairwiseModelOptions;
  }

  private preferredPairwiseValue(dimension: UsagePairwiseDimension, fallback: string): string {
    const options = this.pairwiseOptionsFor(dimension);
    for (const option of options) {
      if (option !== fallback) return option;
    }
    return options[0] ?? "";
  }

  private ensurePairwiseSelection(): boolean {
    const current = this.pairwiseSelection;
    const currentLeftOptions = this.pairwiseOptionsFor(current.left.dimension);
    const currentRightOptions = this.pairwiseOptionsFor(current.right.dimension);
    const leftValid = current.left.value !== "" && currentLeftOptions.includes(current.left.value);
    const rightValid =
      current.right.value !== "" && currentRightOptions.includes(current.right.value);
    if (leftValid && rightValid) return false;

    const modelOptions = this.pairwiseModelOptions;
    const projectOptions = this.pairwiseProjectOptions;
    let next = emptyPairwiseSelection();
    if (modelOptions.length >= 2) {
      next = {
        left: { dimension: "model", value: modelOptions[0] ?? "" },
        right: { dimension: "model", value: modelOptions[1] ?? "" },
      };
    } else if (projectOptions.length >= 2) {
      next = {
        left: { dimension: "project", value: projectOptions[0] ?? "" },
        right: { dimension: "project", value: projectOptions[1] ?? "" },
      };
    } else if (modelOptions.length > 0 && projectOptions.length > 0) {
      next = {
        left: { dimension: "model", value: modelOptions[0] ?? "" },
        right: { dimension: "project", value: projectOptions[0] ?? "" },
      };
    } else {
      next = emptyPairwiseSelection();
    }
    if (samePairwiseSelection(current, next)) {
      return false;
    }
    this.pairwiseSelection = next;
    return true;
  }

  private clearPairwiseComparisonState(): void {
    this.pairwiseComparison = null;
    this.errors.pairwise = null;
  }

  applyDateRange(from: string, to: string) {
    if (this.isPinned && this.from === from && this.to === to) return;
    this.selectedTimeRange = null;
    this.timeSeriesContextSummary = null;
    this.isTimeRangeSummaryProvisional = false;
    this.isPinned = true;
    this.from = from;
    this.to = to;
  }

  applyRollingWindow(days: number) {
    const range = rollingRange(days);
    if (
      !this.isPinned &&
      this.windowDays === days &&
      this.from === range.from &&
      this.to === range.to
    )
      return;
    this.selectedTimeRange = null;
    this.timeSeriesContextSummary = null;
    this.isTimeRangeSummaryProvisional = false;
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

  setTimeRange(from: string, to: string) {
    if (
      from === to ||
      (this.selectedTimeRange?.from === from && this.selectedTimeRange.to === to)
    ) {
      return;
    }
    if (this.selectedTimeRange === null) {
      this.timeSeriesContextSummary = this.summary;
    }
    this.selectedTimeRange = { from, to };
    if (this.timeSeriesContextSummary) {
      this.summary = summaryForDateRange(this.timeSeriesContextSummary, from, to);
      this.isTimeRangeSummaryProvisional = true;
    }
    if (this.referenceSummary) this.attributionSummary = summaryForDateRange(this.referenceSummary, from, to);
    this.topSessions = null;
    this.errors.topSessions = null;
    void this.fetchAll({ preserveTimeRange: true, refreshTimeSeriesContext: this.fullWindowPending });
  }

  clearTimeRange(refresh = true) {
    if (this.selectedTimeRange === null) return;
    this.selectedTimeRange = null;
    if (this.timeSeriesContextSummary) {
      this.summary = this.timeSeriesContextSummary;
      this.timeSeriesContextSummary = null;
    }
    if (this.referenceSummary) this.attributionSummary = this.referenceSummary;
    this.isTimeRangeSummaryProvisional = false;
    if (refresh) void this.fetchAll();
  }

  setPairwiseSide(side: UsagePairwiseSide, updates: Partial<UsagePairwiseSideSelection>): void {
    const next: UsagePairwiseSelection = {
      left: { ...this.pairwiseSelection.left },
      right: { ...this.pairwiseSelection.right },
    };
    const prev = next[side];
    const dimension = updates.dimension ?? prev.dimension;
    const options = this.pairwiseOptionsFor(dimension);
    const value =
      updates.value ??
      (options.includes(prev.value) && prev.dimension === dimension
        ? prev.value
        : this.preferredPairwiseValue(dimension, next[side === "left" ? "right" : "left"].value));

    next[side] = { dimension, value };
    this.pairwiseSelection = next;
    if (this.summary) {
      this.clearPairwiseComparisonState();
      void this.fetchPairwise(this.versions.summary, this.baseParams());
    }
  }

  isSelected(by: GroupBy, id: string): boolean {
    return by === "agent"
      ? sessions.isAgentSelected(id)
      : (by === "project" ? this.selectedProjectKey : this.selectedModel) === id;
  }

  hasSelection(by: GroupBy): boolean {
    return (by === "agent" ? sessions.filters.agent : by === "project" ? this.selectedProjectKey : this.selectedModel) !== "";
  }

  clearSelection(by: GroupBy): void {
    if (by === "agent") {
      sessions.filters.agent = "";
      return;
    }
    if (by === "project") {
      this.selectedProjectKey = "";
      this.backToProjects();
    } else this.selectedModel = "";
    void this.fetchAll({ preserveTimeRange: true });
  }

  toggleSelection(by: GroupBy, id: string): void {
    const clear = this.isSelected(by, id);
    if (by === "agent") {
      sessions.filters.agent = clear ? sessions.filters.agent.split(",").filter((agent) => agent !== id).join(",") : id;
      return;
    }
    if (by === "project") {
      this.selectedProjectKey = clear ? "" : id;
      if (clear) this.backToProjects();
    } else this.selectedModel = clear ? "" : id;
    void this.fetchAll({ preserveTimeRange: true });
  }

  private dropExcludedSelection(): void {
    sessions.filters.agent = sessions.filters.agent.split(",").filter((agent) => !this.isAgentExcluded(agent)).join(",");
    if (this.selectedProjectKey && this.isProjectKeyExcluded(this.selectedProjectKey)) {
      this.selectedProjectKey = "";
      this.backToProjects();
    }
    if (this.selectedModel && this.isModelExcluded(this.selectedModel)) this.selectedModel = "";
  }

  // Toggle an item's exclusion. Clicking an included item
  // excludes it; clicking an excluded item re-includes it.
  toggleProject(name: string): void {
    this.excludedProjects = this.toggleCsv(this.excludedProjects, name);
    this.fetchAll();
  }

  toggleProjectKey(key: string, options: { preserveTimeRange?: boolean } = {}): void {
    const previous = this.excludedProjectKeys;
    const hadSelectedTimeRange = options.preserveTimeRange && this.selectedTimeRange !== null;
    this.excludedProjectKeys = this.toggleCsv(this.excludedProjectKeys, key);
    const changed = this.excludedProjectKeys;
    void this.fetchAllWithResult(options).then((result) => {
      if (result !== "error" || !hadSelectedTimeRange || this.excludedProjectKeys !== changed)
        return;
      this.excludedProjectKeys = previous;
      void this.fetchAll({ preserveTimeRange: true });
    });
  }

  toggleAgent(name: string, options: { preserveTimeRange?: boolean } = {}): void {
    const previous = this.excludedAgents;
    const hadSelectedTimeRange = options.preserveTimeRange && this.selectedTimeRange !== null;
    this.excludedAgents = this.toggleCsv(this.excludedAgents, name);
    const changed = this.excludedAgents;
    void this.fetchAllWithResult(options).then((result) => {
      if (result !== "error" || !hadSelectedTimeRange || this.excludedAgents !== changed) return;
      this.excludedAgents = previous;
      void this.fetchAll({ preserveTimeRange: true });
    });
  }

  toggleModel(name: string, options: { preserveTimeRange?: boolean } = {}): void {
    const previous = this.excludedModels;
    const hadSelectedTimeRange = options.preserveTimeRange && this.selectedTimeRange !== null;
    this.excludedModels = this.toggleCsv(this.excludedModels, name);
    const changed = this.excludedModels;
    void this.fetchAllWithResult(options).then((result) => {
      if (result !== "error" || !hadSelectedTimeRange || this.excludedModels !== changed) return;
      this.excludedModels = previous;
      void this.fetchAll({ preserveTimeRange: true });
    });
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

  isProjectKeyExcluded(key: string): boolean {
    if (!this.excludedProjectKeys) return false;
    return this.excludedProjectKeys.split(",").includes(key);
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
    this.excludedProjectKeys = "";
    this.fetchAll();
  }

  deselectAllProjectKeys(all: string[]): void {
    const excluded = new Set(
      this.excludedProjectKeys ? this.excludedProjectKeys.split(",").filter(Boolean) : [],
    );
    for (const key of all) excluded.add(key);
    this.excludedProjectKeys = [...excluded].join(",");
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
    this.excludedModels = joinCsvParts(this.excludedModels, all.join(","));
    this.fetchAll();
  }

  clearFilters(refresh = true): void {
    this.selectedProjectKey = "";
    this.selectedModel = "";
    this.backToProjects();
    this.excludedProjects = "";
    this.excludedProjectKeys = "";
    this.excludedAgents = "";
    this.excludedModels = "";
    if (refresh) this.fetchAll();
  }

  get hasActiveFilters(): boolean {
    return (
      this.selectedProjectKey !== "" ||
      this.selectedModel !== "" ||
      this.excludedProjects !== "" ||
      this.excludedProjectKeys !== "" ||
      this.excludedAgents !== "" ||
      this.excludedModels !== ""
    );
  }

  get isQuerying(): boolean {
    return Object.values(this.querying).some(Boolean);
  }

  setMode(mode: UsageMode): boolean {
    if (this.mode === mode) return false;
    this.mode = mode;
    if (this.zoomedProject?.key) void this.fetchZoom();
    this.invalidatePanel("topSessions");
    this.topSessions = null;
    this.errors.topSessions = null;
    this.loading.topSessions = false;
    return true;
  }

  setSelectedTokenTypes(selected: readonly UsageTokenType[]): boolean {
    const canonical = canonicalTokenTypes(selected);
    if (canonical.length === 0) return false;
    if (
      canonical.length === this.selectedTokenTypes.length &&
      canonical.every((tokenType, index) => tokenType === this.selectedTokenTypes[index])
    ) {
      return false;
    }
    this.selectedTokenTypes = canonical;
    if (this.zoomedProject?.key) void this.fetchZoom();
    this.invalidatePanel("topSessions");
    this.topSessions = null;
    this.errors.topSessions = null;
    this.loading.topSessions = false;
    return true;
  }

  setTimeSeriesGroupBy(g: GroupBy) {
    this.setGroupBy(g);
  }

  setTimeSeriesView(v: TimeSeriesView) {
    this.toggles.timeSeries.view = v;
    saveToggles(this.toggles);
  }

  setAttributionGroupBy(g: GroupBy) {
    this.setGroupBy(g);
  }

  private setGroupBy(g: GroupBy): void {
    const previous = this.toggles.attribution.groupBy;
    if (g === previous) return;
    this.toggles.timeSeries.groupBy = g;
    this.toggles.attribution.groupBy = g;
    saveToggles(this.toggles);
    this.backToProjects();
    if (this.hasSelection(previous) || this.hasSelection(g))
      void this.fetchAll({ preserveTimeRange: true });
  }

  setAttributionView(v: AttributionView) {
    this.toggles.attribution.view = v;
    saveToggles(this.toggles);
  }

  private rollDates(): void {
    if (this.isPinned) return;
    const { from, to } = rollingRange(this.windowDays);
    this.from = from;
    this.to = to;
  }

  async fetchAll(options: FetchAllOptions = {}): Promise<void> {
    await this.fetchAllWithResult(options);
  }

  private async fetchAllWithResult(options: FetchAllOptions = {}): Promise<FetchResult> {
    this.dropExcludedSelection();
    const startedAt = performance.now();
    this.stepTimings.clear();
    this.refreshStartedAt = startedAt;
    const live = this.liveQuery.begin(startedAt);
    try {
      return await this.fetchAllSteps(startedAt, options);
    } finally {
      this.liveQuery.end(live);
    }
  }

  private async fetchAllSteps(startedAt: number, options: FetchAllOptions): Promise<FetchResult> {
    const selectedRangeAtStart = this.selectedTimeRange ? { ...this.selectedTimeRange } : null;
    if (!options.preserveTimeRange && this.selectedTimeRange !== null) {
      this.selectedTimeRange = null;
      if (this.timeSeriesContextSummary) {
        this.summary = this.timeSeriesContextSummary;
        this.timeSeriesContextSummary = null;
      }
      this.isTimeRangeSummaryProvisional = false;
    }
    const fetchVersion = ++this.fetchAllVersion;
    this.invalidatePanel("pairwise");
    this.invalidatePanel("zoom");
    this.invalidatePanel("topSessions");
    this.rollDates();
    saveUsageFilters(this);
    const params = this.baseParams();
    const contextParams =
      options.preserveTimeRange &&
      options.refreshTimeSeriesContext !== false &&
      selectedRangeAtStart !== null
        ? { ...params, from: this.from, to: this.to }
        : undefined;
    if (contextParams || this.selectedTimeRange === null) this.fullWindowPending = true;
    const summaryPromise = this.fetchSummary({
      loadComparison: false,
      params,
      contextParams,
    });
    const topSessionsPromise = this.fetchTopSessions(params);
    const zoomPromise = this.fetchZoom(params);
    const loadedSummary = await summaryPromise;
    if (fetchVersion !== this.fetchAllVersion) {
      await Promise.all([topSessionsPromise, zoomPromise]);
      return "aborted";
    }
    if (!loadedSummary) {
      await Promise.all([topSessionsPromise, zoomPromise]);
      if (fetchVersion !== this.fetchAllVersion) return "aborted";
      if (
        selectedRangeAtStart !== null &&
        this.selectedTimeRange === null &&
        fetchVersion === this.fetchAllVersion
      ) {
        this.invalidatePanel("topSessions");
        this.topSessions = null;
        this.errors.topSessions = null;
        await Promise.all([
          this.fetchTopSessions(this.baseParams()),
          this.fetchZoom(this.baseParams()),
        ]);
      }
      return "error";
    }
    const currentTopSessionsPromise = loadedSummary.projectScopeRecovered
      ? topSessionsPromise.then(() => {
          if (fetchVersion !== this.fetchAllVersion) return "aborted";
          return this.fetchTopSessions(loadedSummary.params);
        })
      : topSessionsPromise;
    const currentZoomPromise = loadedSummary.projectScopeRecovered
      ? zoomPromise.then(() => {
          if (fetchVersion !== this.fetchAllVersion) return "aborted";
          return this.fetchZoom(loadedSummary.params);
        })
      : zoomPromise;
    const [topSessionsResult, loadedZoomResult, comparisonResult, pairwiseResult] =
      await Promise.all([
        currentTopSessionsPromise,
        currentZoomPromise,
        this.fetchComparison(loadedSummary.version, loadedSummary.summary, loadedSummary.params),
        this.fetchPairwise(loadedSummary.version, loadedSummary.params),
      ]);
    const zoomResult = this.zoomedProject === null ? "ok" : loadedZoomResult;
    if (
      fetchVersion === this.fetchAllVersion &&
      topSessionsResult === "ok" &&
      zoomResult === "ok" &&
      comparisonResult === "ok" &&
      pairwiseResult === "ok"
    ) {
      this.markRefreshComplete(startedAt);
      return "ok";
    }
    if (
      fetchVersion !== this.fetchAllVersion ||
      topSessionsResult === "aborted" ||
      zoomResult === "aborted" ||
      comparisonResult === "aborted" ||
      pairwiseResult === "aborted"
    ) {
      return "aborted";
    }
    return "error";
  }

  async fetchSummary(
    options: {
      loadComparison?: boolean;
      params?: UsageParams;
      contextParams?: UsageParams;
      recoverProjectScope?: boolean;
    } = {},
  ): Promise<LoadedUsageSummary | null> {
    const loadComparison = options.loadComparison ?? true;
    const recoverProjectScope = options.recoverProjectScope ?? true;
    const attributionParams = this.attributionParams();
    const v = ++this.versions.summary;
    this.abortPanel("comparison");
    this.abortPanel("pairwise");
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
    const liveStep = this.liveQuery.start("summary", started);
    const liveContextStep = options.contextParams
      ? this.liveQuery.start("contextSummary", started)
      : 0;
    const liveAttributionStep = attributionParams ? this.liveQuery.start("attributionSummary", started) : 0;
    // Unbrushed, the attribution request already covers the full window.
    const referenceParams = attributionParams && options.contextParams ? { ...attributionParams, from: options.contextParams.from, to: options.contextParams.to } : undefined;
    const liveReferenceStep = referenceParams ? this.liveQuery.start("referenceSummary", started) : 0;
    let status: Extract<PerfEntryStatus, "ok" | "error" | "aborted"> = "ok";
    const params = options.params ?? this.baseParams();
    try {
      const contextParams = options.contextParams;
      let data: UsageSummaryResponse;
      let contextData: UsageSummaryResponse | null = null;
      let attributionData: UsageSummaryResponse | null = null;
      let referenceData: UsageSummaryResponse | null = null;
      if (!contextParams && !attributionParams) {
        data = await UsageService.getApiV1UsageSummary(params, { signal });
      } else {
        [data, contextData, attributionData, referenceData] = await Promise.all([
          UsageService.getApiV1UsageSummary(params, { signal }),
          contextParams ? UsageService.getApiV1UsageSummary(contextParams, { signal }) : null,
          attributionParams ? UsageService.getApiV1UsageSummary(attributionParams, { signal }) : null,
          referenceParams ? UsageService.getApiV1UsageSummary(referenceParams, { signal }) : null,
        ]);
      }
      if (this.versions.summary === v) {
        this.summary = data;
        this.summarySelection = selectionKey(params);
        if (contextData !== null || this.selectedTimeRange === null) this.fullWindowPending = false;
        this.attributionSummary = attributionData;
        if (!attributionParams) this.referenceSummary = null;
        else if (referenceData || !this.selectedTimeRange) this.referenceSummary = referenceData ?? attributionData;
        // Both responses are applied together, so each request's apply
        // phase starts once the later body has arrived; the earlier one
        // shows a gap while it waited for its sibling.
        const bodies = [data, contextData, attributionData, referenceData]
          .map((body) => responseTimingOf(body)?.bodyAt)
          .filter((at): at is number => at !== undefined);
        const applyStartedAt = bodies.length > 0 ? Math.max(...bodies) : undefined;
        this.noteStep("summary", liveStep, started, data, applyStartedAt);
        if (contextData !== null) {
          this.noteStep("contextSummary", liveContextStep, started, contextData, applyStartedAt);
        }
        if (attributionData)
          this.noteStep(
            "attributionSummary",
            liveAttributionStep,
            started,
            attributionData,
            applyStartedAt,
          );
        if (referenceData) this.noteStep("referenceSummary", liveReferenceStep, started, referenceData, applyStartedAt);
        this.isTimeRangeSummaryProvisional = false;
        if (contextData !== null) {
          this.timeSeriesContextSummary = contextData;
        } else if (this.selectedTimeRange === null) {
          this.timeSeriesContextSummary = null;
        }
        this.errors.summary = null;
        this.ensurePairwiseSelection();
        this.clearPairwiseComparisonState();
        const loaded = {
          version: v,
          summary: data,
          params,
          projectScopeRecovered: false,
        };
        if (loadComparison) {
          void this.fetchComparison(v, data, params);
          void this.fetchPairwise(v, params);
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
      if (
        recoverProjectScope &&
        this.versions.summary === v &&
        (this.excludedProjectKeys !== "" || this.selectedProjectKey !== "") &&
        isUnknownProjectKeyError(e)
      ) {
        this.excludedProjectKeys = "";
        if (this.selectedProjectKey) {
          this.selectedProjectKey = "";
          this.backToProjects();
        }
        this.abortPanel("topSessions");
        const recoveredParams = this.baseParams();
        const loaded = await this.fetchSummary({
          loadComparison,
          params: recoveredParams,
          contextParams: options.contextParams
            ? {
                ...recoveredParams,
                from: options.contextParams.from,
                to: options.contextParams.to,
              }
            : undefined,
          recoverProjectScope: false,
        });
        return loaded === null ? null : { ...loaded, projectScopeRecovered: true };
      }
      if (this.versions.summary === v) {
        // A selected-range summary is synthesized from daily data while the
        // request is in flight. If that request fails, restore the parent
        // window rather than leaving provisional values under an active brush.
        // Other cached refetch failures keep their existing values visible.
        if (
          this.isTimeRangeSummaryProvisional &&
          this.selectedTimeRange !== null &&
          this.timeSeriesContextSummary !== null
        ) {
          this.summary = this.timeSeriesContextSummary;
          this.selectedTimeRange = null;
          this.timeSeriesContextSummary = null;
          this.isTimeRangeSummaryProvisional = false;
          this.errors.summary = e instanceof Error ? e.message : m.shared_failed_to_load();
          // The reference already holds this scope's full-window attribution.
          this.attributionSummary = this.referenceSummary;
          const restoredAttributionParams = this.attributionSummary ? undefined : this.attributionParams();
          if (restoredAttributionParams) {
            try {
              const restoredAttribution = await UsageService.getApiV1UsageSummary(restoredAttributionParams, { signal });
              if (this.versions.summary === v) {
                this.attributionSummary = restoredAttribution;
                this.referenceSummary = restoredAttribution;
              }
            } catch (attributionError) {
              if (!isAbortError(attributionError)) console.warn("usage attribution restore failed:", attributionError);
            }
          }
        } else if (this.summary === null || (this.summarySelection !== null && selectionKey(params) !== this.summarySelection)) {
          this.errors.summary = e instanceof Error ? e.message : m.shared_failed_to_load();
        } else {
          console.warn("usage.fetchSummary refetch failed:", e);
        }
      }
    } finally {
      this.recordStep("summary", started, status);
      this.liveQuery.abandon(liveStep);
      this.liveQuery.abandon(liveContextStep);
      this.liveQuery.abandon(liveAttributionStep);
      this.liveQuery.abandon(liveReferenceStep);
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
    const liveStep = this.liveQuery.start("comparison", started);
    let status: Extract<PerfEntryStatus, "ok" | "error" | "aborted"> = "ok";
    try {
      const comparison = await UsageService.getApiV1UsageComparison(
        {
          ...params,
          current_microdollars: summary.totals.totalCost.microdollars,
        },
        { signal },
      );
      if (this.versions.summary === summaryVersion) {
        this.summary = { ...summary, comparison };
        this.noteStep("comparison", liveStep, started, comparison);
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
      this.recordStep("comparison", started, status);
      this.liveQuery.abandon(liveStep);
      this.clearAbortSignal("comparison", signal);
    }
  }

  private currentPairwiseParams(params: UsageParams): UsagePairwiseParams | null {
    const selection = this.pairwiseSelection;
    if (!selection.left.value || !selection.right.value) {
      return null;
    }
    return {
      ...params,
      left_dimension: selection.left.dimension,
      left_value: selection.left.value,
      right_dimension: selection.right.dimension,
      right_value: selection.right.value,
    };
  }

  private async fetchPairwise(summaryVersion: number, params: UsageParams): Promise<FetchResult> {
    if (this.versions.summary !== summaryVersion) return "aborted";
    const pairwiseVersion = ++this.versions.pairwise;
    const request = this.currentPairwiseParams(params);
    if (!request) {
      this.pairwiseComparison = null;
      this.errors.pairwise = null;
      this.loading.pairwise = false;
      this.abortPanel("pairwise");
      return "ok";
    }
    const signal = this.nextAbortSignal("pairwise");
    const isFirstLoad = this.pairwiseComparison === null;
    if (isFirstLoad) this.loading.pairwise = true;
    if (isFirstLoad) this.errors.pairwise = null;
    const started = performance.now();
    const liveStep = this.liveQuery.start("pairwise", started);
    let status: Extract<PerfEntryStatus, "ok" | "error" | "aborted"> = "ok";
    try {
      const comparison = await UsageService.getApiV1UsagePairwiseComparison(request, { signal });
      if (this.versions.summary === summaryVersion && this.versions.pairwise === pairwiseVersion) {
        this.pairwiseComparison = comparison;
        this.errors.pairwise = null;
        this.noteStep("pairwise", liveStep, started, comparison);
        return "ok";
      }
      return "aborted";
    } catch (e) {
      if (isAbortError(e)) {
        status = "aborted";
        return "aborted";
      }
      status = "error";
      if (this.versions.summary === summaryVersion && this.versions.pairwise === pairwiseVersion) {
        if (this.pairwiseComparison === null) {
          this.errors.pairwise = e instanceof Error ? e.message : m.shared_failed_to_load();
        } else {
          console.warn("usage.fetchPairwise failed:", e);
        }
      }
      return "error";
    } finally {
      this.recordStep("pairwise", started, status);
      this.liveQuery.abandon(liveStep);
      this.clearAbortSignal("pairwise", signal);
      if (this.versions.summary === summaryVersion && this.versions.pairwise === pairwiseVersion) {
        this.loading.pairwise = false;
      }
    }
  }

  backToProjects(): void {
    this.invalidatePanel("zoom");
    this.zoomed = false;
    this.zoomRows = null;
    this.errors.zoom = null;
  }

  setOpenProject(key: string | null): void {
    if (key === null) {
      this.backToProjects();
      return;
    }
    const needsFocus = !this.isSelected("project", key);
    this.selectedProjectKey = key;
    this.zoomed = true;
    if (needsFocus) void this.fetchAll({ preserveTimeRange: true });
    else void this.fetchZoom();
  }

  private async fetchZoom(params: UsageParams = this.baseParams()): Promise<FetchResult> {
    const projectKey = this.zoomedProject?.key;
    if (!projectKey) return "ok";
    return this.fetchRanking("zoom", { ...params, group_by: "group", limit: 100 });
  }

  async fetchTopSessions(params: UsageParams | null = null): Promise<FetchResult> {
    return this.fetchRanking("topSessions", params ?? this.baseParams());
  }

  private async fetchRanking(
    panel: "topSessions" | "zoom",
    query: UsageParams & { group_by?: "group"; limit?: number },
  ): Promise<FetchResult> {
    const version = ++this.versions[panel];
    const signal = this.nextAbortSignal(panel);
    if (panel === "zoom") this.zoomRows = null;
    const isFirstLoad = (panel === "zoom" ? this.zoomRows : this.topSessions) === null;
    if (isFirstLoad) {
      this.loading[panel] = true;
      this.errors[panel] = null;
    }
    const started = performance.now();
    const liveStep = this.liveQuery.start(panel, started);
    let status: Extract<PerfEntryStatus, "ok" | "error" | "aborted"> = "ok";
    try {
      const data = await UsageService.getApiV1UsageTopSessions(
        {
          ...query,
          sort: this.mode === "token" ? "tokens" : "cost",
          token_types:
            this.mode === "token" && this.selectedTokenTypes.length < ALL_TOKEN_TYPES.length
              ? this.selectedTokenTypes.join(",")
              : undefined,
        },
        { signal },
      );
      if (this.versions[panel] !== version) return "aborted";
      if (panel === "zoom") this.zoomRows = data;
      else this.topSessions = data;
      this.errors[panel] = null;
      this.noteStep(panel, liveStep, started, data);
      return "ok";
    } catch (error) {
      status = isAbortError(error) ? "aborted" : "error";
      if (status === "error" && this.versions[panel] === version) {
        if ((panel === "zoom" ? this.zoomRows : this.topSessions) === null) {
          this.errors[panel] = error instanceof Error ? error.message : m.shared_failed_to_load();
        } else console.warn(`usage.${panel} refetch failed:`, error);
      }
      return status;
    } finally {
      this.recordStep(panel, started, status);
      this.liveQuery.abandon(liveStep);
      this.clearAbortSignal(panel, signal);
      if (this.versions[panel] === version) this.loading[panel] = false;
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
    if (panel === "pairwise" || panel === "zoom") {
      this.loading[panel] = false;
    }
  }

  private nextAbortSignal(panel: UsagePanel): AbortSignal {
    this.abortControllers[panel]?.abort();
    const controller = new AbortController();
    this.abortControllers[panel] = controller;
    this.querying[panel] = true;
    return controller.signal;
  }

  private clearAbortSignal(panel: UsagePanel, signal: AbortSignal): boolean {
    if (this.abortControllers[panel]?.signal === signal) {
      delete this.abortControllers[panel];
      this.querying[panel] = false;
      return true;
    }
    return false;
  }

  cancelInFlightReads(): void {
    this.fetchAllVersion++;
    this.liveQuery.end();
    this.versions.summary++;
    this.versions.pairwise++;
    this.versions.topSessions++;
    this.versions.zoom++;
    for (const panel of Object.keys(this.abortControllers) as UsagePanel[]) {
      this.abortControllers[panel]?.abort();
      delete this.abortControllers[panel];
      this.querying[panel] = false;
    }
    this.loading.summary = false;
    this.loading.pairwise = false;
    this.loading.topSessions = false;
    this.loading.zoom = false;
    this.zoomRows = null;
  }

  private markRefreshComplete(startedAt: number): void {
    this.lastUpdatedAt = Date.now();
    this.lastQueryDurationMs = performance.now() - startedAt;
    this.lastQuerySteps = USAGE_STEP_ORDER.flatMap((name) => this.stepTimings.get(name) ?? []);
    this.hasNewData = false;
  }

  private recordStep(
    panel: UsagePanel,
    startedAt: number,
    status: "ok" | "error" | "aborted",
  ): void {
    perf.recordPanel({
      route: "usage",
      name: panel,
      durationMs: performance.now() - startedAt,
      status,
    });
  }

  // Called once a request's data is applied: the step spans request sent to
  // data applied and carries the request's wait/download/apply phases.
  // `liveStep` is the handle the live query gave the request when it began.
  private noteStep(
    step: UsageStep,
    liveStep: number,
    startedAt: number,
    data: unknown,
    applyStartedAt?: number,
  ): void {
    const timing = queryStepFrom(
      step,
      responseTimingOf(data),
      startedAt,
      performance.now(),
      this.refreshStartedAt,
      applyStartedAt,
    );
    this.stepTimings.set(step, timing);
    this.liveQuery.settle(liveStep, timing);
  }
}

export const usage = new UsageStore();

export interface UsageUrlState {
  from: string;
  to: string;
  isPinned: boolean;
  windowDays: number;
  excludedProjects: string;
  excludedProjectKeys: string;
  excludedAgents: string;
  excludedModels: string;
}

export const USAGE_DEFAULT_WINDOW_DAYS = DEFAULT_WINDOW_DAYS;

export function parseWindowDays(raw: string | undefined): number | null {
  if (!raw) return null;
  const n = Number.parseInt(raw, 10);
  if (!Number.isFinite(n) || n <= 0 || n > MAX_WINDOW_DAYS || String(n) !== raw) {
    return null;
  }
  return n;
}

export function buildUsageUrlParams(state: UsageUrlState): Record<string, string> {
  const params: Record<string, string> = {};
  if (state.isPinned) {
    if (state.from) params["from"] = state.from;
    if (state.to) params["to"] = state.to;
  } else if (state.windowDays > 0 && state.windowDays !== DEFAULT_WINDOW_DAYS) {
    params["window_days"] = String(state.windowDays);
  }
  if (state.excludedModels) {
    params["exclude_model"] = state.excludedModels;
  }
  if (state.excludedProjects) {
    params["exclude_project"] = state.excludedProjects;
  }
  // Shared-store project keys are scoped to the current aggregate archive
  // set. Keep them in live request state only; URLs outlive that scope.
  if (state.excludedAgents) {
    params["exclude_agent"] = state.excludedAgents;
  }
  return params;
}

const CSV_MERGE_URL_KEYS = new Set(["exclude_project"]);
const SESSION_DATE_URL_KEYS = new Set(["date", "date_from", "date_to"]);

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
