import type {
  UsageComparison,
  UsagePairwiseComparisonResponse,
  UsagePairwiseDimension,
  UsageSummaryResponse,
  TopUsageSessionsResponse,
} from "../api/types/usage.js";
import { MetadataService, UsageService } from "../api/generated/index";
import {
  ApiError,
  callGenerated,
  configureGeneratedClient,
  isAbortError,
} from "../api/runtime.js";
import { sessions } from "./sessions.svelte.js";
import { sync } from "./sync.svelte.js";
import { perf, type PerfEntryStatus } from "./perf.svelte.js";
import { rollingRange, today } from "../utils/dates.js";
import {
  BRANCH_LIST_SEP,
  NO_BRANCH_MATCH_TOKEN,
  splitBranchFilterToken,
} from "../branchFilters.js";
import { toggleListValue } from "../utils/lists.js";
import type { BranchInfo } from "../api/types/core.js";

type UsageParams = Parameters<typeof UsageService.getApiV1UsageSummary>[0];
type UsagePairwiseParams =
  Parameters<typeof UsageService.getApiV1UsagePairwiseComparison>[0];
type UsagePanel = "summary" | "comparison" | "pairwise" | "topSessions";
type FetchResult = "ok" | "error" | "aborted";
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

export type GroupBy = "project" | "model" | "agent" | "branch";
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
  return (
    value === "project" ||
    value === "model" ||
    value === "agent" ||
    value === "branch"
  );
}

function isUnknownProjectKeyError(error: unknown): boolean {
  return error instanceof ApiError &&
    error.status === 400 &&
    error.code === "unknown_project_key";
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
// inside Date#setDate's safe range, so rollingRange(MAX_WINDOW_DAYS)
// always produces valid YYYY-MM-DD strings.
const MAX_WINDOW_DAYS = 36500;

const USAGE_FILTERS_KEY = "usage-filters";

export interface UsageFilterState {
  excludedProjects: string;
  excludedProjectKeys?: string;
  excludedAgents: string;
  selectedGitBranch: string;
  excludedModels: string;
  selectedModels: string;
}

function loadUsageFilters(): UsageFilterState {
  try {
    const raw = localStorage.getItem(USAGE_FILTERS_KEY);
    if (raw) {
      // Saved excludedGitBranch exclusion lists from the retired
      // exclude-mode branch filter are dropped: an exclusion set cannot
      // be mapped onto an include selection, so those views reset to
      // "all branches".
      const saved = JSON.parse(raw) as Partial<UsageFilterState>;
      return {
        excludedProjects: saved.excludedProjects ?? "",
        excludedProjectKeys: "",
        excludedAgents: saved.excludedAgents ?? "",
        selectedGitBranch: saved.selectedGitBranch ?? "",
        excludedModels: "",
        selectedModels: saved.selectedModels ?? "",
      };
    }
  } catch {
    // Corrupted localStorage — fall back to defaults.
  }
  return {
    excludedProjects: "",
    excludedProjectKeys: "",
    excludedAgents: "",
    selectedGitBranch: "",
    excludedModels: "",
    selectedModels: "",
  };
}

function saveUsageFilters(f: UsageFilterState): void {
  try {
    const data: UsageFilterState = {
      excludedProjects: f.excludedProjects,
      excludedAgents: f.excludedAgents,
      selectedGitBranch: f.selectedGitBranch,
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

type Endpoint = "summary" | "pairwise" | "topSessions";

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
  return left.left.dimension === right.left.dimension &&
    left.left.value === right.left.value &&
    left.right.dimension === right.right.dimension &&
    left.right.value === right.right.value;
}

class UsageStore {
  from: string = $state(rollingRange(DEFAULT_WINDOW_DAYS).from);
  to: string = $state(today());
  isPinned: boolean = $state(false);
  windowDays: number = $state(DEFAULT_WINDOW_DAYS);

  // Excluded project/agent items and included model/branch items
  // (separator-joined strings). Empty models/branches = all.
  // Branch selection is include-based: an exclusion complement would
  // grow with the branch catalog (deselect-all over thousands of
  // (project, branch) pairs) instead of with the user's selection.
  // Initialized from localStorage to survive tab switches.
  excludedProjects: string = $state("");
  excludedProjectKeys: string = $state("");
  excludedAgents: string = $state("");
  selectedGitBranch: string = $state("");
  excludedModels: string = $state("");
  selectedModels: string = $state("");

  constructor() {
    const saved = loadUsageFilters();
    this.excludedProjects = saved.excludedProjects;
    this.excludedProjectKeys = saved.excludedProjectKeys ?? "";
    this.excludedAgents = saved.excludedAgents;
    this.selectedGitBranch = saved.selectedGitBranch;
    this.excludedModels = saved.excludedModels;
    this.selectedModels = saved.selectedModels;
  }

  summary = $state<UsageSummaryResponse | null>(null);
  branches: BranchInfo[] = $state([]);
  private branchesLoaded = false;
  private branchesPromise: Promise<void> | null = null;
  private branchesVersion = 0;
  pairwiseComparison =
    $state<UsagePairwiseComparisonResponse | null>(null);
  pairwiseSelection = $state<UsagePairwiseSelection>(
    emptyPairwiseSelection(),
  );
  topSessions = $state<TopUsageSessionsResponse | null>(null);
  lastUpdatedAt: number | null = $state(null);
  hasNewData: boolean = $state(false);

  loading = $state({
    summary: false,
    pairwise: false,
    topSessions: false,
  });
  querying = $state<Record<UsagePanel, boolean>>({
    summary: false,
    comparison: false,
    pairwise: false,
    topSessions: false,
  });
  errors = $state<Record<Endpoint, string | null>>({
    summary: null,
    pairwise: null,
    topSessions: null,
  });

  toggles: Toggles = $state(loadToggles());

  private versions: Record<Endpoint, number> = {
    summary: 0,
    pairwise: 0,
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
      gitBranch: this.effectiveGitBranch(sessionFilters.branch),
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
    if (this.excludedProjectKeys) {
      p.excludeProjectKey = this.excludedProjectKeys;
    }
    if (this.excludedAgents) {
      p.excludeAgent = this.excludedAgents;
    }
    if (this.selectedModels) {
      p.model = this.selectedModels;
    }
    return p;
  }

  // A pinned sidebar project filter contradicts branch tokens from any
  // other project (project = X AND a branch of Y matches no session),
  // so drop off-project tokens from the local selection at query time.
  // The stored selection is left intact and takes effect again when
  // the project filter clears.
  private projectScopedLocalBranch(): string {
    const project = sessions.filters.project;
    const local = this.selectedGitBranch;
    if (!project || !local) return local;
    return local
      .split(BRANCH_LIST_SEP)
      .filter(
        (token) => splitBranchFilterToken(token).project === project,
      )
      .join(BRANCH_LIST_SEP);
  }

  // The sidebar branch filter and the usage page's own selection are
  // both include lists but share one git_branch API param, so AND them
  // by intersecting. If both controls are active and their selections
  // do not overlap, send a fail-closed token so the visible local
  // selection is not silently ignored.
  private effectiveGitBranch(
    sidebarBranch: string,
  ): string | undefined {
    const local = this.projectScopedLocalBranch();
    if (!sidebarBranch) return local || undefined;
    if (!local) return sidebarBranch;
    const sidebar = new Set(sidebarBranch.split(BRANCH_LIST_SEP));
    const both = local
      .split(BRANCH_LIST_SEP)
      .filter((token) => sidebar.has(token));
    return both.length > 0
      ? both.join(BRANCH_LIST_SEP)
      : NO_BRANCH_MATCH_TOKEN;
  }

  get pairwiseModelOptions(): string[] {
    return (this.summary?.modelTotals ?? []).map((entry) => entry.model);
  }

  get pairwiseProjectOptions(): string[] {
    return (this.summary?.projectTotals ?? []).map(
      (entry) => entry.project_key,
    );
  }

  pairwiseProjectLabel(key: string): string {
    return this.summary?.projectTotals.find(
      (entry) => entry.project_key === key,
    )?.project ?? "";
  }

  private pairwiseOptionsFor(
    dimension: UsagePairwiseDimension,
  ): string[] {
    return dimension === "project"
      ? this.pairwiseProjectOptions
      : this.pairwiseModelOptions;
  }

  private preferredPairwiseValue(
    dimension: UsagePairwiseDimension,
    fallback: string,
  ): string {
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
    const leftValid = current.left.value !== "" &&
      currentLeftOptions.includes(current.left.value);
    const rightValid = current.right.value !== "" &&
      currentRightOptions.includes(current.right.value);
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

  setPairwiseSide(
    side: UsagePairwiseSide,
    updates: Partial<UsagePairwiseSideSelection>,
  ): void {
    const next: UsagePairwiseSelection = {
      left: { ...this.pairwiseSelection.left },
      right: { ...this.pairwiseSelection.right },
    };
    const prev = next[side];
    const dimension = updates.dimension ?? prev.dimension;
    const options = this.pairwiseOptionsFor(dimension);
    const value = updates.value ??
      (options.includes(prev.value) && prev.dimension === dimension
        ? prev.value
        : this.preferredPairwiseValue(
            dimension,
            next[side === "left" ? "right" : "left"].value,
          ));

    next[side] = { dimension, value };
    this.pairwiseSelection = next;
    if (this.summary) {
      this.clearPairwiseComparisonState();
      void this.fetchPairwise(this.versions.summary, this.baseParams());
    }
  }

  // Toggle an item's exclusion. Clicking an included item
  // excludes it; clicking an excluded item re-includes it.
  toggleProject(name: string): void {
    this.excludedProjects = toggleListValue(
      this.excludedProjects, name, ",",
    );
    this.fetchAll();
  }

  toggleProjectKey(key: string): void {
    this.excludedProjectKeys = toggleListValue(
      this.excludedProjectKeys, key, ",",
    );
    this.fetchAll();
  }

  toggleAgent(name: string): void {
    this.excludedAgents = toggleListValue(
      this.excludedAgents, name, ",",
    );
    this.fetchAll();
  }

  toggleModel(name: string): void {
    this.selectedModels = toggleListValue(
      this.selectedModels, name, ",",
    );
    this.excludedModels = "";
    this.fetchAll();
  }

  async loadBranches(): Promise<void> {
    if (this.branchesLoaded) return;
    if (this.branchesPromise) return this.branchesPromise;
    const version = this.branchesVersion;
    this.branchesPromise = (async () => {
      try {
        configureGeneratedClient();
        // Scope "all" counts subagent and fork sessions like the usage
        // aggregation does, so branches whose usage comes only from
        // fork or subagent sessions stay selectable.
        const res = await MetadataService.getApiV1Branches({
          includeOneShot: true,
          includeAutomated: true,
          scope: "all",
        }) as unknown as { branches: BranchInfo[] };
        // A sync completing mid-fetch invalidated this response; let the
        // next loadBranches() refetch instead of caching stale options.
        if (version !== this.branchesVersion) return;
        this.branches = res.branches;
        this.branchesLoaded = true;
      } catch {
        // Non-fatal; the branch dropdown stays empty.
      } finally {
        this.branchesPromise = null;
      }
    })();
    return this.branchesPromise;
  }

  /**
   * Drop the cached branch options so the next loadBranches() refetches.
   * Invoked when a sync/import completes, mirroring the sessions and
   * activity stores: newly imported sessions can introduce branches the
   * branch dropdown must offer.
   */
  invalidateBranches(): void {
    this.branchesVersion++;
    this.branchesLoaded = false;
    this.branchesPromise = null;
  }

  toggleBranch(token: string): void {
    this.selectedGitBranch = toggleListValue(
      this.selectedGitBranch, token, BRANCH_LIST_SEP,
    );
    this.fetchAll();
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

  isModelSelected(name: string): boolean {
    if (!this.selectedModels) return false;
    return this.selectedModels.split(",").includes(name);
  }

  isBranchSelected(token: string): boolean {
    if (!this.selectedGitBranch) return false;
    return this.selectedGitBranch
      .split(BRANCH_LIST_SEP)
      .includes(token);
  }

  selectAllProjects(): void {
    this.excludedProjects = "";
    this.excludedProjectKeys = "";
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

  selectAllBranches(): void {
    this.selectedGitBranch = "";
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
    this.excludedProjectKeys = "";
    this.excludedAgents = "";
    this.selectedGitBranch = "";
    this.excludedModels = "";
    this.selectedModels = "";
    this.fetchAll();
  }

  get hasActiveFilters(): boolean {
    return (
      this.excludedProjects !== "" ||
      this.excludedProjectKeys !== "" ||
      this.excludedAgents !== "" ||
      this.selectedGitBranch !== "" ||
      this.selectedModels !== ""
    );
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
    const { from, to } = rollingRange(this.windowDays);
    this.from = from;
    this.to = to;
  }

  async fetchAll() {
    const fetchVersion = ++this.fetchAllVersion;
    // No-op while the branch cache is warm; refetches options after a sync
    // invalidated them, so a data refresh also refreshes the dropdown.
    void this.loadBranches();
    this.invalidatePanel("pairwise");
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
    const currentTopSessionsPromise = loadedSummary.projectScopeRecovered
      ? topSessionsPromise.then(() => {
        if (fetchVersion !== this.fetchAllVersion) return "aborted";
        return this.fetchTopSessions(loadedSummary.params);
      })
      : topSessionsPromise;
    const [topSessionsResult, comparisonResult, pairwiseResult] = await Promise.all([
      currentTopSessionsPromise,
      this.fetchComparison(
        loadedSummary.version,
        loadedSummary.summary,
        loadedSummary.params,
      ),
      this.fetchPairwise(loadedSummary.version, loadedSummary.params),
    ]);
    if (
      fetchVersion === this.fetchAllVersion &&
      topSessionsResult === "ok" &&
      comparisonResult === "ok" &&
      pairwiseResult === "ok"
    ) {
      this.markRefreshComplete();
    }
  }

  async fetchSummary(
    options: {
      loadComparison?: boolean;
      params?: UsageParams;
      recoverProjectScope?: boolean;
    } = {},
  ): Promise<LoadedUsageSummary | null> {
    const loadComparison = options.loadComparison ?? true;
    const recoverProjectScope = options.recoverProjectScope ?? true;
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
        this.excludedProjectKeys !== "" &&
        isUnknownProjectKeyError(e)
      ) {
        this.excludedProjectKeys = "";
        this.abortPanel("topSessions");
        const loaded = await this.fetchSummary({
          loadComparison,
          params: this.baseParams(),
          recoverProjectScope: false,
        });
        return loaded === null
          ? null
          : { ...loaded, projectScopeRecovered: true };
      }
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

  private currentPairwiseParams(
    params: UsageParams,
  ): UsagePairwiseParams | null {
    const selection = this.pairwiseSelection;
    if (!selection.left.value || !selection.right.value) {
      return null;
    }
    return {
      ...params,
      leftDimension: selection.left.dimension,
      leftValue: selection.left.value,
      rightDimension: selection.right.dimension,
      rightValue: selection.right.value,
    };
  }

  private async fetchPairwise(
    summaryVersion: number,
    params: UsageParams,
  ): Promise<FetchResult> {
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
    let status: Extract<PerfEntryStatus, "ok" | "error" | "aborted"> = "ok";
    try {
      const comparison = await callGenerated(() =>
        UsageService.getApiV1UsagePairwiseComparison(request),
        signal,
      ) as unknown as UsagePairwiseComparisonResponse;
      if (
        this.versions.summary === summaryVersion &&
        this.versions.pairwise === pairwiseVersion
      ) {
        this.pairwiseComparison = comparison;
        this.errors.pairwise = null;
        return "ok";
      }
      return "aborted";
    } catch (e) {
      if (isAbortError(e)) {
        status = "aborted";
        return "aborted";
      }
      status = "error";
      if (
        this.versions.summary === summaryVersion &&
        this.versions.pairwise === pairwiseVersion
      ) {
        if (this.pairwiseComparison === null) {
          this.errors.pairwise =
            e instanceof Error ? e.message : "Failed to load";
        } else {
          console.warn("usage.fetchPairwise failed:", e);
        }
      }
      return "error";
    } finally {
      perf.recordPanel({
        route: "usage",
        name: "pairwise",
        durationMs: performance.now() - started,
        status,
      });
      this.clearAbortSignal("pairwise", signal);
      if (
        this.versions.summary === summaryVersion &&
        this.versions.pairwise === pairwiseVersion
      ) {
        this.loading.pairwise = false;
      }
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
    if (panel === "pairwise") {
      this.loading.pairwise = false;
    }
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

  cancelInFlightReads(): void {
    this.fetchAllVersion++;
    this.versions.summary++;
    this.versions.pairwise++;
    this.versions.topSessions++;
    for (const panel of Object.keys(this.abortControllers) as UsagePanel[]) {
      this.abortControllers[panel]?.abort();
      delete this.abortControllers[panel];
      this.querying[panel] = false;
    }
    this.loading.summary = false;
    this.loading.pairwise = false;
    this.loading.topSessions = false;
  }

  private markRefreshComplete(): void {
    this.lastUpdatedAt = Date.now();
    this.hasNewData = false;
  }
}

export const usage = new UsageStore();

// Refresh the branch options after any sync/import, mirroring the sessions
// and activity stores. The invalidated cache is picked up lazily by the next
// loadBranches() call (UsagePage mount), so no eager refetch is needed here.
sync.onSyncComplete(() => {
  usage.invalidateBranches();
});

export interface UsageUrlState {
  from: string;
  to: string;
  isPinned: boolean;
  windowDays: number;
  excludedProjects: string;
  excludedProjectKeys: string;
  excludedAgents: string;
  selectedGitBranch: string;
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
  // Shared-store project keys are scoped to the current aggregate archive
  // set. Keep them in live request state only; URLs outlive that scope.
  if (state.excludedAgents) {
    params["exclude_agent"] = state.excludedAgents;
  }
  // "branch" (not "git_branch") because session filters already own the
  // git_branch key when usage and session params merge into one URL.
  if (state.selectedGitBranch) {
    params["branch"] = state.selectedGitBranch;
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
