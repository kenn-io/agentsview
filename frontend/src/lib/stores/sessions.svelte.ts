import * as api from "../api/client.js";
import type { Session, ProjectInfo, AgentInfo } from "../api/types.js";
import { sync } from "./sync.svelte.js";

const SESSION_PAGE_SIZE = 500;

export interface SessionGroup {
  key: string;
  project: string;
  sessions: Session[];
  /** Unfiltered session list for ancestry classification.
   *  Set when a filter (e.g. starred) removes sessions from the group. */
  allSessions?: Session[];
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
  includeOneShot: boolean;
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
    includeOneShot: false,
  };
}

class SessionsStore {
  sessions: Session[] = $state([]);
  projects: ProjectInfo[] = $state([]);
  agents: AgentInfo[] = $state([]);
  activeSessionId: string | null = $state(null);
  childSessions: Map<string, Session> = $state(new Map());
  nextCursor: string | null = $state(null);
  total: number = $state(0);
  loading: boolean = $state(false);
  filters: Filters = $state(defaultFilters());

  /** Set before router.navigate() to survive initFromParams() reset. */
  pendingNavTarget: string | null = null;

  private loadVersion: number = 0;
  private projectsLoaded: boolean = false;
  private projectsPromise: Promise<void> | null = null;
  private projectsVersion: number = 0;
  private agentsLoaded: boolean = false;
  private agentsPromise: Promise<void> | null = null;
  private agentsVersion: number = 0;
  private refreshVersion: number = 0;
  private childSessionsVersion: number = 0;

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
      include_one_shot: f.includeOneShot || undefined,
      include_children: true,
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

    const prevOneShot = this.filters.includeOneShot;
    const nextOneShot =
      params["include_one_shot"] === "true";

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
      includeOneShot: nextOneShot,
    };
    if (prevOneShot !== nextOneShot) {
      this.invalidateFilterCaches();
    }
    if (this.pendingNavTarget) {
      this.setActiveSession(this.pendingNavTarget);
      this.pendingNavTarget = null;
    } else {
      this.setActiveSession(null);
    }
  }

  async load() {
    const version = ++this.loadVersion;
    this.loading = true;
    // Preserve old data during reload — clearing eagerly
    // causes a flash because the sidebar and content area
    // briefly see an empty session list.
    const prev = {
      sessions: this.sessions,
      nextCursor: this.nextCursor,
      total: this.total,
    };
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
    const ver = this.projectsVersion;
    this.projectsPromise = (async () => {
      try {
        const params = this.filters.includeOneShot
          ? { include_one_shot: true as const }
          : {};
        const res = await api.getProjects(params);
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
        const params = this.filters.includeOneShot
          ? { include_one_shot: true as const }
          : {};
        const res = await api.getAgents(params);
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

  private setActiveSession(id: string | null) {
    if (id === this.activeSessionId) return;
    this.activeSessionId = id;
    this.refreshVersion++;
    this.childSessionsVersion++;
  }

  selectSession(id: string) {
    this.setActiveSession(id);
  }

  /**
   * Navigate to a session by ID, loading it into the sessions list if
   * not already present (e.g. subagent sessions filtered from groups).
   */
  async navigateToSession(id: string) {
    const existing = this.sessions.find((s) => s.id === id);
    if (!existing) {
      try {
        const session = await api.getSession(id);
        this.sessions = [...this.sessions, session];
      } catch {
        // Session not found - still attempt to select
      }
    }
    this.setActiveSession(id);
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
      const session = await api.getSession(id);
      if (
        this.refreshVersion !== version ||
        this.activeSessionId !== id
      ) {
        return;
      }
      const idx = this.sessions.findIndex((s) => s.id === id);
      if (idx >= 0) {
        this.sessions[idx] = session;
      }
    } catch {
      // Session may have been deleted
    }
  }

  async loadChildSessions(parentId: string) {
    const version = ++this.childSessionsVersion;
    try {
      const children = await api.getChildSessions(parentId);
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
      this.setActiveSession(list[edge]!.id);
      return;
    }
    const next = idx + delta;
    if (next >= 0 && next < list.length) {
      this.setActiveSession(list[next]!.id);
    }
  }

  setProjectFilter(project: string) {
    const wasOneShot = this.filters.includeOneShot;
    this.filters = { ...defaultFilters(), project, agent: this.filters.agent };
    this.setActiveSession(null);
    if (wasOneShot) this.invalidateFilterCaches();
    this.load();
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

  get hasActiveFilters(): boolean {
    const f = this.filters;
    return !!(
      f.agent ||
      f.recentlyActive ||
      f.hideUnknownProject ||
      f.dateFrom ||
      f.dateTo ||
      f.date ||
      f.minUserMessages > 0 ||
      f.includeOneShot
    );
  }

  clearSessionFilters() {
    const project = this.filters.project;
    const wasOneShot = this.filters.includeOneShot;
    this.filters = { ...defaultFilters(), project };
    this.setActiveSession(null);
    if (wasOneShot) this.invalidateFilterCaches();
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
    await api.restoreSession(id);
    this.clearRecentlyDeleted(id);
    this.invalidateFilterCaches();
    await this.load();
  }

  invalidateFilterCaches() {
    this.projectsVersion++;
    this.projectsLoaded = false;
    this.projectsPromise = null;
    this.agentsVersion++;
    this.agentsLoaded = false;
    this.agentsPromise = null;
    this.loadProjects();
    this.loadAgents();
    sync.loadStats(
      this.filters.includeOneShot
        ? { include_one_shot: true }
        : {},
    );
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
  const isTeammateSession = (s: Session) =>
    s.first_message?.includes("<teammate-message") ?? false;

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

  return insertionOrder
    .filter((k) => !keysToRemove.has(k))
    .map((k) => groupMap.get(k)!);
}

export const sessions = createSessionsStore();

// Refresh project/agent dropdowns whenever a sync completes
// (local trigger or detected via status polling).
sync.onSyncComplete(() => {
  sessions.invalidateFilterCaches();
  sessions.load();
});
