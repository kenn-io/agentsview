import * as api from "../api/client.js";
import type { Session, ProjectInfo, AgentInfo } from "../api/types.js";

const SESSION_PAGE_SIZE = 500;

export interface SessionGroup {
  key: string;
  project: string;
  sessions: Session[];
  primarySessionId: string;
  totalMessages: number;
  firstMessage: string | null;
  startedAt: string | null;
  endedAt: string | null;
}

interface Filters {
  project: string;
  agent: string;
  date: string;
  dateFrom: string;
  dateTo: string;
  recentlyActive: boolean;
  hideUnknownProject: boolean;
  minMessages: number;
  maxMessages: number;
  minUserMessages: number;
}

function defaultFilters(): Filters {
  return {
    project: "",
    agent: "",
    date: "",
    dateFrom: "",
    dateTo: "",
    recentlyActive: false,
    hideUnknownProject: false,
    minMessages: 0,
    maxMessages: 0,
    minUserMessages: 0,
  };
}

class SessionsStore {
  sessions: Session[] = $state([]);
  projects: ProjectInfo[] = $state([]);
  agents: AgentInfo[] = $state([]);
  activeSessionId: string | null = $state(null);
  nextCursor: string | null = $state(null);
  total: number = $state(0);
  loading: boolean = $state(false);
  filters: Filters = $state(defaultFilters());

  /** Set before router.navigate() to survive initFromParams() reset. */
  pendingNavTarget: string | null = null;

  private loadVersion: number = 0;
  private projectsLoaded: boolean = false;
  private projectsPromise: Promise<void> | null = null;
  private agentsLoaded: boolean = false;
  private agentsPromise: Promise<void> | null = null;

  get activeSession(): Session | undefined {
    return this.sessions.find((s) => s.id === this.activeSessionId);
  }

  get groupedSessions(): SessionGroup[] {
    return buildSessionGroups(this.sessions);
  }

  private get apiParams() {
    const f = this.filters;
    // Don't exclude "unknown" when explicitly viewing it.
    const exclude =
      f.hideUnknownProject && f.project !== "unknown"
        ? "unknown"
        : undefined;
    return {
      project: f.project || undefined,
      exclude_project: exclude,
      agent: f.agent || undefined,
      date: f.date || undefined,
      date_from: f.dateFrom || undefined,
      date_to: f.dateTo || undefined,
      active_since: f.recentlyActive
        ? new Date(
            Date.now() - 24 * 60 * 60 * 1000,
          ).toISOString()
        : undefined,
      min_messages:
        f.minMessages > 0 ? f.minMessages : undefined,
      max_messages:
        f.maxMessages > 0 ? f.maxMessages : undefined,
      min_user_messages:
        f.minUserMessages > 0 ? f.minUserMessages : undefined,
    };
  }

  private resetPagination() {
    this.sessions = [];
    this.nextCursor = null;
    this.total = 0;
  }

  initFromParams(params: Record<string, string>) {
    const minMsgs = parseInt(
      params["min_messages"] ?? "",
      10,
    );
    const maxMsgs = parseInt(
      params["max_messages"] ?? "",
      10,
    );
    const minUserMsgs = parseInt(
      params["min_user_messages"] ?? "",
      10,
    );

    const hideUnknown =
      params["exclude_project"] === "unknown";
    let project = params["project"] ?? "";
    if (hideUnknown && project === "unknown") {
      project = "";
    }

    this.filters = {
      project,
      agent: params["agent"] ?? "",
      date: params["date"] ?? "",
      dateFrom: params["date_from"] ?? "",
      dateTo: params["date_to"] ?? "",
      recentlyActive: params["active_since"] === "true",
      hideUnknownProject: hideUnknown,
      minMessages: Number.isFinite(minMsgs) ? minMsgs : 0,
      maxMessages: Number.isFinite(maxMsgs) ? maxMsgs : 0,
      minUserMessages: Number.isFinite(minUserMsgs)
        ? minUserMsgs
        : 0,
    };
    if (this.pendingNavTarget) {
      this.activeSessionId = this.pendingNavTarget;
      this.pendingNavTarget = null;
    } else {
      this.activeSessionId = null;
    }
    this.resetPagination();
  }

  async load() {
    const version = ++this.loadVersion;
    this.loading = true;
    this.resetPagination();
    try {
      let cursor: string | undefined = undefined;
      let loaded: Session[] = [];

      for (;;) {
        if (this.loadVersion !== version) return;
        const page = await api.listSessions({
          ...this.apiParams,
          cursor,
          limit: SESSION_PAGE_SIZE,
        });
        if (this.loadVersion !== version) return;

        if (page.sessions.length === 0) {
          this.sessions = loaded;
          this.nextCursor = null;
          this.total = loaded.length;
          break;
        }

        loaded = [...loaded, ...page.sessions];
        this.sessions = loaded;
        // Keep total aligned with loaded rows to avoid blank
        // virtual space while we fetch remaining pages.
        this.total = loaded.length;

        cursor = page.next_cursor ?? undefined;
        this.nextCursor = cursor ?? null;
        if (!cursor) {
          this.total = loaded.length;
          break;
        }
      }
    } finally {
      if (this.loadVersion === version) {
        this.loading = false;
      }
    }
  }

  async loadMore() {
    if (!this.nextCursor || this.loading) return;
    const version = ++this.loadVersion;
    this.loading = true;
    try {
      const page = await api.listSessions({
        ...this.apiParams,
        cursor: this.nextCursor,
        limit: SESSION_PAGE_SIZE,
      });
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
    this.projectsPromise = (async () => {
      try {
        const res = await api.getProjects();
        this.projects = res.projects;
        this.projectsLoaded = true;
      } finally {
        this.projectsPromise = null;
      }
    })();
    return this.projectsPromise;
  }

  async loadAgents() {
    if (this.agentsLoaded) return;
    if (this.agentsPromise) return this.agentsPromise;
    this.agentsPromise = (async () => {
      try {
        const res = await api.getAgents();
        this.agents = res.agents;
        this.agentsLoaded = true;
      } finally {
        this.agentsPromise = null;
      }
    })();
    return this.agentsPromise;
  }

  selectSession(id: string) {
    this.activeSessionId = id;
  }

  deselectSession() {
    this.activeSessionId = null;
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
      this.activeSessionId = list[edge]!.id;
      return;
    }
    const next = idx + delta;
    if (next >= 0 && next < list.length) {
      this.activeSessionId = list[next]!.id;
    }
  }

  setProjectFilter(project: string) {
    this.filters = { ...defaultFilters(), project, agent: this.filters.agent };
    this.activeSessionId = null;
    this.resetPagination();
    this.load();
  }

  setAgentFilter(agent: string) {
    if (this.filters.agent === agent) {
      this.filters.agent = "";
    } else {
      this.filters.agent = agent;
    }
    this.activeSessionId = null;
    this.resetPagination();
    this.load();
  }

  setRecentlyActiveFilter(active: boolean) {
    this.filters.recentlyActive = active;
    this.activeSessionId = null;
    this.resetPagination();
    this.load();
  }

  setMinUserMessagesFilter(n: number) {
    this.filters.minUserMessages = n;
    this.activeSessionId = null;
    this.resetPagination();
    this.load();
  }

  setHideUnknownProjectFilter(hide: boolean) {
    this.filters.hideUnknownProject = hide;
    if (hide && this.filters.project === "unknown") {
      this.filters.project = "";
    }
    this.activeSessionId = null;
    this.resetPagination();
    this.load();
  }

  get hasActiveFilters(): boolean {
    const f = this.filters;
    return !!(
      f.agent ||
      f.recentlyActive ||
      f.hideUnknownProject ||
      f.dateFrom ||
      f.dateTo ||
      f.date ||
      f.minUserMessages > 0
    );
  }

  clearSessionFilters() {
    const project = this.filters.project;
    this.filters = { ...defaultFilters(), project };
    this.activeSessionId = null;
    this.resetPagination();
    this.load();
  }

  /** Recently deleted session IDs for undo toast. */
  recentlyDeleted: { id: string; timer: ReturnType<typeof setTimeout> }[] =
    $state([]);

  async deleteSession(id: string) {
    await api.deleteSession(id);
    const before = this.sessions.length;
    this.sessions = this.sessions.filter((s) => s.id !== id);
    const removed = before - this.sessions.length;
    if (removed > 0) {
      this.total = Math.max(0, this.total - removed);
    }
    if (this.activeSessionId === id) {
      this.activeSessionId = null;
    }
    const timer = setTimeout(() => {
      this.recentlyDeleted = this.recentlyDeleted.filter(
        (d) => d.id !== id,
      );
    }, 10_000);
    this.recentlyDeleted = [...this.recentlyDeleted, { id, timer }];
  }

  async restoreSession(id: string) {
    await api.restoreSession(id);
    this.clearRecentlyDeleted(id);
    await this.load();
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
    const updated = await api.renameSession(id, displayName);
    const idx = this.sessions.findIndex((s) => s.id === id);
    if (idx !== -1) {
      this.sessions[idx] = { ...this.sessions[idx]!, ...updated };
    }
  }
}

export function createSessionsStore(): SessionsStore {
  return new SessionsStore();
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

function recencyKey(s: Session): string {
  return s.ended_at ?? s.started_at ?? s.created_at;
}

const RECENTLY_ACTIVE_MS = 10 * 60 * 1000;

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

/**
 * Walk parent_session_id chains to find the root session.
 * If a link is missing from the loaded set, the walk stops
 * there, forming a separate group for each disconnected
 * subchain.
 */
function findRoot(
  id: string,
  byId: Map<string, Session>,
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
    if (s.relationship_type === "fork") break;
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

export function buildSessionGroups(sessions: Session[]): SessionGroup[] {
  const byId = new Map<string, Session>();
  for (const s of sessions) {
    byId.set(s.id, s);
  }

  const rootCache = new Map<string, string>();
  const groupMap = new Map<string, SessionGroup>();
  const insertionOrder: string[] = [];

  for (const s of sessions) {
    // Subagent sessions are only visible through their parent.
    if (s.relationship_type === "subagent") continue;

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

  for (const group of groupMap.values()) {
    if (group.sessions.length > 1) {
      group.sessions.sort((a, b) => {
        const ta = a.started_at ?? "";
        const tb = b.started_at ?? "";
        return ta < tb ? -1 : ta > tb ? 1 : 0;
      });
    }
    group.firstMessage = group.sessions[0]?.first_message ?? null;

    let bestIdx = 0;
    let bestKey = recencyKey(group.sessions[0]!);
    for (let i = 1; i < group.sessions.length; i++) {
      const key = recencyKey(group.sessions[i]!);
      if (key > bestKey) {
        bestKey = key;
        bestIdx = i;
      }
    }
    group.primarySessionId = group.sessions[bestIdx]!.id;
  }

  return insertionOrder.map((k) => groupMap.get(k)!);
}

export const sessions = createSessionsStore();
