import {
  describe,
  it,
  expect,
  vi,
  beforeEach,
} from "vite-plus/test";
import {
  createSessionsStore,
  buildSessionGroups,
  getSessionStatus,
  parseFiltersFromParams,
  filtersToParams,
  splitExcludeProjectParam,
} from "./sessions.svelte.js";
import { SessionsService } from "../api/generated/index";
import { starred } from "./starred.svelte.js";
import { yokedDates } from "./yokedDates.svelte.js";
import type { Filters } from "./sessions.svelte.js";
import type { Session } from "../api/types.js";
import {
  ApiError as RuntimeApiError,
  callGenerated,
} from "../api/runtime.js";
import { rollingRange } from "../utils/dates.js";

const api = vi.hoisted(() => ({
  listSessions: vi.fn(),
  getSidebarSessionIndex: vi.fn(),
  getSession: vi.fn(),
  getProjects: vi.fn(),
  getAgents: vi.fn(),
  getMachines: vi.fn(),
  deleteSession: vi.fn(),
  batchDeleteSessions: vi.fn(),
  restoreSession: vi.fn(),
  renameSession: vi.fn(),
  getStats: vi.fn().mockResolvedValue({
    session_count: 0,
    message_count: 0,
    project_count: 0,
    machine_count: 0,
    earliest_session: null,
  }),
  watchEvents: vi.fn(() => ({ close: () => {} })),
}));

type SidebarIndexParams = Parameters<typeof api.getSidebarSessionIndex>[0];

// Install a minimal localStorage mock for the test environment.
const storageData = new Map<string, string>();
Object.defineProperty(globalThis, "localStorage", {
  value: {
    getItem: (key: string) => storageData.get(key) ?? null,
    setItem: (key: string, value: string) => { storageData.set(key, value); },
    removeItem: (key: string) => { storageData.delete(key); },
    clear: () => { storageData.clear(); },
  },
  configurable: true,
  writable: true,
});

vi.mock("../api/client.js", () => ({
  watchEvents: api.watchEvents,
}));

vi.mock("../api/runtime.js", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/runtime.js")>();
  return {
    ...actual,
    configureGeneratedClient: vi.fn(),
    callGenerated: vi.fn((request: () => Promise<unknown>) => request()),
    isAbortError: vi.fn(
      (error: unknown) =>
        error instanceof DOMException && error.name === "AbortError",
    ),
  };
});

vi.mock("../api/generated/index", () => ({
  SessionsService: {
    getApiV1Sessions: vi.fn((params) => api.listSessions(params)),
    getApiV1SessionsSidebarIndex: vi.fn((params) =>
      api.getSidebarSessionIndex(params)
    ),
    getApiV1SessionsId: vi.fn(({ id }) => api.getSession(id)),
    deleteApiV1SessionsId: vi.fn(({ id }) => api.deleteSession(id)),
    postApiV1SessionsBatchDelete: vi.fn(({ requestBody }) =>
      api.batchDeleteSessions(requestBody.session_ids)
    ),
    postApiV1SessionsIdRestore: vi.fn(({ id }) => api.restoreSession(id)),
    patchApiV1SessionsIdRename: vi.fn(({ id, requestBody }) =>
      api.renameSession(id, requestBody.display_name)
    ),
    getApiV1SessionsIdChildren: vi.fn().mockResolvedValue([]),
  },
  MetadataService: {
    getApiV1Projects: vi.fn((params) => api.getProjects(params)),
    getApiV1Agents: vi.fn((params) => api.getAgents(params)),
    getApiV1Machines: vi.fn((params) => api.getMachines(params)),
    getApiV1Stats: vi.fn((params) => api.getStats(params)),
  },
}));

function mockSidebarPage(
  overrides?: Partial<{ next_cursor: string }>,
) {
  vi.mocked(api.getSidebarSessionIndex).mockResolvedValue({
    sessions: [],
    total: 0,
    ...overrides,
  });
}

// Store code never sees the generated ApiError class: production callGenerated
// converts it to the runtime ApiError before rethrowing. Reject fixtures with
// the runtime class to model that boundary faithfully.
function makeNotFoundError(id: string): RuntimeApiError {
  return new RuntimeApiError(404, `session ${id} not found`);
}

function rejectGeneratedRequestOnAbort(
  request: () => Promise<unknown>,
  signal?: AbortSignal,
): Promise<unknown> {
  const result = request();
  if (!signal) return result;
  return new Promise((resolve, reject) => {
    signal.addEventListener(
      "abort",
      () => reject(new DOMException("aborted", "AbortError")),
      { once: true },
    );
    void result.then(resolve, reject);
  });
}

type SkinnySessionRow = {
  id: string;
  parent_session_id?: string | null;
  relationship_type?: string | null;
  project: string;
  machine: string;
  agent: string;
  agent_label?: string | null;
  entrypoint?: string | null;
  display_name?: string | null;
  started_at: string | null;
  ended_at: string | null;
  created_at: string;
  termination_status?: string | null;
  message_count: number;
  user_message_count: number;
  is_automated: boolean;
  is_teammate?: boolean;
};

function makeSkinnyRow(
  overrides: Partial<SkinnySessionRow> & { id: string },
): SkinnySessionRow {
  return {
    project: "proj",
    machine: "local",
    agent: "claude",
    agent_label: null,
    entrypoint: null,
    display_name: null,
    started_at: null,
    ended_at: null,
    created_at: "2024-01-01T00:00:00Z",
    termination_status: null,
    message_count: 1,
    user_message_count: 1,
    is_automated: false,
    is_teammate: false,
    ...overrides,
  };
}

function mockSidebarIndex(sessions: SkinnySessionRow[] = []) {
  vi.mocked(api.getSidebarSessionIndex).mockResolvedValue({
    sessions,
    total: sessions.length,
    next_cursor: null,
  });
}

function mockGetProjects() {
  vi.mocked(api.getProjects).mockResolvedValue({
    projects: [{ name: "proj", session_count: 1 }],
  });
}

function expectSidebarIndexCalledWith(
  expected: Partial<SidebarIndexParams>,
) {
  expect(api.getSidebarSessionIndex).toHaveBeenLastCalledWith(
    expect.objectContaining(generatedParams(expected)),
  );
}

function expectPaginatedSidebarIndexCalledWith(
  expected: Partial<SidebarIndexParams>,
) {
  expect(api.getSidebarSessionIndex).toHaveBeenLastCalledWith(
    expect.objectContaining(generatedParams(expected)),
  );
}

function generatedParams(
  params: Partial<Record<string, unknown>>,
): Record<string, unknown> {
  const names: Record<string, string> = {
    active_since: "activeSince",
    date_from: "dateFrom",
    date_to: "dateTo",
    exclude_project: "excludeProject",
    health_grade: "healthGrade",
    include_automated: "includeAutomated",
    include_children: "includeChildren",
    include_one_shot: "includeOneShot",
    max_messages: "maxMessages",
    min_messages: "minMessages",
    min_tool_failures: "minToolFailures",
    min_user_messages: "minUserMessages",
  };
  return Object.fromEntries(
    Object.entries(params).map(([key, value]) => [
      names[key] ?? key,
      value,
    ]),
  );
}

describe("SessionsStore", () => {
  let sessions: ReturnType<typeof createSessionsStore>;

  beforeEach(() => {
    vi.clearAllMocks();
    vi.mocked(callGenerated).mockImplementation(
      (request: () => Promise<unknown>) => request(),
    );
    storageData.clear();
    mockSidebarPage();
    mockSidebarIndex();
    starred.filterOnly = false;
    starred.ids = new Set();
    yokedDates.setEnabled(false);
    sessions = createSessionsStore();
  });

  describe("initFromParams", () => {
    it("should parse project and date params", () => {
      sessions.initFromParams({
        project: "myproj",
        date: "2024-06-15",
      });
      expect(sessions.filters.project).toBe("myproj");
      expect(sessions.filters.date).toBe("2024-06-15");
    });

    it("should parse date_from and date_to", () => {
      sessions.initFromParams({
        date_from: "2024-06-01",
        date_to: "2024-06-30",
      });
      expect(sessions.filters.dateFrom).toBe("2024-06-01");
      expect(sessions.filters.dateTo).toBe("2024-06-30");
    });

    it("should parse numeric min_messages", () => {
      sessions.initFromParams({ min_messages: "5" });
      expect(sessions.filters.minMessages).toBe(5);
    });

    it("should parse numeric max_messages", () => {
      sessions.initFromParams({ max_messages: "100" });
      expect(sessions.filters.maxMessages).toBe(100);
    });

    it("should default non-numeric min/max to 0", () => {
      sessions.initFromParams({
        min_messages: "abc",
        max_messages: "",
      });
      expect(sessions.filters.minMessages).toBe(0);
      expect(sessions.filters.maxMessages).toBe(0);
    });

    it("should default missing params to empty/zero", () => {
      sessions.initFromParams({});
      expect(sessions.filters.project).toBe("");
      expect(sessions.filters.date).toBe("");
      expect(sessions.filters.minMessages).toBe(0);
      expect(sessions.filters.maxMessages).toBe(0);
    });
  });

  describe("localStorage persistence", () => {
    it("should save filters to localStorage on load", async () => {
      sessions.filters.project = "myproj";
      sessions.filters.agent = "claude";
      await sessions.load();

      const saved = JSON.parse(
        localStorage.getItem("session-filters") ?? "{}",
      );
      expect(saved.project).toBe("myproj");
      expect(saved.agent).toBe("claude");
    });

    it("should restore filters from localStorage on create", async () => {
      localStorage.setItem(
        "session-filters",
        JSON.stringify({ project: "saved-proj", agent: "codex" }),
      );
      const store = createSessionsStore();
      expect(store.filters.project).toBe("saved-proj");
      expect(store.filters.agent).toBe("codex");
      // Defaults for fields not in localStorage
      expect(store.filters.minMessages).toBe(0);
      expect(store.filters.includeOneShot).toBe(true);
    });

    it("should fall back to defaults on corrupted localStorage", () => {
      localStorage.setItem("session-filters", "not json");
      const store = createSessionsStore();
      expect(store.filters.project).toBe("");
      expect(store.filters.includeOneShot).toBe(true);
    });

    it("clears date bounds from legacy unversioned entries", () => {
      // Entries written before provenance tracking may hold rolling bounds
      // persisted as if explicit; only their date fields are dropped.
      localStorage.setItem(
        "session-filters",
        JSON.stringify({
          project: "saved-proj",
          dateFrom: "2025-07-07",
          dateTo: "2026-07-06",
          date: "2025-07-07",
        }),
      );
      const store = createSessionsStore();
      expect(store.filters.project).toBe("saved-proj");
      expect(store.filters.dateFrom).toBe("");
      expect(store.filters.dateTo).toBe("");
      expect(store.filters.date).toBe("");
      // Migration is written back so it runs only once.
      const saved = JSON.parse(
        localStorage.getItem("session-filters") ?? "{}",
      );
      expect(saved.version).toBe(2);
      expect(saved.dateFrom).toBe("");
    });

    it("keeps date bounds from versioned entries", () => {
      localStorage.setItem(
        "session-filters",
        JSON.stringify({
          version: 2,
          dateFrom: "2026-01-01",
          dateTo: "2026-01-31",
        }),
      );
      const store = createSessionsStore();
      expect(store.filters.dateFrom).toBe("2026-01-01");
      expect(store.filters.dateTo).toBe("2026-01-31");
    });

    it("stamps the storage version when persisting", async () => {
      sessions.filters.project = "myproj";
      await sessions.load();
      const saved = JSON.parse(
        localStorage.getItem("session-filters") ?? "{}",
      );
      expect(saved.version).toBe(2);
    });

    it("persists rolling bounds as windowDays intent, not dates", async () => {
      sessions.filters.project = "myproj";
      sessions.applyPanelDateFilters(
        { date_from: "2025-07-07", date_to: "2026-07-06" },
        365,
      );
      await sessions.load();

      const saved = JSON.parse(
        localStorage.getItem("session-filters") ?? "{}",
      );
      expect(saved.dateFrom).toBe("");
      expect(saved.dateTo).toBe("");
      expect(saved.date).toBe("");
      expect(saved.windowDays).toBe(365);
      expect(saved.project).toBe("myproj");
      // The current tab still queries with the materialized bounds.
      expect(sessions.filters.dateFrom).toBe("2025-07-07");
      expect(sessions.filters.dateTo).toBe("2026-07-06");
    });

    it("rematerializes a persisted rolling window on load", () => {
      localStorage.setItem(
        "session-filters",
        JSON.stringify({ version: 2, project: "p", windowDays: 30 }),
      );
      const store = createSessionsStore();
      const range = rollingRange(30);
      expect(store.filters.dateFrom).toBe(range.from);
      expect(store.filters.dateTo).toBe(range.to);
      expect(store.dateFiltersWindowDays).toBe(30);
      expect(store.filters.project).toBe("p");
    });

    it("ignores an invalid persisted windowDays", () => {
      localStorage.setItem(
        "session-filters",
        JSON.stringify({ version: 2, windowDays: -5 }),
      );
      const store = createSessionsStore();
      expect(store.filters.dateFrom).toBe("");
      expect(store.dateFiltersWindowDays).toBe(null);
    });

    it("persists explicitly chosen fixed date bounds", async () => {
      sessions.applyPanelDateFilters(
        { date_from: "2026-01-01", date_to: "2026-01-31" },
        null,
      );
      await sessions.load();

      const saved = JSON.parse(
        localStorage.getItem("session-filters") ?? "{}",
      );
      expect(saved.dateFrom).toBe("2026-01-01");
      expect(saved.dateTo).toBe("2026-01-31");
      expect(saved.windowDays).toBeUndefined();
    });

    it("treats deep-linked window_days date bounds as rolling intent", async () => {
      sessions.initFromParams({
        window_days: "365",
        date_from: "2025-07-07",
        date_to: "2026-07-06",
      });
      await sessions.load();

      const saved = JSON.parse(
        localStorage.getItem("session-filters") ?? "{}",
      );
      expect(saved.dateFrom).toBe("");
      expect(saved.dateTo).toBe("");
      expect(saved.windowDays).toBe(365);
      expect(sessions.filters.dateFrom).toBe("2025-07-07");
    });

    it("treats an invalid deep-linked window_days as explicit bounds", async () => {
      sessions.initFromParams({
        window_days: "abc",
        date_from: "2026-01-01",
        date_to: "2026-01-31",
      });
      expect(sessions.dateFiltersWindowDays).toBe(null);
      await sessions.load();

      const saved = JSON.parse(
        localStorage.getItem("session-filters") ?? "{}",
      );
      expect(saved.dateFrom).toBe("2026-01-01");
    });

    it("resumes persisting dates when a rolling range is replaced by an explicit one", async () => {
      sessions.applyPanelDateFilters(
        { date_from: "2025-07-07", date_to: "2026-07-06" },
        365,
      );
      await sessions.load();
      sessions.applyPanelDateFilters(
        { date_from: "2026-01-01", date_to: "2026-01-31" },
        null,
      );
      await sessions.load();

      const saved = JSON.parse(
        localStorage.getItem("session-filters") ?? "{}",
      );
      expect(saved.dateFrom).toBe("2026-01-01");
      expect(saved.dateTo).toBe("2026-01-31");
      expect(saved.windowDays).toBeUndefined();
    });

    it("persists a provenance flip even when the bounds are identical", async () => {
      // Fixed range persisted, then a rolling preset materializes to the
      // exact same bounds. Callers that diff serialized filters see no
      // change and skip load(), so the store must persist on apply.
      sessions.applyPanelDateFilters(
        { date_from: "2025-07-07", date_to: "2026-07-06" },
        null,
      );
      await sessions.load();
      sessions.applyPanelDateFilters(
        { date_from: "2025-07-07", date_to: "2026-07-06" },
        365,
      );

      const saved = JSON.parse(
        localStorage.getItem("session-filters") ?? "{}",
      );
      expect(saved.dateFrom).toBe("");
      expect(saved.dateTo).toBe("");
      expect(saved.windowDays).toBe(365);
    });

    it("clears the rolling intent on wholesale filter resets", async () => {
      sessions.applyPanelDateFilters(
        { date_from: "2025-07-07", date_to: "2026-07-06" },
        365,
      );
      sessions.clearSessionFilters();
      expect(sessions.dateFiltersWindowDays).toBe(null);

      sessions.applyPanelDateFilters(
        { date_from: "2025-07-07", date_to: "2026-07-06" },
        365,
      );
      sessions.setProjectFilter("myproj");
      expect(sessions.dateFiltersWindowDays).toBe(null);
    });

    it("persists deep-linked explicit date bounds", async () => {
      sessions.initFromParams({
        date_from: "2026-01-01",
        date_to: "2026-01-31",
      });
      await sessions.load();

      const saved = JSON.parse(
        localStorage.getItem("session-filters") ?? "{}",
      );
      expect(saved.dateFrom).toBe("2026-01-01");
      expect(saved.dateTo).toBe("2026-01-31");
    });
  });

  describe("sidebar loading", () => {
    it("does not load sessions when no sidebar consumer is mounted", async () => {
      sessions.refreshSidebarIfAttached();

      expect(api.listSessions).not.toHaveBeenCalled();
      expect(api.getSidebarSessionIndex).not.toHaveBeenCalled();
      expect(sessions.sessions).toHaveLength(0);
    });

    it("loads a bounded first page when the sidebar is mounted", async () => {
      const rows = Array.from({ length: 500 }, (_, i) =>
        makeSkinnyRow({ id: `s${i}` })
      );
      vi.mocked(api.getSidebarSessionIndex).mockResolvedValue({
        sessions: rows,
        total: 86312,
        next_cursor: "next",
      });

      const detach = sessions.attachSidebar();
      await sessions.load();
      detach();

      expect(api.listSessions).not.toHaveBeenCalled();
      expect(api.getSidebarSessionIndex).toHaveBeenLastCalledWith(
        expect.objectContaining({
          limit: 500,
        }),
      );
      expect(sessions.sessions).toHaveLength(500);
      expect(sessions.total).toBe(86312);
      expect(sessions.nextCursor).toBe("next");
    });

    it("keeps sidebar rows skinny and carries teammate classification", async () => {
      vi.mocked(api.getSidebarSessionIndex).mockResolvedValue({
        sessions: [
          makeSkinnyRow({
            id: "team",
            is_teammate: true,
          }),
        ],
        total: 1,
        next_cursor: null,
      });

      const detach = sessions.attachSidebar();
      await sessions.load();
      detach();

      expect(api.listSessions).not.toHaveBeenCalled();
      expect(sessions.sessions[0]).toMatchObject({
        id: "team",
        is_teammate: true,
        is_index_only: true,
      });
      expect(sessions.sessions[0]!.first_message).toBeNull();
    });

    it("coalesces duplicate sidebar loads for the same filter signature", async () => {
      let resolvePage!: (value: {
        sessions: SkinnySessionRow[];
        total: number;
        next_cursor?: string | null;
      }) => void;
      vi.mocked(api.getSidebarSessionIndex).mockReturnValue(
        new Promise((resolve) => {
          resolvePage = resolve;
        }),
      );

      const detach = sessions.attachSidebar();
      const first = sessions.load();
      const second = sessions.load();
      await Promise.resolve();

      expect(api.getSidebarSessionIndex).toHaveBeenCalledTimes(1);

      resolvePage({ sessions: [], total: 0, next_cursor: null });
      await Promise.all([first, second]);
      detach();
    });

    it("aborts an in-flight sidebar load when the filter signature changes", async () => {
      const signals: AbortSignal[] = [];
      vi.mocked(callGenerated).mockImplementation(
        (request: () => Promise<unknown>, signal?: AbortSignal) => {
          if (signal) signals.push(signal);
          return request();
        },
      );

      vi.mocked(api.getSidebarSessionIndex)
        .mockReturnValueOnce(new Promise(() => {}))
        .mockResolvedValueOnce({
          sessions: [],
          total: 0,
          next_cursor: null,
        });

      const detach = sessions.attachSidebar();
      void sessions.load();
      await Promise.resolve();
      expect(signals[0]?.aborted).toBe(false);

      sessions.filters.project = "changed";
      await sessions.load();

      expect(signals[0]?.aborted).toBe(true);
      expect(api.getSidebarSessionIndex).toHaveBeenCalledTimes(2);
      detach();
    });

    it("serializes only the sidebar filter subset for the index request", async () => {
      sessions.filters.project = "proj";
      sessions.filters.machine = "host-a";
      sessions.filters.agent = "codex";
      sessions.filters.date = "2026-05-23";
      sessions.filters.dateFrom = "2026-05-01";
      sessions.filters.dateTo = "2026-05-31";
      sessions.filters.recentlyActive = true;
      sessions.filters.minMessages = 2;
      sessions.filters.maxMessages = 20;
      sessions.filters.minUserMessages = 1;
      sessions.filters.includeOneShot = false;
      sessions.filters.includeAutomated = true;
      sessions.filters.hideUnknownProject = true;

      const detach = sessions.attachSidebar();
      await sessions.load();
      detach();

      const params = vi.mocked(api.getSidebarSessionIndex)
        .mock.calls[0]![0];
      expect(params).toMatchObject({
        project: "proj",
        excludeProject: "unknown",
        machine: "host-a",
        agent: "codex",
        date: "2026-05-23",
        dateFrom: "2026-05-01",
        dateTo: "2026-05-31",
        minMessages: 2,
        maxMessages: 20,
        minUserMessages: 1,
        includeOneShot: undefined,
        includeAutomated: true,
        limit: 500,
      });
      expect(typeof params.activeSince).toBe("string");
      expect(params.cursor).toBeUndefined();
      expect(params.health_grade).toBeUndefined();
      expect(params.outcome).toBeUndefined();
      expect(params.min_tool_failures).toBeUndefined();
      expect(params.starred).toBeUndefined();
    });

    it("requests a server-side starred sidebar page when starred-only is active", async () => {
      starred.filterOnly = true;

      const detach = sessions.attachSidebar();
      await sessions.load();
      detach();

      const params = vi.mocked(api.getSidebarSessionIndex)
        .mock.calls[0]![0];
      expect(params.starred).toBe(true);
      expect(params.cursor).toBeUndefined();
      expect(params.limit).toBe(500);
    });

    it("keeps display_name available without hydration", async () => {
      mockSidebarIndex([
        makeSkinnyRow({
          id: "renamed",
          display_name: "Renamed sidebar title",
        }),
      ]);

      await sessions.load();

      expect(sessions.sessions[0]!.display_name).toBe(
        "Renamed sidebar title",
      );
      expect(sessions.sessions[0]!.first_message).toBeNull();
    });

    it("marks skinny sidebar rows as index-only until hydrated", async () => {
      mockSidebarIndex([makeSkinnyRow({ id: "skinny" })]);

      await sessions.load();

      expect(sessions.sessions[0]!.is_index_only).toBe(true);
    });

    it("preserves hydrated active rows when reloading the index", async () => {
      mockSidebarIndex([makeSkinnyRow({ id: "active", message_count: 1 })]);
      vi.mocked(api.getSession).mockResolvedValue(
        makeSession({
          id: "active",
          first_message: "hydrated active detail",
          message_count: 1,
        }),
      );

      await sessions.load();
      sessions.selectSession("active");
      await vi.waitFor(() => {
        expect(sessions.activeSession?.first_message).toBe(
          "hydrated active detail",
        );
      });

      mockSidebarIndex([makeSkinnyRow({ id: "active", message_count: 9 })]);
      await sessions.load();

      expect(sessions.sessions).toHaveLength(1);
      expect(sessions.sessions[0]!.is_index_only).toBe(false);
      expect(sessions.sessions[0]!.message_count).toBe(9);
      expect(sessions.activeSession?.first_message).toBe(
        "hydrated active detail",
      );
    });

    it("keeps refreshed active-row fields when a later reload excludes the row", async () => {
      mockSidebarIndex([
        makeSkinnyRow({ id: "active", display_name: "old-name" }),
      ]);
      vi.mocked(api.getSession).mockResolvedValue(
        makeSession({ id: "active", first_message: "hydrated detail" }),
      );

      await sessions.load();
      sessions.selectSession("active");
      await vi.waitFor(() => {
        expect(sessions.activeSession?.first_message).toBe("hydrated detail");
      });

      // An index refresh renames the still-included active row...
      mockSidebarIndex([
        makeSkinnyRow({ id: "active", display_name: "new-name" }),
      ]);
      await sessions.load();

      // ...and a later reload excludes it: the breadcrumb must keep the
      // refreshed fields, not revert to the pre-refresh cached snapshot.
      mockSidebarIndex([]);
      await sessions.load();

      expect(sessions.activeSession?.first_message).toBe("hydrated detail");
      expect(sessions.activeSession?.display_name).toBe("new-name");
    });

    it("keeps active-row fields refreshed by a later page through an excluding reload", async () => {
      vi.mocked(api.getSidebarSessionIndex).mockResolvedValueOnce({
        sessions: [makeSkinnyRow({ id: "active", display_name: "old-name" })],
        total: 2,
        next_cursor: "page-2",
      });
      vi.mocked(api.getSession).mockResolvedValue(
        makeSession({ id: "active", first_message: "hydrated detail" }),
      );

      await sessions.load();
      sessions.selectSession("active");
      await vi.waitFor(() => {
        expect(sessions.activeSession?.first_message).toBe("hydrated detail");
      });

      // The index shifted while paginating: page 2 re-lists the active row
      // with a refreshed name.
      vi.mocked(api.getSidebarSessionIndex).mockResolvedValueOnce({
        sessions: [makeSkinnyRow({ id: "active", display_name: "new-name" })],
        total: 2,
        next_cursor: null,
      });
      await sessions.loadMore();

      mockSidebarIndex([]);
      await sessions.load();

      expect(sessions.activeSession?.first_message).toBe("hydrated detail");
      expect(sessions.activeSession?.display_name).toBe("new-name");
    });

    it("folds index refreshes of a cache-only session's returning row into the cache", async () => {
      mockSidebarIndex([]);
      vi.mocked(api.getSession).mockResolvedValue(
        makeSession({
          id: "offpage",
          display_name: "old-name",
          first_message: "hydrated offpage detail",
        }),
      );

      await sessions.load();
      await sessions.navigateToSession("offpage");
      expect(sessions.activeSession?.display_name).toBe("old-name");

      // The open cache-only session's row enters the index (filter change,
      // index shift) carrying a refreshed name, as an index-only stub...
      mockSidebarIndex([
        makeSkinnyRow({ id: "offpage", display_name: "new-name" }),
      ]);
      await sessions.load();

      // ...and a later reload excludes it again: the breadcrumb must keep
      // the refreshed index fields on top of the cached hydrated detail.
      mockSidebarIndex([]);
      await sessions.load();

      expect(sessions.activeSession?.first_message).toBe(
        "hydrated offpage detail",
      );
      expect(sessions.activeSession?.display_name).toBe("new-name");
    });

    it("keeps the open off-page session when the reloaded index omits it", async () => {
      mockSidebarIndex([makeSkinnyRow({ id: "listed" })]);
      vi.mocked(api.getSession).mockResolvedValue(
        makeSession({
          id: "offpage",
          first_message: "hydrated offpage detail",
        }),
      );

      await sessions.load();
      await sessions.navigateToSession("offpage");
      expect(sessions.activeSession?.first_message).toBe(
        "hydrated offpage detail",
      );

      await sessions.load();

      // The open session is backed by the detail cache, not injected into
      // the filtered sidebar collection, so the reload neither drops the
      // breadcrumb nor grows the list.
      expect(sessions.sessions.map((s) => s.id)).toEqual(["listed"]);
      expect(sessions.activeSession?.first_message).toBe(
        "hydrated offpage detail",
      );
    });

    it("keeps the open session through pagination reaching its real row", async () => {
      vi.mocked(api.getSidebarSessionIndex).mockResolvedValueOnce({
        sessions: [makeSkinnyRow({ id: "listed" })],
        total: 2,
        next_cursor: "page-2",
      });
      vi.mocked(api.getSession).mockResolvedValue(
        makeSession({
          id: "offpage",
          first_message: "hydrated offpage detail",
        }),
      );

      await sessions.load();
      await sessions.navigateToSession("offpage");
      // The off-page target backs activeSession from the detail cache
      // without being injected into the sidebar collection.
      expect(sessions.sessions.map((s) => s.id)).toEqual(["listed"]);

      vi.mocked(api.getSidebarSessionIndex).mockResolvedValueOnce({
        sessions: [makeSkinnyRow({ id: "middle" })],
        total: 4,
        next_cursor: "page-3",
      });
      await sessions.loadMore();
      expect(sessions.sessions.map((s) => s.id)).toEqual([
        "listed",
        "middle",
      ]);
      expect(sessions.activeSession?.first_message).toBe(
        "hydrated offpage detail",
      );

      // Pagination reaches the open session's real row: it appears in its
      // index position exactly once, and the breadcrumb keeps the cached
      // detail while the row arrives as an index-only stub.
      vi.mocked(api.getSidebarSessionIndex).mockResolvedValueOnce({
        sessions: [
          makeSkinnyRow({ id: "offpage" }),
          makeSkinnyRow({ id: "last" }),
        ],
        total: 4,
        next_cursor: null,
      });
      await sessions.loadMore();

      expect(sessions.sessions.map((s) => s.id)).toEqual([
        "listed",
        "middle",
        "offpage",
        "last",
      ]);
      expect(sessions.activeSession?.first_message).toBe(
        "hydrated offpage detail",
      );
    });

    it("merges a re-listed row in place when a later page repeats it", async () => {
      vi.mocked(api.getSidebarSessionIndex).mockResolvedValueOnce({
        sessions: [
          makeSkinnyRow({ id: "first" }),
          makeSkinnyRow({ id: "shifted" }),
        ],
        total: 3,
        next_cursor: "page-2",
      });
      await sessions.load();
      vi.mocked(api.getSession).mockResolvedValue(
        makeSession({ id: "shifted", first_message: "hydrated shifted" }),
      );
      await sessions.hydrateVisibleSessions(["shifted"]);

      // The index shifted while paginating and page 2 re-lists a row from
      // page 1: it must move to its new position once, not duplicate, and
      // carry its hydrated fields.
      vi.mocked(api.getSidebarSessionIndex).mockResolvedValueOnce({
        sessions: [
          makeSkinnyRow({ id: "shifted" }),
          makeSkinnyRow({ id: "last" }),
        ],
        total: 3,
        next_cursor: null,
      });
      await sessions.loadMore();

      expect(sessions.sessions.map((s) => s.id)).toEqual([
        "first",
        "shifted",
        "last",
      ]);
      expect(
        sessions.sessions.find((s) => s.id === "shifted")?.first_message,
      ).toBe("hydrated shifted");
    });

    it("refreshes hydrated agent identity fields from the sidebar index", async () => {
      mockSidebarIndex([
        makeSkinnyRow({
          id: "active",
          agent_label: "old-label",
          entrypoint: "old-entrypoint",
        }),
      ]);
      vi.mocked(api.getSession).mockResolvedValue(
        makeSession({
          id: "active",
          agent_label: "old-label",
          entrypoint: "old-entrypoint",
          first_message: "hydrated active detail",
        }),
      );

      await sessions.load();
      await sessions.hydrateVisibleSessions(["active"]);

      mockSidebarIndex([
        makeSkinnyRow({
          id: "active",
          agent_label: "triage",
          entrypoint: "sdk-cli",
        }),
      ]);
      await sessions.load();

      expect(sessions.sessions[0]!.is_index_only).toBe(false);
      expect(sessions.sessions[0]!.first_message).toBe("hydrated active detail");
      expect(sessions.sessions[0]!.agent_label).toBe("triage");
      expect(sessions.sessions[0]!.entrypoint).toBe("sdk-cli");
    });

    it("clears stale display names from hydrated rows when the index has none", async () => {
      mockSidebarIndex([
        makeSkinnyRow({
          id: "renamed",
          display_name: "Old custom name",
        }),
      ]);
      vi.mocked(api.getSession).mockResolvedValue(
        makeSession({
          id: "renamed",
          display_name: "Old custom name",
          first_message: "hydrated detail",
        }),
      );

      await sessions.load();
      await sessions.hydrateVisibleSessions(["renamed"]);
      expect(sessions.sessions[0]!.display_name).toBe("Old custom name");

      mockSidebarIndex([
        makeSkinnyRow({
          id: "renamed",
          display_name: null,
        }),
      ]);
      await sessions.load();

      expect(sessions.sessions[0]!.display_name).toBeNull();
      expect(sessions.sessions[0]!.first_message).toBe("hydrated detail");
      expect(sessions.sessions[0]!.is_index_only).toBe(false);
    });

    it("merges hydrated full rows without changing index order", async () => {
      mockSidebarIndex([
        makeSkinnyRow({ id: "second" }),
        makeSkinnyRow({ id: "first" }),
      ]);
      vi.mocked(api.getSession).mockResolvedValue(
        makeSession({ id: "first", first_message: "full detail" }),
      );

      await sessions.load();
      await (sessions as any).hydrateVisibleSessions(["first"]);

      expect(sessions.sessions.map((s) => s.id)).toEqual([
        "second",
        "first",
      ]);
      expect(sessions.sessions[1]!.first_message).toBe("full detail");
      expect(sessions.sessions[1]!.is_index_only).toBe(false);
    });

    it("drops stale-version hydration results", async () => {
      mockSidebarIndex([makeSkinnyRow({ id: "stale" })]);
      await sessions.load();
      const staleVersion = (sessions as any).sidebarIndexVersion;

      let resolveDetail: ((session: Session) => void) | null = null;
      vi.mocked(api.getSession).mockReturnValueOnce(
        new Promise<Session>((resolve) => {
          resolveDetail = resolve;
        }),
      );
      const hydratePromise = (sessions as any).hydrateVisibleSessions(
        ["stale"],
        staleVersion,
      );

      mockSidebarIndex([makeSkinnyRow({ id: "fresh" })]);
      await sessions.load();
      resolveDetail!(makeSession({
        id: "stale",
        first_message: "stale detail",
      }));
      await hydratePromise;

      expect(sessions.sessions.map((s) => s.id)).toEqual(["fresh"]);
      expect(sessions.sessions[0]!.first_message).toBeNull();
    });

    it("prunes hydration caches from stale index versions", async () => {
      mockSidebarIndex([makeSkinnyRow({ id: "old" })]);
      vi.mocked(api.getSession).mockResolvedValue(
        makeSession({ id: "old", first_message: "old detail" }),
      );
      await sessions.load();
      const oldVersion = (sessions as any).sidebarIndexVersion;
      await (sessions as any).hydrateVisibleSessions(["old"]);

      expect((sessions as any).hydratedSessionsByVersion.has(oldVersion))
        .toBe(true);
      expect(
        (sessions as any).sidebarHydrationInflightByVersion.has(oldVersion),
      ).toBe(true);
      expect(
        (sessions as any).sidebarHydrationEpochByVersion.has(oldVersion),
      ).toBe(true);

      mockSidebarIndex([makeSkinnyRow({ id: "new" })]);
      await sessions.load();
      const newVersion = (sessions as any).sidebarIndexVersion;

      expect(oldVersion).not.toBe(newVersion);
      expect((sessions as any).hydratedSessionsByVersion.has(oldVersion))
        .toBe(false);
      expect(
        (sessions as any).sidebarHydrationInflightByVersion.has(oldVersion),
      ).toBe(false);
      expect(
        (sessions as any).sidebarHydrationEpochByVersion.has(oldVersion),
      ).toBe(false);
      expect([...((sessions as any).hydratedSessionsByVersion.keys())])
        .toEqual([newVersion]);
      expect([...((sessions as any).sidebarHydrationEpochByVersion.keys())])
        .toEqual([newVersion]);
    });

    it("dedupes overlapping visible hydration for the same session", async () => {
      mockSidebarIndex([makeSkinnyRow({ id: "same" })]);
      await sessions.load();

      let resolveDetail!: (session: Session) => void;
      vi.mocked(api.getSession).mockReturnValue(
        new Promise<Session>((resolve) => {
          resolveDetail = resolve;
        }),
      );

      const first = (sessions as any).hydrateVisibleSessions(["same"]);
      const second = (sessions as any).hydrateVisibleSessions(["same"]);
      await Promise.resolve();

      expect(api.getSession).toHaveBeenCalledTimes(1);

      resolveDetail(makeSession({ id: "same", first_message: "detail" }));
      await Promise.all([first, second]);

      expect(sessions.sessions[0]!.first_message).toBe("detail");
    });

    it("bounds visible hydration concurrency", async () => {
      const rows = Array.from({ length: 10 }, (_, i) =>
        makeSkinnyRow({ id: `s${i}` })
      );
      mockSidebarIndex(rows);
      await sessions.load();

      const resolvers: Array<() => void> = [];
      let inFlight = 0;
      let maxInFlight = 0;
      vi.mocked(api.getSession).mockImplementation((id: string) => {
        inFlight++;
        maxInFlight = Math.max(maxInFlight, inFlight);
        return new Promise<Session>((resolve) => {
          resolvers.push(() => {
            inFlight--;
            resolve(makeSession({ id }));
          });
        });
      });

      const hydrate = (sessions as any).hydrateVisibleSessions(
        rows.map((row) => row.id),
      );

      await vi.waitFor(() => {
        expect(resolvers.length).toBeGreaterThan(0);
      });
      expect(maxInFlight).toBeLessThanOrEqual(6);

      while (resolvers.length > 0) {
        resolvers.shift()!();
        await Promise.resolve();
      }
      await hydrate;
      expect(api.getSession).toHaveBeenCalledTimes(10);
    });

    it("refreshing the active session preserves teammate metadata", async () => {
      mockSidebarIndex([
        makeSkinnyRow({ id: "team", is_teammate: true }),
      ]);
      await sessions.load();
      sessions.selectSession("team");
      vi.mocked(api.getSession).mockResolvedValue(
        makeSession({ id: "team", first_message: "full detail" }),
      );

      await sessions.refreshActiveSession();

      expect(sessions.sessions[0]!.first_message).toBe("full detail");
      expect(sessions.sessions[0]!.is_teammate).toBe(true);
      expect(sessions.sessions[0]!.is_index_only).toBe(false);
    });

    it("selecting an index-only session hydrates it", async () => {
      mockSidebarIndex([makeSkinnyRow({ id: "select-me" })]);
      await sessions.load();
      vi.mocked(api.getSession).mockResolvedValue(
        makeSession({
          id: "select-me",
          first_message: "hydrated on select",
        }),
      );

      sessions.selectSession("select-me");

      await vi.waitFor(() => {
        expect(sessions.sessions[0]!.first_message).toBe(
          "hydrated on select",
        );
      });
      expect(sessions.sessions[0]!.is_index_only).toBe(false);
    });

    it("does not expose index-only rows through activeSession", async () => {
      mockSidebarIndex([makeSkinnyRow({ id: "active" })]);
      await sessions.load();
      let resolveDetail!: (session: Session) => void;
      vi.mocked(api.getSession).mockReturnValue(
        new Promise<Session>((resolve) => {
          resolveDetail = resolve;
        }),
      );

      sessions.selectSession("active");

      expect(sessions.activeSession).toBeUndefined();

      resolveDetail(makeSession({
        id: "active",
        first_message: "ready for detail consumers",
      }));
      await vi.waitFor(() => {
        expect(sessions.activeSession?.first_message).toBe(
          "ready for detail consumers",
        );
      });
    });

    it("delete removes an index row locally and invalidates metadata", async () => {
      mockSidebarIndex([
        makeSkinnyRow({ id: "remove-me" }),
        makeSkinnyRow({ id: "keep-me" }),
      ]);
      vi.mocked((api as any).deleteSession).mockResolvedValue(undefined);
      vi.mocked(api.getProjects).mockResolvedValue({ projects: [] });
      vi.mocked(api.getAgents).mockResolvedValue({ agents: [] });
      vi.mocked((api as any).getMachines).mockResolvedValue({ machines: [] });

      await sessions.load();
      await sessions.deleteSession("remove-me");

      expect(sessions.sessions.map((s) => s.id)).toEqual(["keep-me"]);
      expect(sessions.total).toBe(1);
      expect(api.getProjects).toHaveBeenCalled();
      expect(api.getAgents).toHaveBeenCalled();
      expect((api as any).getMachines).toHaveBeenCalled();
    });

    it("batch delete creates one undo entry for the whole batch", async () => {
      vi.mocked(api.getSidebarSessionIndex)
        .mockResolvedValueOnce({
          sessions: [
            makeSkinnyRow({ id: "remove-a" }),
            makeSkinnyRow({ id: "remove-b" }),
            makeSkinnyRow({ id: "keep-me" }),
          ],
          total: 3,
          next_cursor: null,
        })
        .mockResolvedValueOnce({
          sessions: [makeSkinnyRow({ id: "keep-me" })],
          total: 1,
          next_cursor: null,
        });
      vi.mocked(api.batchDeleteSessions).mockResolvedValue(undefined);
      vi.mocked(api.getProjects).mockResolvedValue({ projects: [] });
      vi.mocked(api.getAgents).mockResolvedValue({ agents: [] });
      vi.mocked((api as any).getMachines).mockResolvedValue({ machines: [] });

      await sessions.load();
      await sessions.batchDeleteSessions(["remove-a", "remove-b"]);

      expect(api.batchDeleteSessions).toHaveBeenCalledWith([
        "remove-a",
        "remove-b",
      ]);
      expect(sessions.sessions.map((s) => s.id)).toEqual(["keep-me"]);
      expect(sessions.total).toBe(1);
      expect(sessions.recentlyDeleted).toHaveLength(1);
      expect(sessions.recentlyDeleted[0]!.ids).toEqual([
        "remove-a",
        "remove-b",
      ]);
    });

    it("reloads sidebar totals after deleting child rows", async () => {
      vi.mocked(api.getSidebarSessionIndex)
        .mockResolvedValueOnce({
          sessions: [
            makeSkinnyRow({ id: "parent" }),
            makeSkinnyRow({
              id: "child",
              parent_session_id: "parent",
            }),
          ],
          total: 1,
          next_cursor: null,
        })
        .mockResolvedValueOnce({
          sessions: [makeSkinnyRow({ id: "parent" })],
          total: 1,
          next_cursor: null,
        });
      vi.mocked(api.batchDeleteSessions).mockResolvedValue(undefined);
      vi.mocked(api.getProjects).mockResolvedValue({ projects: [] });
      vi.mocked(api.getAgents).mockResolvedValue({ agents: [] });
      vi.mocked((api as any).getMachines).mockResolvedValue({ machines: [] });

      await sessions.load();
      await sessions.batchDeleteSessions(["child"]);

      expect(api.getSidebarSessionIndex).toHaveBeenCalledTimes(2);
      expect(sessions.sessions.map((s) => s.id)).toEqual(["parent"]);
      expect(sessions.total).toBe(1);
      expect(sessions.recentlyDeleted[0]!.ids).toEqual(["child"]);
    });

    it("does not reuse a pre-delete in-flight sidebar load after batch delete", async () => {
      let resolveStaleLoad!: (value: {
        sessions: SkinnySessionRow[];
        total: number;
        next_cursor?: string | null;
      }) => void;
      vi.mocked(api.getSidebarSessionIndex)
        .mockReturnValueOnce(new Promise((resolve) => {
          resolveStaleLoad = resolve;
        }))
        .mockResolvedValueOnce({
          sessions: [makeSkinnyRow({ id: "keep-me" })],
          total: 1,
          next_cursor: null,
        });
      vi.mocked(api.batchDeleteSessions).mockResolvedValue(undefined);
      vi.mocked(api.getProjects).mockResolvedValue({ projects: [] });
      vi.mocked(api.getAgents).mockResolvedValue({ agents: [] });
      vi.mocked((api as any).getMachines).mockResolvedValue({ machines: [] });

      const staleLoad = sessions.load();
      await Promise.resolve();

      const deletePromise = sessions.batchDeleteSessions(["remove-me"]);

      await vi.waitFor(() => {
        expect(api.getSidebarSessionIndex).toHaveBeenCalledTimes(2);
      });

      resolveStaleLoad({
        sessions: [
          makeSkinnyRow({ id: "remove-me" }),
          makeSkinnyRow({ id: "keep-me" }),
        ],
        total: 2,
        next_cursor: null,
      });
      await Promise.all([staleLoad, deletePromise]);

      expect(sessions.sessions.map((s) => s.id)).toEqual(["keep-me"]);
      expect(sessions.total).toBe(1);
    });

    it("restore reloads the sidebar index", async () => {
      mockSidebarIndex([makeSkinnyRow({ id: "before" })]);
      vi.mocked((api as any).restoreSession).mockResolvedValue(undefined);
      await sessions.load();
      mockSidebarIndex([makeSkinnyRow({ id: "after" })]);

      await sessions.restoreSession("before");

      expect((api as any).restoreSession).toHaveBeenCalledWith("before");
      expect(api.getSidebarSessionIndex).toHaveBeenCalledTimes(2);
      expect(sessions.sessions.map((s) => s.id)).toEqual(["after"]);
    });

    it("removes only one id from a recently deleted batch", () => {
      const timer = setTimeout(() => {}, 10_000);
      sessions.recentlyDeleted = [
        { key: 1, ids: ["restore-a", "restore-b"], timer },
      ];

      sessions.clearRecentlyDeleted("restore-a");

      expect(sessions.recentlyDeleted).toHaveLength(1);
      expect(sessions.recentlyDeleted[0]!.ids).toEqual(["restore-b"]);

      sessions.clearRecentlyDeleted();
    });

    it("restores all sessions from one recently deleted batch", async () => {
      const timer = setTimeout(() => {}, 10_000);
      sessions.recentlyDeleted = [
        { key: 1, ids: ["restore-a", "restore-b"], timer },
      ];
      vi.mocked((api as any).restoreSession).mockResolvedValue(undefined);
      mockSidebarIndex([
        makeSkinnyRow({ id: "restore-a" }),
        makeSkinnyRow({ id: "restore-b" }),
      ]);

      await sessions.restoreRecentlyDeleted(sessions.recentlyDeleted[0]!);

      expect((api as any).restoreSession).toHaveBeenNthCalledWith(
        1,
        "restore-a",
      );
      expect((api as any).restoreSession).toHaveBeenNthCalledWith(
        2,
        "restore-b",
      );
      expect(sessions.recentlyDeleted).toEqual([]);
      expect(api.getSidebarSessionIndex).toHaveBeenCalledTimes(1);
    });

    it("does not reuse a post-delete in-flight sidebar load after batch undo", async () => {
      let resolveDeleteReload!: (value: {
        sessions: SkinnySessionRow[];
        total: number;
        next_cursor?: string | null;
      }) => void;
      vi.mocked(api.getSidebarSessionIndex)
        .mockReturnValueOnce(new Promise((resolve) => {
          resolveDeleteReload = resolve;
        }))
        .mockResolvedValueOnce({
          sessions: [makeSkinnyRow({ id: "restore-me" })],
          total: 1,
          next_cursor: null,
        });
      vi.mocked(api.batchDeleteSessions).mockResolvedValue(undefined);
      vi.mocked((api as any).restoreSession).mockResolvedValue(undefined);
      vi.mocked(api.getProjects).mockResolvedValue({ projects: [] });
      vi.mocked(api.getAgents).mockResolvedValue({ agents: [] });
      vi.mocked((api as any).getMachines).mockResolvedValue({ machines: [] });

      const deletePromise = sessions.batchDeleteSessions(["restore-me"]);
      await vi.waitFor(() => {
        expect(sessions.recentlyDeleted).toHaveLength(1);
        expect(api.getSidebarSessionIndex).toHaveBeenCalledTimes(1);
      });

      const restorePromise = sessions.restoreRecentlyDeleted(
        sessions.recentlyDeleted[0]!,
      );

      await vi.waitFor(() => {
        expect(api.getSidebarSessionIndex).toHaveBeenCalledTimes(2);
      });

      resolveDeleteReload({
        sessions: [],
        total: 0,
        next_cursor: null,
      });
      await Promise.all([deletePromise, restorePromise]);

      expect(sessions.sessions.map((s) => s.id)).toEqual(["restore-me"]);
      expect(sessions.total).toBe(1);
    });

    it("keeps only failed ids when batch undo partially fails", async () => {
      const timer = setTimeout(() => {}, 10_000);
      sessions.recentlyDeleted = [
        {
          key: 1,
          ids: ["restore-a", "restore-b", "restore-c"],
          timer,
        },
      ];
      vi.mocked((api as any).restoreSession)
        .mockResolvedValueOnce(undefined)
        .mockRejectedValueOnce(new Error("restore failed"))
        .mockResolvedValueOnce(undefined);
      mockSidebarIndex([makeSkinnyRow({ id: "restore-b" })]);

      await expect(
        sessions.restoreRecentlyDeleted(sessions.recentlyDeleted[0]!),
      ).rejects.toThrow("Failed to restore 1 session");

      expect((api as any).restoreSession).toHaveBeenNthCalledWith(
        1,
        "restore-a",
      );
      expect((api as any).restoreSession).toHaveBeenNthCalledWith(
        2,
        "restore-b",
      );
      expect((api as any).restoreSession).toHaveBeenNthCalledWith(
        3,
        "restore-c",
      );
      expect(sessions.recentlyDeleted).toHaveLength(1);
      expect(sessions.recentlyDeleted[0]!.ids).toEqual(["restore-b"]);
      expect(api.getSidebarSessionIndex).toHaveBeenCalledTimes(1);

      sessions.clearRecentlyDeleted();
    });

    it("keeps failed ids retryable if the original timer expires during batch undo", async () => {
      vi.useFakeTimers();
      try {
        const timer = setTimeout(() => {
          sessions.recentlyDeleted = sessions.recentlyDeleted.filter(
            (d) => d.key !== 1,
          );
        }, 10_000);
        sessions.recentlyDeleted = [
          { key: 1, ids: ["restore-a"], timer },
        ];
        vi.mocked((api as any).restoreSession).mockImplementation(
          async () => {
            vi.advanceTimersByTime(10_000);
            throw new Error("restore failed");
          },
        );
        mockSidebarIndex([makeSkinnyRow({ id: "restore-a" })]);

        await expect(
          sessions.restoreRecentlyDeleted(sessions.recentlyDeleted[0]!),
        ).rejects.toThrow("Failed to restore 1 session");

        expect(sessions.recentlyDeleted).toHaveLength(1);
        expect(sessions.recentlyDeleted[0]!.ids).toEqual(["restore-a"]);

        vi.advanceTimersByTime(9_999);
        expect(sessions.recentlyDeleted).toHaveLength(1);

        vi.advanceTimersByTime(1);
        expect(sessions.recentlyDeleted).toEqual([]);
      } finally {
        vi.useRealTimers();
      }
    });
  });

  describe("parseFiltersFromParams", () => {
    it("should parse all known URL params", () => {
      const f = parseFiltersFromParams({
        project: "myproj",
        machine: "host-a",
        agent: "claude",
        date: "2024-06-15",
        date_from: "2024-06-01",
        date_to: "2024-06-30",
        active_since: "true",
        exclude_project: "unknown",
        min_messages: "5",
        max_messages: "100",
        min_user_messages: "3",
        include_one_shot: "false",
        include_automated: "true",
      });
      expect(f.project).toBe("myproj");
      expect(f.machine).toBe("host-a");
      expect(f.agent).toBe("claude");
      expect(f.date).toBe("2024-06-15");
      expect(f.dateFrom).toBe("2024-06-01");
      expect(f.dateTo).toBe("2024-06-30");
      expect(f.recentlyActive).toBe(true);
      expect(f.hideUnknownProject).toBe(true);
      expect(f.minMessages).toBe(5);
      expect(f.maxMessages).toBe(100);
      expect(f.minUserMessages).toBe(3);
      expect(f.includeOneShot).toBe(false);
      expect(f.includeAutomated).toBe(true);
    });

    it("should return defaults for empty params", () => {
      const f = parseFiltersFromParams({});
      expect(f.project).toBe("");
      expect(f.agent).toBe("");
      expect(f.minMessages).toBe(0);
      expect(f.includeOneShot).toBe(true);
      expect(f.includeAutomated).toBe(false);
    });

    it("should clear project=unknown when exclude_project=unknown", () => {
      const f = parseFiltersFromParams({
        project: "unknown",
        exclude_project: "unknown",
      });
      expect(f.project).toBe("");
      expect(f.hideUnknownProject).toBe(true);
    });

    it("should set hideUnknown from CSV exclude_project values", () => {
      const f = parseFiltersFromParams({
        exclude_project: "alpha,unknown",
      });
      expect(f.hideUnknownProject).toBe(true);
    });

    it("should handle non-numeric min_messages", () => {
      const f = parseFiltersFromParams({ min_messages: "abc" });
      expect(f.minMessages).toBe(0);
    });
  });

  describe("filtersToParams", () => {
    it("should return empty params for default filters", () => {
      const params = filtersToParams(parseFiltersFromParams({}));
      expect(params).toEqual({});
    });

    it("should serialize all set filters", () => {
      const f: Filters = {
        project: "myproj",
        machine: "host-a",
        agent: "claude",
        termination: "unclean",
        date: "2024-06-15",
        dateFrom: "2024-06-01",
        dateTo: "2024-06-30",
        recentlyActive: true,
        hideUnknownProject: true,
        minMessages: 5,
        maxMessages: 100,
        minUserMessages: 3,
        includeOneShot: false,
        includeAutomated: true,
      };
      expect(filtersToParams(f)).toEqual({
        project: "myproj",
        machine: "host-a",
        agent: "claude",
        termination: "unclean",
        date: "2024-06-15",
        date_from: "2024-06-01",
        date_to: "2024-06-30",
        active_since: "true",
        exclude_project: "unknown",
        min_messages: "5",
        max_messages: "100",
        min_user_messages: "3",
        include_one_shot: "false",
        include_automated: "true",
      });
    });

    it("should serialize termination filter into the URL", () => {
      const defaults = parseFiltersFromParams({});
      const params = filtersToParams({ ...defaults, termination: "unclean" });
      expect(params.termination).toBe("unclean");
    });

    it("should parse termination from URL params", () => {
      const f = parseFiltersFromParams({ termination: "unclean" });
      expect(f.termination).toBe("unclean");
    });

    it("should round-trip through parseFiltersFromParams", () => {
      const original: Filters = {
        project: "myproj",
        machine: "host-a",
        agent: "claude",
        termination: "unclean",
        date: "2024-06-15",
        dateFrom: "2024-06-01",
        dateTo: "2024-06-30",
        recentlyActive: true,
        hideUnknownProject: true,
        minMessages: 5,
        maxMessages: 100,
        minUserMessages: 3,
        includeOneShot: false,
        includeAutomated: true,
      };
      const params = filtersToParams(original);
      const parsed = parseFiltersFromParams(params);
      expect(parsed).toEqual(original);
    });

    it("should round-trip default filters as empty", () => {
      const defaults = parseFiltersFromParams({});
      const params = filtersToParams(defaults);
      const reparsed = parseFiltersFromParams(params);
      expect(reparsed).toEqual(defaults);
      expect(params).toEqual({});
    });
  });

  describe("load serialization", () => {
    it("should omit min/max_messages when 0", async () => {
      sessions.filters.minMessages = 0;
      sessions.filters.maxMessages = 0;
      await sessions.load();

      expectSidebarIndexCalledWith({
        min_messages: undefined,
        max_messages: undefined,
      });
    });

    it("should include positive min_messages", async () => {
      sessions.filters.minMessages = 5;
      await sessions.load();

      expectSidebarIndexCalledWith({ min_messages: 5 });
    });

    it("should include positive max_messages", async () => {
      sessions.filters.maxMessages = 100;
      await sessions.load();

      expectSidebarIndexCalledWith({ max_messages: 100 });
    });

    it("should pass project filter when set", async () => {
      sessions.filters.project = "myproj";
      await sessions.load();

      expectSidebarIndexCalledWith({ project: "myproj" });
    });

    it("should omit project when empty", async () => {
      sessions.filters.project = "";
      await sessions.load();

      expectSidebarIndexCalledWith({
        project: undefined,
      });
    });

    it("should pass agent filter when set", async () => {
      sessions.filters.agent = "claude";
      await sessions.load();

      expectSidebarIndexCalledWith({ agent: "claude" });
    });

    it("should omit agent when empty", async () => {
      sessions.filters.agent = "";
      await sessions.load();

      expectSidebarIndexCalledWith({ agent: undefined });
    });

    it("should pass date filter when set", async () => {
      sessions.filters.date = "2024-06-15";
      await sessions.load();

      expectSidebarIndexCalledWith({
        date: "2024-06-15",
      });
    });

    it("should omit date when empty", async () => {
      sessions.filters.date = "";
      await sessions.load();

      expectSidebarIndexCalledWith({ date: undefined });
    });

    it("should pass date_from filter when set", async () => {
      sessions.filters.dateFrom = "2024-06-01";
      await sessions.load();

      expectSidebarIndexCalledWith({
        date_from: "2024-06-01",
      });
    });

    it("should omit date_from when empty", async () => {
      sessions.filters.dateFrom = "";
      await sessions.load();

      expectSidebarIndexCalledWith({
        date_from: undefined,
      });
    });

    it("should pass date_to filter when set", async () => {
      sessions.filters.dateTo = "2024-06-30";
      await sessions.load();

      expectSidebarIndexCalledWith({
        date_to: "2024-06-30",
      });
    });

    it("should omit date_to when empty", async () => {
      sessions.filters.dateTo = "";
      await sessions.load();

      expectSidebarIndexCalledWith({
        date_to: undefined,
      });
    });
  });

  describe("loadMore serialization", () => {
    it("should load the sidebar index once with consistent filters", async () => {
      mockSidebarIndex([
        makeSkinnyRow({ id: "s1" }),
        makeSkinnyRow({ id: "s2" }),
      ]);

      sessions.filters.minMessages = 10;
      sessions.filters.maxMessages = 50;
      await sessions.load();

      expect(api.getSidebarSessionIndex).toHaveBeenCalledTimes(1);
      const first = vi.mocked(api.getSidebarSessionIndex)
        .mock.calls[0]?.[0];

      expect(first?.minMessages).toBe(10);
      expect(first?.maxMessages).toBe(50);
      expect(first?.cursor).toBeUndefined();

      expect(sessions.sessions).toHaveLength(2);
      expect(sessions.total).toBe(2);
      expect(sessions.nextCursor).toBeNull();
    });

    it("preserves old sidebar rows while the index reload is in flight", async () => {
      // Pre-populate with a list representing a prior load,
      // then trigger a delayed index reload. The old rows stay
      // visible until the new index request succeeds.
      sessions.sessions = [
        makeSession({ id: "old-a" }),
        makeSession({ id: "old-b" }),
        makeSession({ id: "old-c" }),
      ];
      sessions.total = 3;

      let resolveIndex: ((v: {
        sessions: SkinnySessionRow[];
        total: number;
      }) => void) | null = null;
      const indexPromise = new Promise<{
        sessions: SkinnySessionRow[];
        total: number;
      }>((resolve) => {
        resolveIndex = resolve;
      });

      vi.mocked(api.getSidebarSessionIndex)
        .mockReturnValueOnce(indexPromise);

      const loadPromise = sessions.load();

      // Flush the load start without resolving the index request.
      await Promise.resolve();
      await Promise.resolve();

      expect(sessions.sessions.map((s) => s.id)).toEqual([
        "old-a",
        "old-b",
        "old-c",
      ]);
      expect(sessions.total).toBe(3);
      expect(sessions.loading).toBe(true);

      resolveIndex!({
        sessions: [
          makeSkinnyRow({ id: "new-1" }),
          makeSkinnyRow({ id: "new-2" }),
        ],
        total: 2,
      });
      await loadPromise;

      expect(sessions.sessions.map((s) => s.id)).toEqual([
        "new-1",
        "new-2",
      ]);
      expect(sessions.total).toBe(2);
      expect(sessions.nextCursor).toBeNull();
    });

    it("should omit min/max when 0 in loadMore", async () => {
      sessions.nextCursor = "cur2";

      mockSidebarPage();
      await sessions.loadMore();

      expectPaginatedSidebarIndexCalledWith({
        min_messages: undefined,
        max_messages: undefined,
      });
    });

    it("should omit agent when empty in loadMore", async () => {
      sessions.nextCursor = "cur3";
      sessions.filters.agent = "";

      mockSidebarPage();
      await sessions.loadMore();

      expectPaginatedSidebarIndexCalledWith({ agent: undefined });
    });

    it("should omit date when empty in loadMore", async () => {
      sessions.nextCursor = "cur3";
      sessions.filters.date = "";

      mockSidebarPage();
      await sessions.loadMore();

      expectPaginatedSidebarIndexCalledWith({ date: undefined });
    });

    it("should omit date_from when empty in loadMore", async () => {
      sessions.nextCursor = "cur3";
      sessions.filters.dateFrom = "";

      mockSidebarPage();
      await sessions.loadMore();

      expectPaginatedSidebarIndexCalledWith({
        date_from: undefined,
      });
    });

    it("should omit date_to when empty in loadMore", async () => {
      sessions.nextCursor = "cur3";
      sessions.filters.dateTo = "";

      mockSidebarPage();
      await sessions.loadMore();

      expectPaginatedSidebarIndexCalledWith({
        date_to: undefined,
      });
    });

    it("should pass all filters in loadMore", async () => {
      sessions.nextCursor = "cur3";
      sessions.filters.agent = "codex";
      sessions.filters.date = "2024-07-01";
      sessions.filters.dateFrom = "2024-07-01";
      sessions.filters.dateTo = "2024-07-31";

      mockSidebarPage();
      await sessions.loadMore();

      expectPaginatedSidebarIndexCalledWith({
        agent: "codex",
        date: "2024-07-01",
        date_from: "2024-07-01",
        date_to: "2024-07-31",
      });
    });
  });

  describe("setProjectFilter", () => {
    it("should reset non-project/date filters, preserve agent, and reset pagination", async () => {
      sessions.filters.agent = "codex";
      sessions.filters.date = "2024-06-15";
      sessions.filters.dateFrom = "2024-06-01";
      sessions.filters.dateTo = "2024-06-30";
      sessions.filters.minMessages = 5;
      sessions.filters.maxMessages = 100;
      sessions.activeSessionId = "old-session";

      sessions.setProjectFilter("myproj");
      // Wait for load() triggered by setProjectFilter to complete,
      // not just start — verifies loading clears after the fetch.
      await vi.waitFor(() => {
        expect(api.getSidebarSessionIndex).toHaveBeenCalled();
        expect(sessions.loading).toBe(false);
      });

      expect(sessions.filters.project).toBe("myproj");
      expect(sessions.filters.agent).toBe("codex");
      expect(sessions.filters.date).toBe("");
      expect(sessions.filters.dateFrom).toBe("");
      expect(sessions.filters.dateTo).toBe("");
      expect(sessions.filters.minMessages).toBe(0);
      expect(sessions.filters.maxMessages).toBe(0);
      expect(sessions.activeSessionId).toBeNull();

      expectSidebarIndexCalledWith({
        project: "myproj",
        agent: "codex",
        date: undefined,
        date_from: undefined,
        date_to: undefined,
        min_messages: undefined,
        max_messages: undefined,
      });
    });
  });

  describe("hideUnknownProject filter", () => {
    it("should send exclude_project=unknown when enabled", async () => {
      sessions.filters.hideUnknownProject = true;
      await sessions.load();

      expectSidebarIndexCalledWith({
        exclude_project: "unknown",
      });
    });

    it("should omit exclude_project when disabled", async () => {
      sessions.filters.hideUnknownProject = false;
      await sessions.load();

      expectSidebarIndexCalledWith({
        exclude_project: undefined,
      });
    });

    it("should clear project filter when hiding unknown and project is unknown", async () => {
      sessions.filters.project = "unknown";
      sessions.setHideUnknownProjectFilter(true);
      await vi.waitFor(() => {
        expect(api.getSidebarSessionIndex).toHaveBeenCalled();
      });

      expect(sessions.filters.project).toBe("");
      expect(sessions.filters.hideUnknownProject).toBe(true);
      expectSidebarIndexCalledWith({
        project: undefined,
        exclude_project: "unknown",
      });
    });

    it("should preserve project filter when hiding unknown and project is not unknown", async () => {
      sessions.filters.project = "my_app";
      sessions.setHideUnknownProjectFilter(true);
      await vi.waitFor(() => {
        expect(api.getSidebarSessionIndex).toHaveBeenCalled();
      });

      expect(sessions.filters.project).toBe("my_app");
      expect(sessions.filters.hideUnknownProject).toBe(true);
    });

    it("should round-trip via initFromParams", () => {
      sessions.initFromParams({
        exclude_project: "unknown",
      });
      expect(sessions.filters.hideUnknownProject).toBe(true);
    });

    it("should not set hideUnknown for other exclude values", () => {
      sessions.initFromParams({
        exclude_project: "something_else",
      });
      expect(sessions.filters.hideUnknownProject).toBe(false);
    });

    it("should clear conflicting project=unknown in initFromParams", () => {
      sessions.initFromParams({
        project: "unknown",
        exclude_project: "unknown",
      });
      expect(sessions.filters.project).toBe("");
      expect(sessions.filters.hideUnknownProject).toBe(true);
    });

    it("should split hide-unknown from usage project exclusions", () => {
      expect(
        splitExcludeProjectParam("alpha,unknown,beta"),
      ).toEqual({
        hideUnknownProject: true,
        usageExcludedProjects: "alpha,beta",
      });
    });

    it("should be included in hasActiveFilters", () => {
      sessions.filters.hideUnknownProject = true;
      expect(sessions.hasActiveFilters).toBe(true);
    });

    it("should suppress exclude_project when project is unknown", async () => {
      sessions.filters.hideUnknownProject = true;
      sessions.filters.project = "unknown";
      await sessions.load();

      expectSidebarIndexCalledWith({
        project: "unknown",
        exclude_project: undefined,
      });
    });

    it("should be cleared by clearSessionFilters", async () => {
      sessions.filters.hideUnknownProject = true;
      sessions.clearSessionFilters();
      await vi.waitFor(() => {
        expect(api.getSidebarSessionIndex).toHaveBeenCalled();
      });

      expect(sessions.filters.hideUnknownProject).toBe(false);
    });
  });

  describe("hasActiveFilters", () => {
    it("should be false with default filters", () => {
      expect(sessions.hasActiveFilters).toBe(false);
    });

    it("should be true when machine filter is set", () => {
      sessions.filters.machine = "host-a";
      expect(sessions.hasActiveFilters).toBe(true);
    });

    it("should be true when agent filter is set", () => {
      sessions.filters.agent = "claude";
      expect(sessions.hasActiveFilters).toBe(true);
    });

    it("should be true when recentlyActive filter is set", () => {
      sessions.filters.recentlyActive = true;
      expect(sessions.hasActiveFilters).toBe(true);
    });

    it("should be true when minUserMessages filter is set", () => {
      sessions.filters.minUserMessages = 3;
      expect(sessions.hasActiveFilters).toBe(true);
    });

    it("should be false after clearSessionFilters", async () => {
      sessions.filters.agent = "claude";
      sessions.filters.recentlyActive = true;
      sessions.filters.minUserMessages = 5;
      expect(sessions.hasActiveFilters).toBe(true);

      sessions.clearSessionFilters();
      await vi.waitFor(() => {
        expect(api.getSidebarSessionIndex).toHaveBeenCalled();
      });

      expect(sessions.hasActiveFilters).toBe(false);
    });

    it("should preserve project filter after clearSessionFilters", async () => {
      sessions.filters.project = "myproj";
      sessions.filters.agent = "claude";
      sessions.clearSessionFilters();
      await vi.waitFor(() => {
        expect(api.getSidebarSessionIndex).toHaveBeenCalled();
      });

      expect(sessions.filters.project).toBe("myproj");
      expect(sessions.hasActiveFilters).toBe(false);
    });

    it("clears the date yoke before clearing the active session", () => {
      sessions.activeSessionId = "session-1";
      sessions.filters.dateFrom = "2025-05-01";
      sessions.filters.dateTo = "2025-05-31";
      yokedDates.setEnabled(true);
      yokedDates.updateFromPanel({
        from: "2025-05-01",
        to: "2025-05-31",
        mode: "rolling",
        windowDays: 30,
      });
      expect(yokedDates.range).not.toBeNull();

      const store = sessions as unknown as {
        setActiveSession: (id: string | null) => void;
      };
      const setActiveSession = store.setActiveSession.bind(sessions);
      const spy = vi
        .spyOn(store, "setActiveSession")
        .mockImplementation((id) => {
          expect(yokedDates.range).toBeNull();
          setActiveSession(id);
        });

      sessions.clearSessionFilters();

      expect(spy).toHaveBeenCalledWith(null);
      expect(sessions.activeSessionId).toBeNull();
      expect(yokedDates.range).toBeNull();
    });

    it("clears the date yoke before clearing the active session when requested by route intent", () => {
      sessions.activeSessionId = "session-1";
      sessions.filters.agent = "codex";
      yokedDates.setEnabled(true);
      yokedDates.updateFromPanel({
        from: "2025-05-01",
        to: "2025-05-31",
        mode: "rolling",
        windowDays: 30,
      });
      expect(yokedDates.range).not.toBeNull();

      const store = sessions as unknown as {
        setActiveSession: (id: string | null) => void;
      };
      const setActiveSession = store.setActiveSession.bind(sessions);
      const spy = vi
        .spyOn(store, "setActiveSession")
        .mockImplementation((id) => {
          expect(yokedDates.range).toBeNull();
          setActiveSession(id);
        });

      sessions.clearSessionFilters({ clearDateYoke: true });

      expect(spy).toHaveBeenCalledWith(null);
      expect(sessions.activeSessionId).toBeNull();
      expect(yokedDates.range).toBeNull();
    });

    it("keeps the date yoke for non-date filter clears without route date intent", () => {
      sessions.filters.agent = "codex";
      yokedDates.setEnabled(true);
      yokedDates.updateFromPanel({
        from: "2025-05-01",
        to: "2025-05-31",
        mode: "fixed",
      });

      sessions.clearSessionFilters();

      expect(yokedDates.range).toMatchObject({
        from: "2025-05-01",
        to: "2025-05-31",
        mode: "fixed",
      });
    });
  });

  describe("machine filter", () => {
    it("should toggle one machine on and serialize it", async () => {
      sessions.toggleMachineFilter("host-a");
      await vi.waitFor(() => {
        expect(api.getSidebarSessionIndex).toHaveBeenCalled();
      });

      expect(sessions.filters.machine).toBe("host-a");
      expect(sessions.selectedMachines).toEqual(["host-a"]);
      expect(sessions.isMachineSelected("host-a")).toBe(true);
      expectSidebarIndexCalledWith({ machine: "host-a" });
    });

    it("should allow multiple selected machines", async () => {
      sessions.toggleMachineFilter("host-a");
      await vi.waitFor(() => {
        expect(api.getSidebarSessionIndex).toHaveBeenCalledTimes(1);
      });

      sessions.toggleMachineFilter("host-b");
      await vi.waitFor(() => {
        expect(api.getSidebarSessionIndex).toHaveBeenCalledTimes(2);
      });

      expect(sessions.filters.machine).toBe("host-a,host-b");
      expect(sessions.selectedMachines).toEqual([
        "host-a",
        "host-b",
      ]);
      expect(sessions.isMachineSelected("host-b")).toBe(true);
      expectSidebarIndexCalledWith({
        machine: "host-a,host-b",
      });
    });

    it("should toggle an already-selected machine off", async () => {
      sessions.filters.machine = "host-a,host-b";

      sessions.toggleMachineFilter("host-a");
      await vi.waitFor(() => {
        expect(api.getSidebarSessionIndex).toHaveBeenCalled();
      });

      expect(sessions.filters.machine).toBe("host-b");
      expect(sessions.selectedMachines).toEqual(["host-b"]);
      expect(sessions.isMachineSelected("host-a")).toBe(false);
      expectSidebarIndexCalledWith({ machine: "host-b" });
    });

    it("should clear the filter when the last machine is removed", async () => {
      sessions.filters.machine = "host-a";

      sessions.toggleMachineFilter("host-a");
      await vi.waitFor(() => {
        expect(api.getSidebarSessionIndex).toHaveBeenCalled();
      });

      expect(sessions.filters.machine).toBe("");
      expect(sessions.selectedMachines).toEqual([]);
      expectSidebarIndexCalledWith({ machine: undefined });
    });
  });

  describe("agent filter", () => {
    it("should clear the filter when the last agent is removed", async () => {
      sessions.filters.agent = "opencode";

      sessions.toggleAgentFilter("opencode");
      await vi.waitFor(() => {
        expect(api.getSidebarSessionIndex).toHaveBeenCalled();
      });

      expect(sessions.filters.agent).toBe("");
      expect(sessions.selectedAgents).toEqual([]);
      expect(sessions.isAgentSelected("opencode")).toBe(false);
      expectSidebarIndexCalledWith({ agent: undefined });
    });
  });

  describe("navigateSession", () => {
    function seedSessions(store: typeof sessions) {
      store.sessions = [
        makeSession({ id: "s1" }),
        makeSession({ id: "s2" }),
        makeSession({ id: "s3" }),
      ];
    }

    it("should navigate forward in the full list", () => {
      seedSessions(sessions);
      sessions.activeSessionId = "s1";
      sessions.navigateSession(1);
      expect(sessions.activeSessionId).toBe("s2");
    });

    it("should navigate backward in the full list", () => {
      seedSessions(sessions);
      sessions.activeSessionId = "s2";
      sessions.navigateSession(-1);
      expect(sessions.activeSessionId).toBe("s1");
    });

    it("should not go past the end of the list", () => {
      seedSessions(sessions);
      sessions.activeSessionId = "s3";
      sessions.navigateSession(1);
      expect(sessions.activeSessionId).toBe("s3");
    });

    it("should not go before the start of the list", () => {
      seedSessions(sessions);
      sessions.activeSessionId = "s1";
      sessions.navigateSession(-1);
      expect(sessions.activeSessionId).toBe("s1");
    });

    it("should be a no-op when no sessions are loaded", () => {
      sessions.sessions = [];
      sessions.activeSessionId = null;
      sessions.navigateSession(1);
      expect(sessions.activeSessionId).toBeNull();
    });

    it("should be a no-op when no session is selected (delta > 0)", () => {
      seedSessions(sessions);
      sessions.activeSessionId = null;
      sessions.navigateSession(1);
      expect(sessions.activeSessionId).toBeNull();
    });

    it("should be a no-op when no session is selected (delta < 0)", () => {
      seedSessions(sessions);
      sessions.activeSessionId = null;
      sessions.navigateSession(-1);
      expect(sessions.activeSessionId).toBeNull();
    });

    it("should jump to first when active session excluded by filter and delta > 0", () => {
      seedSessions(sessions);
      sessions.activeSessionId = "s2";
      const filter = (s: { id: string }) => s.id !== "s2";
      sessions.navigateSession(1, filter);
      expect(sessions.activeSessionId).toBe("s1");
    });

    it("should jump to last when active session excluded by filter and delta < 0", () => {
      seedSessions(sessions);
      sessions.activeSessionId = "s2";
      const filter = (s: { id: string }) => s.id !== "s2";
      sessions.navigateSession(-1, filter);
      expect(sessions.activeSessionId).toBe("s3");
    });

    it("should be a no-op when filtered list is empty", () => {
      seedSessions(sessions);
      sessions.activeSessionId = "s1";
      const filter = () => false;
      sessions.navigateSession(1, filter);
      expect(sessions.activeSessionId).toBe("s1");
    });

    it("should be a no-op when no session selected and filter provided", () => {
      seedSessions(sessions);
      sessions.activeSessionId = null;
      const filter = (s: { id: string }) => s.id === "s1";
      sessions.navigateSession(1, filter);
      expect(sessions.activeSessionId).toBeNull();
    });

    it("should navigate within filtered subset", () => {
      seedSessions(sessions);
      sessions.activeSessionId = "s1";
      const filter = (s: { id: string }) => s.id !== "s2";
      sessions.navigateSession(1, filter);
      expect(sessions.activeSessionId).toBe("s3");
    });

    it("hydrates an index-only target after keyboard navigation", async () => {
      sessions.sessions = [
        makeSession({ id: "s1" }),
        makeSession({
          id: "skinny",
          first_message: null,
          is_index_only: true,
        }),
      ];
      sessions.activeSessionId = "s1";
      vi.mocked(api.getSession).mockResolvedValue(
        makeSession({
          id: "skinny",
          first_message: "hydrated from navigation",
        }),
      );

      sessions.navigateSession(1);

      expect(sessions.activeSessionId).toBe("skinny");
      expect(sessions.activeSession).toBeUndefined();
      await vi.waitFor(() => {
        expect(sessions.activeSession?.first_message).toBe(
          "hydrated from navigation",
        );
      });
      expect(api.getSession).toHaveBeenCalledWith("skinny");
      expect(sessions.sessions[1]!.is_index_only).toBe(false);
    });
  });

  describe("renameSession", () => {
    it("clears display_name in store when rename is cleared and response omits the field", async () => {
      // Session starts with a custom user rename.
      mockSidebarIndex([
        makeSkinnyRow({ id: "s1", display_name: "custom-name" }),
      ]);
      await sessions.load();
      expect(sessions.sessions[0]!.display_name).toBe("custom-name");

      // Backend clears the name but finds no agent name to restore, so
      // display_name is absent from the JSON response (omitempty on nil).
      vi.mocked(api.renameSession).mockResolvedValue(
        makeSession({ id: "s1" }),
      );

      await sessions.renameSession("s1", null);

      expect(sessions.sessions[0]!.display_name).toBeNull();
    });

    it("keeps agent name restored by backend when rename is cleared", async () => {
      mockSidebarIndex([
        makeSkinnyRow({ id: "s1", display_name: "custom-name" }),
      ]);
      await sessions.load();

      // Backend re-parsed the file and restored the agent name.
      vi.mocked(api.renameSession).mockResolvedValue(
        makeSession({ id: "s1", display_name: "agent-name" }),
      );

      await sessions.renameSession("s1", null);

      expect(sessions.sessions[0]!.display_name).toBe("agent-name");
    });

    it("renames an active session backed only by the detail cache", async () => {
      // Open a session that is absent from the (empty) sidebar list, so it is
      // held solely in activeSessionDetail.
      mockSidebarIndex([]);
      await sessions.load();
      vi.mocked(api.getSession).mockResolvedValue(
        makeSession({ id: "cached", display_name: null, project: "proj-b" }),
      );
      await sessions.navigateToSession("cached");
      expect(sessions.sessions.some((s) => s.id === "cached")).toBe(false);

      vi.mocked(api.renameSession).mockResolvedValue(
        makeSession({ id: "cached", display_name: "renamed-in-breadcrumb" }),
      );
      await sessions.renameSession("cached", "renamed-in-breadcrumb");

      expect(sessions.activeSession?.display_name).toBe("renamed-in-breadcrumb");
    });

    it("clears a cache-backed active session name when the response omits it", async () => {
      mockSidebarIndex([]);
      await sessions.load();
      vi.mocked(api.getSession).mockResolvedValue(
        makeSession({ id: "cached", display_name: "custom-name" }),
      );
      await sessions.navigateToSession("cached");
      expect(sessions.activeSession?.display_name).toBe("custom-name");

      // Backend clears the name and omits display_name (omitempty on nil).
      vi.mocked(api.renameSession).mockResolvedValue(makeSession({ id: "cached" }));
      await sessions.renameSession("cached", null);

      expect(sessions.activeSession?.display_name).toBeNull();
    });
  });

  describe("loadProjects dedup", () => {
    beforeEach(() => {
      mockGetProjects();
    });

    it("should only call API once across multiple loadProjects", async () => {
      await sessions.loadProjects();
      await sessions.loadProjects();
      await sessions.loadProjects();

      expect(api.getProjects).toHaveBeenCalledTimes(1);
    });

    it("should not fire concurrent requests", async () => {
      const p1 = sessions.loadProjects();
      const p2 = sessions.loadProjects();
      await Promise.all([p1, p2]);

      expect(api.getProjects).toHaveBeenCalledTimes(1);
    });

    it("should let concurrent callers await the same result", async () => {
      const p1 = sessions.loadProjects();
      const p2 = sessions.loadProjects();
      await Promise.all([p1, p2]);

      expect(sessions.projects).toHaveLength(1);
      expect(sessions.projects[0]!.name).toBe("proj");
    });

    it("should resolve without throwing when API rejects", async () => {
      vi.mocked(api.getProjects).mockRejectedValueOnce(
        new Error("network"),
      );

      await expect(
        sessions.loadProjects(),
      ).resolves.toBeUndefined();
      // Projects stay at default (empty).
      expect(sessions.projects).toHaveLength(0);
    });

    it("should allow retry after a failed load", async () => {
      vi.mocked(api.getProjects).mockRejectedValueOnce(
        new Error("network"),
      );
      await sessions.loadProjects();

      // Second attempt should succeed.
      mockGetProjects();
      await sessions.loadProjects();
      expect(sessions.projects).toHaveLength(1);
    });
  });

  describe("non-throwing background loads", () => {
    it("load preserves previous sessions on failure", async () => {
      const existing = [makeSession({ id: "s1" })];
      sessions.sessions = existing;
      sessions.total = 1;

      vi.mocked(api.getSidebarSessionIndex).mockRejectedValueOnce(
        new Error("network"),
      );
      await sessions.load();

      expect(sessions.loading).toBe(false);
      expect(sessions.sessions).toHaveLength(1);
      expect(sessions.sessions[0]!.id).toBe("s1");
      expect(sessions.total).toBe(1);
    });

    it("initFromParams + load preserves sessions on failure", async () => {
      const existing = [makeSession({ id: "s1" })];
      sessions.sessions = existing;
      sessions.total = 1;

      vi.mocked(api.getSidebarSessionIndex).mockRejectedValueOnce(
        new Error("network"),
      );
      sessions.initFromParams({ project: "other" });
      await sessions.load();

      expect(sessions.loading).toBe(false);
      expect(sessions.sessions).toHaveLength(1);
      expect(sessions.sessions[0]!.id).toBe("s1");
      expect(sessions.total).toBe(1);
    });

    it("filter change preserves sessions on failure", async () => {
      const existing = [makeSession({ id: "s1" })];
      sessions.sessions = existing;
      sessions.total = 1;

      vi.mocked(api.getSidebarSessionIndex).mockRejectedValueOnce(
        new Error("network"),
      );
      sessions.setAgentFilter("claude");
      await vi.waitFor(() => {
        expect(sessions.loading).toBe(false);
      });

      expect(sessions.sessions).toHaveLength(1);
      expect(sessions.sessions[0]!.id).toBe("s1");
      expect(sessions.total).toBe(1);
    });

    it("loadProjects resolves when API rejects", async () => {
      vi.mocked(api.getProjects).mockRejectedValueOnce(
        new Error("network"),
      );
      await expect(
        sessions.loadProjects(),
      ).resolves.toBeUndefined();
      expect(sessions.projects).toHaveLength(0);
    });

    it("loadAgents resolves when API rejects", async () => {
      vi.mocked(api.getAgents).mockRejectedValueOnce(
        new Error("network"),
      );
      await expect(
        sessions.loadAgents(),
      ).resolves.toBeUndefined();
      expect(sessions.agents).toHaveLength(0);
    });
  });

  describe("invalidateFilterCaches version guard", () => {
    beforeEach(() => {
      // Both loadProjects and loadAgents fire inside
      // invalidateFilterCaches, so supply defaults for the
      // API the test isn't explicitly controlling.
      vi.mocked(api.getProjects).mockResolvedValue({
        projects: [],
      });
      vi.mocked(api.getAgents).mockResolvedValue({
        agents: [],
      });
    });

    it("discards stale projects response after invalidation", async () => {
      let resolveStale!: (v: { projects: { name: string; session_count: number }[] }) => void;
      const stalePromise = new Promise<{ projects: { name: string; session_count: number }[] }>(
        (r) => { resolveStale = r; },
      );
      vi.mocked(api.getProjects)
        .mockReturnValueOnce(stalePromise)
        .mockResolvedValueOnce({
          projects: [{ name: "fresh-proj", session_count: 5 }],
        });

      // Start first load (will hang on stalePromise).
      sessions.loadProjects();

      // Invalidate before stale resolves — bumps version,
      // clears promise, and starts a fresh load.
      sessions.invalidateFilterCaches();

      // Now resolve the stale request.
      resolveStale({
        projects: [{ name: "stale-proj", session_count: 1 }],
      });
      await vi.waitFor(() => {
        expect(sessions.projects).toHaveLength(1);
      });

      // Fresh response should win.
      expect(sessions.projects[0]!.name).toBe("fresh-proj");
    });

    it("discards stale agents response after invalidation", async () => {
      type AgentsRes = { agents: { name: string; session_count: number }[] };
      let resolveStale!: (v: AgentsRes) => void;
      const stalePromise = new Promise<AgentsRes>(
        (r) => { resolveStale = r; },
      );
      vi.mocked(api.getAgents)
        .mockReturnValueOnce(stalePromise)
        .mockResolvedValueOnce({
          agents: [{ name: "fresh-agent", session_count: 3 }],
        });

      sessions.loadAgents();
      sessions.invalidateFilterCaches();

      resolveStale({
        agents: [{ name: "stale-agent", session_count: 1 }],
      });
      await vi.waitFor(() => {
        expect(sessions.agents).toHaveLength(1);
      });

      expect(sessions.agents[0]!.name).toBe("fresh-agent");
    });
  });

  describe("navigateToSession", () => {
    it("sets activeSessionId synchronously before fetching", async () => {
      let resolveGet!: (s: Session) => void;
      const getPromise = new Promise<Session>((r) => {
        resolveGet = r;
      });
      vi.mocked(api.getSession).mockReturnValue(getPromise);
      mockSidebarPage();

      const promise = sessions.navigateToSession("new-id");

      // activeSessionId must be set before the await resolves
      expect(sessions.activeSessionId).toBe("new-id");
      expect(sessions.sessions).toHaveLength(0);

      resolveGet(makeSession({ id: "new-id" }));
      await promise;

      // The out-of-list session backs activeSession via the detail cache
      // without being injected into the (filtered) sidebar collection.
      expect(sessions.sessions).toHaveLength(0);
      expect(sessions.activeSession?.id).toBe("new-id");
    });

    it("skips fetch for already-loaded session", async () => {
      mockSidebarPage();
      sessions.sessions = [makeSession({ id: "existing" })];

      await sessions.navigateToSession("existing");

      expect(sessions.activeSessionId).toBe("existing");
      expect(api.getSession).not.toHaveBeenCalled();
    });

    it("hydrates an already-loaded index-only session", async () => {
      sessions.sessions = [
        makeSession({
          id: "existing",
          first_message: null,
          is_index_only: true,
        }),
      ];
      vi.mocked(api.getSession).mockResolvedValue(
        makeSession({
          id: "existing",
          first_message: "hydrated navigation",
        }),
      );

      await sessions.navigateToSession("existing");

      expect(api.getSession).toHaveBeenCalledWith("existing");
      expect(sessions.activeSessionId).toBe("existing");
      expect(sessions.sessions[0]!.first_message).toBe(
        "hydrated navigation",
      );
      expect(sessions.sessions[0]!.is_index_only).toBe(false);
    });

    it("merges a navigation fetch if the index row arrives while fetching", async () => {
      let resolveGet!: (s: Session) => void;
      vi.mocked(api.getSession).mockReturnValue(
        new Promise<Session>((resolve) => {
          resolveGet = resolve;
        }),
      );

      const promise = sessions.navigateToSession("racy");
      sessions.sessions = [
        makeSession({
          id: "racy",
          first_message: null,
          is_index_only: true,
        }),
      ];
      resolveGet(makeSession({
        id: "racy",
        first_message: "fetched during navigation",
      }));
      await promise;

      expect(sessions.sessions).toHaveLength(1);
      expect(sessions.sessions[0]!.first_message).toBe(
        "fetched during navigation",
      );
      expect(sessions.sessions[0]!.is_index_only).toBe(false);
      expect(sessions.activeSession?.first_message).toBe(
        "fetched during navigation",
      );
    });

    it("keeps a search-navigated out-of-index session across a sidebar reload", async () => {
      // Sidebar is filtered so the searched session never appears in the
      // index. Opening it from search backs activeSession via the detail
      // cache; a later index reload (route/filter re-evaluation) must not drop
      // the open session, and the session must not leak into the sidebar list.
      mockSidebarIndex([makeSkinnyRow({ id: "in-index", project: "proj-a" })]);
      await sessions.load();

      vi.mocked(api.getSession).mockResolvedValue(
        makeSession({
          id: "searched",
          project: "proj-b",
          first_message: "opened from search",
        }),
      );
      await sessions.navigateToSession("searched");
      expect(sessions.activeSession?.project).toBe("proj-b");
      expect(sessions.sessions.some((s) => s.id === "searched")).toBe(false);

      // The filtered index still excludes the searched session.
      mockSidebarIndex([makeSkinnyRow({ id: "in-index", project: "proj-a" })]);
      await sessions.load();

      expect(sessions.activeSessionId).toBe("searched");
      expect(sessions.activeSession?.project).toBe("proj-b");
      // Not conflated with the filtered sidebar collection.
      expect(sessions.sessions.map((s) => s.id)).toEqual(["in-index"]);
    });

    it("does not duplicate an active session that appears on a later page", async () => {
      // Page 1 of the filtered index omits the active session; it lives on a
      // later page reachable through loadMore. Backing it via the detail cache
      // (not the sidebar list) means the later page appends it exactly once.
      vi.mocked(api.getSidebarSessionIndex)
        .mockResolvedValueOnce({
          sessions: [makeSkinnyRow({ id: "page1" })],
          total: 2,
          next_cursor: "cursor-2",
        })
        .mockResolvedValueOnce({
          sessions: [makeSkinnyRow({ id: "later", project: "proj-b" })],
          total: 2,
          next_cursor: null,
        });
      await sessions.load();

      vi.mocked(api.getSession).mockResolvedValue(
        makeSession({ id: "later", project: "proj-b", first_message: "from search" }),
      );
      await sessions.navigateToSession("later");
      expect(sessions.activeSession?.id).toBe("later");
      expect(sessions.sessions.some((s) => s.id === "later")).toBe(false);

      await sessions.loadMore();

      expect(sessions.sessions.filter((s) => s.id === "later")).toHaveLength(1);
      expect(sessions.activeSession?.id).toBe("later");
    });

    it("keeps a sidebar-selected session across a reload that filters it out", async () => {
      // Select a hydrated sidebar row, then reload with a filtered index that
      // no longer includes it. The breadcrumb must not go blank.
      mockSidebarIndex([makeSkinnyRow({ id: "sel", project: "proj-a" })]);
      vi.mocked(api.getSession).mockResolvedValue(
        makeSession({ id: "sel", project: "proj-a", first_message: "detail" }),
      );
      await sessions.load();
      await sessions.hydrateVisibleSessions(["sel"]);
      expect(sessions.sessions[0]!.is_index_only).toBe(false);

      sessions.selectSession("sel");
      expect(sessions.activeSession?.id).toBe("sel");

      mockSidebarIndex([makeSkinnyRow({ id: "other", project: "proj-b" })]);
      await sessions.load();

      expect(sessions.activeSessionId).toBe("sel");
      expect(sessions.activeSession?.project).toBe("proj-a");
    });

    it("keeps a keyboard-navigated session across an excluding reload", async () => {
      mockSidebarIndex([
        makeSkinnyRow({ id: "a", project: "proj-a" }),
        makeSkinnyRow({ id: "b", project: "proj-a" }),
      ]);
      vi.mocked(api.getSession).mockImplementation((id: string) =>
        Promise.resolve(
          makeSession({ id, project: "proj-a", first_message: `${id}-detail` }),
        ),
      );
      await sessions.load();
      sessions.selectSession("a");
      await vi.waitFor(() => expect(sessions.activeSession?.id).toBe("a"));

      // Keyboard next moves to "b" via navigateSession, not selectSession.
      sessions.navigateSession(1);
      await vi.waitFor(() => expect(sessions.activeSession?.id).toBe("b"));

      // A reload excludes the keyboard-navigated session.
      mockSidebarIndex([makeSkinnyRow({ id: "c", project: "proj-b" })]);
      await sessions.load();

      expect(sessions.activeSessionId).toBe("b");
      expect(sessions.activeSession?.id).toBe("b");
    });

    it("caches an index-only selection even when a reload supersedes hydration", async () => {
      mockSidebarIndex([makeSkinnyRow({ id: "sel", project: "proj-a" })]);
      await sessions.load();

      // Hydration request stays in flight until after the reload below.
      let resolveGet!: (s: Session) => void;
      vi.mocked(api.getSession).mockReturnValue(
        new Promise<Session>((r) => {
          resolveGet = r;
        }),
      );
      sessions.selectSession("sel");
      await Promise.resolve();

      // A reload bumps the sidebar index version and drops "sel".
      mockSidebarIndex([makeSkinnyRow({ id: "other", project: "proj-b" })]);
      await sessions.load();

      // The superseded hydration resolves; its version is stale for the list
      // but the active-session cache must still capture it.
      resolveGet(makeSession({ id: "sel", project: "proj-a", first_message: "detail" }));
      await vi.waitFor(() => {
        expect(sessions.activeSession?.project).toBe("proj-a");
      });

      expect(sessions.activeSessionId).toBe("sel");
    });

    it("does not let a superseded hydration overwrite fresher active detail", async () => {
      mockSidebarIndex([]);
      await sessions.load();

      // A hydration is issued first and stays in flight...
      const staleVersion = (sessions as any).sidebarIndexVersion - 1;
      let resolveHydration!: (s: Session) => void;
      vi.mocked(api.getSession).mockReturnValueOnce(
        new Promise<Session>((r) => {
          resolveHydration = r;
        }),
      );
      const hydration = (sessions as any).hydrateVisibleSessions(
        ["sel"],
        staleVersion,
      );
      await Promise.resolve();

      // ...then a navigation commits fresher detail for the same session.
      vi.mocked(api.getSession).mockResolvedValueOnce(
        makeSession({ id: "sel", project: "proj-a", first_message: "FRESH" }),
      );
      await sessions.navigateToSession("sel");
      expect(sessions.activeSession?.first_message).toBe("FRESH");

      // The earlier-issued hydration resolves late; its response predates
      // the navigation's commit and must not replace the newer detail.
      resolveHydration(
        makeSession({ id: "sel", project: "proj-a", first_message: "STALE" }),
      );
      await hydration;

      expect(sessions.activeSession?.first_message).toBe("FRESH");
    });

    it("restores an empty out-of-list active cache on refresh", async () => {
      // Initial navigation fetch fails, leaving activeSession undefined.
      mockSidebarIndex([]);
      await sessions.load();
      vi.mocked(api.getSession).mockRejectedValueOnce(new Error("fetch failed"));
      await sessions.navigateToSession("gone");
      expect(sessions.activeSessionId).toBe("gone");
      expect(sessions.activeSession).toBeUndefined();

      // A watcher-driven refresh succeeds and must populate the empty cache.
      vi.mocked(api.getSession).mockResolvedValueOnce(
        makeSession({ id: "gone", project: "proj-b", first_message: "restored" }),
      );
      await sessions.refreshActiveSession();

      expect(sessions.activeSession?.first_message).toBe("restored");
    });

    it("does not let a pre-rename refresh response revert a rename", async () => {
      mockSidebarIndex([]);
      await sessions.load();
      vi.mocked(api.getSession).mockResolvedValueOnce(
        makeSession({ id: "sel", display_name: null, first_message: "detail" }),
      );
      await sessions.navigateToSession("sel");

      // Refresh fetch carrying a pre-rename snapshot stays in flight...
      let resolveRefresh!: (s: Session) => void;
      vi.mocked(api.getSession).mockReturnValueOnce(
        new Promise<Session>((r) => {
          resolveRefresh = r;
        }),
      );
      const refresh = sessions.refreshActiveSession();
      await Promise.resolve();

      // ...while a rename completes and updates the cached detail.
      vi.mocked(api.renameSession).mockResolvedValue(
        makeSession({ id: "sel", display_name: "renamed" }),
      );
      await sessions.renameSession("sel", "renamed");
      expect(sessions.activeSession?.display_name).toBe("renamed");

      resolveRefresh(
        makeSession({ id: "sel", display_name: null, first_message: "detail" }),
      );
      await refresh;

      expect(sessions.activeSession?.display_name).toBe("renamed");
    });

    it("does not let an in-flight hydration revert a rename of an index-only row", async () => {
      mockSidebarIndex([
        makeSkinnyRow({ id: "sel", project: "proj-a", display_name: "stale" }),
      ]);
      await sessions.load();

      // Hydration of the index-only row stays in flight while the user
      // selects it (selection joins the request) and renames it. The detail
      // cache is still empty at rename time, so the rename must advance the
      // freshness generation without relying on a populated cache.
      let resolveHydration!: (s: Session) => void;
      vi.mocked(api.getSession).mockReturnValueOnce(
        new Promise<Session>((r) => {
          resolveHydration = r;
        }),
      );
      const hydration = sessions.hydrateVisibleSessions(["sel"]);
      await Promise.resolve();
      sessions.selectSession("sel");

      vi.mocked(api.renameSession).mockResolvedValue(
        makeSession({ id: "sel", display_name: "renamed" }),
      );
      await sessions.renameSession("sel", "renamed");
      expect(sessions.sessions[0]?.display_name).toBe("renamed");
      // The rename response backs the breadcrumb: the row is still index-only
      // and its in-flight hydration was invalidated by the rename.
      expect(sessions.activeSession?.display_name).toBe("renamed");

      // The pre-rename hydration snapshot must not overwrite the row or the
      // breadcrumb cache with the old name.
      resolveHydration(
        makeSession({ id: "sel", project: "proj-a", display_name: "stale" }),
      );
      await hydration;

      expect(sessions.sessions[0]?.display_name).toBe("renamed");
      expect(sessions.activeSession?.display_name).toBe("renamed");
    });

    it("joins an in-flight navigation for the same active session", async () => {
      mockSidebarIndex([]);
      await sessions.load();

      let resolveNavigate!: (s: Session) => void;
      vi.mocked(api.getSession).mockReturnValueOnce(
        new Promise<Session>((r) => {
          resolveNavigate = r;
        }),
      );
      const first = sessions.navigateToSession("sel");
      await Promise.resolve();

      // A reactive caller (App's deep-link effect) re-requests hydration
      // while the fetch is pending; it must join the in-flight read rather
      // than cancel and restart it.
      const second = sessions.navigateToSession("sel");
      resolveNavigate(
        makeSession({ id: "sel", first_message: "detail" }),
      );
      await Promise.all([first, second]);

      expect(api.getSession).toHaveBeenCalledTimes(1);
      expect(sessions.activeSession?.first_message).toBe("detail");
    });

    it("commits the rename response when it lands during the navigation fetch", async () => {
      mockSidebarIndex([]);
      await sessions.load();

      // Navigation fetch for an out-of-list session stays in flight, so the
      // detail cache is still empty when the rename resolves.
      let resolveNavigate!: (s: Session) => void;
      vi.mocked(api.getSession).mockReturnValueOnce(
        new Promise<Session>((r) => {
          resolveNavigate = r;
        }),
      );
      const nav = sessions.navigateToSession("sel");
      await Promise.resolve();

      vi.mocked(api.renameSession).mockResolvedValue(
        makeSession({ id: "sel", display_name: "renamed" }),
      );
      await sessions.renameSession("sel", "renamed");
      expect(sessions.activeSession?.display_name).toBe("renamed");

      // The cancelled pre-rename navigation response must not replace the
      // committed rename snapshot.
      resolveNavigate(
        makeSession({ id: "sel", display_name: "stale" }),
      );
      await nav;

      expect(sessions.activeSession?.display_name).toBe("renamed");
    });

    it("seeds the cache from a hydration already in flight at selection", async () => {
      mockSidebarIndex([makeSkinnyRow({ id: "sel", project: "proj-a" })]);
      await sessions.load();

      // Viewport hydration of the row is already in flight when the user
      // selects it; selection joins that request rather than issuing a new
      // one, so its response must still hydrate the row and seed the cache.
      let resolveHydration!: (s: Session) => void;
      vi.mocked(api.getSession).mockReturnValueOnce(
        new Promise<Session>((r) => {
          resolveHydration = r;
        }),
      );
      const hydration = sessions.hydrateVisibleSessions(["sel"]);
      await Promise.resolve();
      sessions.selectSession("sel");

      resolveHydration(
        makeSession({ id: "sel", project: "proj-a", first_message: "detail" }),
      );
      await hydration;

      expect(api.getSession).toHaveBeenCalledTimes(1);
      expect(sessions.activeSession?.first_message).toBe("detail");

      // The seeded cache must survive a reload that drops the row.
      mockSidebarIndex([makeSkinnyRow({ id: "other", project: "proj-b" })]);
      await sessions.load();
      expect(sessions.activeSession?.first_message).toBe("detail");
    });

    it("keeps a joined hydration fresh across another session's detail read", async () => {
      mockSidebarIndex([makeSkinnyRow({ id: "sel", project: "proj-a" })]);
      await sessions.load();

      // Viewport hydration of the row is in flight...
      let resolveHydration!: (s: Session) => void;
      vi.mocked(api.getSession).mockReturnValueOnce(
        new Promise<Session>((r) => {
          resolveHydration = r;
        }),
      );
      const hydration = sessions.hydrateVisibleSessions(["sel"]);
      await Promise.resolve();

      // ...while a navigation resolves detail for a DIFFERENT session.
      vi.mocked(api.getSession).mockResolvedValueOnce(
        makeSession({ id: "other", project: "proj-b" }),
      );
      await sessions.navigateToSession("other");
      expect(sessions.activeSession?.id).toBe("other");

      // The user then selects the still-hydrating row; selection joins the
      // in-flight request. The other session's read must not mark this
      // session's hydration stale — no newer read for THIS session exists,
      // so the joined response still owns the row and the detail cache.
      sessions.selectSession("sel");
      resolveHydration(
        makeSession({ id: "sel", project: "proj-a", first_message: "detail" }),
      );
      await hydration;

      expect(api.getSession).toHaveBeenCalledTimes(2);
      expect(sessions.activeSession?.first_message).toBe("detail");
      expect(
        sessions.sessions.find((s) => s.id === "sel")?.is_index_only,
      ).toBe(false);
    });

    it("restores the active row when a stale index resolves after a rename", async () => {
      mockSidebarIndex([makeSkinnyRow({ id: "sel", project: "proj-a" })]);
      await sessions.load();
      vi.mocked(api.getSession).mockResolvedValueOnce(
        makeSession({ id: "sel", project: "proj-a", first_message: "detail" }),
      );
      await sessions.hydrateVisibleSessions(["sel"]);
      sessions.selectSession("sel");

      // An index reload is in flight when the user renames the session...
      let resolveIndex!: (v: unknown) => void;
      vi.mocked(api.getSidebarSessionIndex).mockReturnValueOnce(
        new Promise((r) => {
          resolveIndex = r;
        }),
      );
      const reload = sessions.load({ force: true });

      vi.mocked(api.renameSession).mockResolvedValue(
        makeSession({ id: "sel", display_name: "renamed" }),
      );
      await sessions.renameSession("sel", "renamed");
      expect(sessions.activeSession?.display_name).toBe("renamed");

      // ...and resolves afterward with a pre-rename snapshot of the row.
      // The committed rename must keep backing both the sidebar row and the
      // breadcrumb, not survive only inside the detail cache while the
      // stale row is what gets displayed.
      resolveIndex({
        sessions: [makeSkinnyRow({ id: "sel", project: "proj-a" })],
        total: 1,
      });
      await reload;

      expect(sessions.activeSession?.display_name).toBe("renamed");
      expect(
        sessions.sessions.find((s) => s.id === "sel")?.display_name,
      ).toBe("renamed");
    });

    it("keeps newer index fields when a failed refresh preceded the commit", async () => {
      mockSidebarIndex([
        makeSkinnyRow({ id: "sel", project: "proj-a", display_name: "old" }),
      ]);
      await sessions.load();
      vi.mocked(api.getSession).mockResolvedValueOnce(
        makeSession({ id: "sel", project: "proj-a", display_name: "old" }),
      );
      await sessions.hydrateVisibleSessions(["sel"]);
      sessions.selectSession("sel");

      // An index reload is in flight, carrying a newer rename from another
      // machine...
      let resolveIndex!: (v: unknown) => void;
      vi.mocked(api.getSidebarSessionIndex).mockReturnValueOnce(
        new Promise((r) => {
          resolveIndex = r;
        }),
      );
      const reload = sessions.load({ force: true });

      // ...when a watcher refresh starts and fails transiently. The read
      // began but committed nothing, so the cache holds no fresher state
      // than the index — restoring it would revert the newer rename.
      vi.mocked(api.getSession).mockRejectedValueOnce(new Error("network"));
      await sessions.refreshActiveSession();

      resolveIndex({
        sessions: [
          makeSkinnyRow({
            id: "sel",
            project: "proj-a",
            display_name: "server-renamed",
          }),
        ],
        total: 1,
      });
      await reload;

      expect(
        sessions.sessions.find((s) => s.id === "sel")?.display_name,
      ).toBe("server-renamed");
      expect(sessions.activeSession?.display_name).toBe("server-renamed");
    });

    it("does not let a stale index resurrect a 404-deleted active row", async () => {
      mockSidebarIndex([makeSkinnyRow({ id: "gone", project: "proj-a" })]);
      await sessions.load();
      vi.mocked(api.getSession).mockResolvedValueOnce(
        makeSession({ id: "gone", project: "proj-a", first_message: "detail" }),
      );
      await sessions.hydrateVisibleSessions(["gone"]);
      sessions.selectSession("gone");

      // An index reload whose server snapshot predates the deletion is in
      // flight when a refresh 404s and removes the row...
      let resolveIndex!: (v: unknown) => void;
      vi.mocked(api.getSidebarSessionIndex).mockReturnValueOnce(
        new Promise((r) => {
          resolveIndex = r;
        }),
      );
      const reload = sessions.load({ force: true });

      vi.mocked(api.getSession).mockRejectedValueOnce(makeNotFoundError("gone"));
      await sessions.refreshActiveSession();
      expect(sessions.activeSession).toBeUndefined();
      expect(sessions.sessions.some((s) => s.id === "gone")).toBe(false);

      // ...the stale index still lists the session; committing it must honor
      // the deletion instead of resurrecting the row.
      resolveIndex({
        sessions: [makeSkinnyRow({ id: "gone", project: "proj-a" })],
        total: 1,
      });
      await reload;

      expect(sessions.sessions.some((s) => s.id === "gone")).toBe(false);
      expect(sessions.activeSession).toBeUndefined();
      expect(sessions.total).toBe(0);
    });

    it("commits a hydration that resolves after a failed refresh", async () => {
      mockSidebarIndex([makeSkinnyRow({ id: "sel", project: "proj-a" })]);
      await sessions.load();

      // Hydration of the selected index-only row is in flight...
      let resolveHydration!: (s: Session) => void;
      vi.mocked(api.getSession).mockReturnValueOnce(
        new Promise<Session>((r) => {
          resolveHydration = r;
        }),
      );
      const hydration = sessions.hydrateVisibleSessions(["sel"]);
      await Promise.resolve();
      sessions.selectSession("sel");

      // ...when a watcher refresh starts and fails transiently. The read
      // began but committed nothing newer, so the hydration's successful
      // response is still the freshest detail there is — discarding it
      // would leave the row index-only and the breadcrumb blank.
      vi.mocked(api.getSession).mockRejectedValueOnce(new Error("network"));
      await sessions.refreshActiveSession();

      resolveHydration(
        makeSession({ id: "sel", project: "proj-a", first_message: "detail" }),
      );
      await hydration;

      expect(sessions.activeSession?.first_message).toBe("detail");
      expect(
        sessions.sessions.find((s) => s.id === "sel")?.is_index_only,
      ).toBe(false);
    });

    it("keeps hydration-committed detail over a staler index response", async () => {
      mockSidebarIndex([
        makeSkinnyRow({ id: "sel", project: "proj-a", display_name: "old" }),
      ]);
      await sessions.load();

      // An index reload whose server snapshot predates a remote rename is in
      // flight...
      let resolveIndex!: (v: unknown) => void;
      vi.mocked(api.getSidebarSessionIndex).mockReturnValueOnce(
        new Promise((r) => {
          resolveIndex = r;
        }),
      );
      const reload = sessions.load({ force: true });

      // ...while selection hydrates the index-only row, resolving with the
      // renamed session and committing it as the active detail.
      vi.mocked(api.getSession).mockResolvedValueOnce(
        makeSession({ id: "sel", project: "proj-a", display_name: "renamed" }),
      );
      sessions.selectSession("sel");
      await sessions.hydrateVisibleSessions(["sel"]);
      expect(sessions.activeSession?.display_name).toBe("renamed");

      // The staler index must not overwrite the committed hydration in the
      // row or drag the cache back through the row resync.
      resolveIndex({
        sessions: [
          makeSkinnyRow({ id: "sel", project: "proj-a", display_name: "old" }),
        ],
        total: 1,
      });
      await reload;

      expect(sessions.activeSession?.display_name).toBe("renamed");
      expect(
        sessions.sessions.find((s) => s.id === "sel")?.display_name,
      ).toBe("renamed");
    });

    it("resyncs the cache from a newer index despite an uncommitted read", async () => {
      mockSidebarIndex([
        makeSkinnyRow({ id: "sel", project: "proj-a", display_name: "old" }),
      ]);
      await sessions.load();
      vi.mocked(api.getSession).mockResolvedValueOnce(
        makeSession({ id: "sel", project: "proj-a", display_name: "old" }),
      );
      await sessions.hydrateVisibleSessions(["sel"]);
      sessions.selectSession("sel");

      // An index reload carrying a newer remote rename is in flight when a
      // watcher refresh starts and fails without committing anything...
      let resolveIndex!: (v: unknown) => void;
      vi.mocked(api.getSidebarSessionIndex).mockReturnValueOnce(
        new Promise((r) => {
          resolveIndex = r;
        }),
      );
      const reload = sessions.load({ force: true });
      vi.mocked(api.getSession).mockRejectedValueOnce(new Error("network"));
      await sessions.refreshActiveSession();

      resolveIndex({
        sessions: [
          makeSkinnyRow({
            id: "sel",
            project: "proj-a",
            display_name: "server-renamed",
          }),
        ],
        total: 1,
      });
      await reload;

      // The committed index is at least as fresh as any pre-flight commit,
      // so the cache must absorb it even though a read began mid-flight —
      // otherwise a later reload that excludes the row falls back to the
      // stale cached name.
      mockSidebarIndex([makeSkinnyRow({ id: "other", project: "proj-b" })]);
      await sessions.load();
      expect(sessions.activeSession?.display_name).toBe("server-renamed");
    });

    it("lets a refresh join an in-flight navigation for the same session", async () => {
      mockSidebarIndex([]);
      await sessions.load();

      // Deep-link navigation to an off-list session is in flight...
      let resolveNavigate!: (s: Session) => void;
      vi.mocked(api.getSession).mockReturnValueOnce(
        new Promise<Session>((r) => {
          resolveNavigate = r;
        }),
      );
      const nav = sessions.navigateToSession("deep");
      await Promise.resolve();

      // ...when a watcher refresh fires for the same session. The refresh
      // must not cancel the pending navigation (a cancelled navigation plus
      // a transiently failing refresh would leave the cache empty with
      // nothing to retry): it joins the navigation, then chases with a
      // fresh read. Here the chase fails transiently — the navigation's
      // committed result must survive.
      vi.mocked(api.getSession).mockRejectedValueOnce(new Error("network"));
      const refresh = sessions.refreshActiveSession();

      resolveNavigate(
        makeSession({ id: "deep", project: "proj-b", first_message: "detail" }),
      );
      await Promise.all([nav, refresh]);

      expect(api.getSession).toHaveBeenCalledTimes(2);
      expect(sessions.activeSession?.first_message).toBe("detail");
    });

    it("chases a joined navigation with a fresh read for the newer event", async () => {
      mockSidebarIndex([]);
      await sessions.load();

      // Deep-link navigation is in flight with a pre-event snapshot...
      let resolveNavigate!: (s: Session) => void;
      vi.mocked(api.getSession).mockReturnValueOnce(
        new Promise<Session>((r) => {
          resolveNavigate = r;
        }),
      );
      const nav = sessions.navigateToSession("deep");
      await Promise.resolve();

      // ...when an event-driven refresh fires because the session just
      // changed. Joining alone would commit the pre-event snapshot with no
      // guaranteed later refresh; after the navigation settles, the refresh
      // must issue a fresh read that observes the post-event state.
      vi.mocked(api.getSession).mockResolvedValueOnce(
        makeSession({
          id: "deep",
          project: "proj-b",
          display_name: "post-event",
        }),
      );
      const refresh = sessions.refreshActiveSession();
      resolveNavigate(
        makeSession({
          id: "deep",
          project: "proj-b",
          display_name: "pre-event",
        }),
      );
      await Promise.all([nav, refresh]);

      expect(api.getSession).toHaveBeenCalledTimes(2);
      expect(sessions.activeSession?.display_name).toBe("post-event");
    });

    it("clears a deleted session after a refresh joined its navigation", async () => {
      mockSidebarIndex([]);
      await sessions.load();

      let resolveNavigate!: (s: Session) => void;
      vi.mocked(api.getSession).mockReturnValueOnce(
        new Promise<Session>((r) => {
          resolveNavigate = r;
        }),
      );
      const nav = sessions.navigateToSession("gone");
      await Promise.resolve();

      // The session is deleted server-side; the deletion event triggers a
      // refresh while the pre-delete navigation is still in flight. The
      // chase read 404s — the navigation's ghost snapshot must not stand.
      vi.mocked(api.getSession).mockRejectedValueOnce(makeNotFoundError("gone"));
      const refresh = sessions.refreshActiveSession();
      resolveNavigate(
        makeSession({ id: "gone", project: "proj-b", first_message: "ghost" }),
      );
      await Promise.all([nav, refresh]);

      expect(sessions.activeSession).toBeUndefined();
    });

    it("does not reconcile against an index commit that skipped the row", async () => {
      vi.mocked(api.getSidebarSessionIndex).mockResolvedValueOnce({
        sessions: [
          makeSkinnyRow({ id: "sel", project: "proj-a", display_name: "old" }),
        ],
        total: 2,
        next_cursor: "page-2",
      });
      await sessions.load();
      vi.mocked(api.getSession).mockResolvedValueOnce(
        makeSession({ id: "sel", project: "proj-a", display_name: "old" }),
      );
      await sessions.hydrateVisibleSessions(["sel"]);
      sessions.selectSession("sel");

      // A refresh carrying a genuinely newer rename is in flight...
      let resolveRefresh!: (s: Session) => void;
      vi.mocked(api.getSession).mockReturnValueOnce(
        new Promise<Session>((r) => {
          resolveRefresh = r;
        }),
      );
      const refresh = sessions.refreshActiveSession();
      await Promise.resolve();

      // ...when an unrelated loadMore page commits WITHOUT re-listing the
      // active row. That commit says nothing about this row's index fields,
      // so the response must still apply wholesale.
      vi.mocked(api.getSidebarSessionIndex).mockResolvedValueOnce({
        sessions: [makeSkinnyRow({ id: "other", project: "proj-a" })],
        total: 2,
        next_cursor: null,
      });
      await sessions.loadMore();

      resolveRefresh(
        makeSession({
          id: "sel",
          project: "proj-a",
          display_name: "fresh-title",
        }),
      );
      await refresh;

      expect(sessions.activeSession?.display_name).toBe("fresh-title");
      expect(
        sessions.sessions.find((s) => s.id === "sel")?.display_name,
      ).toBe("fresh-title");
    });

    it("does not count a pre-reload hydration as fresher than the reload", async () => {
      mockSidebarIndex([
        makeSkinnyRow({ id: "sel", project: "proj-a", display_name: "old" }),
      ]);
      await sessions.load();

      // Hydration of the selected row is issued BEFORE the index reload...
      let resolveHydration!: (s: Session) => void;
      vi.mocked(api.getSession).mockReturnValueOnce(
        new Promise<Session>((r) => {
          resolveHydration = r;
        }),
      );
      const hydration = sessions.hydrateVisibleSessions(["sel"]);
      await Promise.resolve();
      sessions.selectSession("sel");

      // ...then a reload starts whose server snapshot carries a newer remote
      // rename, and the older hydration resolves while it is in flight.
      let resolveIndex!: (v: unknown) => void;
      vi.mocked(api.getSidebarSessionIndex).mockReturnValueOnce(
        new Promise((r) => {
          resolveIndex = r;
        }),
      );
      const reload = sessions.load({ force: true });
      resolveHydration(
        makeSession({ id: "sel", project: "proj-a", display_name: "old" }),
      );
      await hydration;

      // The hydration request predates the reload, so its snapshot cannot
      // outrank the reload's: the newer index fields must win in the row and
      // be absorbed into the cache, not be replaced by the older hydration.
      resolveIndex({
        sessions: [
          makeSkinnyRow({
            id: "sel",
            project: "proj-a",
            display_name: "server-renamed",
          }),
        ],
        total: 1,
      });
      await reload;

      expect(
        sessions.sessions.find((s) => s.id === "sel")?.display_name,
      ).toBe("server-renamed");
      expect(sessions.activeSession?.display_name).toBe("server-renamed");
    });

    it("keeps mid-flight index commits' fields over an older refresh response", async () => {
      mockSidebarIndex([
        makeSkinnyRow({ id: "sel", project: "proj-a", display_name: "old" }),
      ]);
      await sessions.load();
      vi.mocked(api.getSession).mockResolvedValueOnce(
        makeSession({
          id: "sel",
          project: "proj-a",
          display_name: "old",
          first_message: "detail-1",
        }),
      );
      await sessions.hydrateVisibleSessions(["sel"]);
      sessions.selectSession("sel");

      // A watcher refresh is in flight...
      let resolveRefresh!: (s: Session) => void;
      vi.mocked(api.getSession).mockReturnValueOnce(
        new Promise<Session>((r) => {
          resolveRefresh = r;
        }),
      );
      const refresh = sessions.refreshActiveSession();
      await Promise.resolve();

      // ...when an index reload commits a newer remote rename.
      mockSidebarIndex([
        makeSkinnyRow({
          id: "sel",
          project: "proj-a",
          display_name: "server-renamed",
        }),
      ]);
      await sessions.load({ force: true });
      expect(sessions.activeSession?.display_name).toBe("server-renamed");

      // The refresh response predates that commit. Its detail-owned fields
      // still apply, but the committed index's fields must not be reverted.
      resolveRefresh(
        makeSession({
          id: "sel",
          project: "proj-a",
          display_name: "old",
          first_message: "detail-2",
        }),
      );
      await refresh;

      expect(sessions.activeSession?.display_name).toBe("server-renamed");
      expect(sessions.activeSession?.first_message).toBe("detail-2");
      const row = sessions.sessions.find((s) => s.id === "sel");
      expect(row?.display_name).toBe("server-renamed");
      expect(row?.first_message).toBe("detail-2");
    });

    it("seeds a post-reload cache with the reload's index fields", async () => {
      mockSidebarIndex([
        makeSkinnyRow({ id: "sel", project: "proj-a", display_name: "old" }),
      ]);
      await sessions.load();

      // Hydration of the selected row is in flight when a full reload
      // commits a newer remote rename, re-listing the row as index-only...
      let resolveHydration!: (s: Session) => void;
      vi.mocked(api.getSession).mockReturnValueOnce(
        new Promise<Session>((r) => {
          resolveHydration = r;
        }),
      );
      const hydration = sessions.hydrateVisibleSessions(["sel"]);
      await Promise.resolve();
      sessions.selectSession("sel");
      mockSidebarIndex([
        makeSkinnyRow({
          id: "sel",
          project: "proj-a",
          display_name: "server-renamed",
        }),
      ]);
      await sessions.load({ force: true });

      // ...and the superseded hydration then resolves. It may still seed the
      // empty cache (the breadcrumb needs detail), but with the committed
      // row's newer index-owned fields, not its own pre-reload snapshot.
      resolveHydration(
        makeSession({
          id: "sel",
          project: "proj-a",
          display_name: "old",
          first_message: "detail",
        }),
      );
      await hydration;

      expect(sessions.activeSession?.display_name).toBe("server-renamed");
      expect(sessions.activeSession?.first_message).toBe("detail");
    });

    it("prefers a later-issued index over a detail commit issued earlier", async () => {
      mockSidebarIndex([
        makeSkinnyRow({ id: "sel", project: "proj-a", display_name: "old" }),
      ]);
      await sessions.load();
      vi.mocked(api.getSession).mockResolvedValueOnce(
        makeSession({ id: "sel", project: "proj-a", display_name: "old" }),
      );
      await sessions.hydrateVisibleSessions(["sel"]);
      sessions.selectSession("sel");

      // A refresh is issued first...
      let resolveRefresh!: (s: Session) => void;
      vi.mocked(api.getSession).mockReturnValueOnce(
        new Promise<Session>((r) => {
          resolveRefresh = r;
        }),
      );
      const refresh = sessions.refreshActiveSession();
      await Promise.resolve();

      // ...then a reload is issued, whose server snapshot carries a newer
      // remote rename. The refresh commits while the reload is pending.
      let resolveIndex!: (v: unknown) => void;
      vi.mocked(api.getSidebarSessionIndex).mockReturnValueOnce(
        new Promise((r) => {
          resolveIndex = r;
        }),
      );
      const reload = sessions.load({ force: true });
      resolveRefresh(
        makeSession({
          id: "sel",
          project: "proj-a",
          display_name: "old",
          first_message: "detail-2",
        }),
      );
      await refresh;

      // The refresh's request predates the reload's, so its commit must not
      // outrank the later-issued index snapshot: the newer rename wins and
      // the refresh contributes its detail-owned fields through the row.
      resolveIndex({
        sessions: [
          makeSkinnyRow({
            id: "sel",
            project: "proj-a",
            display_name: "server-renamed",
          }),
        ],
        total: 1,
      });
      await reload;

      expect(sessions.activeSession?.display_name).toBe("server-renamed");
      expect(sessions.activeSession?.first_message).toBe("detail-2");
      const row = sessions.sessions.find((s) => s.id === "sel");
      expect(row?.display_name).toBe("server-renamed");
    });

    it("refetches enriched detail after renaming an unhydrated session", async () => {
      mockSidebarIndex([]);
      await sessions.load();

      // Deep-link navigation failed transiently: the session is active with
      // an empty detail cache.
      vi.mocked(api.getSession).mockRejectedValueOnce(new Error("network"));
      await sessions.navigateToSession("sel");

      // The rename endpoint returns a plain DB-session shape without the
      // derived detail-only fields, so committing it can only be an interim
      // snapshot: a fresh detail GET must follow to fetch the enriched
      // shape the detail UI needs.
      vi.mocked(api.renameSession).mockResolvedValue(
        makeSession({ id: "sel", display_name: "renamed" }),
      );
      vi.mocked(api.getSession).mockResolvedValueOnce(
        makeSession({
          id: "sel",
          project: "proj-b",
          display_name: "renamed",
          first_message: "enriched-detail",
        }),
      );
      await sessions.renameSession("sel", "renamed");
      expect(sessions.activeSession?.display_name).toBe("renamed");

      await new Promise((r) => setTimeout(r, 0));
      expect(sessions.activeSession?.first_message).toBe("enriched-detail");
      expect(sessions.activeSession?.display_name).toBe("renamed");
    });

    it("reconciles against the cache when the re-published row was dropped", async () => {
      mockSidebarIndex([
        makeSkinnyRow({ id: "sel", project: "proj-a", display_name: "old" }),
      ]);
      await sessions.load();
      vi.mocked(api.getSession).mockResolvedValueOnce(
        makeSession({ id: "sel", project: "proj-a", display_name: "old" }),
      );
      await sessions.hydrateVisibleSessions(["sel"]);
      sessions.selectSession("sel");

      // A refresh is in flight...
      let resolveRefresh!: (s: Session) => void;
      vi.mocked(api.getSession).mockReturnValueOnce(
        new Promise<Session>((r) => {
          resolveRefresh = r;
        }),
      );
      const refresh = sessions.refreshActiveSession();
      await Promise.resolve();

      // ...while one reload re-publishes the row with a newer rename (the
      // cache absorbs it), and a second reload then excludes the row.
      mockSidebarIndex([
        makeSkinnyRow({
          id: "sel",
          project: "proj-a",
          display_name: "server-renamed",
        }),
      ]);
      await sessions.load({ force: true });
      mockSidebarIndex([makeSkinnyRow({ id: "other", project: "proj-b" })]);
      await sessions.load({ force: true });
      expect(sessions.activeSession?.display_name).toBe("server-renamed");

      // The stale refresh response resolves with the row now absent: the
      // absorbed index fields live only in the cache, and the response must
      // reconcile against them rather than applying wholesale.
      resolveRefresh(
        makeSession({
          id: "sel",
          project: "proj-a",
          display_name: "old",
          first_message: "detail-2",
        }),
      );
      await refresh;

      expect(sessions.activeSession?.display_name).toBe("server-renamed");
      expect(sessions.activeSession?.first_message).toBe("detail-2");
    });

    it("lets a detail read issued after an index request keep its fields", async () => {
      mockSidebarIndex([
        makeSkinnyRow({ id: "sel", project: "proj-a", display_name: "old" }),
      ]);
      await sessions.load();
      vi.mocked(api.getSession).mockResolvedValueOnce(
        makeSession({ id: "sel", project: "proj-a", display_name: "old" }),
      );
      await sessions.hydrateVisibleSessions(["sel"]);
      sessions.selectSession("sel");

      // An index reload is issued first...
      let resolveIndex!: (v: unknown) => void;
      vi.mocked(api.getSidebarSessionIndex).mockReturnValueOnce(
        new Promise((r) => {
          resolveIndex = r;
        }),
      );
      const reload = sessions.load({ force: true });

      // ...then a refresh is issued, observing a rename the reload's older
      // server snapshot cannot contain. The reload commits first.
      let resolveRefresh!: (s: Session) => void;
      vi.mocked(api.getSession).mockReturnValueOnce(
        new Promise<Session>((r) => {
          resolveRefresh = r;
        }),
      );
      const refresh = sessions.refreshActiveSession();
      await Promise.resolve();
      resolveIndex({
        sessions: [
          makeSkinnyRow({ id: "sel", project: "proj-a", display_name: "old" }),
        ],
        total: 1,
      });
      await reload;

      // The index committed after the refresh began, but its request was
      // issued before the refresh's — its snapshot is older, so the
      // later-issued refresh response must apply wholesale.
      resolveRefresh(
        makeSession({
          id: "sel",
          project: "proj-a",
          display_name: "fresh-rename",
          first_message: "detail-2",
        }),
      );
      await refresh;

      expect(sessions.activeSession?.display_name).toBe("fresh-rename");
      expect(
        sessions.sessions.find((s) => s.id === "sel")?.display_name,
      ).toBe("fresh-rename");
    });

    it("chases again when a second rename cancels the enrichment fetch", async () => {
      mockSidebarIndex([]);
      await sessions.load();
      vi.mocked(api.getSession).mockRejectedValueOnce(new Error("network"));
      await sessions.navigateToSession("sel");

      // First rename on the empty cache commits an interim snapshot and
      // starts an enrichment fetch, which stays in flight...
      vi.mocked(api.renameSession).mockResolvedValueOnce(
        makeSession({ id: "sel", display_name: "one" }),
      );
      let resolveChase!: (s: Session) => void;
      vi.mocked(api.getSession).mockReturnValueOnce(
        new Promise<Session>((r) => {
          resolveChase = r;
        }),
      );
      await sessions.renameSession("sel", "one");

      // ...when a second rename cancels it. The cache is still only the
      // interim rename shape, so a fresh enrichment fetch must be issued —
      // otherwise the detail-only fields never arrive.
      vi.mocked(api.renameSession).mockResolvedValueOnce(
        makeSession({ id: "sel", display_name: "two" }),
      );
      vi.mocked(api.getSession).mockResolvedValueOnce(
        makeSession({
          id: "sel",
          project: "proj-b",
          display_name: "two",
          first_message: "enriched-detail",
        }),
      );
      await sessions.renameSession("sel", "two");
      await new Promise((r) => setTimeout(r, 0));

      // The cancelled first chase resolving late must stay discarded.
      resolveChase(
        makeSession({ id: "sel", display_name: "one", first_message: "stale" }),
      );
      await new Promise((r) => setTimeout(r, 0));

      expect(sessions.activeSession?.display_name).toBe("two");
      expect(sessions.activeSession?.first_message).toBe("enriched-detail");
    });

    it("does not let a superseded hydration invalidate a newer one", async () => {
      mockSidebarIndex([makeSkinnyRow({ id: "sel", project: "proj-a" })]);
      await sessions.load();

      // Hydration H1 is in flight under the old index version...
      let resolveH1!: (s: Session) => void;
      vi.mocked(api.getSession).mockReturnValueOnce(
        new Promise<Session>((r) => {
          resolveH1 = r;
        }),
      );
      const h1 = sessions.hydrateVisibleSessions(["sel"]);
      await Promise.resolve();
      sessions.selectSession("sel");

      // ...a reload commits a new index version, and hydration H2 starts
      // under it.
      mockSidebarIndex([makeSkinnyRow({ id: "sel", project: "proj-a" })]);
      await sessions.load({ force: true });
      let resolveH2!: (s: Session) => void;
      vi.mocked(api.getSession).mockReturnValueOnce(
        new Promise<Session>((r) => {
          resolveH2 = r;
        }),
      );
      const h2 = sessions.hydrateVisibleSessions(["sel"]);
      await Promise.resolve();

      // H1 resolves first and seeds the empty cache (its commit is
      // older-issued). That commit must not invalidate the later-issued H2,
      // whose response is the newer one.
      resolveH1(
        makeSession({ id: "sel", project: "proj-a", first_message: "H1" }),
      );
      await h1;
      resolveH2(
        makeSession({ id: "sel", project: "proj-a", first_message: "H2-newer" }),
      );
      await h2;

      expect(sessions.activeSession?.first_message).toBe("H2-newer");
      expect(
        sessions.sessions.find((s) => s.id === "sel")?.is_index_only,
      ).toBe(false);
    });

    it("does not resurrect a 404-deleted row when a reload fails", async () => {
      mockSidebarIndex([makeSkinnyRow({ id: "gone", project: "proj-a" })]);
      await sessions.load();
      vi.mocked(api.getSession).mockResolvedValueOnce(
        makeSession({ id: "gone", project: "proj-a", first_message: "detail" }),
      );
      await sessions.hydrateVisibleSessions(["gone"]);
      sessions.selectSession("gone");
      expect(sessions.activeSession?.id).toBe("gone");

      // A reload is in flight when a refresh 404s and removes the row...
      let rejectIndex!: (e: unknown) => void;
      vi.mocked(api.getSidebarSessionIndex).mockReturnValueOnce(
        new Promise((_r, rej) => {
          rejectIndex = rej;
        }),
      );
      const reload = sessions.load({ force: true });
      vi.mocked(api.getSession).mockRejectedValueOnce(makeNotFoundError("gone"));
      await sessions.refreshActiveSession();
      expect(sessions.activeSession).toBeUndefined();
      expect(sessions.sessions.some((s) => s.id === "gone")).toBe(false);

      // ...and the reload then fails. Restoring the pre-reload list must
      // reapply the deletion instead of resurrecting the hydrated ghost row.
      rejectIndex(new Error("network"));
      await reload;

      expect(sessions.sessions.some((s) => s.id === "gone")).toBe(false);
      expect(sessions.activeSession).toBeUndefined();
      expect(sessions.total).toBe(0);
    });

    it("orders same-generation detail reads by issue, not resolution", async () => {
      mockSidebarIndex([makeSkinnyRow({ id: "sel", project: "proj-a" })]);
      await sessions.load();
      vi.mocked(api.getSession).mockResolvedValueOnce(
        makeSession({ id: "sel", project: "proj-a", first_message: "A" }),
      );
      await sessions.hydrateVisibleSessions(["sel"]);
      sessions.selectSession("sel");

      // A refresh is issued first and stays in flight...
      let resolveRefresh!: (s: Session) => void;
      vi.mocked(api.getSession).mockReturnValueOnce(
        new Promise<Session>((r) => {
          resolveRefresh = r;
        }),
      );
      const refresh = sessions.refreshActiveSession();
      await Promise.resolve();

      // ...then a messages event invalidates hydration caches and a NEW
      // hydration is issued for the same row, in the same index generation.
      (sessions as any).invalidateHydratedSessionDetails();
      let resolveHydration!: (s: Session) => void;
      vi.mocked(api.getSession).mockReturnValueOnce(
        new Promise<Session>((r) => {
          resolveHydration = r;
        }),
      );
      const hydration = sessions.hydrateVisibleSessions(["sel"]);
      await Promise.resolve();

      // The earlier-issued refresh resolves first with older data. Its
      // commit must not invalidate the later-issued hydration, whose
      // response is the newer one.
      resolveRefresh(
        makeSession({ id: "sel", project: "proj-a", first_message: "R-stale" }),
      );
      await refresh;
      resolveHydration(
        makeSession({ id: "sel", project: "proj-a", first_message: "H-newer" }),
      );
      await hydration;

      expect(sessions.activeSession?.first_message).toBe("H-newer");
    });

    it("does not resurrect an explicitly deleted row when a reload fails", async () => {
      mockSidebarIndex([makeSkinnyRow({ id: "gone", project: "proj-a" })]);
      await sessions.load();
      vi.mocked(api.getSession).mockResolvedValueOnce(
        makeSession({ id: "gone", project: "proj-a", first_message: "detail" }),
      );
      await sessions.hydrateVisibleSessions(["gone"]);

      // A reload is in flight when the user explicitly deletes the row...
      let rejectIndex!: (e: unknown) => void;
      vi.mocked(api.getSidebarSessionIndex).mockReturnValueOnce(
        new Promise((_r, rej) => {
          rejectIndex = rej;
        }),
      );
      const reload = sessions.load({ force: true });
      vi.mocked(api.deleteSession).mockResolvedValueOnce({});
      await sessions.deleteSession("gone");
      expect(sessions.sessions.some((s) => s.id === "gone")).toBe(false);

      // ...and the reload then fails. The restored pre-reload snapshot must
      // not bring the deleted row back as a sidebar ghost.
      rejectIndex(new Error("network"));
      await reload;

      expect(sessions.sessions.some((s) => s.id === "gone")).toBe(false);
      expect(sessions.total).toBe(0);
    });

    it("does not reinsert a deleted row from a stale successful reload", async () => {
      mockSidebarIndex([makeSkinnyRow({ id: "gone", project: "proj-a" })]);
      await sessions.load();
      vi.mocked(api.getSession).mockResolvedValueOnce(
        makeSession({ id: "gone", project: "proj-a", first_message: "detail" }),
      );
      await sessions.hydrateVisibleSessions(["gone"]);

      // A reload whose server snapshot predates the deletion is in flight
      // when the user deletes the (non-active) row...
      let resolveIndex!: (v: unknown) => void;
      vi.mocked(api.getSidebarSessionIndex).mockReturnValueOnce(
        new Promise((r) => {
          resolveIndex = r;
        }),
      );
      const reload = sessions.load({ force: true });
      vi.mocked(api.deleteSession).mockResolvedValueOnce({});
      await sessions.deleteSession("gone");
      expect(sessions.sessions.some((s) => s.id === "gone")).toBe(false);

      // ...and the reload then resolves successfully, still listing the row.
      // Publishing must honor the deletion tombstone for every row, not just
      // the active session's.
      resolveIndex({
        sessions: [makeSkinnyRow({ id: "gone", project: "proj-a" })],
        total: 1,
      });
      await reload;

      expect(sessions.sessions.some((s) => s.id === "gone")).toBe(false);
      expect(sessions.total).toBe(0);
    });

    it("does not let an older refresh overwrite a later hydration commit", async () => {
      mockSidebarIndex([makeSkinnyRow({ id: "sel", project: "proj-a" })]);
      await sessions.load();
      vi.mocked(api.getSession).mockResolvedValueOnce(
        makeSession({ id: "sel", project: "proj-a", first_message: "A" }),
      );
      await sessions.hydrateVisibleSessions(["sel"]);
      sessions.selectSession("sel");

      // A refresh is issued and stays in flight...
      let resolveRefresh!: (s: Session) => void;
      vi.mocked(api.getSession).mockReturnValueOnce(
        new Promise<Session>((r) => {
          resolveRefresh = r;
        }),
      );
      const refresh = sessions.refreshActiveSession();
      await Promise.resolve();

      // ...then a messages event invalidates hydration caches and a
      // later-issued hydration resolves and commits newer detail first.
      (sessions as any).invalidateHydratedSessionDetails();
      vi.mocked(api.getSession).mockResolvedValueOnce(
        makeSession({ id: "sel", project: "proj-a", first_message: "H-newer" }),
      );
      await sessions.hydrateVisibleSessions(["sel"]);
      expect(sessions.activeSession?.first_message).toBe("H-newer");

      // The earlier-issued refresh resolving late must not overwrite the
      // later-issued hydration's commit with its older snapshot.
      resolveRefresh(
        makeSession({ id: "sel", project: "proj-a", first_message: "R-stale" }),
      );
      await refresh;

      expect(sessions.activeSession?.first_message).toBe("H-newer");
    });

    it("does not decrement the root total when a child row is removed", async () => {
      vi.mocked(api.getSidebarSessionIndex).mockResolvedValue({
        sessions: [
          makeSkinnyRow({ id: "root1", project: "proj-a" }),
          makeSkinnyRow({
            id: "c1",
            project: "proj-a",
            parent_session_id: "root1",
          }),
        ],
        total: 1,
        next_cursor: null,
      });
      await sessions.load();
      expect(sessions.total).toBe(1);

      // A reload whose snapshot predates the deletion is in flight when the
      // child is explicitly deleted; the paginated total counts root groups,
      // so neither the delete nor the tombstoned publish may decrement it.
      let resolveIndex!: (v: unknown) => void;
      vi.mocked(api.getSidebarSessionIndex).mockReturnValueOnce(
        new Promise((r) => {
          resolveIndex = r;
        }),
      );
      const reload = sessions.load({ force: true });
      vi.mocked(api.deleteSession).mockResolvedValueOnce({});
      await sessions.deleteSession("c1");
      expect(sessions.total).toBe(1);

      resolveIndex({
        sessions: [
          makeSkinnyRow({ id: "root1", project: "proj-a" }),
          makeSkinnyRow({
            id: "c1",
            project: "proj-a",
            parent_session_id: "root1",
          }),
        ],
        total: 1,
      });
      await reload;

      expect(sessions.sessions.some((s) => s.id === "c1")).toBe(false);
      expect(sessions.total).toBe(1);
    });

    it("does not append a deleted row from a stale loadMore page", async () => {
      vi.mocked(api.getSidebarSessionIndex).mockResolvedValueOnce({
        sessions: [makeSkinnyRow({ id: "keep", project: "proj-a" })],
        total: 2,
        next_cursor: "page-2",
      });
      await sessions.load();

      // Page 2 is in flight when the not-yet-loaded session it carries is
      // explicitly deleted...
      let resolvePage!: (v: unknown) => void;
      vi.mocked(api.getSidebarSessionIndex).mockReturnValueOnce(
        new Promise((r) => {
          resolvePage = r;
        }),
      );
      const more = sessions.loadMore();
      vi.mocked(api.deleteSession).mockResolvedValueOnce({});
      await sessions.deleteSession("gone");

      // ...and the stale page then resolves still listing it. The appended
      // page must honor the tombstone and adjust the root total.
      resolvePage({
        sessions: [makeSkinnyRow({ id: "gone", project: "proj-a" })],
        total: 2,
        next_cursor: null,
      });
      await more;

      expect(sessions.sessions.some((s) => s.id === "gone")).toBe(false);
      expect(sessions.sessions.some((s) => s.id === "keep")).toBe(true);
      expect(sessions.total).toBe(1);
    });

    it("keeps a rename on a row after the user selects another session", async () => {
      mockSidebarIndex([
        makeSkinnyRow({ id: "a", project: "proj-a", display_name: "old" }),
        makeSkinnyRow({ id: "b", project: "proj-a" }),
      ]);
      await sessions.load();
      vi.mocked(api.getSession).mockResolvedValueOnce(
        makeSession({ id: "a", project: "proj-a", display_name: "old" }),
      );
      await sessions.hydrateVisibleSessions(["a"]);
      sessions.selectSession("a");

      // A re-hydration of A is in flight when A is renamed and the user
      // then selects B...
      (sessions as any).invalidateHydratedSessionDetails();
      let resolveHydration!: (s: Session) => void;
      vi.mocked(api.getSession).mockReturnValueOnce(
        new Promise<Session>((r) => {
          resolveHydration = r;
        }),
      );
      const hydration = sessions.hydrateVisibleSessions(["a"]);
      await Promise.resolve();
      vi.mocked(api.renameSession).mockResolvedValueOnce(
        makeSession({ id: "a", display_name: "renamed" }),
      );
      await sessions.renameSession("a", "renamed");
      vi.mocked(api.getSession).mockResolvedValueOnce(
        makeSession({ id: "b", project: "proj-a", first_message: "b-detail" }),
      );
      sessions.selectSession("b");

      // ...and A's stale pre-rename response resolves. A is no longer the
      // active session, but its committed rename must still win.
      resolveHydration(
        makeSession({ id: "a", project: "proj-a", display_name: "old" }),
      );
      await hydration;

      expect(
        sessions.sessions.find((s) => s.id === "a")?.display_name,
      ).toBe("renamed");
    });

    it("removes batch-deleted rows even when the forced reload fails", async () => {
      mockSidebarIndex([makeSkinnyRow({ id: "gone", project: "proj-a" })]);
      await sessions.load();
      expect(sessions.total).toBe(1);

      vi.mocked(api.batchDeleteSessions).mockResolvedValueOnce({});
      vi.mocked(api.getSidebarSessionIndex).mockRejectedValueOnce(
        new Error("network"),
      );
      await sessions.batchDeleteSessions(["gone"]);

      expect(sessions.sessions.some((s) => s.id === "gone")).toBe(false);
      expect(sessions.total).toBe(0);
    });

    it("keeps local root deletions when a later page's stale total arrives", async () => {
      vi.mocked(api.getSidebarSessionIndex).mockResolvedValueOnce({
        sessions: [makeSkinnyRow({ id: "root1", project: "proj-a" })],
        total: 2,
        next_cursor: "page-2",
      });
      await sessions.load();
      expect(sessions.total).toBe(2);

      // A root loaded on page 1 is deleted...
      vi.mocked(api.deleteSession).mockResolvedValueOnce({});
      await sessions.deleteSession("root1");
      expect(sessions.total).toBe(1);

      // ...then page 2 resolves carrying the cursor's page-1-era total.
      // The stale snapshot total must not undo the local deletion.
      vi.mocked(api.getSidebarSessionIndex).mockResolvedValueOnce({
        sessions: [makeSkinnyRow({ id: "root2", project: "proj-a" })],
        total: 2,
        next_cursor: null,
      });
      await sessions.loadMore();

      expect(sessions.total).toBe(1);
    });

    it("counts a promoted orphan row as a root when it is deleted", async () => {
      vi.mocked(api.getSidebarSessionIndex).mockResolvedValue({
        sessions: [
          makeSkinnyRow({
            id: "orphan",
            project: "proj-a",
            parent_session_id: "missing-root",
          }),
        ],
        total: 1,
        next_cursor: null,
      });
      await sessions.load();
      expect(sessions.total).toBe(1);

      // The orphan's parent is not in the list, so the sidebar promotes it
      // to its own root group; deleting it must decrement the root total.
      vi.mocked(api.deleteSession).mockResolvedValueOnce({});
      await sessions.deleteSession("orphan");

      expect(sessions.total).toBe(0);
    });

    it("publishes a committed rename for a row with no prior sidebar row", async () => {
      mockSidebarIndex([]);
      await sessions.load();

      // A cache-only session (opened from search, off the filtered list) is
      // active...
      vi.mocked(api.getSession).mockResolvedValueOnce(
        makeSession({ id: "a", project: "proj-a", display_name: "old" }),
      );
      await sessions.navigateToSession("a");

      // ...an index reload is issued, then the session is renamed and the
      // user navigates to another session, discarding the cache.
      let resolveIndex!: (v: unknown) => void;
      vi.mocked(api.getSidebarSessionIndex).mockReturnValueOnce(
        new Promise((r) => {
          resolveIndex = r;
        }),
      );
      const reload = sessions.load({ force: true });
      vi.mocked(api.renameSession).mockResolvedValueOnce(
        makeSession({ id: "a", display_name: "renamed" }),
      );
      await sessions.renameSession("a", "renamed");
      vi.mocked(api.getSession).mockResolvedValueOnce(
        makeSession({ id: "b", project: "proj-b", first_message: "b-detail" }),
      );
      await sessions.navigateToSession("b");

      // The pre-rename index resolves, listing the renamed session for the
      // first time. There is no prior sidebar row to preserve — the
      // committed rename snapshot must still outrank the stale index name.
      resolveIndex({
        sessions: [
          makeSkinnyRow({ id: "a", project: "proj-a", display_name: "old" }),
        ],
        total: 1,
      });
      await reload;

      expect(
        sessions.sessions.find((s) => s.id === "a")?.display_name,
      ).toBe("renamed");
    });

    it("does not double-subtract a deleted root re-listed by a stale page", async () => {
      vi.mocked(api.getSidebarSessionIndex).mockResolvedValueOnce({
        sessions: [makeSkinnyRow({ id: "root1", project: "proj-a" })],
        total: 2,
        next_cursor: "page-2",
      });
      await sessions.load();

      // Page 2 is in flight when an already-loaded root is deleted; the
      // delete path decrements the total once.
      let resolvePage!: (v: unknown) => void;
      vi.mocked(api.getSidebarSessionIndex).mockReturnValueOnce(
        new Promise((r) => {
          resolvePage = r;
        }),
      );
      const more = sessions.loadMore();
      vi.mocked(api.deleteSession).mockResolvedValueOnce({});
      await sessions.deleteSession("root1");
      expect(sessions.total).toBe(1);

      // The stale page re-lists the deleted root alongside a new one.
      // Dropping the tombstoned duplicate must not decrement again.
      resolvePage({
        sessions: [
          makeSkinnyRow({ id: "root1", project: "proj-a" }),
          makeSkinnyRow({ id: "root2", project: "proj-a" }),
        ],
        total: 2,
        next_cursor: null,
      });
      await more;

      expect(sessions.sessions.some((s) => s.id === "root1")).toBe(false);
      expect(sessions.total).toBe(1);
    });

    it("ignores a stale 404 after a later read confirmed the session", async () => {
      mockSidebarIndex([makeSkinnyRow({ id: "sel", project: "proj-a" })]);
      await sessions.load();
      vi.mocked(api.getSession).mockResolvedValueOnce(
        makeSession({ id: "sel", project: "proj-a", first_message: "A" }),
      );
      await sessions.hydrateVisibleSessions(["sel"]);
      sessions.selectSession("sel");

      // A refresh is in flight...
      let rejectRefresh!: (e: unknown) => void;
      vi.mocked(api.getSession).mockReturnValueOnce(
        new Promise<Session>((_r, rej) => {
          rejectRefresh = rej;
        }),
      );
      const refresh = sessions.refreshActiveSession();
      await Promise.resolve();

      // ...while a later-issued hydration confirms the session exists.
      (sessions as any).invalidateHydratedSessionDetails();
      vi.mocked(api.getSession).mockResolvedValueOnce(
        makeSession({ id: "sel", project: "proj-a", first_message: "H" }),
      );
      await sessions.hydrateVisibleSessions(["sel"]);
      expect(sessions.activeSession?.first_message).toBe("H");

      // The earlier refresh then 404s. Newer evidence outranks the stale
      // response: the session must not be tombstoned.
      rejectRefresh(makeNotFoundError("sel"));
      await refresh;

      expect(sessions.activeSession?.first_message).toBe("H");
      expect(sessions.sessions.some((s) => s.id === "sel")).toBe(true);
      expect(sessions.total).toBe(1);
    });

    it("classifies dropped page rows against all loaded rows", async () => {
      vi.mocked(api.getSidebarSessionIndex).mockResolvedValueOnce({
        sessions: [makeSkinnyRow({ id: "root1", project: "proj-a" })],
        total: 2,
        next_cursor: "page-2",
      });
      await sessions.load();

      // A never-loaded child of an already-loaded root is deleted while
      // page 2 is in flight...
      let resolvePage!: (v: unknown) => void;
      vi.mocked(api.getSidebarSessionIndex).mockReturnValueOnce(
        new Promise((r) => {
          resolvePage = r;
        }),
      );
      const more = sessions.loadMore();
      vi.mocked(api.deleteSession).mockResolvedValueOnce({});
      await sessions.deleteSession("child1");
      expect(sessions.total).toBe(2);

      // ...and the stale page re-lists it alongside another root. The child
      // belongs to root1's group (loaded on page 1): dropping it must not
      // count as a dropped root group.
      resolvePage({
        sessions: [
          makeSkinnyRow({
            id: "child1",
            project: "proj-a",
            parent_session_id: "root1",
          }),
          makeSkinnyRow({ id: "root2", project: "proj-a" }),
        ],
        total: 2,
        next_cursor: null,
      });
      await more;

      expect(sessions.sessions.some((s) => s.id === "child1")).toBe(false);
      expect(sessions.sessions.some((s) => s.id === "root2")).toBe(true);
      expect(sessions.total).toBe(2);
    });

    it("reconciles a stale response from the stamped row after exclusion", async () => {
      mockSidebarIndex([
        makeSkinnyRow({ id: "sel", project: "proj-a", display_name: "old" }),
      ]);
      await sessions.load();

      // Selection hydration of the index-only row is pinned in flight...
      let resolveHydration!: (s: Session) => void;
      vi.mocked(api.getSession).mockReturnValueOnce(
        new Promise<Session>((r) => {
          resolveHydration = r;
        }),
      );
      sessions.selectSession("sel");
      await Promise.resolve();

      // ...a reload publishes a newer remote rename onto the still
      // index-only row (the empty cache cannot absorb it), then a second
      // reload excludes the row entirely.
      mockSidebarIndex([
        makeSkinnyRow({
          id: "sel",
          project: "proj-a",
          display_name: "server-renamed",
        }),
      ]);
      await sessions.load({ force: true });
      mockSidebarIndex([makeSkinnyRow({ id: "other", project: "proj-b" })]);
      await sessions.load({ force: true });

      // The stale pre-rename response resolves with row and cache both
      // absent: the newer index fields survive only in the stamped publish
      // snapshot, which must win the reconciliation.
      resolveHydration(
        makeSession({
          id: "sel",
          project: "proj-a",
          display_name: "old",
          first_message: "detail",
        }),
      );
      await Promise.resolve();
      await Promise.resolve();

      expect(sessions.activeSession?.display_name).toBe("server-renamed");
      expect(sessions.activeSession?.first_message).toBe("detail");
    });

    it("removes local descendants when their root is deleted", async () => {
      vi.mocked(api.getSidebarSessionIndex).mockResolvedValue({
        sessions: [
          makeSkinnyRow({ id: "root1", project: "proj-a" }),
          makeSkinnyRow({
            id: "child1",
            project: "proj-a",
            parent_session_id: "root1",
          }),
        ],
        total: 1,
        next_cursor: null,
      });
      await sessions.load();
      expect(sessions.total).toBe(1);

      // A stale reload is in flight when the root is deleted. The backend
      // removes the whole subtree from sidebar queries; local state must
      // match: the descendant goes with its root, counted once.
      let resolveIndex!: (v: unknown) => void;
      vi.mocked(api.getSidebarSessionIndex).mockReturnValueOnce(
        new Promise((r) => {
          resolveIndex = r;
        }),
      );
      const reload = sessions.load({ force: true });
      vi.mocked(api.deleteSession).mockResolvedValueOnce({});
      await sessions.deleteSession("root1");

      expect(sessions.sessions.some((s) => s.id === "child1")).toBe(false);
      expect(sessions.total).toBe(0);

      // The stale reload still lists the subtree; publishing must drop the
      // descendant along with its tombstoned root.
      resolveIndex({
        sessions: [
          makeSkinnyRow({ id: "root1", project: "proj-a" }),
          makeSkinnyRow({
            id: "child1",
            project: "proj-a",
            parent_session_id: "root1",
          }),
        ],
        total: 1,
      });
      await reload;

      expect(sessions.sessions.length).toBe(0);
      expect(sessions.total).toBe(0);
    });

    it("protects a non-active hydration from an older index request", async () => {
      mockSidebarIndex([
        makeSkinnyRow({ id: "a", project: "proj-a", display_name: "old" }),
        makeSkinnyRow({ id: "b", project: "proj-a" }),
      ]);
      await sessions.load();
      vi.mocked(api.getSession).mockResolvedValueOnce(
        makeSession({ id: "b", project: "proj-a", first_message: "b-detail" }),
      );
      sessions.selectSession("b");
      await Promise.resolve();

      // An index reload is issued...
      let resolveIndex!: (v: unknown) => void;
      vi.mocked(api.getSidebarSessionIndex).mockReturnValueOnce(
        new Promise((r) => {
          resolveIndex = r;
        }),
      );
      const reload = sessions.load({ force: true });

      // ...then the NON-active row hydrates with a newer remote rename and
      // the user selects it.
      vi.mocked(api.getSession).mockResolvedValueOnce(
        makeSession({
          id: "a",
          project: "proj-a",
          display_name: "hydra-renamed",
        }),
      );
      await sessions.hydrateVisibleSessions(["a"]);
      sessions.selectSession("a");
      expect(sessions.activeSession?.display_name).toBe("hydra-renamed");

      // The older index resolves with the pre-rename name. The hydration
      // was issued after this index request, so its fields must win even
      // though the row was not active when it committed.
      resolveIndex({
        sessions: [
          makeSkinnyRow({ id: "a", project: "proj-a", display_name: "old" }),
          makeSkinnyRow({ id: "b", project: "proj-a" }),
        ],
        total: 2,
      });
      await reload;

      expect(sessions.activeSession?.display_name).toBe("hydra-renamed");
      expect(
        sessions.sessions.find((s) => s.id === "a")?.display_name,
      ).toBe("hydra-renamed");
    });

    it("removes known descendants when a refresh 404s for their root", async () => {
      vi.mocked(api.getSidebarSessionIndex).mockResolvedValue({
        sessions: [
          makeSkinnyRow({ id: "root1", project: "proj-a" }),
          makeSkinnyRow({
            id: "child1",
            project: "proj-a",
            parent_session_id: "root1",
          }),
        ],
        total: 1,
        next_cursor: null,
      });
      await sessions.load();
      vi.mocked(api.getSession).mockResolvedValueOnce(
        makeSession({ id: "root1", project: "proj-a", first_message: "d" }),
      );
      sessions.selectSession("root1");
      await Promise.resolve();

      // The root is deleted elsewhere; a refresh 404s. The backend hides
      // the whole subtree, so the loaded descendant must go with its root.
      vi.mocked(api.getSession).mockRejectedValueOnce(
        makeNotFoundError("root1"),
      );
      await sessions.refreshActiveSession();

      expect(sessions.activeSession).toBeUndefined();
      expect(sessions.sessions.length).toBe(0);
      expect(sessions.total).toBe(0);
    });

    it("clears an active descendant when its ancestor is deleted", async () => {
      vi.mocked(api.getSidebarSessionIndex).mockResolvedValue({
        sessions: [
          makeSkinnyRow({ id: "root1", project: "proj-a" }),
          makeSkinnyRow({
            id: "child1",
            project: "proj-a",
            parent_session_id: "root1",
          }),
        ],
        total: 1,
        next_cursor: null,
      });
      await sessions.load();
      vi.mocked(api.getSession).mockResolvedValueOnce(
        makeSession({ id: "child1", project: "proj-a", first_message: "c" }),
      );
      sessions.selectSession("child1");
      await sessions.hydrateVisibleSessions(["child1"]);
      expect(sessions.activeSession?.id).toBe("child1");

      // Deleting the parent removes the whole subtree; the active child is
      // part of it, so the selection must clear rather than leaving the
      // cached detail backing a ghost active session.
      vi.mocked(api.deleteSession).mockResolvedValueOnce({});
      await sessions.deleteSession("root1");

      expect(sessions.activeSession).toBeUndefined();
      expect(sessions.activeSessionId).toBeNull();
    });

    it("prunes reconciliation state once no in-flight request needs it", async () => {
      const makePage = (prefix: string) =>
        Array.from({ length: 40 }, (_v, i) =>
          makeSkinnyRow({ id: `${prefix}-${i}`, project: "proj-a" }));

      // Three successive filter-change reloads publish disjoint row sets,
      // with no requests left in flight between them.
      for (const prefix of ["one", "two", "three"]) {
        mockSidebarIndex(makePage(prefix));
        await sessions.load({ force: true });
      }

      // Reconciliation bookkeeping must stay bounded by live rows plus
      // in-flight work, not accumulate every row ever published.
      const stamps = (sessions as any).indexCommitByRow as Map<
        string,
        unknown
      >;
      expect(stamps.size).toBeLessThanOrEqual(40 + 1);
    });

    it("clears a cache-only active descendant when its ancestor is deleted", async () => {
      mockSidebarIndex([makeSkinnyRow({ id: "root1", project: "proj-a" })]);
      await sessions.load();

      // A child session opened from a deep link exists only in the detail
      // cache; its parent is the listed root.
      vi.mocked(api.getSession).mockResolvedValueOnce(
        makeSession({
          id: "child1",
          project: "proj-a",
          parent_session_id: "root1",
          first_message: "c",
        }),
      );
      await sessions.navigateToSession("child1");
      expect(sessions.activeSession?.id).toBe("child1");

      // Deleting the root removes the subtree backend-side; the cache-only
      // active child belongs to it and must be cleared, not left as a ghost.
      vi.mocked(api.deleteSession).mockResolvedValueOnce({});
      await sessions.deleteSession("root1");

      expect(sessions.activeSession).toBeUndefined();
      expect(sessions.activeSessionId).toBeNull();
    });

    it("lets a later-issued hydration supersede a transient refresh 404", async () => {
      mockSidebarIndex([makeSkinnyRow({ id: "sel", project: "proj-a" })]);
      await sessions.load();
      vi.mocked(api.getSession).mockResolvedValueOnce(
        makeSession({ id: "sel", project: "proj-a", first_message: "A" }),
      );
      await sessions.hydrateVisibleSessions(["sel"]);
      sessions.selectSession("sel");

      // A refresh is in flight...
      let rejectRefresh!: (e: unknown) => void;
      vi.mocked(api.getSession).mockReturnValueOnce(
        new Promise<Session>((_r, rej) => {
          rejectRefresh = rej;
        }),
      );
      const refresh = sessions.refreshActiveSession();
      await Promise.resolve();

      // ...then a later-issued hydration starts, and the refresh 404s
      // first (a transient server-side gap, e.g. mid-resync).
      (sessions as any).invalidateHydratedSessionDetails();
      let resolveHydration!: (s: Session) => void;
      vi.mocked(api.getSession).mockReturnValueOnce(
        new Promise<Session>((r) => {
          resolveHydration = r;
        }),
      );
      const hydration = sessions.hydrateVisibleSessions(["sel"]);
      await Promise.resolve();
      rejectRefresh(makeNotFoundError("sel"));
      await refresh;

      // The hydration was issued after the 404's request: its successful
      // response is newer evidence and must supersede the read-derived
      // tombstone instead of being discarded.
      resolveHydration(
        makeSession({ id: "sel", project: "proj-a", first_message: "H" }),
      );
      await hydration;

      expect(sessions.activeSession?.first_message).toBe("H");
    });

    it("restores the sidebar row when a hydration revives a 404d session", async () => {
      mockSidebarIndex([makeSkinnyRow({ id: "sel", project: "proj-a" })]);
      await sessions.load();
      vi.mocked(api.getSession).mockResolvedValueOnce(
        makeSession({ id: "sel", project: "proj-a", first_message: "A" }),
      );
      await sessions.hydrateVisibleSessions(["sel"]);
      sessions.selectSession("sel");

      let rejectRefresh!: (e: unknown) => void;
      vi.mocked(api.getSession).mockReturnValueOnce(
        new Promise<Session>((_r, rej) => {
          rejectRefresh = rej;
        }),
      );
      const refresh = sessions.refreshActiveSession();
      await Promise.resolve();

      (sessions as any).invalidateHydratedSessionDetails();
      let resolveHydration!: (s: Session) => void;
      vi.mocked(api.getSession).mockReturnValueOnce(
        new Promise<Session>((r) => {
          resolveHydration = r;
        }),
      );
      const hydration = sessions.hydrateVisibleSessions(["sel"]);
      await Promise.resolve();
      rejectRefresh(makeNotFoundError("sel"));
      await refresh;
      expect(sessions.sessions.some((s) => s.id === "sel")).toBe(false);
      expect(sessions.total).toBe(0);

      // The superseding hydration proves the session exists: besides
      // recovering the breadcrumb, the sidebar row and root total the
      // transient 404 removed must come back.
      resolveHydration(
        makeSession({ id: "sel", project: "proj-a", first_message: "H" }),
      );
      await hydration;

      expect(sessions.activeSession?.first_message).toBe("H");
      expect(sessions.sessions.some((s) => s.id === "sel")).toBe(true);
      expect(sessions.total).toBe(1);
    });

    it("honors a 404 tombstone when a later-issued reload fails", async () => {
      mockSidebarIndex([makeSkinnyRow({ id: "gone", project: "proj-a" })]);
      await sessions.load();
      vi.mocked(api.getSession).mockResolvedValueOnce(
        makeSession({ id: "gone", project: "proj-a", first_message: "d" }),
      );
      await sessions.hydrateVisibleSessions(["gone"]);
      sessions.selectSession("gone");

      // A refresh is issued, then a reload; the refresh 404s while the
      // reload is in flight, and the reload then fails. A failed load
      // carries no newer server state, so its restore must still honor the
      // tombstone even though the load was issued after the 404's request.
      let rejectRefresh!: (e: unknown) => void;
      vi.mocked(api.getSession).mockReturnValueOnce(
        new Promise<Session>((_r, rej) => {
          rejectRefresh = rej;
        }),
      );
      const refresh = sessions.refreshActiveSession();
      await Promise.resolve();
      let rejectIndex!: (e: unknown) => void;
      vi.mocked(api.getSidebarSessionIndex).mockReturnValueOnce(
        new Promise((_r, rej) => {
          rejectIndex = rej;
        }),
      );
      const reload = sessions.load({ force: true });
      rejectRefresh(makeNotFoundError("gone"));
      await refresh;
      expect(sessions.sessions.some((s) => s.id === "gone")).toBe(false);

      rejectIndex(new Error("network"));
      await reload;

      expect(sessions.sessions.some((s) => s.id === "gone")).toBe(false);
      expect(sessions.total).toBe(0);
    });

    it("keeps descendants revivable after their root's transient 404", async () => {
      vi.mocked(api.getSidebarSessionIndex).mockResolvedValue({
        sessions: [
          makeSkinnyRow({ id: "root1", project: "proj-a" }),
          makeSkinnyRow({
            id: "child1",
            project: "proj-a",
            parent_session_id: "root1",
          }),
        ],
        total: 1,
        next_cursor: null,
      });
      await sessions.load();
      vi.mocked(api.getSession).mockResolvedValueOnce(
        makeSession({ id: "root1", project: "proj-a", first_message: "d" }),
      );
      sessions.selectSession("root1");
      await sessions.hydrateVisibleSessions(["root1"]);

      // A refresh is issued, then a reload whose server snapshot lists the
      // subtree; the refresh 404s transiently while the reload is in
      // flight.
      let rejectRefresh!: (e: unknown) => void;
      vi.mocked(api.getSession).mockReturnValueOnce(
        new Promise<Session>((_r, rej) => {
          rejectRefresh = rej;
        }),
      );
      const refresh = sessions.refreshActiveSession();
      await Promise.resolve();
      let resolveIndex!: (v: unknown) => void;
      vi.mocked(api.getSidebarSessionIndex).mockReturnValueOnce(
        new Promise((r) => {
          resolveIndex = r;
        }),
      );
      const reload = sessions.load({ force: true });
      rejectRefresh(makeNotFoundError("root1"));
      await refresh;
      expect(sessions.sessions.length).toBe(0);

      // The later-issued reload confirms the subtree exists. Both the root
      // AND its descendant must supersede the read-derived tombstones —
      // descendants inherit the ancestor's finite tick, not Infinity.
      resolveIndex({
        sessions: [
          makeSkinnyRow({ id: "root1", project: "proj-a" }),
          makeSkinnyRow({
            id: "child1",
            project: "proj-a",
            parent_session_id: "root1",
          }),
        ],
        total: 1,
      });
      await reload;

      expect(sessions.sessions.some((s) => s.id === "root1")).toBe(true);
      expect(sessions.sessions.some((s) => s.id === "child1")).toBe(true);
      expect(sessions.total).toBe(1);
    });

    it("does not inject a cache-only session into the sidebar on revival", async () => {
      mockSidebarIndex([]);
      await sessions.load();
      vi.mocked(api.getSession).mockResolvedValueOnce(
        makeSession({ id: "c", project: "proj-b", first_message: "detail" }),
      );
      await sessions.navigateToSession("c");
      expect(sessions.sessions.length).toBe(0);

      // The cache-only session (excluded by the current filter) transiently
      // 404s...
      vi.mocked(api.getSession).mockRejectedValueOnce(makeNotFoundError("c"));
      await sessions.refreshActiveSession();
      expect(sessions.activeSession).toBeUndefined();

      // ...and a later refresh confirms it exists. The detail cache must
      // recover, but the tombstone never removed a sidebar row: nothing may
      // be appended to the filtered list and the root total must not grow.
      vi.mocked(api.getSession).mockResolvedValueOnce(
        makeSession({ id: "c", project: "proj-b", first_message: "again" }),
      );
      await sessions.refreshActiveSession();

      expect(sessions.activeSession?.first_message).toBe("again");
      expect(sessions.sessions.length).toBe(0);
      expect(sessions.total).toBe(0);
    });

    it("updates a populated cache from a hydration after an excluding reload", async () => {
      mockSidebarIndex([makeSkinnyRow({ id: "sel", project: "proj-a" })]);
      await sessions.load();
      vi.mocked(api.getSession).mockResolvedValueOnce(
        makeSession({ id: "sel", project: "proj-a", first_message: "v1" }),
      );
      await sessions.hydrateVisibleSessions(["sel"]);
      sessions.selectSession("sel");
      expect(sessions.activeSession?.first_message).toBe("v1");

      // A messages event invalidates hydration caches and a fresh hydration
      // is issued...
      (sessions as any).invalidateHydratedSessionDetails();
      let resolveHydration!: (s: Session) => void;
      vi.mocked(api.getSession).mockReturnValueOnce(
        new Promise<Session>((r) => {
          resolveHydration = r;
        }),
      );
      const hydration = sessions.hydrateVisibleSessions(["sel"]);
      await Promise.resolve();

      // ...then a reload excludes the active row from the sidebar.
      mockSidebarIndex([makeSkinnyRow({ id: "other", project: "proj-b" })]);
      await sessions.load({ force: true });

      // The hydration resolves with newer detail. The populated (older)
      // cache is now the only thing backing the off-list session, and no
      // retry will come — the fresh response must replace it.
      resolveHydration(
        makeSession({ id: "sel", project: "proj-a", first_message: "v2" }),
      );
      await hydration;

      expect(sessions.activeSession?.first_message).toBe("v2");
    });

    it("counts a revived subtree once when the child revives first", async () => {
      vi.mocked(api.getSidebarSessionIndex).mockResolvedValue({
        sessions: [
          makeSkinnyRow({ id: "root1", project: "proj-a" }),
          makeSkinnyRow({
            id: "child1",
            project: "proj-a",
            parent_session_id: "root1",
          }),
        ],
        total: 1,
        next_cursor: null,
      });
      await sessions.load();
      vi.mocked(api.getSession).mockResolvedValueOnce(
        makeSession({ id: "root1", project: "proj-a", first_message: "d" }),
      );
      sessions.selectSession("root1");
      await sessions.hydrateVisibleSessions(["root1"]);

      // The root transiently 404s, removing the subtree.
      vi.mocked(api.getSession).mockRejectedValueOnce(
        makeNotFoundError("root1"),
      );
      await sessions.refreshActiveSession();
      expect(sessions.sessions.length).toBe(0);
      expect(sessions.total).toBe(0);

      // The child revives first (as a promoted orphan root, +1)...
      (sessions as any).invalidateHydratedSessionDetails();
      vi.mocked(api.getSession).mockResolvedValueOnce(
        makeSession({
          id: "child1",
          project: "proj-a",
          parent_session_id: "root1",
          first_message: "c",
        }),
      );
      await sessions.hydrateVisibleSessions(["child1"]);
      expect(sessions.total).toBe(1);

      // ...then the root revives, demoting the child back to nested. The
      // group must be counted once, not twice.
      vi.mocked(api.getSession).mockResolvedValueOnce(
        makeSession({ id: "root1", project: "proj-a", first_message: "d2" }),
      );
      await sessions.refreshActiveSession();

      expect(sessions.sessions.some((s) => s.id === "root1")).toBe(true);
      expect(sessions.sessions.some((s) => s.id === "child1")).toBe(true);
      expect(sessions.total).toBe(1);
    });

    it("revives the row after consecutive 404s for the same session", async () => {
      mockSidebarIndex([makeSkinnyRow({ id: "sel", project: "proj-a" })]);
      await sessions.load();
      vi.mocked(api.getSession).mockResolvedValueOnce(
        makeSession({ id: "sel", project: "proj-a", first_message: "A" }),
      );
      await sessions.hydrateVisibleSessions(["sel"]);
      sessions.selectSession("sel");

      // Two consecutive transient 404s: the first removes the row, the
      // second finds it already gone. The record of the removal must
      // survive both.
      vi.mocked(api.getSession).mockRejectedValueOnce(makeNotFoundError("sel"));
      await sessions.refreshActiveSession();
      vi.mocked(api.getSession).mockRejectedValueOnce(makeNotFoundError("sel"));
      await sessions.refreshActiveSession();
      expect(sessions.sessions.length).toBe(0);

      // A later successful refresh revives the session: the sidebar row and
      // total must come back, not just the detail cache.
      vi.mocked(api.getSession).mockResolvedValueOnce(
        makeSession({ id: "sel", project: "proj-a", first_message: "B" }),
      );
      await sessions.refreshActiveSession();

      expect(sessions.activeSession?.first_message).toBe("B");
      expect(sessions.sessions.some((s) => s.id === "sel")).toBe(true);
      expect(sessions.total).toBe(1);
    });

    it("ignores a stale hydration resolving after a newer refresh", async () => {
      mockSidebarIndex([makeSkinnyRow({ id: "sel", project: "proj-a" })]);
      await sessions.load();

      // Hydration of the visible row stays in flight...
      let resolveHydration!: (s: Session) => void;
      vi.mocked(api.getSession).mockReturnValueOnce(
        new Promise<Session>((r) => {
          resolveHydration = r;
        }),
      );
      const hydration = sessions.hydrateVisibleSessions(["sel"]);
      await Promise.resolve();

      // ...the user selects that row, and a watcher-driven refresh resolves
      // first with a fresher snapshot for both the row and the cache.
      sessions.selectSession("sel");
      vi.mocked(api.getSession).mockResolvedValueOnce(
        makeSession({ id: "sel", project: "proj-a", first_message: "NEWER" }),
      );
      await sessions.refreshActiveSession();
      expect(sessions.activeSession?.first_message).toBe("NEWER");

      // The older hydration passes the sidebar version/epoch checks (no
      // reload happened) but must not clobber the newer active detail.
      resolveHydration(
        makeSession({ id: "sel", project: "proj-a", first_message: "STALE" }),
      );
      await hydration;

      expect(sessions.activeSession?.first_message).toBe("NEWER");
    });

    it("clears the cached detail when the active session was deleted", async () => {
      mockSidebarIndex([]);
      await sessions.load();
      vi.mocked(api.getSession).mockResolvedValueOnce(
        makeSession({ id: "gone", project: "proj-b", first_message: "detail" }),
      );
      await sessions.navigateToSession("gone");
      expect(sessions.activeSession?.id).toBe("gone");

      // The session is deleted elsewhere; a watcher-driven refresh 404s. The
      // breadcrumb must empty rather than keep showing the ghost session.
      vi.mocked(api.getSession).mockRejectedValueOnce(makeNotFoundError("gone"));
      await sessions.refreshActiveSession();

      expect(sessions.activeSession).toBeUndefined();
    });

    it("does not let an in-flight hydration resurrect a deleted session", async () => {
      mockSidebarIndex([makeSkinnyRow({ id: "gone", project: "proj-a" })]);
      await sessions.load();

      let resolveHydration!: (s: Session) => void;
      vi.mocked(api.getSession).mockReturnValueOnce(
        new Promise<Session>((r) => {
          resolveHydration = r;
        }),
      );
      const hydration = sessions.hydrateVisibleSessions(["gone"]);
      await Promise.resolve();
      sessions.selectSession("gone");

      // The session is deleted elsewhere; a refresh 404s and clears the
      // cache while the hydration response is still in flight.
      vi.mocked(api.getSession).mockRejectedValueOnce(makeNotFoundError("gone"));
      await sessions.refreshActiveSession();
      expect(sessions.activeSession).toBeUndefined();

      // The pre-deletion hydration snapshot must not refill the cache.
      resolveHydration(
        makeSession({ id: "gone", project: "proj-a", first_message: "ghost" }),
      );
      await hydration;

      expect(sessions.activeSession).toBeUndefined();
    });

    it("drops a hydrated sidebar row when a refresh 404s for the active session", async () => {
      mockSidebarIndex([makeSkinnyRow({ id: "gone", project: "proj-a" })]);
      await sessions.load();
      vi.mocked(api.getSession).mockResolvedValueOnce(
        makeSession({ id: "gone", project: "proj-a", first_message: "detail" }),
      );
      await sessions.hydrateVisibleSessions(["gone"]);
      sessions.selectSession("gone");
      expect(sessions.activeSession?.first_message).toBe("detail");
      expect(sessions.total).toBe(1);

      // The session is deleted elsewhere; a refresh 404s. Clearing only the
      // detail cache is not enough — the hydrated sidebar row would keep
      // backing activeSession as a ghost of the deleted session.
      vi.mocked(api.getSession).mockRejectedValueOnce(makeNotFoundError("gone"));
      await sessions.refreshActiveSession();

      expect(sessions.activeSession).toBeUndefined();
      expect(sessions.sessions.some((s) => s.id === "gone")).toBe(false);
      expect(sessions.total).toBe(0);
    });

    it("keeps the cached detail when a refresh fails transiently", async () => {
      mockSidebarIndex([]);
      await sessions.load();
      vi.mocked(api.getSession).mockResolvedValueOnce(
        makeSession({ id: "flaky", project: "proj-b", first_message: "detail" }),
      );
      await sessions.navigateToSession("flaky");

      // A non-404 failure (server restart, network) must not blank the
      // breadcrumb; only a definitive not-found may clear the cache.
      vi.mocked(api.getSession).mockRejectedValueOnce(new Error("network down"));
      await sessions.refreshActiveSession();

      expect(sessions.activeSession?.first_message).toBe("detail");
    });

    it("fills the cached detail with fetched signal detail for an off-list session", async () => {
      mockSidebarIndex([]);
      await sessions.load();
      vi.mocked(api.getSession).mockResolvedValueOnce(
        makeSession({ id: "offlist", first_message: "detail" }),
      );
      await sessions.navigateToSession("offlist");
      expect(sessions.activeSession?.health_score_basis).toBeUndefined();

      vi.mocked(api.getSession).mockResolvedValueOnce(
        makeSession({
          id: "offlist",
          first_message: "detail",
          health_score_basis: ["clean_termination"],
          health_penalties: { tool_failures: 5 },
        }),
      );
      await sessions.fetchSignalDetail("offlist");

      expect(sessions.activeSession?.health_score_basis).toEqual([
        "clean_termination",
      ]);
      expect(sessions.activeSession?.health_penalties).toEqual({
        tool_failures: 5,
      });
    });
  });

  describe("route cancellation", () => {
    it("aborts pagination and treats cancellation as normal completion", async () => {
      const signals: AbortSignal[] = [];
      vi.mocked(callGenerated).mockImplementation(
        (request: () => Promise<unknown>, signal?: AbortSignal) => {
          if (signal) signals.push(signal);
          return rejectGeneratedRequestOnAbort(request, signal);
        },
      );
      vi.mocked(api.getSidebarSessionIndex).mockReturnValue(
        new Promise(() => {}),
      );
      sessions.nextCursor = "next";

      const load = sessions.loadMore();
      await Promise.resolve();
      sessions.cancelRouteReads();

      expect(signals).toHaveLength(1);
      expect(signals[0]?.aborted).toBe(true);
      await expect(load).resolves.toBeUndefined();
    });

    it("keeps a replacement signal-detail request registered", async () => {
      vi.mocked(callGenerated).mockImplementation(
        rejectGeneratedRequestOnAbort,
      );
      vi.mocked(api.getSession).mockReturnValue(
        new Promise(() => {}),
      );

      const obsolete = sessions.fetchSignalDetail("detail");
      await Promise.resolve();
      sessions.cancelRouteReads();
      void sessions.fetchSignalDetail("detail");
      await obsolete;

      void sessions.fetchSignalDetail("detail");
      await Promise.resolve();

      expect(api.getSession).toHaveBeenCalledTimes(2);
    });
  });
});

function makeSession(
  overrides: Partial<Session> & { id: string },
): Session {
  return {
    project: "proj",
    machine: "local",
    agent: "claude",
    first_message: null,
    started_at: null,
    ended_at: null,
    message_count: 1,
    user_message_count: 1,
    total_output_tokens: 0,
    peak_context_tokens: 0,
    is_automated: false,
    created_at: "2024-01-01T00:00:00Z",
    ...overrides,
  };
}

describe("buildSessionGroups", () => {
  it("sidebar index rows preserve status-tier order", () => {
    const isoAgo = (ms: number) => new Date(Date.now() - ms).toISOString();
    const rows = [
      makeSkinnyRow({
        id: "unclean",
        ended_at: isoAgo(2 * 60 * 60 * 1000),
        termination_status: "tool_call_pending",
      }),
      makeSkinnyRow({
        id: "quiet",
        ended_at: isoAgo(30 * 60 * 1000),
        termination_status: "clean",
      }),
      makeSkinnyRow({
        id: "stale",
        ended_at: isoAgo(30 * 60 * 1000),
        termination_status: "truncated",
      }),
      makeSkinnyRow({
        id: "idle",
        ended_at: isoAgo(5 * 60 * 1000),
      }),
      makeSkinnyRow({
        id: "waiting",
        ended_at: isoAgo(5 * 60 * 1000),
        termination_status: "awaiting_user",
      }),
      makeSkinnyRow({
        id: "working",
        ended_at: isoAgo(30 * 1000),
      }),
    ];

    const groups = buildSessionGroups(rows as any);

    expect(groups.map((g) => g.primarySessionId)).toEqual([
      "working",
      "waiting",
      "idle",
      "stale",
      "quiet",
      "unclean",
    ]);
  });

  it("sidebar index freshness rollup uses every skinny group member", () => {
    const rows = [
      makeSkinnyRow({
        id: "rolled-root",
        ended_at: "2024-01-01T00:00:00Z",
      }),
      makeSkinnyRow({
        id: "plain",
        ended_at: "2024-01-02T00:00:00Z",
      }),
      makeSkinnyRow({
        id: "rolled-child",
        parent_session_id: "rolled-root",
        relationship_type: "subagent",
        ended_at: "2024-01-03T00:00:00Z",
      }),
    ];

    const groups = buildSessionGroups(rows as any);

    expect(groups.map((g) => g.key)).toEqual(["rolled-root", "plain"]);
  });

  it("sidebar index orphan teammate adoption uses is_teammate", () => {
    const rows = [
      makeSkinnyRow({
        id: "main",
        project: "proj",
        started_at: "2024-01-01T00:00:00Z",
      }),
      makeSkinnyRow({
        id: "teammate",
        project: "proj",
        is_teammate: true,
        started_at: "2024-01-01T00:01:00Z",
      }),
    ];

    const groups = buildSessionGroups(rows as any);

    expect(groups).toHaveLength(1);
    expect(groups[0]!.sessions.map((s) => s.id)).toContain("teammate");
  });

  it("groups two-session chain", () => {
    const sessions = [
      makeSession({
        id: "s1",
        project: "proj",
        started_at: "2024-01-01T00:00:00Z",
        ended_at: "2024-01-01T01:00:00Z",
      }),
      makeSession({
        id: "s2",
        project: "proj",
        parent_session_id: "s1",
        started_at: "2024-01-01T02:00:00Z",
        ended_at: "2024-01-01T03:00:00Z",
      }),
    ];

    const groups = buildSessionGroups(sessions);
    expect(groups).toHaveLength(1);
    expect(groups[0]!.sessions).toHaveLength(2);
  });

  it("keeps sessions without parent ungrouped", () => {
    const sessions = [
      makeSession({ id: "s1", project: "proj" }),
      makeSession({ id: "s2", project: "proj" }),
    ];

    const groups = buildSessionGroups(sessions);
    expect(groups).toHaveLength(2);
    expect(groups[0]!.sessions).toHaveLength(1);
    expect(groups[1]!.sessions).toHaveLength(1);
  });

  it("missing middle link creates separate groups", () => {
    // Chain: s1 -> s2 -> s3, but s2 is not in the loaded set
    const sessions = [
      makeSession({
        id: "s1",
        project: "proj",
        started_at: "2024-01-01T00:00:00Z",
      }),
      makeSession({
        id: "s3",
        project: "proj",
        parent_session_id: "s2",
        started_at: "2024-01-03T00:00:00Z",
      }),
    ];

    const groups = buildSessionGroups(sessions);
    // s3 can't walk to s1 because s2 is missing
    expect(groups).toHaveLength(2);
  });

  it("three-session chain groups correctly", () => {
    const sessions = [
      makeSession({
        id: "s1",
        project: "proj",
        started_at: "2024-01-01T00:00:00Z",
        ended_at: "2024-01-01T01:00:00Z",
      }),
      makeSession({
        id: "s2",
        project: "proj",
        parent_session_id: "s1",
        started_at: "2024-01-01T02:00:00Z",
        ended_at: "2024-01-01T03:00:00Z",
      }),
      makeSession({
        id: "s3",
        project: "proj",
        parent_session_id: "s2",
        started_at: "2024-01-01T04:00:00Z",
        ended_at: "2024-01-01T05:00:00Z",
      }),
    ];

    const groups = buildSessionGroups(sessions);
    expect(groups).toHaveLength(1);
    expect(groups[0]!.sessions).toHaveLength(3);
    // Sorted by started_at asc
    expect(groups[0]!.sessions[0]!.id).toBe("s1");
    expect(groups[0]!.sessions[1]!.id).toBe("s2");
    expect(groups[0]!.sessions[2]!.id).toBe("s3");
  });

  it("computes correct group metadata", () => {
    const sessions = [
      makeSession({
        id: "s1",
        project: "proj",
        message_count: 10,
        first_message: "first session msg",
        started_at: "2024-01-01T00:00:00Z",
        ended_at: "2024-01-01T01:00:00Z",
      }),
      makeSession({
        id: "s2",
        project: "proj",
        parent_session_id: "s1",
        message_count: 5,
        first_message: "second session msg",
        started_at: "2024-01-01T02:00:00Z",
        ended_at: "2024-01-01T04:00:00Z",
      }),
    ];

    const groups = buildSessionGroups(sessions);
    expect(groups).toHaveLength(1);

    const g = groups[0]!;
    expect(g.totalMessages).toBe(15);
    expect(g.startedAt).toBe("2024-01-01T00:00:00Z");
    expect(g.endedAt).toBe("2024-01-01T04:00:00Z");
    expect(g.firstMessage).toBe("first session msg");
    expect(g.primarySessionId).toBe("s2");
  });

  it("selects primary by ended_at not started_at", () => {
    const sessions = [
      makeSession({
        id: "s1",
        project: "proj",
        started_at: "2024-01-01T00:00:00Z",
        ended_at: "2024-01-01T05:00:00Z",
      }),
      makeSession({
        id: "s2",
        project: "proj",
        parent_session_id: "s1",
        started_at: "2024-01-02T00:00:00Z",
        ended_at: "2024-01-02T01:00:00Z",
      }),
    ];

    const groups = buildSessionGroups(sessions);
    expect(groups[0]!.primarySessionId).toBe("s2");
  });

  it("selects primary by ended_at when started_at later", () => {
    const sessions = [
      makeSession({
        id: "s1",
        project: "proj",
        started_at: "2024-01-02T00:00:00Z",
        ended_at: "2024-01-02T01:00:00Z",
      }),
      makeSession({
        id: "s2",
        project: "proj",
        parent_session_id: "s1",
        started_at: "2024-01-01T00:00:00Z",
        ended_at: "2024-01-03T00:00:00Z",
      }),
    ];

    const groups = buildSessionGroups(sessions);
    expect(groups[0]!.primarySessionId).toBe("s2");
  });

  it("null ended_at falls back to started_at for primary", () => {
    const sessions = [
      makeSession({
        id: "s1",
        project: "proj",
        started_at: "2024-01-01T00:00:00Z",
        ended_at: "2024-01-01T05:00:00Z",
      }),
      makeSession({
        id: "s2",
        project: "proj",
        parent_session_id: "s1",
        started_at: "2024-01-02T00:00:00Z",
        ended_at: null,
      }),
    ];

    const groups = buildSessionGroups(sessions);
    // s2 recencyKey = started_at "2024-01-02" > s1 ended_at "2024-01-01T05"
    expect(groups[0]!.primarySessionId).toBe("s2");
  });

  it("completed session wins over in-progress when ended_at later", () => {
    const sessions = [
      makeSession({
        id: "s1",
        project: "proj",
        started_at: "2024-01-01T00:00:00Z",
        ended_at: "2024-01-03T00:00:00Z",
      }),
      makeSession({
        id: "s2",
        project: "proj",
        parent_session_id: "s1",
        started_at: "2024-01-02T00:00:00Z",
        ended_at: null,
      }),
    ];

    const groups = buildSessionGroups(sessions);
    // s1 recencyKey = ended_at "2024-01-03" > s2 started_at "2024-01-02"
    expect(groups[0]!.primarySessionId).toBe("s1");
  });

  it("selects primary by created_at when both null", () => {
    const sessions = [
      makeSession({
        id: "s1",
        project: "proj",
        started_at: null,
        ended_at: null,
        created_at: "2024-01-01T00:00:00Z",
      }),
      makeSession({
        id: "s2",
        project: "proj",
        parent_session_id: "s1",
        started_at: null,
        ended_at: null,
        created_at: "2024-01-02T00:00:00Z",
      }),
    ];

    const groups = buildSessionGroups(sessions);
    expect(groups[0]!.primarySessionId).toBe("s2");
  });

  it("equal ended_at picks earliest started_at deterministically", () => {
    const sessions = [
      makeSession({
        id: "s1",
        project: "proj",
        started_at: "2024-01-02T00:00:00Z",
        ended_at: "2024-01-03T00:00:00Z",
      }),
      makeSession({
        id: "s2",
        project: "proj",
        parent_session_id: "s1",
        started_at: "2024-01-01T00:00:00Z",
        ended_at: "2024-01-03T00:00:00Z",
      }),
    ];

    const groups = buildSessionGroups(sessions);
    // Both have same ended_at, so recencyKey ties;
    // after started_at asc sort, s2 is first -> kept as primary
    expect(groups[0]!.primarySessionId).toBe("s2");
  });

  it("sorts sessions within group by startedAt asc", () => {
    const sessions = [
      makeSession({
        id: "s2",
        project: "proj",
        parent_session_id: "s1",
        started_at: "2024-01-02T00:00:00Z",
      }),
      makeSession({
        id: "s1",
        project: "proj",
        started_at: "2024-01-01T00:00:00Z",
      }),
    ];

    const groups = buildSessionGroups(sessions);
    expect(groups[0]!.sessions[0]!.id).toBe("s1");
    expect(groups[0]!.sessions[1]!.id).toBe("s2");
  });

  it("handles empty sessions array", () => {
    const groups = buildSessionGroups([]);
    expect(groups).toHaveLength(0);
  });

  it("mixes grouped and ungrouped sessions", () => {
    const sessions = [
      makeSession({
        id: "s1",
        project: "proj",
        ended_at: "2024-01-03T00:00:00Z",
      }),
      makeSession({
        id: "s2",
        project: "proj",
        ended_at: "2024-01-02T00:00:00Z",
      }),
      makeSession({
        id: "s3",
        project: "proj",
        parent_session_id: "s1",
        ended_at: "2024-01-01T00:00:00Z",
      }),
    ];

    const groups = buildSessionGroups(sessions);
    expect(groups).toHaveLength(2);
    expect(groups[0]!.sessions).toHaveLength(2);
    expect(groups[1]!.sessions).toHaveLength(1);
  });

  it("aged awaiting_user falls through to quiet", () => {
    // The waiting bubble is meant for freshly-blocked sessions.
    // Once an awaiting_user session ages past the 10m active
    // window it must fall through to quiet, not stay on the
    // bubble forever.
    const old = new Date(Date.now() - 30 * 60 * 1000).toISOString();
    const fresh = new Date(Date.now() - 30 * 1000).toISOString();
    expect(
      getSessionStatus({
        ended_at: old,
        termination_status: "awaiting_user",
      } as Session),
    ).toBe("quiet");
    expect(
      getSessionStatus({
        ended_at: fresh,
        termination_status: "awaiting_user",
      } as Session),
    ).toBe("waiting");
  });

  it("status-tier sort puts unclean below quiet", () => {
    // All four sessions are >1h idle so the time-based tier is
    // either quiet (clean/null) or unclean (flagged). Within a
    // tier, freshness wins. Order should be:
    //   quiet-newer → quiet-older → unclean-newer → unclean-older
    // i.e. unclean sinks to the very bottom regardless of
    // recency relative to quiet rows.
    const sessions = [
      makeSession({
        id: "unclean-newer",
        project: "u-new",
        ended_at: "2024-01-04T00:00:00Z",
        termination_status: "tool_call_pending",
      }),
      makeSession({
        id: "quiet-older",
        project: "q-old",
        ended_at: "2024-01-01T00:00:00Z",
      }),
      makeSession({
        id: "unclean-older",
        project: "u-old",
        ended_at: "2024-01-02T00:00:00Z",
        termination_status: "truncated",
      }),
      makeSession({
        id: "quiet-newer",
        project: "q-new",
        ended_at: "2024-01-03T00:00:00Z",
      }),
    ];

    const groups = buildSessionGroups(sessions);
    expect(groups.map((g) => g.sessions[0]!.id)).toEqual([
      "quiet-newer",
      "quiet-older",
      "unclean-newer",
      "unclean-older",
    ]);
  });
});

describe("SessionsStore live refresh", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    storageData.clear();
    mockSidebarPage();
    mockSidebarIndex();
    mockGetProjects();
  });

  it("messages events invalidate hydrated detail without reloading the index", async () => {
    const { events } = await import("./events.svelte.js");
    let registered: ((e: { scope: string }) => void) | null = null;
    const spy = vi
      .spyOn(events, "subscribe")
      .mockImplementation((fn) => {
        registered = fn as (e: { scope: string }) => void;
        return () => {};
      });

    mockSidebarIndex([makeSkinnyRow({ id: "row" })]);
    const sessions = createSessionsStore();
    const detach = sessions.attachSidebar();
    await sessions.load();
    vi.mocked(api.getSession)
      .mockResolvedValueOnce(makeSession({
        id: "row",
        first_message: "first hydrate",
      }))
      .mockResolvedValueOnce(makeSession({
        id: "row",
        first_message: "second hydrate",
      }));
    await sessions.hydrateVisibleSessions(["row"]);

    expect(api.getSidebarSessionIndex).toHaveBeenCalledTimes(1);
    expect(api.getSession).toHaveBeenCalledTimes(1);
    expect(spy).toHaveBeenCalled();
    expect(registered).not.toBeNull();

    registered!({ scope: "messages" });
    await sessions.hydrateVisibleSessions(["row"]);

    expect(api.getSidebarSessionIndex).toHaveBeenCalledTimes(1);
    expect(api.getSession).toHaveBeenCalledTimes(2);
    expect(sessions.sessions[0]!.first_message).toBe("second hydrate");

    detach();
    spy.mockRestore();
  });

  it("messages events refresh active child sessions", async () => {
    const { events } = await import("./events.svelte.js");
    let registered: ((e: { scope: string }) => void) | null = null;
    const spy = vi
      .spyOn(events, "subscribe")
      .mockImplementation((fn) => {
        registered = fn as (e: { scope: string }) => void;
        return () => {};
      });

    vi.mocked(SessionsService.getApiV1SessionsIdChildren)
      .mockResolvedValueOnce([
        makeSession({
          id: "child",
          parent_session_id: "root",
          transcript_revision: "child-rev-1",
        }),
      ] as Session[])
      .mockResolvedValueOnce([
        makeSession({
          id: "child",
          parent_session_id: "root",
          transcript_revision: "child-rev-2",
        }),
      ] as Session[]);

    const sessions = createSessionsStore();
    const detach = sessions.attachSidebar();
    sessions.activeSessionId = "root";
    await sessions.loadChildSessions("root");
    expect(sessions.childSessions.get("child")?.transcript_revision).toBe("child-rev-1");
    expect(sessions.activeSessionUsageVersion).toBe(0);

    registered!({ scope: "messages" });

    await vi.waitFor(() => {
      expect(sessions.childSessions.get("child")?.transcript_revision).toBe("child-rev-2");
    });
    expect(SessionsService.getApiV1SessionsIdChildren).toHaveBeenCalledTimes(2);
    expect(sessions.activeSessionUsageVersion).toBe(1);

    detach();
    spy.mockRestore();
  });

  it("sessions and sync events coalesce to one debounced index reload", async () => {
    vi.useFakeTimers();
    const { events } = await import("./events.svelte.js");
    let registered: ((e: { scope: string }) => void) | null = null;
    const spy = vi
      .spyOn(events, "subscribe")
      .mockImplementation((fn) => {
        registered = fn as (e: { scope: string }) => void;
        return () => {};
      });

    const sessions = createSessionsStore();
    const detach = sessions.attachSidebar();
    await sessions.load();
    expect(api.getSidebarSessionIndex).toHaveBeenCalledTimes(1);

    registered!({ scope: "sessions" });
    registered!({ scope: "sync" });
    await vi.advanceTimersByTimeAsync(299);
    expect(api.getSidebarSessionIndex).toHaveBeenCalledTimes(1);

    await vi.advanceTimersByTimeAsync(1);
    expect(api.getSidebarSessionIndex).toHaveBeenCalledTimes(2);

    detach();
    spy.mockRestore();
    vi.useRealTimers();
  });

  it("refetches on the 5-minute safety-net interval", async () => {
    vi.useFakeTimers();
    const { events } = await import("./events.svelte.js");
    const spy = vi
      .spyOn(events, "subscribe")
      .mockReturnValue(() => {});

    const sessions = createSessionsStore();
    const detach = sessions.attachSidebar();
    await sessions.load();
    expect(api.getSidebarSessionIndex).toHaveBeenCalledTimes(1);

    // Advance exactly one interval — avoids the runAllTimers infinite
    // loop that recurring setInterval plus a promise-resolving
    // listSessions mock would produce.
    await vi.advanceTimersByTimeAsync(5 * 60 * 1000);
    expect(api.getSidebarSessionIndex).toHaveBeenCalledTimes(2);

    detach();
    spy.mockRestore();
    vi.useRealTimers();
  });

  it("refreshes active child sessions on the 5-minute safety-net interval", async () => {
    vi.useFakeTimers();
    const { events } = await import("./events.svelte.js");
    const spy = vi
      .spyOn(events, "subscribe")
      .mockReturnValue(() => {});

    vi.mocked(SessionsService.getApiV1SessionsIdChildren)
      .mockResolvedValueOnce([
        makeSession({
          id: "child",
          parent_session_id: "root",
          total_output_tokens: 1,
        }),
      ] as Session[])
      .mockResolvedValueOnce([
        makeSession({
          id: "child",
          parent_session_id: "root",
          total_output_tokens: 9,
        }),
      ] as Session[]);

    const sessions = createSessionsStore();
    const detach = sessions.attachSidebar();
    sessions.activeSessionId = "root";
    await sessions.load();
    await sessions.loadChildSessions("root");
    expect(sessions.childSessions.get("child")?.total_output_tokens).toBe(1);
    expect(sessions.activeSessionUsageVersion).toBe(0);

    await vi.advanceTimersByTimeAsync(5 * 60 * 1000);

    await vi.waitFor(() => {
      expect(sessions.childSessions.get("child")?.total_output_tokens).toBe(9);
    });
    expect(SessionsService.getApiV1SessionsIdChildren).toHaveBeenCalledTimes(2);
    expect(sessions.activeSessionUsageVersion).toBe(1);

    detach();
    spy.mockRestore();
    vi.useRealTimers();
  });

  it("dispose() unsubscribes and clears the safety-net timer", async () => {
    vi.useFakeTimers();
    const { events } = await import("./events.svelte.js");
    const unsub = vi.fn();
    const spy = vi
      .spyOn(events, "subscribe")
      .mockReturnValue(unsub);

    const sessions = createSessionsStore();
    sessions.attachSidebar();
    await sessions.load();
    expect(api.getSidebarSessionIndex).toHaveBeenCalledTimes(1);

    sessions.dispose();
    expect(unsub).toHaveBeenCalledTimes(1);

    // After dispose the interval is cleared, so advancing well past
    // 5 minutes triggers no further fetches.
    await vi.advanceTimersByTimeAsync(10 * 60 * 1000);
    expect(api.getSidebarSessionIndex).toHaveBeenCalledTimes(1);

    spy.mockRestore();
    vi.useRealTimers();
  });
});
