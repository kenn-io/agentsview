import type {
  AnalyticsSummary,
  ActivityResponse,
  HeatmapResponse,
  ProjectsAnalyticsResponse,
  HourOfWeekResponse,
  SessionShapeResponse,
  VelocityResponse,
  ToolsAnalyticsResponse,
  TopSessionsResponse,
  Granularity,
  HeatmapMetric,
  TopSessionsMetric,
  SignalsAnalyticsResponse,
} from "../api/types.js";
import {
  getAnalyticsSummary,
  getAnalyticsActivity,
  getAnalyticsHeatmap,
  getAnalyticsProjects,
  getAnalyticsHourOfWeek,
  getAnalyticsSessionShape,
  getAnalyticsVelocity,
  getAnalyticsTools,
  getAnalyticsTopSessions,
  getAnalyticsSignals,
  type AnalyticsParams,
} from "../api/client.js";
import { sessions } from "./sessions.svelte.js";

export type { Granularity, HeatmapMetric, TopSessionsMetric };

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

type Panel =
  | "summary"
  | "activity"
  | "heatmap"
  | "projects"
  | "hourOfWeek"
  | "sessionShape"
  | "velocity"
  | "tools"
  | "topSessions"
  | "signals";

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
  topSessions = $state<TopSessionsResponse | null>(null);
  signals = $state<SignalsAnalyticsResponse | null>(null);
  topMetric: TopSessionsMetric = $state("messages");

  loading = $state({
    summary: false,
    activity: false,
    heatmap: false,
    projects: false,
    hourOfWeek: false,
    sessionShape: false,
    velocity: false,
    tools: false,
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
    topSessions: 0,
    signals: 0,
  };

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
      this.includeAutomated ||
      this.recentlyActive ||
      this.selectedDow !== null ||
      this.selectedHour !== null
    );
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
    sessions.filters.includeAutomated = false;
    sessions.activeSessionId = null;
    sessions.invalidateFilterCaches();
    sessions.load();
    this.fetchAll();
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
      p.min_user_messages = this.minUserMessages;
    }
    if (this.includeOneShot) {
      p.include_one_shot = true;
    }
    if (this.includeAutomated) {
      p.include_automated = true;
    }
    if (this.recentlyActive) {
      p.active_since = new Date(
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
        p.min_user_messages = this.minUserMessages;
      }
      if (this.includeOneShot) {
        p.include_one_shot = true;
      }
      if (this.includeAutomated) {
        p.include_automated = true;
      }
      if (this.recentlyActive) {
        p.active_since = new Date(
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

  private async executeFetch<T>(
    panel: Panel,
    fetchRequest: () => Promise<T>,
    onSuccess: (data: T) => void,
    hasExistingData: () => boolean = () => false,
  ) {
    const v = ++this.versions[panel];
    // Only show the skeleton when we don't already have data to
    // display. Refetches triggered by live events or filter changes
    // replace data in place instead of flashing to loading state.
    const isFirstLoad = !hasExistingData();
    if (isFirstLoad) this.loading[panel] = true;
    // On refetch, keep any prior error state in place until we have
    // a definitive result. First-load clears up front so we start
    // fresh.
    if (isFirstLoad) this.errors[panel] = null;
    try {
      const data = await fetchRequest();
      if (this.versions[panel] === v) {
        onSuccess(data);
        this.errors[panel] = null;
      }
    } catch (e) {
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
    } finally {
      if (this.versions[panel] === v) {
        this.loading[panel] = false;
      }
    }
  }

  private rollDates(): void {
    if (this.isPinned) return;
    this.from = daysAgo(this.windowDays);
    this.to = today();
  }

  async fetchAll() {
    this.rollDates();
    await Promise.all([
      this.fetchSummary(),
      this.fetchActivity(),
      this.fetchHeatmap(),
      this.fetchProjects(),
      this.fetchHourOfWeek(),
      this.fetchSessionShape(),
      this.fetchVelocity(),
      this.fetchTools(),
      this.fetchTopSessions(),
      this.fetchSignals(),
    ]);
  }

  async fetchSummary() {
    await this.executeFetch(
      "summary",
      () => getAnalyticsSummary(this.filterParams()),
      (data) => {
        this.summary = data;
      },
      () => this.summary !== null,
    );
  }

  // Activity always uses the full date range so the timeline
  // stays visible as context when a date is selected (the
  // selected bar is highlighted instead of re-fetching).
  async fetchActivity() {
    await this.executeFetch(
      "activity",
      () =>
        getAnalyticsActivity({
          ...this.baseParams(),
          granularity: this.granularity,
        }),
      (data) => {
        this.activity = data;
      },
      () => this.activity !== null,
    );
  }

  async fetchHeatmap() {
    await this.executeFetch(
      "heatmap",
      () =>
        getAnalyticsHeatmap({
          ...this.baseParams(),
          metric: this.metric,
        }),
      (data) => {
        this.heatmap = data;
      },
      () => this.heatmap !== null,
    );
  }

  // Projects chart always shows all projects (no project
  // filter) so the selected project can be highlighted in
  // context rather than shown in isolation.
  async fetchProjects() {
    await this.executeFetch(
      "projects",
      () => getAnalyticsProjects(this.filterParams({ includeProject: false })),
      (data) => {
        this.projects = data;
      },
      () => this.projects !== null,
    );
  }

  async fetchHourOfWeek() {
    await this.executeFetch(
      "hourOfWeek",
      () => getAnalyticsHourOfWeek(this.baseParams({ includeTime: false })),
      (data) => {
        this.hourOfWeek = data;
      },
      () => this.hourOfWeek !== null,
    );
  }

  async fetchSessionShape() {
    await this.executeFetch(
      "sessionShape",
      () => getAnalyticsSessionShape(this.filterParams()),
      (data) => {
        this.sessionShape = data;
      },
      () => this.sessionShape !== null,
    );
  }

  async fetchVelocity() {
    await this.executeFetch(
      "velocity",
      () => getAnalyticsVelocity(this.filterParams()),
      (data) => {
        this.velocity = data;
      },
      () => this.velocity !== null,
    );
  }

  async fetchTools() {
    await this.executeFetch(
      "tools",
      () => getAnalyticsTools(this.filterParams()),
      (data) => {
        this.tools = data;
      },
      () => this.tools !== null,
    );
  }

  async fetchTopSessions() {
    await this.executeFetch(
      "topSessions",
      () =>
        getAnalyticsTopSessions({
          ...this.filterParams(),
          metric: this.topMetric,
        }),
      (data) => {
        this.topSessions = data;
      },
      () => this.topSessions !== null,
    );
  }

  async fetchSignals() {
    await this.executeFetch(
      "signals",
      () => getAnalyticsSignals(this.filterParams()),
      (data) => {
        this.signals = data;
      },
      () => this.signals !== null,
    );
  }

  setTopMetric(m: TopSessionsMetric) {
    this.topMetric = m;
    this.fetchTopSessions();
  }

  setDateRange(from: string, to: string) {
    this.isPinned = true;
    this.from = from;
    this.to = to;
    this.selectedDate = null;
    this.selectedDow = null;
    this.selectedHour = null;
    this.fetchAll();
  }

  setRollingWindow(days: number) {
    this.windowDays = days;
    this.isPinned = false;
    this.selectedDate = null;
    this.selectedDow = null;
    this.selectedHour = null;
    this.rollDates();
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
