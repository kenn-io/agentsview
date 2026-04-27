import {
  describe,
  it,
  expect,
  vi,
  beforeEach,
} from "vitest";
import {
  createSessionsStore,
  buildSessionGroups,
  parseFiltersFromParams,
  filtersToParams,
} from "./sessions.svelte.js";
import type { Filters } from "./sessions.svelte.js";
import type { Session } from "../api/types.js";
import * as api from "../api/client.js";
import type { ListSessionsParams } from "../api/client.js";

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
  listSessions: vi.fn(),
  getSession: vi.fn(),
  getProjects: vi.fn(),
  getAgents: vi.fn(),
  // invalidateFilterCaches() triggers sync.loadStats() which calls
  // getStats(). Provide a default so the stale-state guards we
  // exercise don't trip noisy "no export" stderr from the mock.
  getStats: vi.fn().mockResolvedValue({
    session_count: 0,
    message_count: 0,
    project_count: 0,
    machine_count: 0,
    earliest_session: null,
  }),
  // Live-refresh subscription opens an EventSource via watchEvents.
  // Stub it so the mocked client doesn't blow up when the store
  // calls events.subscribeDebounced() during load().
  watchEvents: vi.fn(() => ({ close: () => {} })),
}));

function mockListSessions(
  overrides?: Partial<{ next_cursor: string }>,
) {
  vi.mocked(api.listSessions).mockResolvedValue({
    sessions: [],
    total: 0,
    ...overrides,
  });
}

function mockGetProjects() {
  vi.mocked(api.getProjects).mockResolvedValue({
    projects: [{ name: "proj", session_count: 1 }],
  });
}

function expectListSessionsCalledWith(
  expected: Partial<ListSessionsParams>,
) {
  expect(api.listSessions).toHaveBeenLastCalledWith(
    expect.objectContaining(expected),
  );
}

describe("SessionsStore", () => {
  let sessions: ReturnType<typeof createSessionsStore>;

  beforeEach(() => {
    vi.clearAllMocks();
    storageData.clear();
    mockListSessions();
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

    it("should round-trip through parseFiltersFromParams", () => {
      const original: Filters = {
        project: "myproj",
        machine: "host-a",
        agent: "claude",
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

      expectListSessionsCalledWith({
        min_messages: undefined,
        max_messages: undefined,
      });
    });

    it("should include positive min_messages", async () => {
      sessions.filters.minMessages = 5;
      await sessions.load();

      expectListSessionsCalledWith({ min_messages: 5 });
    });

    it("should include positive max_messages", async () => {
      sessions.filters.maxMessages = 100;
      await sessions.load();

      expectListSessionsCalledWith({ max_messages: 100 });
    });

    it("should pass project filter when set", async () => {
      sessions.filters.project = "myproj";
      await sessions.load();

      expectListSessionsCalledWith({ project: "myproj" });
    });

    it("should omit project when empty", async () => {
      sessions.filters.project = "";
      await sessions.load();

      expectListSessionsCalledWith({
        project: undefined,
      });
    });

    it("should pass agent filter when set", async () => {
      sessions.filters.agent = "claude";
      await sessions.load();

      expectListSessionsCalledWith({ agent: "claude" });
    });

    it("should omit agent when empty", async () => {
      sessions.filters.agent = "";
      await sessions.load();

      expectListSessionsCalledWith({ agent: undefined });
    });

    it("should pass date filter when set", async () => {
      sessions.filters.date = "2024-06-15";
      await sessions.load();

      expectListSessionsCalledWith({
        date: "2024-06-15",
      });
    });

    it("should omit date when empty", async () => {
      sessions.filters.date = "";
      await sessions.load();

      expectListSessionsCalledWith({ date: undefined });
    });

    it("should pass date_from filter when set", async () => {
      sessions.filters.dateFrom = "2024-06-01";
      await sessions.load();

      expectListSessionsCalledWith({
        date_from: "2024-06-01",
      });
    });

    it("should omit date_from when empty", async () => {
      sessions.filters.dateFrom = "";
      await sessions.load();

      expectListSessionsCalledWith({
        date_from: undefined,
      });
    });

    it("should pass date_to filter when set", async () => {
      sessions.filters.dateTo = "2024-06-30";
      await sessions.load();

      expectListSessionsCalledWith({
        date_to: "2024-06-30",
      });
    });

    it("should omit date_to when empty", async () => {
      sessions.filters.dateTo = "";
      await sessions.load();

      expectListSessionsCalledWith({
        date_to: undefined,
      });
    });
  });

  describe("loadMore serialization", () => {
    it("should fetch all pages with consistent filters in load()", async () => {
      vi.mocked(api.listSessions)
        .mockResolvedValueOnce({
          sessions: [
            {
              id: "s1",
              project: "proj",
              machine: "m",
              agent: "a",
              first_message: null,
              started_at: null,
              ended_at: null,
              message_count: 1,
              user_message_count: 1,
              total_output_tokens: 0,
              peak_context_tokens: 0,
              has_total_output_tokens: false,
              has_peak_context_tokens: false,
              is_automated: false,
              created_at: "2024-01-01T00:00:00Z",
            },
          ],
          total: 2,
          next_cursor: "cur1",
        })
        .mockResolvedValueOnce({
          sessions: [
            {
              id: "s2",
              project: "proj",
              machine: "m",
              agent: "a",
              first_message: null,
              started_at: null,
              ended_at: null,
              message_count: 1,
              user_message_count: 1,
              total_output_tokens: 0,
              peak_context_tokens: 0,
              has_total_output_tokens: false,
              has_peak_context_tokens: false,
              is_automated: false,
              created_at: "2024-01-01T00:00:01Z",
            },
          ],
          total: 2,
        });

      sessions.filters.minMessages = 10;
      sessions.filters.maxMessages = 50;
      await sessions.load();

      expect(api.listSessions).toHaveBeenCalledTimes(2);
      const calls = vi.mocked(api.listSessions).mock.calls;
      const first = calls[0]?.[0];
      const second = calls[1]?.[0];

      expect(first?.min_messages).toBe(10);
      expect(first?.max_messages).toBe(50);
      expect(first?.cursor).toBeUndefined();

      expect(second?.min_messages).toBe(10);
      expect(second?.max_messages).toBe(50);
      expect(second?.cursor).toBe("cur1");

      expect(sessions.sessions).toHaveLength(2);
      expect(sessions.total).toBe(2);
      expect(sessions.nextCursor).toBeNull();
    });

    it("swaps sessions atomically after all pages load", async () => {
      // Pre-populate with a list representing a prior load,
      // then trigger a multi-page reload. The visible count
      // must not tick up as pages arrive — old data stays,
      // then the new data replaces it in one step.
      sessions.sessions = [
        makeSession({ id: "old-a" }),
        makeSession({ id: "old-b" }),
        makeSession({ id: "old-c" }),
      ];
      sessions.total = 3;

      let resolvePage2: ((v: {
        sessions: Session[];
        total: number;
        next_cursor?: string;
      }) => void) | null = null;
      const page2Promise = new Promise<{
        sessions: Session[];
        total: number;
        next_cursor?: string;
      }>((resolve) => {
        resolvePage2 = resolve;
      });

      vi.mocked(api.listSessions)
        .mockResolvedValueOnce({
          sessions: [makeSession({ id: "new-1" })],
          total: 2,
          next_cursor: "c1",
        })
        .mockReturnValueOnce(page2Promise);

      const loadPromise = sessions.load();

      // Flush the first page fetch without resolving the second.
      await Promise.resolve();
      await Promise.resolve();

      // Old sessions are still visible while pagination is in flight.
      expect(sessions.sessions.map((s) => s.id)).toEqual([
        "old-a",
        "old-b",
        "old-c",
      ]);
      expect(sessions.total).toBe(3);

      resolvePage2!({
        sessions: [makeSession({ id: "new-2" })],
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

      mockListSessions();
      await sessions.loadMore();

      expectListSessionsCalledWith({
        min_messages: undefined,
        max_messages: undefined,
      });
    });

    it("should omit agent when empty in loadMore", async () => {
      sessions.nextCursor = "cur3";
      sessions.filters.agent = "";

      mockListSessions();
      await sessions.loadMore();

      expectListSessionsCalledWith({ agent: undefined });
    });

    it("should omit date when empty in loadMore", async () => {
      sessions.nextCursor = "cur3";
      sessions.filters.date = "";

      mockListSessions();
      await sessions.loadMore();

      expectListSessionsCalledWith({ date: undefined });
    });

    it("should omit date_from when empty in loadMore", async () => {
      sessions.nextCursor = "cur3";
      sessions.filters.dateFrom = "";

      mockListSessions();
      await sessions.loadMore();

      expectListSessionsCalledWith({
        date_from: undefined,
      });
    });

    it("should omit date_to when empty in loadMore", async () => {
      sessions.nextCursor = "cur3";
      sessions.filters.dateTo = "";

      mockListSessions();
      await sessions.loadMore();

      expectListSessionsCalledWith({
        date_to: undefined,
      });
    });

    it("should pass all filters in loadMore", async () => {
      sessions.nextCursor = "cur3";
      sessions.filters.agent = "codex";
      sessions.filters.date = "2024-07-01";
      sessions.filters.dateFrom = "2024-07-01";
      sessions.filters.dateTo = "2024-07-31";

      mockListSessions();
      await sessions.loadMore();

      expectListSessionsCalledWith({
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
        expect(api.listSessions).toHaveBeenCalled();
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

      expectListSessionsCalledWith({
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

      expectListSessionsCalledWith({
        exclude_project: "unknown",
      });
    });

    it("should omit exclude_project when disabled", async () => {
      sessions.filters.hideUnknownProject = false;
      await sessions.load();

      expectListSessionsCalledWith({
        exclude_project: undefined,
      });
    });

    it("should clear project filter when hiding unknown and project is unknown", async () => {
      sessions.filters.project = "unknown";
      sessions.setHideUnknownProjectFilter(true);
      await vi.waitFor(() => {
        expect(api.listSessions).toHaveBeenCalled();
      });

      expect(sessions.filters.project).toBe("");
      expect(sessions.filters.hideUnknownProject).toBe(true);
      expectListSessionsCalledWith({
        project: undefined,
        exclude_project: "unknown",
      });
    });

    it("should preserve project filter when hiding unknown and project is not unknown", async () => {
      sessions.filters.project = "my_app";
      sessions.setHideUnknownProjectFilter(true);
      await vi.waitFor(() => {
        expect(api.listSessions).toHaveBeenCalled();
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

    it("should be included in hasActiveFilters", () => {
      sessions.filters.hideUnknownProject = true;
      expect(sessions.hasActiveFilters).toBe(true);
    });

    it("should suppress exclude_project when project is unknown", async () => {
      sessions.filters.hideUnknownProject = true;
      sessions.filters.project = "unknown";
      await sessions.load();

      expectListSessionsCalledWith({
        project: "unknown",
        exclude_project: undefined,
      });
    });

    it("should be cleared by clearSessionFilters", async () => {
      sessions.filters.hideUnknownProject = true;
      sessions.clearSessionFilters();
      await vi.waitFor(() => {
        expect(api.listSessions).toHaveBeenCalled();
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
        expect(api.listSessions).toHaveBeenCalled();
      });

      expect(sessions.hasActiveFilters).toBe(false);
    });

    it("should preserve project filter after clearSessionFilters", async () => {
      sessions.filters.project = "myproj";
      sessions.filters.agent = "claude";
      sessions.clearSessionFilters();
      await vi.waitFor(() => {
        expect(api.listSessions).toHaveBeenCalled();
      });

      expect(sessions.filters.project).toBe("myproj");
      expect(sessions.hasActiveFilters).toBe(false);
    });
  });

  describe("machine filter", () => {
    it("should toggle one machine on and serialize it", async () => {
      sessions.toggleMachineFilter("host-a");
      await vi.waitFor(() => {
        expect(api.listSessions).toHaveBeenCalled();
      });

      expect(sessions.filters.machine).toBe("host-a");
      expect(sessions.selectedMachines).toEqual(["host-a"]);
      expect(sessions.isMachineSelected("host-a")).toBe(true);
      expectListSessionsCalledWith({ machine: "host-a" });
    });

    it("should allow multiple selected machines", async () => {
      sessions.toggleMachineFilter("host-a");
      await vi.waitFor(() => {
        expect(api.listSessions).toHaveBeenCalledTimes(1);
      });

      sessions.toggleMachineFilter("host-b");
      await vi.waitFor(() => {
        expect(api.listSessions).toHaveBeenCalledTimes(2);
      });

      expect(sessions.filters.machine).toBe("host-a,host-b");
      expect(sessions.selectedMachines).toEqual([
        "host-a",
        "host-b",
      ]);
      expect(sessions.isMachineSelected("host-b")).toBe(true);
      expectListSessionsCalledWith({
        machine: "host-a,host-b",
      });
    });

    it("should toggle an already-selected machine off", async () => {
      sessions.filters.machine = "host-a,host-b";

      sessions.toggleMachineFilter("host-a");
      await vi.waitFor(() => {
        expect(api.listSessions).toHaveBeenCalled();
      });

      expect(sessions.filters.machine).toBe("host-b");
      expect(sessions.selectedMachines).toEqual(["host-b"]);
      expect(sessions.isMachineSelected("host-a")).toBe(false);
      expectListSessionsCalledWith({ machine: "host-b" });
    });

    it("should clear the filter when the last machine is removed", async () => {
      sessions.filters.machine = "host-a";

      sessions.toggleMachineFilter("host-a");
      await vi.waitFor(() => {
        expect(api.listSessions).toHaveBeenCalled();
      });

      expect(sessions.filters.machine).toBe("");
      expect(sessions.selectedMachines).toEqual([]);
      expectListSessionsCalledWith({ machine: undefined });
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

      vi.mocked(api.listSessions).mockRejectedValueOnce(
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

      vi.mocked(api.listSessions).mockRejectedValueOnce(
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

      vi.mocked(api.listSessions).mockRejectedValueOnce(
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
      mockListSessions();

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
      mockListSessions();
      sessions.sessions = [makeSession({ id: "existing" })];

      await sessions.navigateToSession("existing");

      expect(sessions.activeSessionId).toBe("existing");
      expect(api.getSession).not.toHaveBeenCalled();
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
});

describe("SessionsStore live refresh", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    storageData.clear();
    mockListSessions();
    mockGetProjects();
  });

  it("refetches when an events.subscribeDebounced callback fires", async () => {
    const { events } = await import("./events.svelte.js");
    // Capture the registered callback directly so the test bypasses
    // the events singleton's debounce and any accumulated state.
    let registered: ((e: { scope: string }) => void) | null = null;
    const spy = vi
      .spyOn(events, "subscribeDebounced")
      .mockImplementation((fn) => {
        registered = fn as (e: { scope: string }) => void;
        return () => {};
      });

    const sessions = createSessionsStore();
    await sessions.load();
    expect(api.listSessions).toHaveBeenCalledTimes(1);
    expect(spy).toHaveBeenCalled();
    expect(registered).not.toBeNull();

    registered!({ scope: "messages" });
    // Flush the load() promise chain without advancing timers
    // (the safety-net setInterval is real here and would loop).
    await Promise.resolve();
    await Promise.resolve();
    expect(api.listSessions).toHaveBeenCalledTimes(2);

    sessions.dispose();
    spy.mockRestore();
  });

  it("refetches on the 5-minute safety-net interval", async () => {
    vi.useFakeTimers();
    const { events } = await import("./events.svelte.js");
    const spy = vi
      .spyOn(events, "subscribeDebounced")
      .mockReturnValue(() => {});

    const sessions = createSessionsStore();
    await sessions.load();
    expect(api.listSessions).toHaveBeenCalledTimes(1);

    // Advance exactly one interval — avoids the runAllTimers infinite
    // loop that recurring setInterval plus a promise-resolving
    // listSessions mock would produce.
    await vi.advanceTimersByTimeAsync(5 * 60 * 1000);
    expect(api.listSessions).toHaveBeenCalledTimes(2);

    sessions.dispose();
    spy.mockRestore();
    vi.useRealTimers();
  });

  it("dispose() unsubscribes and clears the safety-net timer", async () => {
    vi.useFakeTimers();
    const { events } = await import("./events.svelte.js");
    const unsub = vi.fn();
    const spy = vi
      .spyOn(events, "subscribeDebounced")
      .mockReturnValue(unsub);

    const sessions = createSessionsStore();
    await sessions.load();
    expect(api.listSessions).toHaveBeenCalledTimes(1);

    sessions.dispose();
    expect(unsub).toHaveBeenCalledTimes(1);

    // After dispose the interval is cleared, so advancing well past
    // 5 minutes triggers no further fetches.
    await vi.advanceTimersByTimeAsync(10 * 60 * 1000);
    expect(api.listSessions).toHaveBeenCalledTimes(1);

    spy.mockRestore();
    vi.useRealTimers();
  });
});
