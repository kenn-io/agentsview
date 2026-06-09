import type { DataChangedEvent } from "../api/client.js";
import {
  MetadataService,
  SessionsService,
} from "../api/generated/index";
import { configureGeneratedClient } from "../api/runtime.js";
import type {
  Session,
  ProjectInfo,
  AgentInfo,
  SidebarSessionIndexResponse,
  SidebarSessionIndexRow,
} from "../api/types.js";
import { sync } from "./sync.svelte.js";
import { events } from "./events.svelte.js";

type SessionListParams = Parameters<
  typeof SessionsService.getApiV1Sessions
>[0];
type MetadataParams = Parameters<
  typeof MetadataService.getApiV1Projects
>[0];

const SESSION_PAGE_SIZE = 500;
const SIDEBAR_HYDRATION_CONCURRENCY = 6;
const LIVE_REFRESH_DEBOUNCE_MS = 300;
const SAFETY_NET_REFRESH_MS = 5 * 60 * 1000;

export interface SessionGroupInput {
  id: string;
  parent_session_id?: string | null;
  relationship_type?: string | null;
  project: string;
  machine: string;
  agent: string;
  first_message?: string | null;
  display_name?: string | null;
  started_at: string | null;
  ended_at: string | null;
  created_at: string;
  termination_status?: string | null;
  message_count: number;
  user_message_count?: number;
  is_automated?: boolean;
  is_teammate?: boolean;
  is_index_only?: boolean;
}

export interface SessionGroup {
  key: string;
  project: string;
  sessions: SessionGroupInput[];
  /** Unfiltered session list for ancestry classification.
   *  Set when a filter (e.g. starred) removes sessions from the group. */
  allSessions?: SessionGroupInput[];
  primarySessionId: string;
  totalMessages: number;
  firstMessage: string | null;
  startedAt: string | null;
  endedAt: string | null;
}

export interface Filters {
  project: string;
  machine: string;
  agent: string;
  termination: string;
  date: string;
  dateFrom: string;
  dateTo: string;
  recentlyActive: boolean;
  hideUnknownProject: boolean;
  minMessages: number;
  maxMessages: number;
  minUserMessages: number;
  includeOneShot: boolean;
  includeAutomated: boolean;
}

function defaultFilters(): Filters {
  return {
    project: "",
    machine: "",
    agent: "",
    termination: "",
    date: "",
    dateFrom: "",
    dateTo: "",
    recentlyActive: false,
    hideUnknownProject: false,
    minMessages: 0,
    maxMessages: 0,
    minUserMessages: 0,
    includeOneShot: true,
    includeAutomated: false,
  };
}

const SESSION_FILTERS_KEY = "session-filters";

function loadSavedFilters(): Filters {
  try {
    const raw = localStorage.getItem(SESSION_FILTERS_KEY);
    if (raw) {
      const saved = JSON.parse(raw) as Partial<Filters>;
      return { ...defaultFilters(), ...saved };
    }
  } catch {
    // Corrupted localStorage — fall back to defaults.
  }
  return defaultFilters();
}

function saveFilters(f: Filters): void {
  try {
    localStorage.setItem(SESSION_FILTERS_KEY, JSON.stringify(f));
  } catch {
    // localStorage full or unavailable — silently skip.
  }
}

/** Serialize a Filters object into URL query params.
 *  Default-valued fields are omitted so the URL stays clean. */
export function filtersToParams(
  f: Filters,
): Record<string, string> {
  const p: Record<string, string> = {};
  if (f.project) p["project"] = f.project;
  if (f.machine) p["machine"] = f.machine;
  if (f.agent) p["agent"] = f.agent;
  if (f.termination) p["termination"] = f.termination;
  if (f.date) p["date"] = f.date;
  if (f.dateFrom) p["date_from"] = f.dateFrom;
  if (f.dateTo) p["date_to"] = f.dateTo;
  if (f.recentlyActive) p["active_since"] = "true";
  if (f.hideUnknownProject) p["exclude_project"] = "unknown";
  if (f.minMessages > 0) p["min_messages"] = String(f.minMessages);
  if (f.maxMessages > 0) p["max_messages"] = String(f.maxMessages);
  if (f.minUserMessages > 0) {
    p["min_user_messages"] = String(f.minUserMessages);
  }
  if (!f.includeOneShot) p["include_one_shot"] = "false";
  if (f.includeAutomated) p["include_automated"] = "true";
  return p;
}

export function splitExcludeProjectParam(
  raw: string | undefined,
): {
  hideUnknownProject: boolean;
  usageExcludedProjects: string;
} {
  const projects: string[] = [];
  const seen = new Set<string>();
  let hideUnknownProject = false;
  for (const value of (raw ?? "").split(",")) {
    const trimmed = value.trim();
    if (!trimmed) continue;
    if (trimmed === "unknown") {
      hideUnknownProject = true;
      continue;
    }
    if (seen.has(trimmed)) continue;
    seen.add(trimmed);
    projects.push(trimmed);
  }
  return {
    hideUnknownProject,
    usageExcludedProjects: projects.join(","),
  };
}

/** Parse URL query params into a typed Filters object.
 *  Unknown/missing params fall back to defaults. */
export function parseFiltersFromParams(
  params: Record<string, string>,
): Filters {
  const minMsgs = parseInt(params["min_messages"] ?? "", 10);
  const maxMsgs = parseInt(params["max_messages"] ?? "", 10);
  const minUserMsgs = parseInt(params["min_user_messages"] ?? "", 10);

  const { hideUnknownProject: hideUnknown } =
    splitExcludeProjectParam(params["exclude_project"]);
  let project = params["project"] ?? "";
  if (hideUnknown && project === "unknown") {
    project = "";
  }

  const oneShotParam = params["include_one_shot"];
  const includeOneShot =
    oneShotParam === undefined ? true : oneShotParam === "true";

  return {
    project,
    machine: params["machine"] ?? "",
    agent: params["agent"] ?? "",
    termination: params["termination"] ?? "",
    date: params["date"] ?? "",
    dateFrom: params["date_from"] ?? "",
    dateTo: params["date_to"] ?? "",
    recentlyActive: params["active_since"] === "true",
    hideUnknownProject: hideUnknown,
    minMessages: Number.isFinite(minMsgs) ? minMsgs : 0,
    maxMessages: Number.isFinite(maxMsgs) ? maxMsgs : 0,
    minUserMessages: Number.isFinite(minUserMsgs) ? minUserMsgs : 0,
    includeOneShot,
    includeAutomated: params["include_automated"] === "true",
  };
}

class SessionsStore {
  sessions: Session[] = $state([]);
  projects: ProjectInfo[] = $state([]);
  agents: AgentInfo[] = $state([]);
  machines: string[] = $state([]);
  activeSessionId: string | null = $state(null);
  childSessions: Map<string, Session> = $state(new Map());
  nextCursor: string | null = $state(null);
  total: number = $state(0);
  loading: boolean = $state(false);
  filters: Filters = $state(loadSavedFilters());

  private signalDetailCache = new Map<
    string,
    {
      basis: string[] | null;
      penalties: Record<string, number> | null;
    }
  >();
  private signalDetailInflight = new Map<
    string,
    Promise<void>
  >();
  signalDetailLoading = $state(false);

  private loadVersion: number = 0;
  private projectsLoaded: boolean = false;
  private projectsPromise: Promise<void> | null = null;
  private projectsVersion: number = 0;
  private agentsLoaded: boolean = false;
  private agentsPromise: Promise<void> | null = null;
  private agentsVersion: number = 0;
  private refreshVersion: number = 0;
  private childSessionsVersion: number = 0;
  private machinesLoaded: boolean = false;
  private machinesPromise: Promise<void> | null = null;
  private machinesVersion: number = 0;
  private sidebarHydrationInflightByVersion = new Map<
    number,
    Map<string, Promise<void>>
  >();
  private sidebarHydrationEpochByVersion = new Map<number, number>();
  private sidebarHydrationQueue: Array<() => void> = [];
  private sidebarHydrationActive = 0;

  private liveRefreshStarted = false;
  private unsubEvents: (() => void) | null = null;
  private liveRefreshTimer: ReturnType<typeof setTimeout> | null = null;
  private safetyNetTimer: ReturnType<typeof setInterval> | null = null;

  get activeSession(): Session | undefined {
    const session = this.sessions.find((s) => s.id === this.activeSessionId);
    return session?.is_index_only ? undefined : session;
  }

  get groupedSessions(): SessionGroup[] {
    return buildSessionGroups(this.sessions);
  }

  private get apiParams(): SessionListParams {
    const f = this.filters;
    // Don't exclude "unknown" when explicitly viewing it.
    const exclude =
      f.hideUnknownProject && f.project !== "unknown"
        ? "unknown"
        : undefined;
    return {
      project: f.project || undefined,
      excludeProject: exclude,
      machine: f.machine || undefined,
      agent: f.agent || undefined,
      termination: f.termination || undefined,
      date: f.date || undefined,
      dateFrom: f.dateFrom || undefined,
      dateTo: f.dateTo || undefined,
      activeSince: f.recentlyActive
        ? new Date(
            Date.now() - 24 * 60 * 60 * 1000,
          ).toISOString()
        : undefined,
      minMessages:
        f.minMessages > 0 ? f.minMessages : undefined,
      maxMessages:
        f.maxMessages > 0 ? f.maxMessages : undefined,
      minUserMessages:
        f.minUserMessages > 0 ? f.minUserMessages : undefined,
      includeOneShot: f.includeOneShot || undefined,
      includeAutomated: f.includeAutomated || undefined,
    };
  }

  private resetPagination() {
    this.sessions = [];
    this.nextCursor = null;
    this.total = 0;
  }

  initFromParams(params: Record<string, string>) {
    const prevOneShot = this.filters.includeOneShot;
    const prevAutomated = this.filters.includeAutomated;
    const next = parseFiltersFromParams(params);
    this.filters = next;
    if (prevOneShot !== next.includeOneShot ||
        prevAutomated !== next.includeAutomated) {
      this.invalidateFilterCaches();
    }
    this.setActiveSession(null);
  }

  async load() {
    saveFilters(this.filters);
    this.startLiveRefresh();
    const version = ++this.loadVersion;
    const indexVersion = this.sidebarIndexVersion + 1;
    // Keep the existing list visible during reloads, but mark
    // loading=true so large filter expansions expose that more
    // pages are still being fetched after page 1 is published.
    this.loading = true;
    // Preserve old data during reload — clearing eagerly causes
    // a flash because the sidebar and content area briefly see
    // an empty session list.
    const prev = {
      sessions: this.sessions,
      nextCursor: this.nextCursor,
      total: this.total,
    };
    try {
      configureGeneratedClient();
      const index = await SessionsService.getApiV1SessionsSidebarIndex(
        this.apiParams,
      ) as unknown as SidebarSessionIndexResponse;
      if (this.loadVersion !== version) return;

      this.sidebarIndexVersion = indexVersion;
      this.hydratedSessionsByVersion.set(indexVersion, new Map());
      this.sidebarHydrationEpochByVersion.set(indexVersion, 0);
      this.pruneSidebarHydrationVersions(indexVersion);
      const previousById = new Map(prev.sessions.map((s) => [s.id, s]));
      this.sessions = index.sessions.map((row) =>
        sidebarIndexRowToSession(row, previousById.get(row.id))
      );
      this.nextCursor = null;
      this.total = index.total;
    } catch {
      // Restore previous state so a transient failure
      // doesn't wipe the visible session list.
      if (this.loadVersion === version) {
        this.sessions = prev.sessions;
        this.nextCursor = prev.nextCursor;
        this.total = prev.total;
      }
    } finally {
      if (this.loadVersion === version) {
        this.loading = false;
      }
    }
  }

  sidebarIndexVersion: number = $state(0);
  hydratedSessionsByVersion: Map<number, Map<string, Session>> =
    $state(new Map());

  private pruneSidebarHydrationVersions(retainVersion: number) {
    for (const version of this.hydratedSessionsByVersion.keys()) {
      if (version !== retainVersion) {
        this.hydratedSessionsByVersion.delete(version);
      }
    }
    for (const version of this.sidebarHydrationInflightByVersion.keys()) {
      if (version !== retainVersion) {
        this.sidebarHydrationInflightByVersion.delete(version);
      }
    }
    for (const version of this.sidebarHydrationEpochByVersion.keys()) {
      if (version !== retainVersion) {
        this.sidebarHydrationEpochByVersion.delete(version);
      }
    }
  }

  async hydrateVisibleSessions(
    ids: string[],
    version: number = this.sidebarIndexVersion,
  ) {
    const uniqueIds = [...new Set(ids)];
    const cache =
      this.hydratedSessionsByVersion.get(version) ?? new Map<string, Session>();
    this.hydratedSessionsByVersion.set(version, cache);
    const inflight = this.sidebarHydrationInflightByVersion.get(version) ??
      new Map<string, Promise<void>>();
    this.sidebarHydrationInflightByVersion.set(version, inflight);
    const epoch = this.sidebarHydrationEpochByVersion.get(version) ?? 0;

    await Promise.all(uniqueIds.map((id) => {
      if (cache.has(id)) return;
      const existing = inflight.get(id);
      if (existing) return existing;

      const promise = this.runSidebarHydration(async () => {
        try {
          configureGeneratedClient();
          const hydrated = await SessionsService.getApiV1SessionsId({
            id,
          }) as unknown as Session;
          if (
            version !== this.sidebarIndexVersion ||
            epoch !== (this.sidebarHydrationEpochByVersion.get(version) ?? 0)
          ) {
            return;
          }
          cache.set(id, hydrated);
          this.mergeHydratedSession(hydrated);
        } catch {
          // Visible hydration is best-effort; the skinny row remains usable.
        } finally {
          inflight.delete(id);
        }
      });
      inflight.set(id, promise);
      return promise;
    }));
  }

  private async runSidebarHydration(task: () => Promise<void>): Promise<void> {
    if (this.sidebarHydrationActive >= SIDEBAR_HYDRATION_CONCURRENCY) {
      await new Promise<void>((resolve) => {
        this.sidebarHydrationQueue.push(resolve);
      });
    }

    this.sidebarHydrationActive++;
    try {
      await task();
    } finally {
      this.sidebarHydrationActive--;
      this.sidebarHydrationQueue.shift()?.();
    }
  }

  private mergeHydratedSession(hydrated: Session) {
    const idx = this.sessions.findIndex((s) => s.id === hydrated.id);
    if (idx < 0) return;
    const current = this.sessions[idx]!;
    this.sessions[idx] = {
      ...current,
      ...hydrated,
      display_name: hydrated.display_name ?? current.display_name,
      is_teammate: hydrated.is_teammate ?? current.is_teammate,
      is_index_only: false,
    };
  }

  private invalidateHydratedSessionDetails() {
    const version = this.sidebarIndexVersion;
    this.hydratedSessionsByVersion.set(version, new Map());
    this.sidebarHydrationInflightByVersion.delete(version);
    this.sidebarHydrationEpochByVersion.set(
      version,
      (this.sidebarHydrationEpochByVersion.get(version) ?? 0) + 1,
    );
    this.signalDetailCache.clear();
    this.signalDetailInflight.clear();
    this.signalDetailLoading = false;
  }

  async loadMore() {
    if (!this.nextCursor || this.loading) return;
    const version = ++this.loadVersion;
    this.loading = true;
    try {
      configureGeneratedClient();
      const page = await SessionsService.getApiV1Sessions({
        ...this.apiParams,
        cursor: this.nextCursor,
        limit: SESSION_PAGE_SIZE,
      }) as unknown as {
        sessions: Session[];
        next_cursor?: string | null;
        total: number;
      };
      if (this.loadVersion !== version) return;
      this.sessions.push(...page.sessions);
      this.nextCursor = page.next_cursor ?? null;
      this.total = page.total;
    } finally {
      if (this.loadVersion === version) {
        this.loading = false;
      }
    }
  }

  /**
   * Load additional pages until the target index is backed by
   * loaded sessions, or until we hit maxPages / end-of-list.
   * Keeps scrollbar jumps from showing placeholders for too long.
   */
  async loadMoreUntil(targetIndex: number, maxPages: number = 5) {
    if (targetIndex < 0) return;
    let pages = 0;
    while (
      this.nextCursor &&
      !this.loading &&
      this.sessions.length <= targetIndex &&
      pages < maxPages
    ) {
      const before = this.sessions.length;
      await this.loadMore();
      pages++;
      if (this.sessions.length <= before) {
        // Defensive: stop if no forward progress.
        break;
      }
    }
  }

  async loadProjects() {
    if (this.projectsLoaded) return;
    if (this.projectsPromise) return this.projectsPromise;
    const ver = this.projectsVersion;
    this.projectsPromise = (async () => {
      try {
        configureGeneratedClient();
        const res = await MetadataService.getApiV1Projects(
          this.metadataParams,
        ) as unknown as { projects: ProjectInfo[] };
        if (ver === this.projectsVersion) {
          this.projects = res.projects;
          this.projectsLoaded = true;
        }
      } catch {
        // Non-fatal; projects list stays stale.
      } finally {
        if (ver === this.projectsVersion) {
          this.projectsPromise = null;
        }
      }
    })();
    return this.projectsPromise;
  }

  async loadAgents() {
    if (this.agentsLoaded) return;
    if (this.agentsPromise) return this.agentsPromise;
    const ver = this.agentsVersion;
    this.agentsPromise = (async () => {
      try {
        configureGeneratedClient();
        const res = await MetadataService.getApiV1Agents(
          this.metadataParams,
        ) as unknown as { agents: AgentInfo[] };
        if (ver === this.agentsVersion) {
          this.agents = res.agents;
          this.agentsLoaded = true;
        }
      } catch {
        // Non-fatal; agents list stays stale.
      } finally {
        if (ver === this.agentsVersion) {
          this.agentsPromise = null;
        }
      }
    })();
    return this.agentsPromise;
  }

  async loadMachines() {
    if (this.machinesLoaded) return;
    if (this.machinesPromise) return this.machinesPromise;
    const ver = this.machinesVersion;
    this.machinesPromise = (async () => {
      try {
        configureGeneratedClient();
        const res = await MetadataService.getApiV1Machines(
          this.metadataParams,
        ) as unknown as { machines: string[] };
        if (ver === this.machinesVersion) {
          this.machines = res.machines;
          this.machinesLoaded = true;
        }
      } catch {
        // Non-fatal; machines list stays stale.
      } finally {
        if (ver === this.machinesVersion) {
          this.machinesPromise = null;
        }
      }
    })();
    return this.machinesPromise;
  }

  private setActiveSession(id: string | null) {
    if (id === this.activeSessionId) return;
    this.activeSessionId = id;
    this.refreshVersion++;
    this.childSessionsVersion++;
  }

  selectSession(id: string) {
    this.setActiveSession(id);
    void this.hydrateSelectedIndexOnlySession(id);
  }

  /**
   * Navigate to a session by ID, loading it into the sessions list if
   * not already present (e.g. subagent sessions filtered from groups).
   */
  async navigateToSession(id: string) {
    this.setActiveSession(id);
    const existing = this.sessions.find((s) => s.id === id);
    if (existing) {
      await this.hydrateSelectedIndexOnlySession(id);
      return;
    }
    try {
      configureGeneratedClient();
      const session = await SessionsService.getApiV1SessionsId({
        id,
      }) as unknown as Session;
      if (this.activeSessionId === id) {
        const idx = this.sessions.findIndex((s) => s.id === id);
        if (idx >= 0) {
          this.mergeHydratedSession(session);
        } else {
          this.sessions = [...this.sessions, session];
        }
      }
    } catch {
      // Session not found — selection stands without metadata
    }
  }

  private async hydrateSelectedIndexOnlySession(id: string) {
    const existing = this.sessions.find((s) => s.id === id);
    if (!existing?.is_index_only) return;
    await this.hydrateVisibleSessions([id]);
  }

  deselectSession() {
    this.setActiveSession(null);
    this.childSessions = new Map();
  }

  async refreshActiveSession() {
    const id = this.activeSessionId;
    if (!id) return;
    const version = ++this.refreshVersion;
    try {
      configureGeneratedClient();
      const session = await SessionsService.getApiV1SessionsId({
        id,
      }) as unknown as Session;
      if (
        this.refreshVersion !== version ||
        this.activeSessionId !== id
      ) {
        return;
      }
      const idx = this.sessions.findIndex((s) => s.id === id);
      if (idx >= 0) {
        this.mergeHydratedSession(session);
      }
    } catch {
      // Session may have been deleted
    }
  }

  async loadChildSessions(parentId: string) {
    const version = ++this.childSessionsVersion;
    try {
      configureGeneratedClient();
      const children = await SessionsService.getApiV1SessionsIdChildren({
        id: parentId,
      }) as unknown as Session[];
      if (
        this.childSessionsVersion !== version ||
        this.activeSessionId !== parentId
      ) {
        return;
      }
      const map = new Map<string, Session>();
      for (const child of children) {
        map.set(child.id, child);
      }
      this.childSessions = map;
    } catch {
      if (
        this.childSessionsVersion !== version ||
        this.activeSessionId !== parentId
      ) {
        return;
      }
      this.childSessions = new Map();
    }
  }

  getSignalDetail(id: string) {
    return this.signalDetailCache.get(id) ?? null;
  }

  async fetchSignalDetail(id: string) {
    if (this.signalDetailCache.has(id)) {
      this.mergeDetailIntoList(id);
      return;
    }
    const inflight = this.signalDetailInflight.get(id);
    if (inflight) return inflight;
    const promise = this.doFetchSignalDetail(id);
    this.signalDetailInflight.set(id, promise);
    await promise;
  }

  private async doFetchSignalDetail(id: string) {
    this.signalDetailLoading = true;
    try {
      configureGeneratedClient();
      const session = await SessionsService.getApiV1SessionsId({
        id,
      }) as unknown as Session;
      this.signalDetailCache.set(id, {
        basis: session.health_score_basis ?? null,
        penalties: session.health_penalties ?? null,
      });
      this.mergeDetailIntoList(id);
    } catch {
      // Signal detail is non-critical
    } finally {
      this.signalDetailInflight.delete(id);
      this.signalDetailLoading =
        this.signalDetailInflight.size > 0;
    }
  }

  private mergeDetailIntoList(id: string) {
    const detail = this.signalDetailCache.get(id);
    if (!detail) return;
    const idx = this.sessions.findIndex(
      (s) => s.id === id,
    );
    if (idx >= 0) {
      const s = this.sessions[idx]!;
      if (
        s.health_score_basis === undefined &&
        detail.basis != null
      ) {
        this.sessions[idx] = {
          ...s,
          health_score_basis: detail.basis,
          health_penalties: detail.penalties,
        };
      }
    }
  }

  navigateSession(delta: number, filter?: (s: Session) => boolean) {
    const list = filter
      ? this.sessions.filter(filter)
      : this.sessions;
    if (list.length === 0) return;
    const idx = list.findIndex((s) => s.id === this.activeSessionId);
    if (idx === -1) {
      // No active session at all — do nothing (preserve no-op behavior).
      if (this.activeSessionId === null) return;
      // Active session exists but isn't in the filtered list (e.g. viewing
      // an unstarred session while starred-only filter is on) — jump to
      // an edge so the keyboard shortcut doesn't silently fail.
      const edge = delta > 0 ? 0 : list.length - 1;
      const id = list[edge]!.id;
      this.setActiveSession(id);
      void this.hydrateSelectedIndexOnlySession(id);
      return;
    }
    const next = idx + delta;
    if (next >= 0 && next < list.length) {
      const id = list[next]!.id;
      this.setActiveSession(id);
      void this.hydrateSelectedIndexOnlySession(id);
    }
  }

  setProjectFilter(project: string) {
    const prev = this.filters;
    this.filters = { ...defaultFilters(), project, agent: prev.agent };
    this.setActiveSession(null);
    if (prev.includeOneShot !== this.filters.includeOneShot ||
        prev.includeAutomated !== this.filters.includeAutomated) {
      this.invalidateFilterCaches();
    }
    this.load();
  }

  setMachineFilter(machine: string) {
    this.filters.machine = this.filters.machine === machine ? "" : machine;
    this.activeSessionId = null;
    this.load();
  }

  toggleMachineFilter(machine: string) {
    const current = this.filters.machine
      ? this.filters.machine.split(",")
      : [];
    const idx = current.indexOf(machine);
    if (idx >= 0) {
      current.splice(idx, 1);
    } else {
      current.push(machine);
    }
    this.filters.machine = current.join(",");
    this.setActiveSession(null);
    this.load();
  }

  isMachineSelected(machine: string): boolean {
    if (!this.filters.machine) return false;
    return this.filters.machine.split(",").includes(machine);
  }

  get selectedMachines(): string[] {
    if (!this.filters.machine) return [];
    return this.filters.machine.split(",");
  }

  setAgentFilter(agent: string) {
    if (this.filters.agent === agent) {
      this.filters.agent = "";
    } else {
      this.filters.agent = agent;
    }
    this.setActiveSession(null);
    this.load();
  }

  toggleAgentFilter(agent: string) {
    const current = this.filters.agent
      ? this.filters.agent.split(",")
      : [];
    const idx = current.indexOf(agent);
    if (idx >= 0) {
      current.splice(idx, 1);
    } else {
      current.push(agent);
    }
    this.filters.agent = current.join(",");
    this.setActiveSession(null);
    this.load();
  }

  isAgentSelected(agent: string): boolean {
    if (!this.filters.agent) return false;
    return this.filters.agent.split(",").includes(agent);
  }

  get selectedAgents(): string[] {
    if (!this.filters.agent) return [];
    return this.filters.agent.split(",");
  }

  setRecentlyActiveFilter(active: boolean) {
    this.filters.recentlyActive = active;
    this.setActiveSession(null);
    this.load();
  }

  setMinUserMessagesFilter(n: number) {
    this.filters.minUserMessages = n;
    this.setActiveSession(null);
    this.load();
  }

  setHideUnknownProjectFilter(hide: boolean) {
    this.filters.hideUnknownProject = hide;
    if (hide && this.filters.project === "unknown") {
      this.filters.project = "";
    }
    this.setActiveSession(null);
    this.load();
  }

  setIncludeOneShotFilter(include: boolean) {
    this.filters.includeOneShot = include;
    this.setActiveSession(null);
    this.invalidateFilterCaches();
    this.load();
  }

  setIncludeAutomatedFilter(include: boolean) {
    this.filters.includeAutomated = include;
    this.setActiveSession(null);
    this.invalidateFilterCaches();
    this.load();
  }

  setTerminationFilter(termination: string) {
    this.filters.termination = termination;
    this.setActiveSession(null);
    this.load();
  }

  /** Add or remove a status from the comma-separated termination
   * filter. Empty list means "no filter". */
  toggleTerminationStatus(status: string) {
    const set = new Set(
      this.filters.termination
        .split(",")
        .filter((s) => s.length > 0),
    );
    if (set.has(status)) set.delete(status);
    else set.add(status);
    this.setTerminationFilter([...set].join(","));
  }

  /** Whether the comma-separated termination filter contains
   * the given status. Used by the multi-select pill UI. */
  hasTerminationStatus(status: string): boolean {
    if (!this.filters.termination) return false;
    return this.filters.termination
      .split(",")
      .includes(status);
  }

  get hasActiveFilters(): boolean {
    const f = this.filters;
    return !!(
      f.machine ||
      f.agent ||
      f.termination ||
      f.recentlyActive ||
      f.hideUnknownProject ||
      f.dateFrom ||
      f.dateTo ||
      f.date ||
      f.minUserMessages > 0 ||
      !f.includeOneShot ||
      f.includeAutomated
    );
  }

  clearSessionFilters() {
    const project = this.filters.project;
    const wasOneShot = this.filters.includeOneShot;
    const wasAutomated = this.filters.includeAutomated;
    this.filters = { ...defaultFilters(), project };
    this.setActiveSession(null);
    if (wasOneShot !== this.filters.includeOneShot || wasAutomated) {
      this.invalidateFilterCaches();
    }
    this.load();
  }

  /** Recently deleted session IDs for undo toast. */
  recentlyDeleted: { id: string; timer: ReturnType<typeof setTimeout> }[] =
    $state([]);

  async deleteSession(id: string) {
    configureGeneratedClient();
    await SessionsService.deleteApiV1SessionsId({ id });
    const before = this.sessions.length;
    this.sessions = this.sessions.filter((s) => s.id !== id);
    const removed = before - this.sessions.length;
    if (removed > 0) {
      this.total = Math.max(0, this.total - removed);
    }
    if (this.activeSessionId === id) {
      this.setActiveSession(null);
    }
    const timer = setTimeout(() => {
      this.recentlyDeleted = this.recentlyDeleted.filter(
        (d) => d.id !== id,
      );
    }, 10_000);
    this.recentlyDeleted = [...this.recentlyDeleted, { id, timer }];
    this.invalidateFilterCaches();
  }

  async restoreSession(id: string) {
    configureGeneratedClient();
    await SessionsService.postApiV1SessionsIdRestore({ id });
    this.clearRecentlyDeleted(id);
    this.invalidateFilterCaches();
    await this.load();
  }

  private get metadataParams(): MetadataParams {
    return {
      includeOneShot: this.filters.includeOneShot || undefined,
      includeAutomated: this.filters.includeAutomated || undefined,
    };
  }

  invalidateFilterCaches() {
    this.projectsVersion++;
    this.projectsLoaded = false;
    this.projectsPromise = null;
    this.agentsVersion++;
    this.agentsLoaded = false;
    this.agentsPromise = null;
    this.machinesVersion++;
    this.machinesLoaded = false;
    this.machinesPromise = null;
    this.loadProjects();
    this.loadAgents();
    this.loadMachines();
    sync.loadStats(this.metadataParams);
  }

  /** Remove one or all entries from the undo toast list. */
  clearRecentlyDeleted(id?: string) {
    if (id) {
      this.recentlyDeleted = this.recentlyDeleted.filter((d) => {
        if (d.id === id) {
          clearTimeout(d.timer);
          return false;
        }
        return true;
      });
    } else {
      for (const d of this.recentlyDeleted) clearTimeout(d.timer);
      this.recentlyDeleted = [];
    }
  }

  async renameSession(id: string, displayName: string | null) {
    configureGeneratedClient();
    const updated = await SessionsService.patchApiV1SessionsIdRename({
      id,
      requestBody: { display_name: displayName },
    }) as unknown as Session;
    const idx = this.sessions.findIndex((s) => s.id === id);
    if (idx !== -1) {
      const merged = { ...this.sessions[idx]!, ...updated };
      // When the caller cleared the rename and the backend found no agent name
      // to restore, display_name is absent from the response (omitempty on nil).
      // Explicitly null it out so the store reflects the cleared state rather
      // than keeping the stale value until the next SSE-triggered refresh.
      if (displayName === null && updated.display_name === undefined) {
        merged.display_name = null;
      }
      this.sessions[idx] = merged;
    }
  }

  private startLiveRefresh() {
    if (this.liveRefreshStarted) return;
    this.liveRefreshStarted = true;
    this.unsubEvents = events.subscribe((event) => {
      this.handleLiveRefreshEvent(event);
    });
    this.safetyNetTimer = setInterval(
      () => { this.load(); },
      SAFETY_NET_REFRESH_MS,
    );
  }

  private handleLiveRefreshEvent(event: DataChangedEvent) {
    if (event.scope === "messages") {
      this.invalidateHydratedSessionDetails();
      return;
    }
    if (event.scope === "sessions" || event.scope === "sync") {
      this.scheduleIndexRefresh();
    }
  }

  private scheduleIndexRefresh() {
    if (this.liveRefreshTimer !== null) {
      clearTimeout(this.liveRefreshTimer);
    }
    this.liveRefreshTimer = setTimeout(() => {
      this.liveRefreshTimer = null;
      this.load();
    }, LIVE_REFRESH_DEBOUNCE_MS);
  }

  dispose() {
    if (this.unsubEvents) {
      this.unsubEvents();
      this.unsubEvents = null;
    }
    if (this.liveRefreshTimer !== null) {
      clearTimeout(this.liveRefreshTimer);
      this.liveRefreshTimer = null;
    }
    if (this.safetyNetTimer !== null) {
      clearInterval(this.safetyNetTimer);
      this.safetyNetTimer = null;
    }
    this.liveRefreshStarted = false;
  }
}

export function createSessionsStore(): SessionsStore {
  return new SessionsStore();
}

function sidebarIndexRowToSession(
  row: SidebarSessionIndexRow,
  existing?: Session,
): Session {
  const skinny: Session = {
    id: row.id,
    project: row.project,
    machine: row.machine,
    agent: row.agent,
    first_message: null,
    display_name: row.display_name ?? null,
    started_at: row.started_at,
    ended_at: row.ended_at,
    message_count: row.message_count,
    user_message_count: row.user_message_count,
    parent_session_id: row.parent_session_id ?? undefined,
    relationship_type: row.relationship_type ?? undefined,
    termination_status: row.termination_status ?? null,
    total_output_tokens: 0,
    peak_context_tokens: 0,
    has_total_output_tokens: false,
    has_peak_context_tokens: false,
    is_automated: row.is_automated,
    is_teammate: row.is_teammate ?? false,
    is_index_only: true,
    created_at: row.created_at,
  };
  if (!existing || existing.is_index_only) return skinny;
  return {
    ...skinny,
    ...existing,
    project: skinny.project,
    machine: skinny.machine,
    agent: skinny.agent,
    display_name: skinny.display_name,
    started_at: skinny.started_at,
    ended_at: skinny.ended_at,
    message_count: skinny.message_count,
    user_message_count: skinny.user_message_count,
    parent_session_id: skinny.parent_session_id,
    relationship_type: skinny.relationship_type,
    termination_status: skinny.termination_status,
    is_automated: skinny.is_automated,
    is_teammate: skinny.is_teammate ?? existing.is_teammate,
    is_index_only: false,
    created_at: skinny.created_at,
  };
}

function maxString(a: string | null, b: string | null): string | null {
  if (a == null) return b;
  if (b == null) return a;
  return a > b ? a : b;
}

function minString(a: string | null, b: string | null): string | null {
  if (a == null) return b;
  if (b == null) return a;
  return a < b ? a : b;
}

/** Minimal shape that StatusDot / getSessionStatus need from a
 * row. Both the full `Session` and the lighter `TopSession`
 * (analytics top list) match it structurally — the recency
 * fields all have safe fallbacks via `??`. */
export interface SessionStatusInput {
  termination_status?: string | null;
  ended_at?: string | null;
  started_at?: string | null;
  created_at?: string;
}

function recencyKey(s: SessionStatusInput): string {
  return s.ended_at ?? s.started_at ?? s.created_at ?? "";
}

const FRESH_MS = 60 * 1000;
const RECENTLY_ACTIVE_MS = 10 * 60 * 1000;
const STALE_MS = 60 * 60 * 1000;

/** Ticking timestamp that updates every 30s so derived
 *  recency checks stay reactive without manual triggers. */
let now = $state(Date.now());
setInterval(() => {
  now = Date.now();
}, 30_000);

export function isRecentlyActive(session: Session): boolean {
  const key = recencyKey(session);
  const ts = new Date(key).getTime();
  return now - ts < RECENTLY_ACTIVE_MS;
}

export type SessionStatus =
  | "working"
  | "waiting"
  | "idle"
  | "stale"
  | "unclean"
  | "quiet";

/** Combine wall-clock recency with the parser's structural fact
 * (termination_status) into a single user-facing status.
 *
 * Precedence (first match wins, see body below):
 *   - waiting: < 10m idle AND termination_status == awaiting_user
 *   - working: < 1m idle AND not awaiting_user
 *   - idle:    1-10m idle AND not awaiting_user
 *   - quiet:   ≥ 10m idle AND clean/NULL
 *   - stale:   10-60m idle AND tool_call_pending/truncated
 *   - unclean: ≥ 60m idle AND tool_call_pending/truncated
 *
 * When a `groupSessions` array is provided, the freshness check
 * uses the freshest activity across the whole group. Two interactions
 * matter:
 *
 *   1. A parent in tool_call_pending whose subagent is currently
 *      writing rolls up to "working" via the freshest member — the
 *      tool_call_pending flag is not consulted at the working/idle
 *      branch, only at the stale/unclean branch.
 *   2. A parent in awaiting_user always renders "waiting" within the
 *      10m window even when a fork or sibling in the group is fresh.
 *      The parser flag is the stronger signal here: the agent has
 *      explicitly said "your turn".
 *
 * The parser flag always comes from the row's own session (the
 * parent's file is what's actually ambiguous), never from a child.
 *
 * Yellow (stale) and red (unclean) only fire when the parser has
 * positively flagged the session. Cleanly-finished or unclassified
 * sessions go straight from active → quiet — short-lived sessions
 * that complete normally don't pollute the sidebar with stale dots. */
export function getSessionStatus(
  session: SessionStatusInput,
  groupSessions?: SessionStatusInput[],
): SessionStatus {
  let freshest = recencyKey(session);
  if (groupSessions && groupSessions.length > 1) {
    for (const g of groupSessions) {
      const k = recencyKey(g);
      if (k > freshest) freshest = k;
    }
  }
  const ts = new Date(freshest).getTime();
  const age = now - ts;
  const term = session.termination_status;
  const flagged = term === "tool_call_pending" || term === "truncated";
  const awaitingUser = term === "awaiting_user";

  // awaiting_user wins over the freshness tier as soon as the
  // parser classifies it. The agent already told us "I'm done,
  // your turn", so we surface the waiting bubble even when a
  // related session in the group (e.g. a fork running in
  // parallel) is currently writing. For tool_call_pending parents
  // the freshness rollup still does its job — that flag isn't
  // checked here, so a parent in tool_call_pending with a fresh
  // subagent falls through to "working" below.
  if (awaitingUser && age < RECENTLY_ACTIVE_MS) return "waiting";

  if (age < FRESH_MS) return "working";
  if (age < RECENTLY_ACTIVE_MS) return "idle";
  if (!flagged) return "quiet";
  if (age < STALE_MS) return "stale";
  return "unclean";
}

/**
 * Walk parent_session_id chains to find the root session.
 * If a link is missing from the loaded set, the walk stops
 * there, forming a separate group for each disconnected
 * subchain.
 */
function findRoot(
  id: string,
  byId: Map<string, SessionGroupInput>,
  rootCache: Map<string, string>,
): string {
  const cached = rootCache.get(id);
  if (cached !== undefined) return cached;

  // Walk up, capping at set size to guard cycles.
  const visited = new Set<string>();
  let cur = id;
  while (true) {
    if (visited.has(cur)) break; // cycle guard
    visited.add(cur);
    const s = byId.get(cur);
    if (!s?.parent_session_id) break;
    const parent = s.parent_session_id;
    if (!byId.has(parent)) break; // missing link
    cur = parent;
  }

  // cur is the root — cache for every node we visited.
  for (const v of visited) {
    rootCache.set(v, cur);
  }
  return cur;
}

export function buildSessionGroups(
  sessions: SessionGroupInput[],
): SessionGroup[] {
  const byId = new Map<string, SessionGroupInput>();
  for (const s of sessions) {
    byId.set(s.id, s);
  }

  const rootCache = new Map<string, string>();
  const groupMap = new Map<string, SessionGroup>();
  const insertionOrder: string[] = [];

  for (const s of sessions) {
    const root = findRoot(s.id, byId, rootCache);
    // Sessions without a parent_session_id that aren't
    // pointed to by anyone get root == their own id, so
    // they form a single-session group naturally.
    const key = root;

    let group = groupMap.get(key);
    if (!group) {
      group = {
        key,
        project: s.project,
        sessions: [],
        primarySessionId: s.id,
        totalMessages: 0,
        firstMessage: null,
        startedAt: null,
        endedAt: null,
      };
      groupMap.set(key, group);
      insertionOrder.push(key);
    }

    group.sessions.push(s);
    group.totalMessages += s.message_count;
    group.startedAt = minString(group.startedAt, s.started_at);
    group.endedAt = maxString(group.endedAt, s.ended_at);
  }

  // Adopt orphaned teammate sessions so they NEVER appear at root level.
  // A session with <teammate-message in first_message is always a child;
  // if parent_session_id is missing, adopt it into the nearest non-teammate
  // root group in the same project (no time limit).
  const isTeammateSession = (s: SessionGroupInput) =>
    s.is_teammate ?? s.first_message?.includes("<teammate-message") ?? false;

  const keysToRemove = new Set<string>();

  // Build a per-project index of non-teammate root groups for adoption.
  const adoptTargets = new Map<string, string[]>(); // project -> group keys
  for (const [key, group] of groupMap) {
    // A valid adoption target is any group whose root session is NOT a teammate.
    const root = group.sessions.find((s) => s.id === key) ?? group.sessions[0]!;
    if (!isTeammateSession(root)) {
      let list = adoptTargets.get(group.project);
      if (!list) {
        list = [];
        adoptTargets.set(group.project, list);
      }
      list.push(key);
    }
  }

  // Collect all orphaned teammate groups (including multi-session ones
  // where the root itself is a teammate, e.g. a teammate that spawned
  // subagents).
  const orphanGroups: Array<{ key: string; group: SessionGroup; time: number }> = [];
  for (const [key, group] of groupMap) {
    const root = group.sessions.find((s) => s.id === key) ?? group.sessions[0]!;
    if (!isTeammateSession(root)) continue;
    if (root.parent_session_id) continue; // linked but parent not loaded — leave as-is
    orphanGroups.push({
      key,
      group,
      time: new Date(root.started_at ?? root.created_at ?? "1970-01-01").getTime(),
    });
  }

  // Pass 1: adopt orphans into the nearest non-teammate group in same project.
  for (const orphan of orphanGroups) {
    const candidates = adoptTargets.get(orphan.group.project);
    if (!candidates || candidates.length === 0) continue;

    let bestKey: string | null = null;
    let bestDist = Infinity;
    for (const ck of candidates) {
      const cg = groupMap.get(ck)!;
      const primary = cg.sessions.find((ss) => ss.id === ck) ?? cg.sessions[0]!;
      const cTime = new Date(primary.started_at ?? primary.created_at ?? "1970-01-01").getTime();
      const dist = Math.abs(orphan.time - cTime);
      if (dist < bestDist) {
        bestDist = dist;
        bestKey = ck;
      }
    }

    if (bestKey) {
      const target = groupMap.get(bestKey)!;
      for (const s of orphan.group.sessions) {
        target.sessions.push(s);
        target.totalMessages += s.message_count;
        target.startedAt = minString(target.startedAt, s.started_at);
        target.endedAt = maxString(target.endedAt, s.ended_at);
      }
      keysToRemove.add(orphan.key);
    }
  }

  // Pass 2: any remaining orphan teammates (project has no non-teammate
  // root group) — cluster all from same project into one group.
  const stillOrphaned = new Map<string, string[]>(); // project -> orphan keys
  for (const orphan of orphanGroups) {
    if (keysToRemove.has(orphan.key)) continue;
    let list = stillOrphaned.get(orphan.group.project);
    if (!list) {
      list = [];
      stillOrphaned.set(orphan.group.project, list);
    }
    list.push(orphan.key);
  }
  for (const [, keys] of stillOrphaned) {
    if (keys.length < 2) continue;
    const targetKey = keys[0]!;
    const target = groupMap.get(targetKey)!;
    for (let i = 1; i < keys.length; i++) {
      const src = groupMap.get(keys[i]!)!;
      for (const s of src.sessions) {
        target.sessions.push(s);
        target.totalMessages += s.message_count;
        target.startedAt = minString(target.startedAt, s.started_at);
        target.endedAt = maxString(target.endedAt, s.ended_at);
      }
      keysToRemove.add(keys[i]!);
    }
  }

  // Remove adopted orphan groups from the map and insertion order.
  for (const key of keysToRemove) {
    groupMap.delete(key);
  }

  for (const group of groupMap.values()) {
    if (group.sessions.length > 1) {
      group.sessions.sort((a, b) => {
        const ta = a.started_at ?? "";
        const tb = b.started_at ?? "";
        return ta < tb ? -1 : ta > tb ? 1 : 0;
      });
    }
    group.firstMessage = group.sessions[0]?.first_message ?? null;

    // For groups containing subagent children, the root session
    // should always be the main entry (not the most recent child).
    const hasSubagents = group.sessions.some(
      (s) => s.relationship_type === "subagent",
    );
    if (hasSubagents) {
      const rootIdx = group.sessions.findIndex((s) => s.id === group.key);
      group.primarySessionId =
        rootIdx >= 0
          ? group.sessions[rootIdx]!.id
          : group.sessions[0]!.id;
    } else {
      // For continuation chains, use the most recently active session.
      let bestIdx = 0;
      let bestKey = recencyKey(group.sessions[0]!);
      for (let i = 1; i < group.sessions.length; i++) {
        const k = recencyKey(group.sessions[i]!);
        if (k > bestKey) {
          bestKey = k;
          bestIdx = i;
        }
      }
      group.primarySessionId = group.sessions[bestIdx]!.id;
    }
  }

  const ordered = insertionOrder
    .filter((k) => !keysToRemove.has(k))
    .map((k) => groupMap.get(k)!);

  // Two-key sort:
  //   1. status priority — working → waiting → idle → stale →
  //      quiet → unclean. Awaiting-user rows sit above idle even
  //      when older, and unclean (terminated mid tool call) sinks
  //      to the very bottom so noise from old crashed sessions
  //      doesn't push live work off-screen.
  //   2. group freshness — within a tier, the group whose
  //      newest member was written most recently wins. Mirrors
  //      the time-since-last-update order the sidebar had before
  //      the status sort was added.
  ordered.sort((a, b) => {
    const sa = statusSortKey(a);
    const sb = statusSortKey(b);
    if (sa !== sb) return sa - sb;
    const ra = groupFreshness(a);
    const rb = groupFreshness(b);
    if (ra > rb) return -1;
    if (ra < rb) return 1;
    return 0;
  });
  return ordered;
}

function statusSortKey(group: SessionGroup): number {
  const primary =
    group.sessions.find((s) => s.id === group.primarySessionId) ??
    group.sessions[0]!;
  const status = getSessionStatus(primary, group.sessions);
  switch (status) {
    case "working":
      return 0;
    case "waiting":
      return 1;
    case "idle":
      return 2;
    case "stale":
      return 3;
    case "quiet":
      return 4;
    case "unclean":
      return 5;
  }
  return 6;
}

function groupFreshness(group: SessionGroup): string {
  // The freshest activity across any member of the group. A
  // subagent child's recent write counts as the group's
  // freshness so a parent waiting on a running child is sorted
  // by the child's activity.
  let best = "";
  for (const s of group.sessions) {
    const k = recencyKey(s);
    if (k > best) best = k;
  }
  return best;
}

export const sessions = createSessionsStore();

// Refresh project/agent dropdowns whenever a sync completes
// (local trigger or detected via status polling).
sync.onSyncComplete(() => {
  sessions.invalidateFilterCaches();
  sessions.load();
});
