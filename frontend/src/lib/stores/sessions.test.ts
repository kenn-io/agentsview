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
import { starred } from "./starred.svelte.js";
import { yokedDates } from "./yokedDates.svelte.js";
import type { Filters } from "./sessions.svelte.js";
import type { Session } from "../api/types.js";
import { callGenerated } from "../api/runtime.js";

const api = vi.hoisted(() => ({
  listSessions: vi.fn(),
  getSidebarSessionIndex: vi.fn(),
  getSession: vi.fn(),
  getProjects: vi.fn(),
  getAgents: vi.fn(),
  getMachines: vi.fn(),
  deleteSession: vi.fn(),
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

vi.mock("../api/runtime.js", () => ({
  configureGeneratedClient: vi.fn(),
  callGenerated: vi.fn((request: () => Promise<unknown>) => request()),
}));

vi.mock("../api/generated/index", () => ({
  SessionsService: {
    getApiV1Sessions: vi.fn((params) => api.listSessions(params)),
    getApiV1SessionsSidebarIndex: vi.fn((params) =>
      api.getSidebarSessionIndex(params)
    ),
    getApiV1SessionsId: vi.fn(({ id }) => api.getSession(id)),
    deleteApiV1SessionsId: vi.fn(({ id }) => api.deleteSession(id)),
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

type SkinnySessionRow = {
  id: string;
  parent_session_id?: string | null;
  relationship_type?: string | null;
  project: string;
  machine: string;
  agent: string;
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
    storageData.clear();
    mockSidebarPage();
    mockSidebarIndex();
    starred.filterOnly = false;
    starred.ids = new Set();
    yokedDates.clear();
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

      expect(sessions.sessions).toHaveLength(1);
      expect(sessions.sessions[0]!.id).toBe("new-id");
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
