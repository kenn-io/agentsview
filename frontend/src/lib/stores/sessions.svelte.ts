import type { DataChangedEvent } from "../api/client.js";
import {
  MetadataService,
  SessionsService,
} from "../api/generated/index";
import {
  ApiError,
  callGenerated,
  configureGeneratedClient,
  isAbortError,
} from "../api/runtime.js";
import type {
  Session,
  ProjectInfo,
  AgentInfo,
  SidebarSessionIndexResponse,
  SidebarSessionIndexRow,
} from "../api/types.js";
import { sync } from "./sync.svelte.js";
import { events } from "./events.svelte.js";
import { starred } from "./starred.svelte.js";
import { yokedDates } from "./yokedDates.svelte.js";
import {
  SESSION_ANALYTICS_WINDOW_PARAM,
  parseWindowDaysParam,
} from "./sessionRouteParams.js";
import { rollingRange } from "../utils/dates.js";
import { LatestRead } from "../utils/latest-read.js";

type SidebarIndexParams = Parameters<
  typeof SessionsService.getApiV1SessionsSidebarIndex
>[0];
type MetadataParams = Parameters<
  typeof MetadataService.getApiV1Projects
>[0];
type ClearSessionFiltersOptions = {
  clearDateYoke?: boolean;
};
type LoadOptions = {
  force?: boolean;
};

const SESSION_PAGE_SIZE = 500;
const SIDEBAR_HYDRATION_CONCURRENCY = 6;
const LIVE_REFRESH_DEBOUNCE_MS = 300;
const SAFETY_NET_REFRESH_MS = 5 * 60 * 1000;
const RECENTLY_DELETED_TTL_MS = 10_000;

export interface SessionGroupInput {
  id: string;
  parent_session_id?: string | null;
  relationship_type?: string | null;
  project: string;
  machine: string;
  agent: string;
  agent_label?: string | null;
  entrypoint?: string | null;
  first_message?: string | null;
  display_name?: string | null;
  started_at: string | null;
  ended_at: string | null;
  created_at: string;
  termination_status?: string | null;
  message_count: number;
  user_message_count?: number;
  transcript_revision?: string;
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

export interface RecentlyDeletedSessions {
  key: number;
  ids: string[];
  timer: ReturnType<typeof setTimeout>;
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
// v2 marks entries whose date bounds carry provenance: rolling bounds are
// persisted as intent (`windowDays`) and rematerialized on load, never as
// pinned dates. Unversioned entries predate that guarantee and may hold
// rolling bounds saved as if explicit (#1086).
const SESSION_FILTERS_VERSION = 2;

interface SavedFilters {
  filters: Filters;
  windowDays: number | null;
}

function validWindowDays(value: unknown): value is number {
  return typeof value === "number" && Number.isInteger(value) && value > 0;
}

function loadSavedFilters(): SavedFilters {
  try {
    const raw = localStorage.getItem(SESSION_FILTERS_KEY);
    if (raw) {
      const { version, windowDays, ...saved } = JSON.parse(
        raw,
      ) as Partial<Filters> & { version?: unknown; windowDays?: unknown };
      const filters = { ...defaultFilters(), ...saved };
      // Deliberately `!==`, not `<`: an entry written by a newer (or older)
      // format is not trusted either way — dropping date bounds is the
      // fail-safe direction in both.
      if (version !== SESSION_FILTERS_VERSION) {
        // Legacy bounds have unknown provenance. Dropping them once is the
        // safe direction: an intentional range is re-picked in one click,
        // while a poisoned one keeps silently hiding new sessions.
        filters.date = "";
        filters.dateFrom = "";
        filters.dateTo = "";
        saveFilters(filters);
        return { filters, windowDays: null };
      }
      if (validWindowDays(windowDays)) {
        // Rolling intent survives restarts; its bounds are recomputed
        // against the current date so the window keeps rolling forward.
        const range = rollingRange(windowDays);
        filters.date = "";
        filters.dateFrom = range.from;
        filters.dateTo = range.to;
        return { filters, windowDays };
      }
      return { filters, windowDays: null };
    }
  } catch {
    // Corrupted localStorage — fall back to defaults.
  }
  return { filters: defaultFilters(), windowDays: null };
}

function saveFilters(f: Filters, windowDays: number | null = null): void {
  // Rolling bounds are persisted as intent (windowDays) and rematerialized
  // on load; the materialized dates themselves are session-scoped. Storing
  // them verbatim would pin the window to the day it was saved, silently
  // hiding newer sessions (#1086).
  const toSave =
    windowDays !== null
      ? { ...f, date: "", dateFrom: "", dateTo: "", windowDays }
      : f;
  try {
    localStorage.setItem(
      SESSION_FILTERS_KEY,
      JSON.stringify({ ...toSave, version: SESSION_FILTERS_VERSION }),
    );
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

function hasDateFilters(f: Filters): boolean {
  return !!(f.date || f.dateFrom || f.dateTo);
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
  // Resolved detail for the open session, held outside the sidebar list so a
  // session that falls outside the current filters or page (e.g. opened from
  // search) can back activeSession without polluting groupedSessions or being
  // double-counted when loadMore appends a later page. Cleared on navigation.
  activeSessionDetail: Session | null = $state(null);
  activeSessionUsageVersion: number = $state(0);
  childSessions: Map<string, Session> = $state(new Map());
  nextCursor: string | null = $state(null);
  total: number = $state(0);
  loading: boolean = $state(false);
  #savedFilters = loadSavedFilters();
  filters: Filters = $state(this.#savedFilters.filters);
  /** Rolling window (in days) behind the current date bounds, or null when
   *  the bounds were chosen explicitly. Persisted as intent and
   *  rematerialized on load so the window keeps rolling forward (#1086). */
  dateFiltersWindowDays: number | null = $state(
    this.#savedFilters.windowDays,
  );

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
  private sidebarConsumers = 0;
  private sidebarLoadPromise: Promise<void> | null = null;
  private sidebarLoadSignature: string | null = null;
  private sidebarAbort: AbortController | null = null;
  private routeAbort: AbortController | null = null;
  // Single coordinator for every read that commits to activeSessionDetail
  // (navigation and watcher refresh): the newest read cancels older in-flight
  // ones, so a stale response resolving late can never overwrite fresher
  // cached detail. The commit generations extend that freshness to sidebar
  // hydration and index reconciliation, which cannot claim the coordinator
  // themselves (hydration shares its fetches with non-active rows). A
  // session's commit generation advances only when fresher state actually
  // COMMITS to its active-detail state: a navigation or refresh resolving, a
  // rename or hydration committing its snapshot, a 404 committing a
  // deletion. A read that merely BEGAN proves nothing — it may fail, and if
  // it commits it commits later and wins — so it never invalidates anything.
  // Generations are drawn from one global counter (see
  // detailCommitGenerationCounter) so entries pruned by
  // pruneReconciliationState can never be recreated with a generation an
  // in-flight snapshot already captured; per-session maps keep activity on
  // one session from invalidating another's in-flight hydration, which a
  // later selection may join.
  private activeDetailRead = new LatestRead();
  // Each commit records the sidebar index-request ordinal current when its
  // underlying request was issued (Infinity for write-derived rename/404
  // commits, which reflect a server mutation and always win). The post-index
  // reconciliation restores committed detail over the index only when the
  // commit's request is at least as new as the index request — a detail read
  // issued before the index was requested cannot outrank the index's later
  // server snapshot.
  private activeDetailCommitBySession = new Map<
    string,
    {
      generation: number;
      deleted: boolean;
      issuedAtIndexOrdinal: number;
      committedAtTick: number;
      // For deleted commits: whether the tombstone actually removed a
      // sidebar row. Revival restores a row only if one was removed — a
      // cache-only session excluded by the filter must not be injected
      // into the list. Root status is recomputed at append time.
      removedRow: boolean;
    }
  >();
  // Generations are globally unique (not per-id counters) so pruning an
  // entry and later recreating it can never produce a generation equal to
  // one an in-flight request captured in its snapshot.
  private detailCommitGenerationCounter = 0;
  // Issue ticks of every in-flight request (index loads and detail reads).
  // Reconciliation entries committed before the oldest in-flight request's
  // issue can no longer influence any comparison — in-flight snapshots saw
  // them as unchanged, and future requests get larger ticks — so publish
  // prunes them, keeping these maps bounded by live rows plus in-flight
  // work instead of every session ever seen.
  private inFlightRequestTicks = new Set<number>();
  // Last committed session snapshot per id, kept so an index publish can
  // honor a superseding commit even when no prior sidebar row exists (a
  // cache-only session renamed and then deselected). Written by non-deletion
  // commits, dropped by deletion commits; overwritten in place, so it stays
  // bounded by the sessions that ever committed detail.
  private committedDetailByRow = new Map<string, Session>();
  // Issue-order counter for sidebar index requests. Read-derived hydration
  // commits only count as commits when no newer index request was issued
  // after the hydration's fetch began: a hydration that predates the index
  // request cannot carry fresher server state than the index's snapshot, so
  // letting it win the post-index reconciliation would revert newer index
  // fields. Rename and 404 commits are write-derived and always win;
  // navigation/refresh commits stay unconditional because the coordinator
  // already serializes them against each other.
  //
  // requestClock is one monotonic issue-order clock shared by sidebar index
  // loads AND detail reads (navigation/refresh/hydration): every request
  // takes a tick when its fetch begins, commits and row stamps record their
  // request's tick, and freshness is strict tick comparison. A shared clock
  // makes ties impossible, so two detail reads issued within the same index
  // generation are still totally ordered.
  private requestClock = 0;
  // Per-row stamp of the index REQUEST ordinal whose commit last
  // re-published each row. A detail response (navigation/refresh/hydration)
  // defers its index-owned fields (display_name, counts, timestamps) to the
  // row only when the stamping index request was issued AFTER the detail
  // request began — then the index's server snapshot is the newer one. An
  // index issued before the detail request, or a commit that never re-listed
  // the row (an unrelated loadMore page), says nothing newer, so the
  // response applies wholesale. Together with the commit records above this
  // makes request-issue order the single freshness rule in both directions.
  private indexCommitByRow = new Map<
    string,
    { ordinal: number; row: Session }
  >();
  // True while activeSessionDetail is a fabricated rename snapshot (the
  // rename endpoint returns the DB-session shape without detail-only
  // fields). Cleared by any read-derived commit for the active session;
  // a rename seeing it still set re-issues the enrichment fetch, so rapid
  // renames cannot cancel the only fetch and strand the interim shape.
  private interimActiveDetail = false;
  private childSessionsRead = new LatestRead();

  private liveRefreshStarted = false;
  private unsubEvents: (() => void) | null = null;
  private liveRefreshTimer: ReturnType<typeof setTimeout> | null = null;
  private safetyNetTimer: ReturnType<typeof setInterval> | null = null;

  get activeSession(): Session | undefined {
    const session = this.sessions.find((s) => s.id === this.activeSessionId);
    if (session && !session.is_index_only) return session;
    // The open session may be absent from the filtered/paginated sidebar list
    // (opened from search, filtered out, or on an unloaded page), or present
    // only as an index-only stub. Fall back to the separately resolved detail.
    const cached = this.activeSessionDetail;
    if (cached && cached.id === this.activeSessionId) return cached;
    return undefined;
  }

  get groupedSessions(): SessionGroup[] {
    return buildSessionGroups(this.sessions);
  }

  private get apiParams(): SidebarIndexParams {
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
      starred: starred.filterOnly || undefined,
    };
  }

  private resetPagination() {
    this.sessions = [];
    this.nextCursor = null;
    this.total = 0;
  }

  attachSidebar(): () => void {
    this.sidebarConsumers++;
    this.startLiveRefresh();
    let detached = false;
    return () => {
      if (detached) return;
      detached = true;
      this.sidebarConsumers = Math.max(0, this.sidebarConsumers - 1);
      if (this.sidebarConsumers === 0) {
        this.dispose();
      }
    };
  }

  /** Set date filters materialized from a panel date state. `windowDays`
   *  carries the rolling intent behind the bounds (null for explicitly
   *  chosen fixed ranges). */
  applyPanelDateFilters(
    dateParams: Record<string, string>,
    windowDays: number | null,
  ): void {
    this.filters.date = dateParams["date"] ?? "";
    this.filters.dateFrom = dateParams["date_from"] ?? "";
    this.filters.dateTo = dateParams["date_to"] ?? "";
    this.dateFiltersWindowDays = windowDays;
    // Persist immediately: a provenance flip with identical bounds does
    // not register as a filter change, so callers that diff serialized
    // filters may never trigger a load() and its save.
    saveFilters(this.filters, windowDays);
  }

  initFromParams(params: Record<string, string>) {
    const prevOneShot = this.filters.includeOneShot;
    const prevAutomated = this.filters.includeAutomated;
    const next = parseFiltersFromParams(params);
    this.filters = next;
    this.dateFiltersWindowDays = parseWindowDaysParam(
      params[SESSION_ANALYTICS_WINDOW_PARAM],
    );
    if (prevOneShot !== next.includeOneShot ||
        prevAutomated !== next.includeAutomated) {
      this.invalidateFilterCaches();
    }
    this.setActiveSession(null);
  }

  async load(options: LoadOptions = {}) {
    saveFilters(this.filters, this.dateFiltersWindowDays);

    const params = {
      ...this.apiParams,
      limit: SESSION_PAGE_SIZE,
    };
    const signature = JSON.stringify(params);
    if (
      !options.force &&
      this.sidebarLoadPromise !== null &&
      this.sidebarLoadSignature === signature
    ) {
      return this.sidebarLoadPromise;
    }

    this.sidebarAbort?.abort();
    const controller = new AbortController();
    this.sidebarAbort = controller;
    const promise = this.loadSidebarPage(params, controller.signal);
    this.sidebarLoadPromise = promise;
    this.sidebarLoadSignature = signature;
    try {
      await promise;
    } finally {
      if (this.sidebarLoadPromise === promise) {
        this.sidebarLoadPromise = null;
        this.sidebarLoadSignature = null;
        if (this.sidebarAbort === controller) {
          this.sidebarAbort = null;
        }
      }
    }
  }

  refreshSidebarIfAttached() {
    if (this.sidebarConsumers === 0) return;
    void this.load();
  }

  private async loadSidebarPage(
    params: SidebarIndexParams,
    signal: AbortSignal,
  ) {
    const version = ++this.loadVersion;
    const indexVersion = this.sidebarIndexVersion + 1;
    const requestOrdinal = ++this.requestClock;
    this.inFlightRequestTicks.add(requestOrdinal);
    const detailCommits = this.snapshotDetailCommits();
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
      const index = await callGenerated(
        () => SessionsService.getApiV1SessionsSidebarIndex(params),
        signal,
      ) as unknown as SidebarSessionIndexResponse;
      if (this.loadVersion !== version) return;

      this.sidebarIndexVersion = indexVersion;
      this.hydratedSessionsByVersion.set(indexVersion, new Map());
      this.sidebarHydrationEpochByVersion.set(indexVersion, 0);
      this.pruneSidebarHydrationVersions(indexVersion);
      const existing = new Map(this.sessions.map((session) => [
        session.id,
        session,
      ]));
      // A deletion that committed while this index was in flight supersedes
      // the index's older server snapshot: drop tombstoned rows — and their
      // descendants, whose groups the backend removes with them — instead
      // of reinserting them.
      const tombstoned = this.expandTombstoned(index.sessions, (rid) =>
        this.deletedSinceSnapshot(rid, detailCommits, requestOrdinal)
      );
      const dropped = index.sessions.filter((row) => tombstoned.has(row.id));
      const rows = index.sessions.filter((row) => !tombstoned.has(row.id));
      const incomingIds = new Set(index.sessions.map((row) => row.id));
      const published = rows.map((row) => {
        const prior = existing.get(row.id);
        // A rename/read that committed for this row after this index was
        // requested outranks the index's snapshot — keep the prior row, or
        // the retained committed snapshot when the session had no row yet.
        if (this.commitSupersedesIndex(row.id, detailCommits, requestOrdinal)) {
          if (prior) return prior;
          const committed = this.committedDetailByRow.get(row.id);
          if (committed) return { ...committed };
        }
        return sidebarIndexRowToSession(row, prior);
      });
      this.sessions = published;
      this.nextCursor = index.next_cursor ?? null;
      this.total = Math.max(
        0,
        index.total - this.countDroppedRoots(dropped, incomingIds),
      );
      for (const session of published) {
        this.indexCommitByRow.set(session.id, {
          ordinal: requestOrdinal,
          row: session,
        });
      }
      this.syncActiveSessionAfterIndexCommit(detailCommits, requestOrdinal);
      this.pruneReconciliationState();
    } catch {
      // Restore previous state so a transient failure
      // doesn't wipe the visible session list — but reapply deletion
      // tombstones committed while this load was in flight: a refresh 404
      // removed its row from the live list, and the pre-load snapshot still
      // contains it.
      if (this.loadVersion === version) {
        const restoredTombstones = this.expandTombstoned(
          prev.sessions,
          // A failed load carries no newer server state, so unlike a
          // successful publish it cannot supersede a read-derived
          // tombstone: honor anything committed after the snapshot.
          (rid) => this.deletedSinceSnapshot(rid, detailCommits, 0),
        );
        const droppedRows = prev.sessions.filter((s) =>
          restoredTombstones.has(s.id)
        );
        const prevIds = new Set(prev.sessions.map((s) => s.id));
        this.sessions = prev.sessions.filter(
          (s) => !restoredTombstones.has(s.id),
        );
        this.nextCursor = prev.nextCursor;
        this.total = Math.max(
          0,
          prev.total - this.countDroppedRoots(droppedRows, prevIds),
        );
      }
    } finally {
      this.inFlightRequestTicks.delete(requestOrdinal);
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
    const signal = this.routeSignal();

    await Promise.all(uniqueIds.map((id) => {
      if (cache.has(id)) return;
      const existing = inflight.get(id);
      if (existing) return existing;

      const promise = this.runSidebarHydration(async () => {
        if (signal.aborted) return;
        const detailCommitGeneration = this.detailCommitGeneration(id);
        const issuedAtIndexOrdinal = ++this.requestClock;
        this.inFlightRequestTicks.add(issuedAtIndexOrdinal);
        try {
          configureGeneratedClient();
          const raw = await callGenerated(
            () => SessionsService.getApiV1SessionsId({ id }),
            signal,
          ) as unknown as Session;
          // The version/epoch checks below catch full reloads, but a
          // loadMore page committing mid-flight re-lists rows without
          // changing the index version; reconcile against that too.
          const hydrated = this.reconcileDetailResponse(
            id,
            raw,
            issuedAtIndexOrdinal,
          );
          // A superseded hydration still carries valid detail for the active
          // session, so let it back the breadcrumb when the new index excludes
          // the row — but only fill an empty cache, and only when no newer
          // detail COMMITTED for THIS session while this response was in
          // flight (another session's activity says nothing about this one,
          // and a read that merely began — possibly failing — proves nothing
          // either; if it does commit, it commits later and wins). A stale
          // response must never overwrite detail already committed for this
          // session by a newer request; the fresh path below
          // (mergeHydratedSession) owns updates once the version check
          // passes. Successful active-detail writes count as commits so a
          // staler index response cannot overwrite them.
          // A commit invalidates this hydration only when its underlying
          // request was issued no earlier than this one's: a superseded
          // hydration from an older index version seeding the cache must
          // not discard this later-issued response, while a rename/404
          // (Infinity) or a read issued at or after this one still does.
          const latestCommit = this.activeDetailCommitBySession.get(id);
          const detailFresh =
            detailCommitGeneration === this.detailCommitGeneration(id) ||
            (latestCommit?.issuedAtIndexOrdinal ?? 0) < issuedAtIndexOrdinal;
          // Commit for the active session even over a populated cache: the
          // reconciled response carries any newer index-owned fields, and
          // when a reload has excluded the row this is the only update the
          // off-list session will get (the version check below discards the
          // row merge, and no hydration retries an absent row).
          if (
            hydrated.id === this.activeSessionId &&
            !hydrated.is_index_only &&
            detailFresh
          ) {
            this.activeSessionDetail = hydrated;
            this.bumpDetailCommit(id, false, issuedAtIndexOrdinal, hydrated);
          }
          if (
            version !== this.sidebarIndexVersion ||
            epoch !== (this.sidebarHydrationEpochByVersion.get(version) ?? 0)
          ) {
            return;
          }
          // A hydration issued before a newer commit resolved must not
          // clobber the row or the cache with its older snapshot. This
          // holds for every row: a rename commits for its session id even
          // if the user has since selected another session.
          if (!detailFresh) {
            return;
          }
          cache.set(id, hydrated);
          this.mergeHydratedSession(hydrated);
          // Record the commit for every merged row, active or not: the
          // session may be selected before an older index request resolves,
          // and the publish-side supersedes check needs the ordinal.
          this.bumpDetailCommit(id, false, issuedAtIndexOrdinal, hydrated);
        } catch {
          // Visible hydration is best-effort; the skinny row remains usable.
        } finally {
          this.inFlightRequestTicks.delete(issuedAtIndexOrdinal);
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
    const merged = {
      ...current,
      ...hydrated,
      display_name: hydrated.display_name ?? current.display_name,
      is_teammate: hydrated.is_teammate ?? current.is_teammate,
      is_index_only: false,
    };
    this.sessions[idx] = merged;
    // Keep the active-session cache in sync so the breadcrumb survives a later
    // reload that drops this row from the filtered/first page.
    if (merged.id === this.activeSessionId) this.activeSessionDetail = merged;
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
    const requestOrdinal = ++this.requestClock;
    this.inFlightRequestTicks.add(requestOrdinal);
    const detailCommits = this.snapshotDetailCommits();
    // Rows already loaded when this request began had any mid-flight
    // deletion applied to the total by the delete path itself; a stale page
    // re-listing one must not subtract it again.
    const loadedIdsAtStart = new Set(this.sessions.map((s) => s.id));
    const signal = this.routeSignal();
    this.loading = true;
    try {
      configureGeneratedClient();
      const index = await callGenerated(
        () => SessionsService.getApiV1SessionsSidebarIndex({
          ...this.apiParams,
          cursor: this.nextCursor!,
          limit: SESSION_PAGE_SIZE,
        }),
        signal,
      ) as unknown as SidebarSessionIndexResponse;
      if (this.loadVersion !== version) return;
      // A later page can re-list a row already present (the index shifts
      // while paginating); merge it into its new position, carrying the
      // hydrated fields, instead of duplicating it.
      const existingById = new Map(
        this.sessions.map((session) => [session.id, session]),
      );
      // Same deletion-tombstone guard as loadSidebarPage for appended
      // pages, including descendants of tombstoned rows.
      const tombstoned = this.expandTombstoned(index.sessions, (rid) =>
        this.deletedSinceSnapshot(rid, detailCommits, requestOrdinal)
      );
      const dropped = index.sessions.filter((row) => tombstoned.has(row.id));
      const rows = index.sessions.filter((row) => !tombstoned.has(row.id));
      const incomingIds = new Set(index.sessions.map((row) => row.id));
      const pageIds = new Set(rows.map((row) => row.id));
      const appended = rows.map((row) => {
        const prior = existingById.get(row.id);
        if (
          this.commitSupersedesIndex(row.id, detailCommits, requestOrdinal)
        ) {
          if (prior) return prior;
          const committed = this.committedDetailByRow.get(row.id);
          if (committed) return { ...committed };
        }
        return sidebarIndexRowToSession(row, prior);
      });
      this.sessions = [
        ...this.sessions.filter((s) => !pageIds.has(s.id)),
        ...appended,
      ];
      this.nextCursor = index.next_cursor ?? null;
      // Cursor responses carry the total from the first page's snapshot,
      // which predates any local deletions since then: keep the locally
      // maintained total and subtract only tombstoned roots this page would
      // have introduced (see loadedIdsAtStart).
      const newlyDropped = dropped.filter(
        (row) => !loadedIdsAtStart.has(row.id),
      );
      // Classify against everything loaded plus this page: a dropped child
      // whose parent lives on an earlier page is not a promoted root.
      const paginationContext = new Set([
        ...loadedIdsAtStart,
        ...incomingIds,
      ]);
      this.total = Math.max(
        0,
        this.total - this.countDroppedRoots(newlyDropped, paginationContext),
      );
      for (const session of appended) {
        this.indexCommitByRow.set(session.id, {
          ordinal: requestOrdinal,
          row: session,
        });
      }
      this.syncActiveSessionAfterIndexCommit(detailCommits, requestOrdinal);
      this.pruneReconciliationState();
    } catch (error) {
      if (signal.aborted || isAbortError(error)) return;
      throw error;
    } finally {
      this.inFlightRequestTicks.delete(requestOrdinal);
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

  private detailCommitGeneration(id: string): number {
    return this.activeDetailCommitBySession.get(id)?.generation ?? 0;
  }

  private bumpDetailCommit(
    id: string,
    deleted: boolean,
    issuedAtIndexOrdinal: number,
    snapshot?: Session,
  ): void {
    const previous = this.activeDetailCommitBySession.get(id);
    this.activeDetailCommitBySession.set(id, {
      generation: ++this.detailCommitGenerationCounter,
      deleted,
      issuedAtIndexOrdinal,
      committedAtTick: this.requestClock,
      removedRow: false,
    });
    if (deleted) {
      this.committedDetailByRow.delete(id);
    } else if (snapshot) {
      this.committedDetailByRow.set(id, snapshot);
    }
    // A read-derived commit (finite ordinal) carries the full detail shape;
    // it supersedes any interim rename snapshot backing the active session.
    if (
      !deleted &&
      Number.isFinite(issuedAtIndexOrdinal) &&
      id === this.activeSessionId
    ) {
      this.interimActiveDetail = false;
    }
    // A live commit superseding a read-derived tombstone proves the session
    // exists: restore the sidebar row and root total the transient 404
    // removed. The appended position is approximate; the scheduled
    // authoritative reload corrects ordering.
    if (
      !deleted &&
      previous !== undefined &&
      previous.deleted &&
      previous.removedRow &&
      Number.isFinite(previous.issuedAtIndexOrdinal) &&
      snapshot !== undefined &&
      !this.sessions.some((row) => row.id === id)
    ) {
      // Adjust the total by the change in locally represented root groups:
      // a revived orphan child counts as a promoted root, and a parent
      // reviving later demotes it again, so the group is counted exactly
      // once regardless of revival order.
      const rootGroupsBefore = this.countRootGroups(this.sessions);
      this.sessions = [...this.sessions, { ...snapshot }];
      this.total = Math.max(
        0,
        this.total + this.countRootGroups(this.sessions) - rootGroupsBefore,
      );
      this.scheduleIndexRefresh();
    }
  }

  // See indexCommitByRow: when an index request issued after this detail
  // read began has committed the row, take only detail-owned fields from
  // the response and keep the row's committed index-owned fields. Otherwise
  // the response is the freshest source for every field (e.g. a refresh
  // propagating a remote rename) and is returned untouched.
  private reconcileDetailResponse(
    id: string,
    session: Session,
    issuedAtIndexOrdinal: number,
  ): Session {
    const stamped = this.indexCommitByRow.get(id);
    if (stamped === undefined || stamped.ordinal <= issuedAtIndexOrdinal) {
      return session;
    }
    const row = this.sessions.find((s) => s.id === id);
    if (row) return mergeIndexFieldsIntoDetail(row, session);
    // The re-published row was since excluded by a later reload; the
    // absorbed index fields survive in the active cache, or failing that in
    // the stamped publish snapshot (an index-only row never fills the
    // cache), so reconcile against those instead of letting the stale
    // response apply wholesale.
    const cached = this.activeSessionDetail;
    if (cached?.id === id) return mergeIndexFieldsIntoDetail(cached, session);
    return mergeIndexFieldsIntoDetail(stamped.row, session);
  }

  // True when a deletion committed for id after the given commit snapshot
  // was taken — i.e. mid-flight relative to whichever request captured it.
  // Index publishing, the reload failure-restore, and reconciliation all
  // honor these tombstones so a response whose server snapshot predates the
  // deletion cannot resurrect the row.
  private deletedSinceSnapshot(
    id: string,
    snapshot: ReadonlyMap<string, number>,
    requestOrdinal: number,
  ): boolean {
    const commit = this.activeDetailCommitBySession.get(id);
    return (
      commit !== undefined &&
      commit.deleted &&
      commit.generation !== (snapshot.get(id) ?? 0) &&
      // A read-derived tombstone (finite tick) only outranks requests
      // issued before it; a response from a later-issued request reflects
      // newer server state and supersedes the transient 404.
      commit.issuedAtIndexOrdinal >= requestOrdinal
    );
  }

  // The paginated sidebar total counts root groups, while rows include
  // roots and their descendants: only removing a row that anchors its own
  // group can shrink the root count — a descendant's removal leaves its
  // group in place. A row anchors a group when it has no parent OR its
  // parent is absent from the surrounding set (the sidebar promotes such
  // orphans to root groups). Deeper orphan chains are settled by the next
  // authoritative index response.
  private countDroppedRoots(
    rows: ReadonlyArray<{ id: string; parent_session_id?: string | null }>,
    presentIds: ReadonlySet<string>,
  ): number {
    return rows.filter(
      (row) =>
        !row.parent_session_id || !presentIds.has(row.parent_session_id),
    ).length;
  }

  // A non-deletion commit that landed while an index request was in flight
  // and whose own request is at least as new as the index's supersedes the
  // index's older snapshot for that row (see syncActiveSessionAfterIndexCommit
  // for the active-session cache side of the same rule).
  private commitSupersedesIndex(
    id: string,
    snapshot: ReadonlyMap<string, number>,
    requestOrdinal: number,
  ): boolean {
    const commit = this.activeDetailCommitBySession.get(id);
    return (
      commit !== undefined &&
      !commit.deleted &&
      commit.generation !== (snapshot.get(id) ?? 0) &&
      commit.issuedAtIndexOrdinal >= requestOrdinal
    );
  }

  // Deleting a session removes its whole subtree from the backend's
  // sidebar queries; extend a tombstone predicate across parent chains so
  // publication, restoration, and local removal treat the group
  // consistently. Runs to a fixpoint over the given rows; ancestors need
  // not be present in the row set (the predicate is consulted for parent
  // ids directly).
  private expandTombstoned(
    rows: ReadonlyArray<{ id: string; parent_session_id?: string | null }>,
    isTombstoned: (id: string) => boolean,
  ): Set<string> {
    const removed = new Set<string>();
    let grew = true;
    while (grew) {
      grew = false;
      for (const row of rows) {
        if (removed.has(row.id)) continue;
        const parent = row.parent_session_id;
        if (
          isTombstoned(row.id) ||
          (parent && (removed.has(parent) || isTombstoned(parent)))
        ) {
          removed.add(row.id);
          grew = true;
        }
      }
    }
    return removed;
  }

  // Remove a deleted session and its known local descendants, tombstoning
  // each so stale responses cannot reinsert them, and decrement the root
  // total once per removed group. The caller records the ancestor's own
  // deletion commit.
  private removeSessionSubtree(
    id: string,
    tombstoneTick: number = Number.POSITIVE_INFINITY,
  ): ReadonlySet<string> {
    // A cache-only active session (deep link, search) has no sidebar row
    // but may descend from the deleted ancestor; include it so callers
    // clear the selection instead of leaving a ghost.
    const cachedActive = this.activeSessionDetail;
    const knownRows =
      cachedActive !== null &&
        !this.sessions.some((row) => row.id === cachedActive.id)
        ? [...this.sessions, cachedActive]
        : this.sessions;
    const subtree = this.expandTombstoned(
      knownRows,
      (rid) => rid === id,
    );
    subtree.add(id);
    for (const rid of subtree) {
      if (rid !== id) {
        this.bumpDetailCommit(rid, true, tombstoneTick);
      }
    }
    const droppedRows = this.sessions.filter((s) => subtree.has(s.id));
    const presentIds = new Set(this.sessions.map((s) => s.id));
    this.sessions = this.sessions.filter((s) => !subtree.has(s.id));
    this.total = Math.max(
      0,
      this.total - this.countDroppedRoots(droppedRows, presentIds),
    );
    for (const row of droppedRows) {
      const commit = this.activeDetailCommitBySession.get(row.id);
      if (commit?.deleted) {
        commit.removedRow = true;
      }
    }
    return subtree;
  }

  private pruneReconciliationState(): void {
    let horizon = Infinity;
    for (const tick of this.inFlightRequestTicks) {
      if (tick < horizon) horizon = tick;
    }
    for (const [id, commit] of this.activeDetailCommitBySession) {
      if (commit.committedAtTick < horizon) {
        this.activeDetailCommitBySession.delete(id);
        this.committedDetailByRow.delete(id);
      }
    }
    for (const [id, stamp] of this.indexCommitByRow) {
      if (stamp.ordinal < horizon) {
        this.indexCommitByRow.delete(id);
      }
    }
  }

  // Count locally represented root groups: rows that are parentless or
  // whose parent is absent from the set (promoted orphans).
  private countRootGroups(rows: ReadonlyArray<Session>): number {
    const ids = new Set(rows.map((row) => row.id));
    return rows.filter(
      (row) => !row.parent_session_id || !ids.has(row.parent_session_id),
    ).length;
  }

  private snapshotDetailCommits(): ReadonlyMap<string, number> {
    return new Map(
      [...this.activeDetailCommitBySession].map(
        ([id, commit]) => [id, commit.generation],
      ),
    );
  }

  private setActiveSession(id: string | null) {
    if (id === this.activeSessionId) return;
    this.activeDetailRead.cancel();
    this.childSessionsRead.cancel();
    this.activeSessionId = id;
    this.activeSessionDetail = null;
    this.interimActiveDetail = false;
    this.activeSessionUsageVersion = 0;
    this.childSessionsVersion++;
  }

  selectSession(id: string) {
    this.setActiveSession(id);
    this.cacheActiveSessionDetailFromList(id);
    void this.hydrateSelectedIndexOnlySession(id);
  }

  // Seed activeSessionDetail from a hydrated sidebar row so the open session
  // survives a later reload that drops it from the filtered/first page. An
  // index-only stub cannot seed an empty cache (hydration does that via
  // mergeHydratedSession), but its index fields still refresh an existing
  // cached detail: a cache-only session whose row re-enters the index must
  // absorb renames and count changes the index carries.
  private cacheActiveSessionDetailFromList(id: string) {
    if (id !== this.activeSessionId) return;
    const row = this.sessions.find((s) => s.id === id);
    if (!row) return;
    if (!row.is_index_only) {
      this.activeSessionDetail = row;
      return;
    }
    const cached = this.activeSessionDetail;
    if (cached?.id === id) {
      this.activeSessionDetail = mergeIndexFieldsIntoDetail(row, cached);
    }
  }

  // After an index commit, reconcile the active session's sidebar row and
  // detail cache in whichever direction is fresher. When something newer
  // COMMITTED mid-flight (navigation/refresh/rename/hydration, or a 404
  // deletion), the committed state wins over the older index snapshot: a
  // deletion removes the re-listed row (the index predates the 404) and
  // committed detail replaces it. When nothing committed mid-flight, the
  // committed index is at least as fresh as the cache — any commit the
  // cache holds resolved before this index request was even issued, so the
  // server produced the index rows afterwards — and the merged row may
  // carry index refreshes (renames, counts) the cache never saw: absorb
  // them into the cache so a later reload that excludes the row doesn't
  // revert the breadcrumb to stale fields. A read that merely began and
  // hasn't committed changes nothing here; if it resolves, it commits
  // strictly fresher detail on top.
  private syncActiveSessionAfterIndexCommit(
    snapshot: ReadonlyMap<string, number>,
    requestOrdinal: number,
  ) {
    const id = this.activeSessionId;
    if (id === null) return;
    const commit = this.activeDetailCommitBySession.get(id);
    if (
      commit === undefined ||
      commit.generation === (snapshot.get(id) ?? 0) ||
      commit.issuedAtIndexOrdinal < requestOrdinal
    ) {
      // Nothing committed mid-flight, or the commit's request predates this
      // index request — the index's server snapshot is at least as fresh, so
      // absorb its refreshes into the cache (the commit's detail-owned
      // fields survive through the row's existing-field carry-over).
      this.cacheActiveSessionDetailFromList(id);
      return;
    }
    if (commit.deleted) {
      // Honor the deletion tombstone instead of letting the stale index
      // resurrect the row (mirrors refreshActiveSession's 404 removal).
      this.removeSessionSubtree(id, commit.issuedAtIndexOrdinal);
      return;
    }
    this.restoreActiveRowFromDetailCache(id);
  }

  // Inverse of cacheActiveSessionDetailFromList: the cached detail is a full
  // post-commit snapshot (it is never index-only), so replace the re-listed
  // row wholesale rather than merging the stale index's fields over it. If
  // the cache is empty (e.g. a navigation is still in flight), leave the row
  // alone — the pending read commits through mergeHydratedSession when it
  // resolves.
  private restoreActiveRowFromDetailCache(id: string) {
    if (id !== this.activeSessionId) return;
    const cached = this.activeSessionDetail;
    if (cached?.id !== id) return;
    const idx = this.sessions.findIndex((s) => s.id === id);
    if (idx < 0) return;
    this.sessions[idx] = { ...cached };
  }

  private navigateInFlight: { id: string; promise: Promise<void> } | null =
    null;

  /**
   * Navigate to a session by ID. If it isn't in the current sidebar list
   * (e.g. opened from search, filtered out, or on an unloaded page), its
   * detail is fetched into activeSessionDetail rather than injected into the
   * sidebar collection, so it can back activeSession without appearing in the
   * filtered list or being double-counted when loadMore appends later pages.
   * Re-invocations for the same still-active session join the in-flight
   * fetch instead of cancelling and restarting it, so reactive callers
   * (App's deep-link effect) can re-request hydration without churning the
   * read coordinator.
   */
  async navigateToSession(id: string) {
    if (this.navigateInFlight?.id === id && this.activeSessionId === id) {
      return this.navigateInFlight.promise;
    }
    this.setActiveSession(id);
    const existing = this.sessions.find((s) => s.id === id);
    if (existing) {
      this.cacheActiveSessionDetailFromList(id);
      await this.hydrateSelectedIndexOnlySession(id);
      return;
    }
    const signal = this.activeDetailRead.begin();
    const issuedAtIndexOrdinal = ++this.requestClock;
    this.inFlightRequestTicks.add(issuedAtIndexOrdinal);
    const entry = { id, promise: Promise.resolve() };
    entry.promise = (async () => {
      try {
        configureGeneratedClient();
        const raw = await callGenerated(
          () => SessionsService.getApiV1SessionsId({ id }),
          signal,
        ) as unknown as Session;
        if (
          this.activeSessionId === id &&
          this.activeDetailRead.isCurrent(signal)
        ) {
          // Same later-issued-read guard as refreshActiveSession.
          const latestTick =
            this.activeDetailCommitBySession.get(id)?.issuedAtIndexOrdinal ??
              0;
          if (
            !Number.isFinite(latestTick) || latestTick <= issuedAtIndexOrdinal
          ) {
            const session = this.reconcileDetailResponse(
              id,
              raw,
              issuedAtIndexOrdinal,
            );
            const idx = this.sessions.findIndex((s) => s.id === id);
            if (idx >= 0) {
              this.mergeHydratedSession(session);
            } else {
              this.activeSessionDetail = session;
            }
            this.bumpDetailCommit(id, false, issuedAtIndexOrdinal, session);
          }
        }
      } catch {
        // Session not found — selection stands without metadata
      } finally {
        this.inFlightRequestTicks.delete(issuedAtIndexOrdinal);
        this.activeDetailRead.finish(signal);
        if (this.navigateInFlight === entry) {
          this.navigateInFlight = null;
        }
      }
    })();
    this.navigateInFlight = entry;
    return entry.promise;
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

  async refreshActiveSession(): Promise<void> {
    const id = this.activeSessionId;
    if (!id) return;
    // Navigation and refresh issue the identical detail GET. Join a pending
    // same-session navigation instead of cancelling it to reissue the same
    // request: if this refresh then failed transiently, the cancelled
    // navigation could no longer fill the empty cache and nothing would
    // retry until the next watcher event. The event that triggered this
    // refresh may postdate the navigation's request, though, so chase with
    // a fresh read once the navigation settles — it observes the post-event
    // state (including a deletion), while a transient chase failure still
    // leaves the navigation's committed result in place.
    if (this.navigateInFlight?.id === id) {
      await this.navigateInFlight.promise;
      if (this.activeSessionId !== id) return;
      return this.refreshActiveSession();
    }
    const signal = this.activeDetailRead.begin();
    const issuedAtIndexOrdinal = ++this.requestClock;
    this.inFlightRequestTicks.add(issuedAtIndexOrdinal);
    try {
      configureGeneratedClient();
      const raw = await callGenerated(
        () => SessionsService.getApiV1SessionsId({ id }),
        signal,
      ) as unknown as Session;
      if (
        this.activeSessionId !== id ||
        !this.activeDetailRead.isCurrent(signal)
      ) {
        return;
      }
      // A commit from a later-issued READ (e.g. a hydration issued after
      // this request began) already holds fresher detail than this
      // response; leave it in place. Write-derived commits (Infinity) are
      // excluded: they invalidate stale reads through the coordinator, and
      // a read issued after them observes post-write state and must commit.
      const latestTick =
        this.activeDetailCommitBySession.get(id)?.issuedAtIndexOrdinal ?? 0;
      if (Number.isFinite(latestTick) && latestTick > issuedAtIndexOrdinal) {
        return;
      }
      const session = this.reconcileDetailResponse(
        id,
        raw,
        issuedAtIndexOrdinal,
      );
      const idx = this.sessions.findIndex((s) => s.id === id);
      if (idx >= 0) {
        this.mergeHydratedSession(session);
      } else {
        // Active session is not in the sidebar list, so back it with the detail
        // cache. Assign unconditionally (id is validated above): this both
        // refreshes an existing cache and restores one left empty by a failed
        // initial navigation or hydration, so watcher refreshes can recover the
        // breadcrumb.
        this.activeSessionDetail = session;
      }
      this.bumpDetailCommit(id, false, issuedAtIndexOrdinal, session);
    } catch (e) {
      // The session may have been deleted, locally or on another machine. On
      // a definitive not-found drop the cached detail so the breadcrumb
      // empties instead of showing a ghost session; transient failures keep
      // the cache and the next watcher refresh retries. callGenerated rethrows
      // generated errors as the runtime ApiError, so match that class.
      // A 404 from a request issued before newer successful evidence — a
      // later-issued read commit or an index that re-listed the row — is a
      // stale response, not proof of deletion; keep the session and let the
      // next refresh decide. Write commits (Infinity) are excluded as
      // evidence: they persist indefinitely and would immunize ever-renamed
      // sessions against genuine deletions.
      const latestCommit = this.activeDetailCommitBySession.get(id);
      const newerEvidence =
        (latestCommit !== undefined &&
          !latestCommit.deleted &&
          Number.isFinite(latestCommit.issuedAtIndexOrdinal) &&
          latestCommit.issuedAtIndexOrdinal > issuedAtIndexOrdinal) ||
        (this.indexCommitByRow.get(id)?.ordinal ?? 0) > issuedAtIndexOrdinal;
      if (
        e instanceof ApiError &&
        e.status === 404 &&
        !newerEvidence &&
        this.activeSessionId === id &&
        this.activeDetailRead.isCurrent(signal)
      ) {
        // Read-derived tombstone: recorded at this refresh's own issue
        // tick so later-issued successful responses can supersede it.
        // Infinity is reserved for confirmed mutations (explicit deletes,
        // renames).
        this.bumpDetailCommit(id, true, issuedAtIndexOrdinal);
        this.activeSessionDetail = null;
        // A hydrated matching row would keep backing activeSession as a
        // ghost of the deleted session; drop it and keep the pagination
        // total consistent, mirroring deleteSession.
        this.removeSessionSubtree(id, issuedAtIndexOrdinal);
      }
    } finally {
      this.inFlightRequestTicks.delete(issuedAtIndexOrdinal);
      this.activeDetailRead.finish(signal);
    }
  }

  async loadChildSessions(parentId: string) {
    const version = ++this.childSessionsVersion;
    const signal = this.childSessionsRead.begin();
    try {
      configureGeneratedClient();
      const children = await callGenerated(
        () => SessionsService.getApiV1SessionsIdChildren({ id: parentId }),
        signal,
      ) as unknown as Session[];
      if (
        this.childSessionsVersion !== version ||
        this.activeSessionId !== parentId ||
        !this.childSessionsRead.isCurrent(signal)
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
    } finally {
      this.childSessionsRead.finish(signal);
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
    try {
      await promise;
    } finally {
      if (this.signalDetailInflight.get(id) === promise) {
        this.signalDetailInflight.delete(id);
      }
      this.signalDetailLoading =
        this.signalDetailInflight.size > 0;
    }
  }

  private async doFetchSignalDetail(id: string) {
    const signal = this.routeSignal();
    this.signalDetailLoading = true;
    try {
      configureGeneratedClient();
      const session = await callGenerated(
        () => SessionsService.getApiV1SessionsId({ id }),
        signal,
      ) as unknown as Session;
      if (signal.aborted) return;
      this.signalDetailCache.set(id, {
        basis: session.health_score_basis ?? null,
        penalties: session.health_penalties ?? null,
      });
      this.mergeDetailIntoList(id);
    } catch {
      // Signal detail is non-critical
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
    // A cache-only session (opened from search, off the loaded pages) has no
    // list row to carry the signal detail; fill the cache the same way.
    const cached = this.activeSessionDetail;
    if (
      cached &&
      cached.id === id &&
      cached.health_score_basis === undefined &&
      detail.basis != null
    ) {
      this.activeSessionDetail = {
        ...cached,
        health_score_basis: detail.basis,
        health_penalties: detail.penalties,
      };
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
      this.selectSession(list[edge]!.id);
      return;
    }
    const next = idx + delta;
    if (next >= 0 && next < list.length) {
      this.selectSession(list[next]!.id);
    }
  }

  setProjectFilter(project: string) {
    const prev = this.filters;
    this.filters = { ...defaultFilters(), project, agent: prev.agent };
    this.dateFiltersWindowDays = null;
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

  clearSessionFilters(options: ClearSessionFiltersOptions = {}) {
    const project = this.filters.project;
    const wasOneShot = this.filters.includeOneShot;
    const wasAutomated = this.filters.includeAutomated;
    if (options.clearDateYoke || hasDateFilters(this.filters)) {
      yokedDates.clear();
    }
    this.filters = { ...defaultFilters(), project };
    this.dateFiltersWindowDays = null;
    this.setActiveSession(null);
    if (wasOneShot !== this.filters.includeOneShot || wasAutomated) {
      this.invalidateFilterCaches();
    }
    this.load();
  }

  /** Recently deleted session batches for undo toast. */
  recentlyDeleted: RecentlyDeletedSessions[] = $state([]);
  private recentlyDeletedNextKey = 0;

  private newRecentlyDeletedTimer(key: number) {
    return setTimeout(() => {
      this.recentlyDeleted = this.recentlyDeleted.filter(
        (d) => d.key !== key,
      );
    }, RECENTLY_DELETED_TTL_MS);
  }

  private addRecentlyDeleted(ids: string[]) {
    if (ids.length === 0) return;
    const key = this.recentlyDeletedNextKey++;
    const timer = this.newRecentlyDeletedTimer(key);
    this.recentlyDeleted = [
      ...this.recentlyDeleted,
      { key, ids: [...ids], timer },
    ];
  }

  /** Multi-select state for batch operations. */
  selectedIds: Set<string> = $state(new Set());
  selectMode: boolean = $state(false);

  toggleSelectMode() {
    this.selectMode = !this.selectMode;
    if (!this.selectMode) {
      this.selectedIds = new Set();
    }
  }

  toggleSelection(id: string) {
    const next = new Set(this.selectedIds);
    if (next.has(id)) {
      next.delete(id);
    } else {
      next.add(id);
    }
    this.selectedIds = next;
  }

  selectAll(ids: string[]) {
    this.selectedIds = new Set(ids);
  }

  clearSelection() {
    this.selectedIds = new Set();
  }

  async deleteSession(id: string) {
    configureGeneratedClient();
    await SessionsService.deleteApiV1SessionsId({ id });
    // Tombstone the explicit delete so an index load that was in flight
    // (and its failure-restore path) cannot resurrect the row.
    this.bumpDetailCommit(id, true, Number.POSITIVE_INFINITY);
    // The backend removes the whole subtree from sidebar queries; mirror it
    // locally so stale responses cannot reinsert descendants either.
    const removed = this.removeSessionSubtree(id);
    // The active session may be a descendant removed with its ancestor.
    if (this.activeSessionId !== null && removed.has(this.activeSessionId)) {
      this.setActiveSession(null);
    }
    this.addRecentlyDeleted([id]);
    this.invalidateFilterCaches();
  }

  async batchDeleteSessions(ids: string[]) {
    if (ids.length === 0) return;
    configureGeneratedClient();
    await SessionsService.postApiV1SessionsBatchDelete({
      requestBody: { session_ids: ids },
    });
    const idSet = new Set(ids);
    // Remove rows and adjust the root total immediately: the forced reload
    // below is authoritative when it succeeds, but if it fails its restore
    // snapshot was taken after these tombstones and would otherwise keep
    // the deleted rows visible. Known local descendants are tombstoned with
    // their roots, mirroring the backend's subtree removal.
    const cachedActive = this.activeSessionDetail;
    const knownRows =
      cachedActive !== null &&
        !this.sessions.some((row) => row.id === cachedActive.id)
        ? [...this.sessions, cachedActive]
        : this.sessions;
    const subtree = this.expandTombstoned(knownRows, (rid) =>
      idSet.has(rid)
    );
    for (const id of ids) subtree.add(id);
    for (const rid of subtree) {
      this.bumpDetailCommit(rid, true, Number.POSITIVE_INFINITY);
    }
    const presentIds = new Set(this.sessions.map((s) => s.id));
    const droppedRows = this.sessions.filter((s) => subtree.has(s.id));
    this.sessions = this.sessions.filter((s) => !subtree.has(s.id));
    this.total = Math.max(
      0,
      this.total - this.countDroppedRoots(droppedRows, presentIds),
    );
    if (this.activeSessionId && subtree.has(this.activeSessionId)) {
      this.setActiveSession(null);
    }
    this.addRecentlyDeleted(ids);
    this.selectedIds = new Set();
    this.selectMode = false;
    this.invalidateFilterCaches();
    await this.load({ force: true });
  }

  async restoreSession(id: string) {
    configureGeneratedClient();
    await SessionsService.postApiV1SessionsIdRestore({ id });
    // Clear the deletion tombstone: the session exists again.
    this.bumpDetailCommit(id, false, Number.POSITIVE_INFINITY);
    this.clearRecentlyDeleted(id);
    this.invalidateFilterCaches();
    await this.load();
  }

  async restoreRecentlyDeleted(deleted: RecentlyDeletedSessions) {
    const ids = [...deleted.ids];
    if (ids.length === 0) return;
    configureGeneratedClient();
    clearTimeout(deleted.timer);
    const failed: string[] = [];
    for (const id of ids) {
      try {
        await SessionsService.postApiV1SessionsIdRestore({ id });
        this.bumpDetailCommit(id, false, Number.POSITIVE_INFINITY);
      } catch {
        failed.push(id);
      }
    }
    this.updateRecentlyDeletedBatch(deleted, failed);
    this.invalidateFilterCaches();
    await this.load({ force: true });
    if (failed.length > 0) {
      const noun = failed.length === 1 ? "session" : "sessions";
      throw new Error(`Failed to restore ${failed.length} ${noun}`);
    }
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
      this.recentlyDeleted = this.recentlyDeleted.flatMap((d) => {
        if (!d.ids.includes(id)) return [d];
        const ids = d.ids.filter((deletedId) => deletedId !== id);
        if (ids.length === 0) {
          clearTimeout(d.timer);
          return [];
        }
        return [{ ...d, ids }];
      });
    } else {
      for (const d of this.recentlyDeleted) clearTimeout(d.timer);
      this.recentlyDeleted = [];
    }
  }

  private updateRecentlyDeletedBatch(
    deleted: RecentlyDeletedSessions,
    ids: string[],
  ) {
    this.recentlyDeleted = this.recentlyDeleted.flatMap((d) => {
      if (d.key !== deleted.key) return [d];
      if (ids.length === 0) {
        clearTimeout(d.timer);
        return [];
      }
      return [
        {
          ...d,
          ids: [...ids],
          timer: this.newRecentlyDeletedTimer(d.key),
        },
      ];
    });
  }

  async renameSession(id: string, displayName: string | null) {
    configureGeneratedClient();
    const updated = await SessionsService.patchApiV1SessionsIdRename({
      id,
      requestBody: { display_name: displayName },
    }) as unknown as Session;
    const applyRename = (target: Session): Session => {
      const merged = { ...target, ...updated };
      // When the caller cleared the rename and the backend found no agent name
      // to restore, display_name is absent from the response (omitempty on nil).
      // Explicitly null it out so the store reflects the cleared state rather
      // than keeping the stale value until the next SSE-triggered refresh.
      if (displayName === null && updated.display_name === undefined) {
        merged.display_name = null;
      }
      return merged;
    };
    const idx = this.sessions.findIndex((s) => s.id === id);
    if (idx !== -1) {
      this.sessions[idx] = applyRename(this.sessions[idx]!);
    }
    // Renaming the active session invalidates any in-flight active-detail
    // read or hydration: both were issued before the rename and would revert
    // the name with a pre-rename snapshot. Cancel and advance the session's
    // generation even when the detail cache is empty (index-only row with
    // hydration in flight) — the cache-population paths all check it.
    // The rename response is itself a full post-rename snapshot (the endpoint
    // reads the session back), so commit it as the active detail: an
    // index-only or out-of-list active session whose pending read was just
    // cancelled has nothing else to back the breadcrumb until the next
    // watcher refresh.
    // Record the write commit for ANY renamed session: hydrations and
    // index publishes consult it per id, so a stale response cannot revert
    // the rename even after the user selects another session. The snapshot
    // prefers full detail (cache, then row); a fabricated fallback stays
    // index-only so a row published from it still hydrates later.
    const renamedRow = this.sessions.find((s) => s.id === id);
    const renamedSnapshot =
      this.activeSessionId === id && this.activeSessionDetail?.id === id
        ? applyRename(this.activeSessionDetail)
        : renamedRow !== undefined
          ? applyRename(renamedRow)
          : applyRename({ ...updated, is_index_only: true });
    this.bumpDetailCommit(
      id,
      false,
      Number.POSITIVE_INFINITY,
      renamedSnapshot,
    );
    if (this.activeSessionId === id) {
      this.activeDetailRead.cancel();
      const cached =
        this.activeSessionDetail?.id === id ? this.activeSessionDetail : null;
      this.activeSessionDetail = applyRename(
        cached ?? { ...updated, is_index_only: false },
      );
      // The rename endpoint returns the plain DB-session shape, without the
      // derived detail-only fields (decode confidence, health explanations).
      // Merging onto a populated cache preserves those, but a fabricated
      // base is only an interim snapshot: chase with a fresh detail read so
      // the enriched shape arrives instead of masquerading as hydrated. A
      // still-interim cache re-chases — this rename just cancelled the
      // previous enrichment fetch.
      if (cached === null || this.interimActiveDetail) {
        this.interimActiveDetail = true;
        void this.refreshActiveSession();
      }
    }
  }

  private startLiveRefresh() {
    if (this.liveRefreshStarted) return;
    this.liveRefreshStarted = true;
    this.unsubEvents = events.subscribe((event) => {
      this.handleLiveRefreshEvent(event);
    });
    this.safetyNetTimer = setInterval(
      () => {
        this.load();
        this.refreshActiveChildSessions();
        this.bumpActiveSessionUsageVersion();
      },
      SAFETY_NET_REFRESH_MS,
    );
  }

  private handleLiveRefreshEvent(event: DataChangedEvent) {
    if (event.scope === "messages") {
      this.invalidateHydratedSessionDetails();
      this.bumpActiveSessionUsageVersion();
      this.refreshActiveChildSessions();
      return;
    }
    if (event.scope === "sessions" || event.scope === "sync") {
      this.scheduleIndexRefresh();
      this.bumpActiveSessionUsageVersion();
      this.refreshActiveChildSessions();
    }
  }

  private scheduleIndexRefresh() {
    if (this.sidebarConsumers === 0) return;
    if (this.liveRefreshTimer !== null) {
      clearTimeout(this.liveRefreshTimer);
    }
    this.liveRefreshTimer = setTimeout(() => {
      this.liveRefreshTimer = null;
      this.load();
    }, LIVE_REFRESH_DEBOUNCE_MS);
  }

  private refreshActiveChildSessions() {
    const id = this.activeSessionId;
    if (!id) return;
    void this.loadChildSessions(id);
  }

  private bumpActiveSessionUsageVersion() {
    if (!this.activeSessionId) return;
    this.activeSessionUsageVersion++;
  }

  private routeSignal(): AbortSignal {
    if (!this.routeAbort || this.routeAbort.signal.aborted) {
      this.routeAbort = new AbortController();
    }
    return this.routeAbort.signal;
  }

  cancelRouteReads(): void {
    this.sidebarAbort?.abort();
    this.sidebarAbort = null;
    this.sidebarLoadPromise = null;
    this.sidebarLoadSignature = null;
    this.routeAbort?.abort();
    this.routeAbort = null;
    this.activeDetailRead.cancel();
    this.childSessionsRead.cancel();
    this.loadVersion++;
    this.childSessionsVersion++;
    this.loading = false;
    this.signalDetailInflight.clear();
    this.signalDetailLoading = false;
    for (const version of this.sidebarHydrationEpochByVersion.keys()) {
      this.sidebarHydrationEpochByVersion.set(
        version,
        (this.sidebarHydrationEpochByVersion.get(version) ?? 0) + 1,
      );
    }
    for (const resume of this.sidebarHydrationQueue.splice(0)) resume();
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
    this.cancelRouteReads();
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
    agent_label: row.agent_label ?? undefined,
    entrypoint: row.entrypoint ?? undefined,
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
    transcript_revision: row.transcript_revision,
    is_automated: row.is_automated,
    is_teammate: row.is_teammate ?? false,
    is_index_only: true,
    created_at: row.created_at,
  };
  if (!existing || existing.is_index_only) return skinny;
  return mergeIndexFieldsIntoDetail(skinny, existing);
}

// Overlay the index-owned fields of a skinny row onto previously hydrated
// detail, keeping the detail-only fields (first_message, tokens, health).
function mergeIndexFieldsIntoDetail(
  skinny: Session,
  existing: Session,
): Session {
  return {
    ...skinny,
    ...existing,
    project: skinny.project,
    machine: skinny.machine,
    agent: skinny.agent,
    agent_label: skinny.agent_label,
    entrypoint: skinny.entrypoint,
    display_name: skinny.display_name,
    started_at: skinny.started_at,
    ended_at: skinny.ended_at,
    message_count: skinny.message_count,
    user_message_count: skinny.user_message_count,
    parent_session_id: skinny.parent_session_id,
    relationship_type: skinny.relationship_type,
    termination_status: skinny.termination_status,
    transcript_revision: skinny.transcript_revision,
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
  sessions.refreshSidebarIfAttached();
});
