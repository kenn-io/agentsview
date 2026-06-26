import type {
  AnalyticsSummary,
  ActivityResponse,
  HeatmapResponse,
  ProjectsAnalyticsResponse,
  HourOfWeekResponse,
  SessionShapeResponse,
  VelocityResponse,
  ToolsAnalyticsResponse,
  SkillsAnalyticsResponse,
  TopSessionsResponse,
  SignalsAnalyticsResponse,
  AutomatedScope,
} from "../api/types.js";
import { AnalyticsService } from "../api/generated/index";
import {
  callGenerated,
  isAbortError,
} from "../api/runtime.js";
import { sessions } from "./sessions.svelte.js";
import { perf, type PerfEntryStatus } from "./perf.svelte.js";
import { daysAgo, today } from "../utils/dates.js";

type AnalyticsParams = Parameters<
  typeof AnalyticsService.getApiV1AnalyticsSummary
>[0];
type ActivityParams = Parameters<
  typeof AnalyticsService.getApiV1AnalyticsActivity
>[0];
type HeatmapParams = Parameters<
  typeof AnalyticsService.getApiV1AnalyticsHeatmap
>[0];
type TopSessionsParams = Parameters<
  typeof AnalyticsService.getApiV1AnalyticsTopSessions
>[0];
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
  from: string = $state(daysAgo(365));
  to: string = $state(today());
  isPinned: boolean = $state(false);
  windowDays: number = $state(365);
  granularity: Granularity = $state("day");
  metric: HeatmapMetric = $state("messages");
  selectedDate: string | null = $state(null);
  project: string = $state("");
  machine: string = $state("");
  agent: string = $state("");
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
  heatmap = $state<HeatmapResponse | null>(null);
  projects = $state<ProjectsAnalyticsResponse | null>(null);
  hourOfWeek = $state<HourOfWeekResponse | null>(null);
  sessionShape = $state<SessionShapeResponse | null>(null);
  velocity = $state<VelocityResponse | null>(null);
  tools = $state<ToolsAnalyticsResponse | null>(null);
  skills = $state<SkillsAnalyticsResponse | null>(null);
  topSessions = $state<TopSessionsResponse | null>(null);
  signals = $state<SignalsAnalyticsResponse | null>(null);
  topMetric: TopSessionsMetric = $state("messages");
  lastUpdatedAt: number | null = $state(null);
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
  private abortControllers: Partial<Record<Panel, AbortController>> = {};

  get timezone(): string {
    return Intl.DateTimeFormat().resolvedOptions().timeZone;
  }

  get hasActiveFilters(): boolean {
    return (
      this.selectedDate !== null ||
      this.project !== "" ||
      this.machine !== "" ||
      this.agent !== "" ||
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
    this.selectedDate = null;
    this.project = "";
    this.machine = "";
    this.agent = "";
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
    this.fetchSignalsForInsights();
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
    const set = new Set(
      this.termination.split(",").filter((s) => s.length > 0),
    );
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
    } = {},
  ): AnalyticsParams {
    const includeProject = opts.includeProject ?? true;
    const includeTime = opts.includeTime ?? true;
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
    if (this.termination) p.termination = this.termination;
    if (this.minUserMessages > 0) {
      p.minUserMessages = this.minUserMessages;
    }
    if (this.includeOneShot) {
      p.includeOneShot = true;
    }
    p.automatedScope = this.effectiveAutomatedScope;
    if (this.recentlyActive) {
      p.activeSince = new Date(
        Date.now() - 24 * 60 * 60 * 1000,
      ).toISOString();
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
    } = {},
  ): AnalyticsParams {
    const includeProject = opts.includeProject ?? true;
    const includeTime = opts.includeTime ?? true;
    if (this.selectedDate) {
      const p: AnalyticsParams = {
        from: this.selectedDate,
        to: this.selectedDate,
        timezone: this.timezone,
      };
      if (includeProject && this.project) {
        p.project = this.project;
      }
      if (this.machine) p.machine = this.machine;
      if (this.agent) p.agent = this.agent;
      if (this.termination) p.termination = this.termination;
      if (this.minUserMessages > 0) {
        p.minUserMessages = this.minUserMessages;
      }
      if (this.includeOneShot) {
        p.includeOneShot = true;
      }
      p.automatedScope = this.effectiveAutomatedScope;
      if (this.recentlyActive) {
        p.activeSince = new Date(
          Date.now() - 24 * 60 * 60 * 1000,
        ).toISOString();
      }
      if (includeTime) {
        if (this.selectedDow !== null) {
          p.dow = this.selectedDow;
        }
        if (this.selectedHour !== null) {
          p.hour = this.selectedHour;
        }
      }
      return p;
    }
    return this.baseParams({ includeProject, includeTime });
  }

  signalEvidenceParams(): AnalyticsParams {
    return this.filterParams();
  }

  private async executeFetch<T>(
    panel: Panel,
    fetchRequest: () => Promise<T>,
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
          this.errors[panel] =
            e instanceof Error ? e.message : "Failed to load";
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

  private clearAbortSignal(
    panel: Panel,
    signal: AbortSignal,
  ): void {
    if (this.abortControllers[panel]?.signal === signal) {
      delete this.abortControllers[panel];
    }
  }

  private markRefreshComplete(): void {
    this.lastUpdatedAt = Date.now();
    this.hasNewData = false;
  }

  private rollDates(): void {
    if (this.isPinned) return;
    this.from = daysAgo(this.windowDays);
    this.to = today();
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
    if (
      fetchVersion === this.fetchAllVersion &&
      results.every((result) => result === "ok")
    ) {
      this.markRefreshComplete();
    }
  }

  async fetchSummary(): Promise<FetchResult> {
    return await this.executeFetch(
      "summary",
      () =>
        AnalyticsService.getApiV1AnalyticsSummary(
          this.filterParams(),
        ) as unknown as Promise<AnalyticsSummary>,
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
    return await this.executeFetch(
      "activity",
      () =>
        AnalyticsService.getApiV1AnalyticsActivity({
          ...this.baseParams(),
          granularity: this.granularity,
        }) as unknown as Promise<ActivityResponse>,
      (data) => {
        this.activity = data;
      },
      () => this.activity !== null,
    );
  }

  async fetchHeatmap(): Promise<FetchResult> {
    return await this.executeFetch(
      "heatmap",
      () =>
        AnalyticsService.getApiV1AnalyticsHeatmap({
          ...this.baseParams(),
          metric: this.metric,
        }) as unknown as Promise<HeatmapResponse>,
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
      () =>
        AnalyticsService.getApiV1AnalyticsProjects(
          this.filterParams({ includeProject: false }),
        ) as unknown as Promise<ProjectsAnalyticsResponse>,
      (data) => {
        this.projects = data;
      },
      () => this.projects !== null,
    );
  }

  async fetchHourOfWeek(): Promise<FetchResult> {
    return await this.executeFetch(
      "hourOfWeek",
      () =>
        AnalyticsService.getApiV1AnalyticsHourOfWeek(
          this.baseParams({ includeTime: false }),
        ) as unknown as Promise<HourOfWeekResponse>,
      (data) => {
        this.hourOfWeek = data;
      },
      () => this.hourOfWeek !== null,
    );
  }

  async fetchSessionShape(): Promise<FetchResult> {
    return await this.executeFetch(
      "sessionShape",
      () =>
        AnalyticsService.getApiV1AnalyticsSessions(
          this.filterParams(),
        ) as unknown as Promise<SessionShapeResponse>,
      (data) => {
        this.sessionShape = data;
      },
      () => this.sessionShape !== null,
    );
  }

  async fetchVelocity(): Promise<FetchResult> {
    return await this.executeFetch(
      "velocity",
      () =>
        AnalyticsService.getApiV1AnalyticsVelocity(
          this.filterParams(),
        ) as unknown as Promise<VelocityResponse>,
      (data) => {
        this.velocity = data;
      },
      () => this.velocity !== null,
    );
  }

  async fetchTools(): Promise<FetchResult> {
    return await this.executeFetch(
      "tools",
      () =>
        AnalyticsService.getApiV1AnalyticsTools(
          this.filterParams(),
        ) as unknown as Promise<ToolsAnalyticsResponse>,
      (data) => {
        this.tools = data;
      },
      () => this.tools !== null,
    );
  }

  async fetchSkills(): Promise<FetchResult> {
    return await this.executeFetch(
      "skills",
      () =>
        AnalyticsService.getApiV1AnalyticsSkills(
          this.filterParams(),
        ) as unknown as Promise<SkillsAnalyticsResponse>,
      (data) => {
        this.skills = data;
      },
      () => this.skills !== null,
    );
  }

  async fetchTopSessions(): Promise<FetchResult> {
    return await this.executeFetch(
      "topSessions",
      () =>
        AnalyticsService.getApiV1AnalyticsTopSessions({
          ...this.filterParams(),
          metric: this.topMetric,
        }) as unknown as Promise<TopSessionsResponse>,
      (data) => {
        this.topSessions = data;
      },
      () => this.topSessions !== null,
    );
  }

  async fetchSignals(): Promise<FetchResult> {
    return await this.executeFetch(
      "signals",
      () =>
        AnalyticsService.getApiV1AnalyticsSignals(
          this.filterParams(),
        ) as unknown as Promise<SignalsAnalyticsResponse>,
      (data) => {
        this.signals = data;
      },
      () => this.signals !== null,
    );
  }

  async fetchSignalsForInsights() {
    this.rollDates();
    this.selectedDate = null;
    this.selectedDow = null;
    this.selectedHour = null;
    await this.fetchSignals();
  }

  setTopMetric(m: TopSessionsMetric) {
    this.topMetric = m;
    this.fetchTopSessions();
  }

  applyDateRange(from: string, to: string) {
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

  selectDate(date: string) {
    if (this.selectedDate === date) {
      this.selectedDate = null;
    } else {
      this.selectedDate = date;
    }
    this.fetchSummary();
    this.fetchProjects();
    this.fetchSessionShape();
    this.fetchVelocity();
    this.fetchTools();
    this.fetchSkills();
    this.fetchTopSessions();
    this.fetchSignals();
  }

  setGranularity(g: Granularity) {
    this.granularity = g;
    this.fetchActivity();
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
