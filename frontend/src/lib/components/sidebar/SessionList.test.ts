// @vitest-environment jsdom
import {
  afterEach,
  beforeEach,
  describe,
  expect,
  it,
  vi,
} from "vitest";
import { mount, tick, unmount } from "svelte";
// @ts-ignore
import SessionList from "./SessionList.svelte";
import sessionFilterControlSource from "../filters/SessionFilterControl.svelte?raw";
import { sessions } from "../../stores/sessions.svelte.js";
import type { Session } from "../../api/types.js";
import { starred } from "../../stores/starred.svelte.js";
import { ITEM_HEIGHT, OVERSCAN } from "./session-list-utils.js";

vi.mock("../../api/client.js", () => ({
  listSessions: vi.fn().mockResolvedValue({
    sessions: [],
    total: 0,
  }),
  getSidebarSessionIndex: vi.fn().mockResolvedValue({
    sessions: [],
    total: 0,
  }),
  getSession: vi.fn(),
  getAgents: vi.fn().mockResolvedValue({ agents: [] }),
  getMachines: vi.fn().mockResolvedValue({ machines: [] }),
  getStats: vi.fn().mockResolvedValue({
    session_count: 0,
    message_count: 0,
    project_count: 0,
    machine_count: 0,
    earliest_session: null,
  }),
  watchEvents: vi.fn(() => ({ close: () => {} })),
  listStarred: vi.fn().mockResolvedValue({ session_ids: [] }),
  bulkStarSessions: vi.fn().mockResolvedValue(undefined),
  starSession: vi.fn().mockResolvedValue(undefined),
  unstarSession: vi.fn().mockResolvedValue(undefined),
}));

class ResizeObserverMock {
  observe = vi.fn();
  disconnect = vi.fn();
}

describe("SessionList filter dropdown", () => {
  let component: ReturnType<typeof mount> | undefined;
  let originalResizeObserver: typeof ResizeObserver | undefined;
  let clientHeightSpy: ReturnType<typeof vi.spyOn> | undefined;
  let rafSpy: ReturnType<typeof vi.spyOn> | undefined;

  beforeEach(() => {
    originalResizeObserver = globalThis.ResizeObserver;
    Object.defineProperty(globalThis, "ResizeObserver", {
      configurable: true,
      writable: true,
      value: ResizeObserverMock,
    });
    clientHeightSpy = vi
      .spyOn(HTMLElement.prototype, "clientHeight", "get")
      .mockReturnValue(ITEM_HEIGHT * 3);
    rafSpy = vi
      .spyOn(globalThis, "requestAnimationFrame")
      .mockImplementation((cb: FrameRequestCallback) => {
        queueMicrotask(() => cb(0));
        return 1;
      });
    sessions.sessions = [];
    sessions.agents = [];
    sessions.machines = [];
    sessions.activeSessionId = null;
    sessions.loading = false;
    sessions.sidebarIndexVersion++;
    sessions.hydratedSessionsByVersion = new Map([
      [sessions.sidebarIndexVersion, new Map()],
    ]);
    starred.filterOnly = false;
    starred.ids = new Set();
  });

  afterEach(() => {
    if (component) {
      unmount(component);
      component = undefined;
    }
    document.body.innerHTML = "";
    Object.defineProperty(globalThis, "ResizeObserver", {
      configurable: true,
      writable: true,
      value: originalResizeObserver,
    });
    clientHeightSpy?.mockRestore();
    rafSpy?.mockRestore();
    vi.restoreAllMocks();
  });

  it("bounds the filter menu to the viewport and lets it scroll", async () => {
    component = mount(SessionList, { target: document.body });
    await tick();

    const filterButton = document.querySelector<HTMLButtonElement>(
      ".filter-btn",
    );
    expect(filterButton).not.toBeNull();

    filterButton!.click();
    await tick();

    const dropdown = document.querySelector<HTMLElement>(
      ".filter-dropdown",
    );
    expect(dropdown).not.toBeNull();

    expect(sessionFilterControlSource).toContain(
      "max-height: min(560px, calc(100vh - 128px));",
    );
    expect(sessionFilterControlSource).toContain("overflow-y: auto;");
  });

  it("labels compact header controls with hover hints", async () => {
    component = mount(SessionList, { target: document.body });
    await tick();

    const filterButton = document.querySelector<HTMLButtonElement>(
      ".filter-btn",
    );

    expect(filterButton).not.toBeNull();
    expect(filterButton?.title).toBe("Filter sessions");
    expect(filterButton?.getAttribute("aria-label")).toBe("Filters");
  });
});

describe("SessionList visible hydration", () => {
  let component: ReturnType<typeof mount> | undefined;
  let originalResizeObserver: typeof ResizeObserver | undefined;
  let clientHeightSpy: ReturnType<typeof vi.spyOn> | undefined;
  let rafSpy: ReturnType<typeof vi.spyOn> | undefined;

  beforeEach(() => {
    originalResizeObserver = globalThis.ResizeObserver;
    Object.defineProperty(globalThis, "ResizeObserver", {
      configurable: true,
      writable: true,
      value: ResizeObserverMock,
    });
    clientHeightSpy = vi
      .spyOn(HTMLElement.prototype, "clientHeight", "get")
      .mockReturnValue(ITEM_HEIGHT * 3);
    rafSpy = vi
      .spyOn(globalThis, "requestAnimationFrame")
      .mockImplementation((cb: FrameRequestCallback) => {
        queueMicrotask(() => cb(0));
        return 1;
      });
    sessions.sessions = [];
    sessions.activeSessionId = null;
    sessions.loading = false;
    sessions.sidebarIndexVersion++;
    sessions.hydratedSessionsByVersion = new Map([
      [sessions.sidebarIndexVersion, new Map()],
    ]);
    starred.filterOnly = false;
    starred.ids = new Set();
  });

  afterEach(() => {
    if (component) {
      unmount(component);
      component = undefined;
    }
    document.body.innerHTML = "";
    Object.defineProperty(globalThis, "ResizeObserver", {
      configurable: true,
      writable: true,
      value: originalResizeObserver,
    });
    clientHeightSpy?.mockRestore();
    rafSpy?.mockRestore();
    vi.restoreAllMocks();
  });

  it("initial hydration target uses viewport rows plus overscan", async () => {
    sessions.sessions = Array.from({ length: 20 }, (_, i) =>
      makeSession({ id: `s${i}`, is_index_only: true }),
    );
    const hydrate = vi
      .spyOn(sessions, "hydrateVisibleSessions")
      .mockResolvedValue(undefined);

    component = mount(SessionList, { target: document.body });
    await tick();

    const expected = Math.ceil((ITEM_HEIGHT * 3) / ITEM_HEIGHT) + OVERSCAN;
    expect(hydrate).toHaveBeenCalledWith(
      Array.from({ length: expected }, (_, i) => `s${i}`),
      sessions.sidebarIndexVersion,
    );
  });

  it("holds first visible index-only rows until initial hydration resolves", async () => {
    sessions.sessions = [
      makeSession({ id: "pending", project: "placeholder", is_index_only: true }),
    ];
    let resolveHydration!: () => void;
    vi.spyOn(sessions, "hydrateVisibleSessions").mockReturnValue(
      new Promise<void>((resolve) => {
        resolveHydration = resolve;
      }),
    );

    component = mount(SessionList, { target: document.body });
    await tick();

    expect(document.querySelector(".session-item")).toBeNull();

    sessions.sessions = [
      makeSession({
        id: "pending",
        first_message: "hydrated visible title",
        is_index_only: false,
      }),
    ];
    resolveHydration();
    await tick();
    await tick();

    expect(document.body.textContent).toContain("hydrated visible title");
  });

  it("renders renamed rows without waiting for hydration", async () => {
    sessions.sessions = [
      makeSession({
        id: "renamed",
        display_name: "Renamed sidebar title",
        is_index_only: true,
      }),
    ];
    vi.spyOn(sessions, "hydrateVisibleSessions").mockReturnValue(
      new Promise<void>(() => {}),
    );

    component = mount(SessionList, { target: document.body });
    await tick();

    expect(document.body.textContent).toContain("Renamed sidebar title");
  });

  it("hydrates newly visible rows after scrolling", async () => {
    sessions.sessions = Array.from({ length: 50 }, (_, i) =>
      makeSession({ id: `s${i}`, is_index_only: true }),
    );
    const hydrate = vi
      .spyOn(sessions, "hydrateVisibleSessions")
      .mockResolvedValue(undefined);

    component = mount(SessionList, { target: document.body });
    await tick();
    hydrate.mockClear();

    const scroller = document.querySelector<HTMLElement>(".session-list-scroll");
    expect(scroller).not.toBeNull();
    scroller!.scrollTop = ITEM_HEIGHT * 20;
    scroller!.dispatchEvent(new Event("scroll"));
    await Promise.resolve();
    await tick();

    expect(hydrate.mock.calls.some(([ids]) => ids.includes("s20"))).toBe(true);
  });

  it("retries visible hydration when an index-only row becomes visible again", async () => {
    sessions.sessions = Array.from({ length: 50 }, (_, i) =>
      makeSession({ id: `s${i}`, is_index_only: true }),
    );
    const hydrate = vi
      .spyOn(sessions, "hydrateVisibleSessions")
      .mockResolvedValue(undefined);

    component = mount(SessionList, { target: document.body });
    await tick();
    hydrate.mockClear();

    const scroller = document.querySelector<HTMLElement>(".session-list-scroll");
    expect(scroller).not.toBeNull();
    scroller!.scrollTop = ITEM_HEIGHT * 20;
    scroller!.dispatchEvent(new Event("scroll"));
    await Promise.resolve();
    await tick();
    hydrate.mockClear();

    scroller!.scrollTop = 0;
    scroller!.dispatchEvent(new Event("scroll"));
    await Promise.resolve();
    await tick();

    expect(hydrate.mock.calls.some(([ids]) => ids.includes("s0"))).toBe(true);
  });

  it("keeps starred-only filtering after grouping", async () => {
    sessions.sessions = [
      makeSession({ id: "root", display_name: "Root", is_index_only: true }),
      makeSession({
        id: "starred-child",
        parent_session_id: "root",
        display_name: "Starred child",
        is_index_only: true,
      }),
      makeSession({
        id: "unstarred",
        display_name: "Unstarred",
        is_index_only: true,
      }),
    ];
    starred.filterOnly = true;
    starred.ids = new Set(["starred-child"]);
    vi.spyOn(sessions, "hydrateVisibleSessions").mockResolvedValue(undefined);

    component = mount(SessionList, { target: document.body });
    await tick();

    expect(document.body.textContent).toContain("Starred child");
    expect(document.body.textContent).not.toContain("Root");
    expect(document.body.textContent).not.toContain("Unstarred");
  });

  it("uses is_teammate for the collapsed group teammate hint", async () => {
    sessions.sessions = [
      makeSession({ id: "root", display_name: "Root", is_index_only: true }),
      makeSession({
        id: "team",
        parent_session_id: "root",
        display_name: "Team task",
        is_teammate: true,
        is_index_only: true,
      }),
    ];
    vi.spyOn(sessions, "hydrateVisibleSessions").mockResolvedValue(undefined);

    component = mount(SessionList, { target: document.body });
    await tick();

    expect(document.querySelectorAll(".group-hint-icon")).toHaveLength(1);
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
    started_at: "2024-01-01T00:00:00Z",
    ended_at: "2024-01-01T00:01:00Z",
    message_count: 1,
    user_message_count: 1,
    total_output_tokens: 0,
    peak_context_tokens: 0,
    is_automated: false,
    created_at: "2024-01-01T00:00:00Z",
    ...overrides,
  };
}
