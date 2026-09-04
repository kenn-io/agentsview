import type {
  AnalyticsSummary,
  ActivityResponse,
  ProjectsAnalyticsResponse,
  HourOfWeekResponse,
  SessionShapeResponse,
  VelocityResponse,
  ToolsAnalyticsResponse,
  SkillsAnalyticsResponse,
  SignalsAnalyticsResponse,
  AutomatedScope,
} from "../api/types.js";
import {
  AnalyticsService,
  type DbHeatmapResponse,
  type DbTopSessionsResponse,
} from "../api/generated/index";
import { callGenerated, isAbortError } from "../api/runtime.js";
import { sessions } from "./sessions.svelte.js";
import { perf, type PerfEntryStatus } from "./perf.svelte.js";
import { rollingRange, today } from "../utils/dates.js";

export const ANALYTICS_DEFAULT_WINDOW_DAYS = 365;

type AnalyticsParams = NonNullable<Parameters<typeof AnalyticsService.getApiV1AnalyticsSummary>[0]>;
type ActivityParams = NonNullable<Parameters<typeof AnalyticsService.getApiV1AnalyticsActivity>[0]>;
type HeatmapParams = NonNullable<Parameters<typeof AnalyticsService.getApiV1AnalyticsHeatmap>[0]>;
type TopSessionsParams = NonNullable<
  Parameters<typeof AnalyticsService.getApiV1AnalyticsTopSessions>[0]
>;
export type Granularity = NonNullable<ActivityParams["granularity"]>;
export type HeatmapMetric = NonNullable<HeatmapParams["metric"]>;
export type TopSessionsMetric = NonNullable<TopSessionsParams["metric"]>;

type Panel =
  | "summary"
  | "activity"
  | "heatmap"
  | "projects"
  | "hourOfWeek"
  | "sessionShape"
  | "velocity"
  | "tools"
  | "skills"
  | "topSessions"
  | "signals";
type FetchResult = "ok" | "error" | "aborted";

class AnalyticsStore {
  from: string = $state(rollingRange(ANALYTICS_DEFAULT_WINDOW_DAYS).from);
  to: string = $state(today());
  isPinned: boolean = $state(false);
  windowDays: number = $state(ANALYTICS_DEFAULT_WINDOW_DAYS);
  granularity: Granularity = $state("day");
  skillsGranularity: Granularity = $state("week");
  metric: HeatmapMetric = $state("messages");
  selectedDate: string | null = $state(null);
  selectedActivityRange: { from: string; to: string } | null = $state(null);
  project: string = $state("");
  machine: string = $state("");
  agent: string = $state("");
  model: string = $state("");
  termination: string = $state("");
  minUserMessages: number = $state(0);
  includeOneShot: boolean = $state(true);
  includeAutomated: boolean = $state(false);
  automatedScope: AutomatedScope = $state("human");
  recentlyActive: boolean = $state(false);
  selectedDow: number | null = $state(null);
  selectedHour: number | null = $state(null);

  summary = $state<AnalyticsSummary | null>(null);
  activity = $state<ActivityResponse | null>(null);
  heatmap = $state<DbHeatmapResponse | null>(null);
  projects = $state<ProjectsAnalyticsResponse | null>(null);
  hourOfWeek = $state<HourOfWeekResponse | null>(null);
  sessionShape = $state<SessionShapeResponse | null>(null);
  velocity = $state<VelocityResponse | null>(null);
  tools = $state<ToolsAnalyticsResponse | null>(null);
  skills = $state<SkillsAnalyticsResponse | null>(null);
  topSessions = $state<DbTopSessionsResponse | null>(null);
  signals = $state<SignalsAnalyticsResponse | null>(null);
  topMetric: TopSessionsMetric = $state("messages");
  lastUpdatedAt: number | null = $state(null);
  qualityLastUpdatedAt: number | null = $state(null);
  hasNewData: boolean = $state(false);

  loading = $state({
    summary: false,
    activity: false,
    heatmap: false,
    projects: false,
    hourOfWeek: false,
    sessionShape: false,
    velocity: false,
    tools: false,
    skills: false,
    topSessions: false,
    signals: false,
  });

  querying = $state({
    summary: false,
    activity: false,
    heatmap: false,
    projects: false,
    hourOfWeek: false,
    sessionShape: false,
    velocity: false,
    tools: false,
    skills: false,
    topSessions: false,
    signals: false,
  });

  errors = $state<Record<Panel, string | null>>({
    summary: null,
    activity: null,
    heatmap: null,
    projects: null,
    hourOfWeek: null,
    sessionShape: null,
    velocity: null,
    tools: null,
    skills: null,
    topSessions: null,
    signals: null,
  });

  private versions: Record<Panel, number> = {
    summary: 0,
    activity: 0,
    heatmap: 0,
    projects: 0,
    hourOfWeek: 0,
    sessionShape: 0,
    velocity: 0,
    tools: 0,
    skills: 0,
    topSessions: 0,
    signals: 0,
  };
  private fetchAllVersion = 0;
  private activityScope: string | null = null;
  private abortControllers: Partial<Record<Panel, AbortController>> = {};
  // Scope key of the cached `signals`: the Analytics-only filters (model plus
  // the heatmap drill-down) the cached data was fetched with. Used to drop the
  // cache when a fetch crosses the Analytics / Quality boundary, where those
  // filters do not exist.
  private signalsScope: string | null = null;

  get timezone(): string {
    return Intl.DateTimeFormat().resolvedOptions().timeZone;
  }

  get hasActiveFilters(): boolean {
    return (
      this.selectedDate !== null ||
      this.selectedActivityRange !== null ||
      this.project !== "" ||
      this.machine !== "" ||
      this.agent !== "" ||
      this.model !== "" ||
      this.termination !== "" ||
      this.minUserMessages > 0 ||
      !this.includeOneShot ||
      this.automatedScope !== "human" ||
      this.recentlyActive ||
      this.selectedDow !== null ||
      this.selectedHour !== null
    );
  }

  get isQuerying(): boolean {
    return Object.values(this.querying).some(Boolean);
  }

  markNewData(): void {
    if (this.lastUpdatedAt === null) return;
    this.hasNewData = true;
  }

  private get effectiveAutomatedScope(): AutomatedScope {
    if (!this.includeAutomated) return "human";
    if (this.automatedScope === "human") return "all";
    return this.automatedScope;
  }

  clearAllFilters() {
    const hadActivityRange = this.selectedActivityRange !== null;
    this.selectedDate = null;
    this.selectedActivityRange = null;
    this.project = "";
    this.machine = "";
    this.agent = "";
    this.model = "";
    this.termination = "";
    this.minUserMessages = 0;
    this.includeOneShot = true;
    this.includeAutomated = false;
    this.automatedScope = "human";
    this.recentlyActive = false;
    this.selectedDow = null;
    this.selectedHour = null;
    sessions.filters.project = "";
    sessions.filters.machine = "";
    sessions.filters.agent = "";
    sessions.filters.termination = "";
    sessions.filters.minUserMessages = 0;
    sessions.filters.includeOneShot = true;
    sessions.filters.includeAutomated = false;
    sessions.filters.recentlyActive = false;
    if (hadActivityRange) {
      sessions.applyPanelDateFilters(
        { date_from: this.from, date_to: this.to },
        this.isPinned ? null : this.windowDays,
      );
    }
    sessions.activeSessionId = null;
    sessions.invalidateFilterCaches();
    sessions.load();
    this.fetchAll();
  }

  clearAgent() {
    this.agent = "";
    sessions.filters.agent = "";
    sessions.activeSessionId = null;
    sessions.load();
    this.fetchAll();
  }

  clearModel() {
    this.model = "";
    this.fetchAll();
  }

  toggleAgent(agent: string) {
    const current = this.agent ? this.agent.split(",") : [];
    const idx = current.indexOf(agent);
    if (idx >= 0) {
      current.splice(idx, 1);
    } else {
      current.push(agent);
    }
    this.agent = current.join(",");
    sessions.filters.agent = this.agent;
    sessions.activeSessionId = null;
    sessions.load();
    this.fetchAll();
  }

  toggleModel(model: string) {
    const current = new Set(this.model.split(",").filter((value) => value.length > 0));
    if (current.has(model)) {
      current.delete(model);
    } else {
      current.add(model);
    }
    this.model = [...current].join(",");
    this.fetchAll();
  }

  clearMinUserMessages() {
    this.minUserMessages = 0;
    sessions.filters.minUserMessages = 0;
    sessions.activeSessionId = null;
    sessions.load();
    this.fetchAll();
  }

  clearIncludeOneShot() {
    this.includeOneShot = true;
    sessions.filters.includeOneShot = true;
    sessions.activeSessionId = null;
    sessions.invalidateFilterCaches();
    sessions.load();
    this.fetchAll();
  }

  clearIncludeAutomated() {
    this.includeAutomated = false;
    this.automatedScope = "human";
    sessions.filters.includeAutomated = false;
    sessions.activeSessionId = null;
    sessions.invalidateFilterCaches();
    sessions.load();
    this.fetchAll();
  }

  setAutomatedScope(scope: AutomatedScope) {
    this.automatedScope = scope;
    this.includeAutomated = scope !== "human";
    this.fetchSignalsForQuality();
  }

  clearRecentlyActive() {
    this.recentlyActive = false;
    sessions.filters.recentlyActive = false;
    sessions.activeSessionId = null;
    sessions.load();
    this.fetchAll();
  }

  clearDate() {
    this.selectedDate = null;
    this.clearDrilldownData({ preserveHourOfWeek: true });
    this.fetchSummary();
    this.fetchProjects();
    this.fetchSessionShape();
    this.fetchVelocity();
    this.fetchTools();
    this.fetchSkills();
    this.fetchTopSessions();
    this.fetchSignals();
  }

  clearProject() {
    this.project = "";
    sessions.filters.project = "";
    sessions.activeSessionId = null;
    sessions.load();
    this.fetchAll();
  }

  clearMachine() {
    this.machine = "";
    sessions.filters.machine = "";
    sessions.activeSessionId = null;
    sessions.load();
    this.fetchAll();
  }

  removeMachine(machine: string) {
    const current = this.machine ? this.machine.split(",") : [];
    this.machine = current.filter((m) => m !== machine).join(",");
    sessions.filters.machine = this.machine;
    sessions.activeSessionId = null;
    sessions.load();
    this.fetchAll();
  }

  clearTermination() {
    this.termination = "";
    sessions.filters.termination = "";
    sessions.activeSessionId = null;
    sessions.load();
    this.fetchAll();
  }

  toggleTerminationStatus(status: string) {
    const set = new Set(this.termination.split(",").filter((s) => s.length > 0));
    if (set.has(status)) set.delete(status);
    else set.add(status);
    const next = [...set].join(",");
    this.termination = next;
    sessions.filters.termination = next;
    sessions.activeSessionId = null;
    sessions.load();
    this.fetchAll();
  }

  clearTimeFilter() {
    this.selectedDow = null;
    this.selectedHour = null;
    this.fetchSummary();
    this.fetchActivity();
    this.fetchHeatmap();
    this.fetchProjects();
    this.fetchSessionShape();
    this.fetchVelocity();
    this.fetchTools();
    this.fetchSkills();
    this.fetchTopSessions();
    this.fetchSignals();
  }

  private baseParams(
    opts: {
      includeProject?: boolean;
      includeTime?: boolean;
      includeModel?: boolean;
    } = {},
  ): AnalyticsParams {
    const includeProject = opts.includeProject ?? true;
    const includeTime = opts.includeTime ?? true;
    const includeModel = opts.includeModel ?? true;
    const p: AnalyticsParams = {
      from: this.from,
      to: this.to,
      timezone: this.timezone,
    };
    if (includeProject && this.project) {
      p.project = this.project;
    }
    if (this.machine) p.machine = this.machine;
    if (this.agent) p.agent = this.agent;
    if (includeModel && this.model) p.model = this.model;
    if (this.termination) p.termination = this.termination;
    if (this.minUserMessages > 0) {
      p.min_user_messages = this.minUserMessages;
    }
    if (this.includeOneShot) {
      p.include_one_shot = true;
    }
    p.automated_scope = this.effectiveAutomatedScope;
    if (this.recentlyActive) {
      p.active_since = new Date(Date.now() - 24 * 60 * 60 * 1000).toISOString();
    }
    if (includeTime) {
      if (this.selectedDow !== null) p.dow = this.selectedDow;
      if (this.selectedHour !== null) {
        p.hour = this.selectedHour;
      }
    }
    return p;
  }

  private filterParams(
    opts: {
      includeProject?: boolean;
      includeTime?: boolean;
      includeModel?: boolean;
    } = {},
  ): AnalyticsParams {
    const p = this.baseParams(opts);
    if (this.selectedDate) {
      p.from = this.selectedDate;
      p.to = this.selectedDate;
    } else if (this.selectedActivityRange) {
      p.from = this.selectedActivityRange.from;
      p.to = this.selectedActivityRange.to;
    }
    return p;
  }

  signalEvidenceParams(): AnalyticsParams {
    // Quality-only drilldown: omit the Analytics model filter so signal
    // evidence matches the unscoped Quality signal facts (the Quality page
    // has no model control).
    return this.filterParams({ includeModel: false });
  }

  private async executeFetch<T>(
    panel: Panel,
    fetchRequest: (options?: { signal?: AbortSignal }) => Promise<T>,
    onSuccess: (data: T) => void,
    hasExistingData: () => boolean = () => false,
  ): Promise<FetchResult> {
    const v = ++this.versions[panel];
    const signal = this.nextAbortSignal(panel);
    // Only show the skeleton when we don't already have data to
    // display. Refetches triggered by live events or filter changes
    // replace data in place instead of flashing to loading state.
    const isFirstLoad = !hasExistingData();
    this.querying[panel] = true;
    if (isFirstLoad) this.loading[panel] = true;
    // On refetch, keep any prior error state in place until we have
    // a definitive result. First-load clears up front so we start
    // fresh.
    if (isFirstLoad) this.errors[panel] = null;
    const started = performance.now();
    let status: Extract<PerfEntryStatus, "ok" | "error" | "aborted"> = "ok";
    try {
      const data = await callGenerated(fetchRequest, signal);
      if (this.versions[panel] === v) {
        onSuccess(data);
        this.errors[panel] = null;
        return "ok";
      }
      return "aborted";
    } catch (e) {
      if (isAbortError(e)) {
        status = "aborted";
        return "aborted";
      }
      status = "error";
      if (this.versions[panel] === v) {
        // On refetch failure with cached data, swallow the error so
        // existing values stay visible instead of flipping to an
        // error state. First-load failures still surface.
        if (isFirstLoad) {
          this.errors[panel] = e instanceof Error ? e.message : "Failed to load";
        } else {
          console.warn(`analytics.${panel} refetch failed:`, e);
        }
      }
      return "error";
    } finally {
      perf.recordPanel({
        route: "analytics",
        name: panel,
        durationMs: performance.now() - started,
        status,
      });
      this.clearAbortSignal(panel, signal);
      if (this.versions[panel] === v) {
        this.querying[panel] = false;
        this.loading[panel] = false;
      }
    }
  }

  private nextAbortSignal(panel: Panel): AbortSignal {
    this.abortControllers[panel]?.abort();
    const controller = new AbortController();
    this.abortControllers[panel] = controller;
    return controller.signal;
  }

  private clearAbortSignal(panel: Panel, signal: AbortSignal): void {
    if (this.abortControllers[panel]?.signal === signal) {
      delete this.abortControllers[panel];
    }
  }

  cancelInFlightReads(): void {
    this.fetchAllVersion++;
    for (const panel of Object.keys(this.abortControllers) as Panel[]) {
      this.versions[panel]++;
      this.abortControllers[panel]?.abort();
      delete this.abortControllers[panel];
      this.querying[panel] = false;
      this.loading[panel] = false;
    }
  }

  private markRefreshComplete(): void {
    this.lastUpdatedAt = Date.now();
    this.hasNewData = false;
  }

  private rollDates(): void {
    if (this.isPinned) return;
    const { from, to } = rollingRange(this.windowDays);
    this.from = from;
    this.to = to;
  }

  async fetchAll() {
    const fetchVersion = ++this.fetchAllVersion;
    this.rollDates();
    const results = await Promise.all([
      this.fetchSummary(),
      this.fetchActivity(),
      this.fetchHeatmap(),
      this.fetchProjects(),
      this.fetchHourOfWeek(),
      this.fetchSessionShape(),
      this.fetchVelocity(),
      this.fetchTools(),
      this.fetchSkills(),
      this.fetchTopSessions(),
      this.fetchSignals(),
    ]);
    if (fetchVersion === this.fetchAllVersion && results.every((result) => result === "ok")) {
      this.markRefreshComplete();
    }
  }

  async fetchSummary(): Promise<FetchResult> {
    return await this.executeFetch(
      "summary",
      (options) => AnalyticsService.getApiV1AnalyticsSummary(this.filterParams(), options),
      (data) => {
        this.summary = data;
      },
      () => this.summary !== null,
    );
  }

  // Activity always uses the full date range so the timeline
  // stays visible as context when a date is selected (the
  // selected bar is highlighted instead of re-fetching).
  async fetchActivity(): Promise<FetchResult> {
    const scope = JSON.stringify([this.from, this.to, this.granularity]);
    if (this.activity !== null && this.activityScope !== scope) {
      this.activity = null;
    }
    return await this.executeFetch(
      "activity",
      (options) =>
        AnalyticsService.getApiV1AnalyticsActivity(
          {
            ...this.baseParams(),
            granularity: this.granularity,
          },
          options,
        ),
      (data) => {
        this.activity = data;
        this.activityScope = scope;
      },
      () => this.activity !== null,
    );
  }

  async fetchHeatmap(): Promise<FetchResult> {
    return await this.executeFetch(
      "heatmap",
      (options) =>
        AnalyticsService.getApiV1AnalyticsHeatmap(
          {
            ...this.baseParams(),
            metric: this.metric,
          },
          options,
        ),
      (data) => {
        this.heatmap = data;
      },
      () => this.heatmap !== null,
    );
  }

  // Projects chart always shows all projects (no project
  // filter) so the selected project can be highlighted in
  // context rather than shown in isolation.
  async fetchProjects(): Promise<FetchResult> {
    return await this.executeFetch(
      "projects",
      (options) =>
        AnalyticsService.getApiV1AnalyticsProjects(
          this.filterParams({ includeProject: false }),
          options,
        ),
      (data) => {
        this.projects = data;
      },
      () => this.projects !== null,
    );
  }

  async fetchHourOfWeek(params: AnalyticsParams | null = null): Promise<FetchResult> {
    let requestParams = params;
    if (requestParams === null) {
      requestParams = this.baseParams({ includeTime: false });
      if (this.selectedActivityRange) {
        requestParams.from = this.selectedActivityRange.from;
        requestParams.to = this.selectedActivityRange.to;
      }
    }
    return await this.executeFetch(
      "hourOfWeek",
      (options) => AnalyticsService.getApiV1AnalyticsHourOfWeek(requestParams, options),
      (data) => {
        this.hourOfWeek = data;
      },
      () => this.hourOfWeek !== null,
    );
  }

  async fetchSessionShape(): Promise<FetchResult> {
    return await this.executeFetch(
      "sessionShape",
      (options) => AnalyticsService.getApiV1AnalyticsSessions(this.filterParams(), options),
      (data) => {
        this.sessionShape = data;
      },
      () => this.sessionShape !== null,
    );
  }

  async fetchVelocity(): Promise<FetchResult> {
    return await this.executeFetch(
      "velocity",
      (options) => AnalyticsService.getApiV1AnalyticsVelocity(this.filterParams(), options),
      (data) => {
        this.velocity = data;
      },
      () => this.velocity !== null,
    );
  }

  async fetchTools(): Promise<FetchResult> {
    return await this.executeFetch(
      "tools",
      (options) => AnalyticsService.getApiV1AnalyticsTools(this.filterParams(), options),
      (data) => {
        this.tools = data;
      },
      () => this.tools !== null,
    );
  }

  async fetchSkills(granularity: Granularity = this.skillsGranularity): Promise<FetchResult> {
    return await this.executeFetch(
      "skills",
      (options) =>
        AnalyticsService.getApiV1AnalyticsSkills(
          {
            ...this.filterParams(),
            granularity,
          },
          options,
        ),
      (data) => {
        this.skills = data;
        this.skillsGranularity = granularity;
      },
      () => this.skills !== null,
    );
  }

  async fetchTopSessions(): Promise<FetchResult> {
    return await this.executeFetch(
      "topSessions",
      (options) =>
        AnalyticsService.getApiV1AnalyticsTopSessions(
          {
            ...this.filterParams(),
            metric: this.topMetric,
          },
          options,
        ),
      (data) => {
        this.topSessions = data;
      },
      () => this.topSessions !== null,
    );
  }

  async fetchSignals(opts: { includeModel?: boolean } = {}): Promise<FetchResult> {
    const includeModel = opts.includeModel ?? true;
    // `signals` is a cache shared by the Analytics page and the Quality page.
    // Key it by the filters that exist on Analytics but not Quality: the model
    // and the heatmap drill-down (date/day/hour), which fetchSignalsForQuality
    // clears. When this fetch's scope differs from the cached one, drop the
    // cache so another scope's signals are never shown while the fetch is in
    // flight or retained if it fails; a matching scope keeps the in-place
    // refetch behavior used for filter changes shared by both pages.
    const scope = JSON.stringify([
      includeModel ? this.model : "",
      this.selectedDate,
      this.selectedActivityRange?.from ?? null,
      this.selectedActivityRange?.to ?? null,
      this.selectedDow,
      this.selectedHour,
    ]);
    if (this.signals !== null && this.signalsScope !== scope) {
      this.signals = null;
    }
    this.signalsScope = scope;
    return await this.executeFetch(
      "signals",
      (options) =>
        AnalyticsService.getApiV1AnalyticsSignals(this.filterParams({ includeModel }), options),
      (data) => {
        this.signals = data;
      },
      () => this.signals !== null,
    );
  }

  async fetchSignalsForQuality() {
    this.rollDates();
    this.selectedDate = null;
    if (this.selectedActivityRange !== null) {
      this.selectedActivityRange = null;
      this.restoreSessionsParentRange();
    }
    this.selectedDow = null;
    this.selectedHour = null;
    // The Quality page has no model control and the model filter is an
    // Analytics-only scope; omit it so a model selected on Analytics does not
    // silently narrow the Quality signal facts.
    const result = await this.fetchSignals({ includeModel: false });
    if (result === "ok") {
      this.qualityLastUpdatedAt = Date.now();
    }
  }

  setTopMetric(m: TopSessionsMetric) {
    this.topMetric = m;
    this.fetchTopSessions();
  }

  applyDateRange(from: string, to: string) {
    if (
      this.selectedActivityRange &&
      (this.selectedActivityRange.from !== from || this.selectedActivityRange.to !== to)
    ) {
      this.selectedActivityRange = null;
    }
    this.isPinned = true;
    this.from = from;
    this.to = to;
    this.selectedDate = null;
    this.selectedDow = null;
    this.selectedHour = null;
  }

  applyRollingWindow(days: number) {
    this.windowDays = days;
    this.isPinned = false;
    this.selectedDate = null;
    this.selectedDow = null;
    this.selectedHour = null;
    this.selectedActivityRange = null;
    this.rollDates();
  }

  setActivitySelection(from: string, to: string) {
    this.selectedDate = null;
    this.selectedActivityRange = { from, to };
    this.clearDrilldownData();
    sessions.applyPanelDateFilters({ date_from: from, date_to: to }, null);
    sessions.activeSessionId = null;
    sessions.load();
    this.fetchSummary();
    this.fetchProjects();
    this.fetchHourOfWeek();
    this.fetchSessionShape();
    this.fetchVelocity();
    this.fetchTools();
    this.fetchSkills();
    this.fetchTopSessions();
    this.fetchSignals();
  }

  clearActivitySelection() {
    if (this.selectedActivityRange === null) return;
    this.selectedActivityRange = null;
    this.clearDrilldownData();
    this.restoreSessionsParentRange();
    this.fetchSummary();
    this.fetchProjects();
    this.fetchHourOfWeek();
    this.fetchSessionShape();
    this.fetchVelocity();
    this.fetchTools();
    this.fetchSkills();
    this.fetchTopSessions();
    this.fetchSignals();
  }

  setDateRange(from: string, to: string) {
    this.applyDateRange(from, to);
    this.fetchAll();
  }

  setRollingWindow(days: number) {
    this.applyRollingWindow(days);
    this.fetchAll();
  }

  selectDate(date: string) {
    const hadActivityRange = this.selectedActivityRange !== null;
    if (hadActivityRange) {
      this.selectedActivityRange = null;
      this.restoreSessionsParentRange();
    }
    if (this.selectedDate === date) {
      this.selectedDate = null;
    } else {
      this.selectedDate = date;
    }
    this.clearDrilldownData({ preserveHourOfWeek: !hadActivityRange });
    this.fetchSummary();
    this.fetchProjects();
    if (hadActivityRange) this.fetchHourOfWeek(this.baseParams({ includeTime: false }));
    this.fetchSessionShape();
    this.fetchVelocity();
    this.fetchTools();
    this.fetchSkills();
    this.fetchTopSessions();
    this.fetchSignals();
  }

  private restoreSessionsParentRange() {
    sessions.applyPanelDateFilters(
      { date_from: this.from, date_to: this.to },
      this.isPinned ? null : this.windowDays,
    );
    sessions.activeSessionId = null;
    void sessions.load();
  }

  private clearDrilldownData({
    preserveHourOfWeek = false,
  }: { preserveHourOfWeek?: boolean } = {}) {
    this.summary = null;
    this.projects = null;
    if (!preserveHourOfWeek) this.hourOfWeek = null;
    this.sessionShape = null;
    this.velocity = null;
    this.tools = null;
    this.skills = null;
    this.topSessions = null;
    this.signals = null;
  }

  setGranularity(g: Granularity) {
    this.granularity = g;
    this.fetchActivity();
  }

  async setSkillsGranularity(g: Granularity): Promise<FetchResult> {
    if (this.skillsGranularity === g) return "ok";
    return await this.fetchSkills(g);
  }

  setMetric(m: HeatmapMetric) {
    this.metric = m;
    this.fetchHeatmap();
  }

  selectHourOfWeek(dow: number | null, hour: number | null) {
    // Toggle off if clicking the same selection
    if (this.selectedDow === dow && this.selectedHour === hour) {
      this.selectedDow = null;
      this.selectedHour = null;
    } else {
      this.selectedDow = dow;
      this.selectedHour = hour;
    }
    this.fetchSummary();
    this.fetchActivity();
    this.fetchHeatmap();
    this.fetchProjects();
    this.fetchSessionShape();
    this.fetchVelocity();
    this.fetchTools();
    this.fetchSkills();
    this.fetchTopSessions();
    this.fetchSignals();
  }

  setProject(name: string) {
    if (this.project === name) {
      this.project = "";
    } else {
      this.project = name;
    }
    this.fetchAll();
  }
}

export const analytics = new AnalyticsStore();
