// @vitest-environment jsdom
import {
  afterEach,
  beforeEach,
  describe,
  expect,
  it,
  vi,
} from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
// @ts-ignore
import SessionList from "./SessionList.svelte";
import sessionFilterControlSource from "../filters/SessionFilterControl.svelte?raw";
import sessionItemSource from "./SessionItem.svelte?raw";
import { sessions } from "../../stores/sessions.svelte.js";
import type { Session } from "../../api/types.js";
import { starred } from "../../stores/starred.svelte.js";
import { setLocale } from "../../i18n/index.js";
import {
  ITEM_HEIGHT,
  OVERSCAN,
  STORAGE_KEY_GROUP,
} from "./session-list-utils.js";

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
    sessions.nextCursor = null;
    sessions.loading = false;
    sessions.selectedIds = new Set();
    sessions.selectMode = false;
    sessions.sidebarIndexVersion++;
    sessions.hydratedSessionsByVersion = new Map([
      [sessions.sidebarIndexVersion, new Map()],
    ]);
    starred.filterOnly = false;
    starred.ids = new Set();
    setLocale("en");
    localStorage.clear();
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

  it("renders translated sidebar filter controls and row actions", async () => {
    setLocale("zh-CN");
    sessions.agents = [
      { name: "claude", session_count: 4 },
      { name: "codex", session_count: 2 },
    ];
    sessions.machines = ["workstation"];
    sessions.sessions = [
      makeSession({
        id: "translated-session",
        display_name: "Translated row",
        is_index_only: false,
      }),
    ];
    vi.spyOn(sessions, "hydrateVisibleSessions").mockResolvedValue(undefined);

    component = mount(SessionList, { target: document.body });
    await tick();

    const filterButton = document.querySelector<HTMLButtonElement>(
      ".filter-btn",
    );
    expect(filterButton).not.toBeNull();
    expect(filterButton?.title).toBe("筛选会话");
    expect(filterButton?.getAttribute("aria-label")).toBe("筛选器");

    filterButton!.click();
    await tick();

    expect(document.body.textContent).toContain("显示");
    expect(document.body.textContent).toContain("按 agent 分组");
    expect(document.body.textContent).toContain("仅显示已固定");
    expect(document.body.textContent).toContain("最近活跃");
    expect(document.body.textContent).toContain("隐藏单轮");
    expect(document.body.textContent).toContain("所有 agents");
    expect(document.body.textContent).toContain("Machine");
    expect(document.body.textContent).toContain("最少提示数");

    const row = document.querySelector<HTMLElement>(".session-item");
    expect(row).not.toBeNull();
    row!.dispatchEvent(
      new MouseEvent("contextmenu", {
        bubbles: true,
        cancelable: true,
        clientX: 7,
        clientY: 8,
      }),
    );
    await tick();

    expect(document.body.textContent).toContain("重命名");
    expect(document.body.textContent).toContain("在新标签页打开");
    expect(document.body.textContent).toContain("删除");
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
    sessions.nextCursor = null;
    sessions.loading = false;
    sessions.selectedIds = new Set();
    sessions.selectMode = false;
    sessions.sidebarIndexVersion++;
    sessions.hydratedSessionsByVersion = new Map([
      [sessions.sidebarIndexVersion, new Map()],
    ]);
    starred.filterOnly = false;
    starred.ids = new Set();
    setLocale("en");
    localStorage.clear();
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

  it("keeps long session labels intact for responsive CSS clipping", async () => {
    const title =
      "test: validate GitLab write parity against a real GitLab instance";
    sessions.sessions = [
      makeSession({
        id: "long-title",
        first_message: title,
        is_index_only: false,
      }),
    ];
    vi.spyOn(sessions, "hydrateVisibleSessions").mockResolvedValue(undefined);

    component = mount(SessionList, { target: document.body });
    await tick();

    const name = document.querySelector<HTMLElement>(".session-name");
    expect(name).not.toBeNull();
    expect(name?.textContent).toBe(title);
  });

  it("marks the active session row for assistive tech", async () => {
    sessions.sessions = [
      makeSession({
        id: "active-session",
        first_message: "Selected transcript",
        is_index_only: false,
      }),
      makeSession({
        id: "other-session",
        first_message: "Other transcript",
        is_index_only: false,
      }),
    ];
    sessions.activeSessionId = "active-session";
    vi.spyOn(sessions, "hydrateVisibleSessions").mockResolvedValue(undefined);

    component = mount(SessionList, { target: document.body });
    await tick();

    const active = document.querySelector<HTMLElement>(
      '[data-session-id="active-session"]',
    );
    const other = document.querySelector<HTMLElement>(
      '[data-session-id="other-session"]',
    );
    expect(active).not.toBeNull();
    expect(active?.getAttribute("aria-current")).toBe("page");
    expect(other).not.toBeNull();
    expect(other?.hasAttribute("aria-current")).toBe(false);
  });

  it("gives active rows a persistent visual indicator distinct from hover", () => {
    expect(sessionItemSource).toContain(".session-item.active::before");
    expect(sessionItemSource).toContain("background: var(--accent-blue)");
    expect(sessionItemSource).toContain(
      "box-shadow: inset 0 0 0 1px color-mix(in srgb, var(--accent-blue) 28%, transparent)",
    );
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

  it("reloads the sidebar when starred-only is toggled", async () => {
    const load = vi.spyOn(sessions, "load").mockResolvedValue(undefined);

    component = mount(SessionList, { target: document.body });
    await tick();
    load.mockClear();

    const filterButton = document.querySelector<HTMLButtonElement>(
      ".filter-btn",
    );
    expect(filterButton).not.toBeNull();
    filterButton!.click();
    await tick();

    const starredButton = Array.from(
      document.querySelectorAll<HTMLButtonElement>(".filter-toggle"),
    ).find((button) => button.textContent?.includes("Starred only"));
    expect(starredButton).not.toBeNull();

    starredButton!.click();
    await tick();

    expect(starred.filterOnly).toBe(true);
    expect(load).toHaveBeenCalledTimes(1);
  });

  it("renders the primary session surface as a native href", async () => {
    sessions.sessions = [
      makeSession({
        id: "native-session",
        display_name: "Native link session",
        is_index_only: false,
      }),
    ];
    vi.spyOn(sessions, "hydrateVisibleSessions").mockResolvedValue(
      undefined,
    );

    component = mount(SessionList, { target: document.body });
    await tick();

    const link = document.querySelector<HTMLAnchorElement>(
      ".session-info-link",
    );
    expect(link).not.toBeNull();
    expect(link?.getAttribute("href")).toBe("/sessions/native-session");
  });

  it("keeps keyboard-style anchor activation on the SPA session path", async () => {
    const selectSession = vi
      .spyOn(sessions, "selectSession")
      .mockImplementation(() => {});
    sessions.sessions = [
      makeSession({
        id: "keyboard-session",
        display_name: "Keyboard target",
        is_index_only: false,
      }),
    ];
    vi.spyOn(sessions, "hydrateVisibleSessions").mockResolvedValue(
      undefined,
    );

    component = mount(SessionList, { target: document.body });
    await tick();

    const link = document.querySelector<HTMLAnchorElement>(
      ".session-info-link",
    );
    expect(link).not.toBeNull();
    const click = new MouseEvent("click", {
      bubbles: true,
      cancelable: true,
      detail: 0,
    });
    link!.dispatchEvent(click);

    expect(click.defaultPrevented).toBe(true);
    expect(selectSession).toHaveBeenCalledWith("keyboard-session");
  });

  it("toggles selection from the session link in select mode", async () => {
    const selectSession = vi
      .spyOn(sessions, "selectSession")
      .mockImplementation(() => {});
    sessions.sessions = [
      makeSession({
        id: "link-select-session",
        display_name: "Link selection target",
        is_index_only: false,
      }),
    ];
    vi.spyOn(sessions, "hydrateVisibleSessions").mockResolvedValue(
      undefined,
    );

    component = mount(SessionList, { target: document.body });
    await tick();

    const selectModeButton = document.querySelector<HTMLButtonElement>(
      ".select-toggle-btn",
    );
    expect(selectModeButton).not.toBeNull();
    selectModeButton!.click();
    await tick();

    const link = document.querySelector<HTMLAnchorElement>(
      ".session-info-link",
    );
    expect(link).not.toBeNull();
    const click = new MouseEvent("click", {
      bubbles: true,
      cancelable: true,
    });
    link!.dispatchEvent(click);

    expect(click.defaultPrevented).toBe(true);
    expect(selectSession).not.toHaveBeenCalled();
    expect([...sessions.selectedIds]).toEqual(["link-select-session"]);
  });

  it("keeps the non-link parts of the row selectable", async () => {
    const selectSession = vi
      .spyOn(sessions, "selectSession")
      .mockImplementation(() => {});
    sessions.sessions = [
      makeSession({
        id: "row-session",
        display_name: "Row target",
        is_index_only: false,
      }),
    ];
    vi.spyOn(sessions, "hydrateVisibleSessions").mockResolvedValue(
      undefined,
    );

    component = mount(SessionList, { target: document.body });
    await tick();

    const sideMeta = document.querySelector<HTMLElement>(".side-meta");
    expect(sideMeta).not.toBeNull();
    sideMeta!.click();

    expect(selectSession).toHaveBeenCalledWith("row-session");
  });

  it("keeps button-discoverable rows alongside native session links", async () => {
    sessions.sessions = [
      makeSession({
        id: "button-session",
        display_name: "Button target",
        is_index_only: false,
      }),
    ];
    vi.spyOn(sessions, "hydrateVisibleSessions").mockResolvedValue(
      undefined,
    );

    component = mount(SessionList, { target: document.body });
    await tick();

    const row = document.querySelector<HTMLElement>(".session-item");
    const link = document.querySelector<HTMLAnchorElement>(
      ".session-info-link",
    );
    expect(row).not.toBeNull();
    expect(row?.getAttribute("role")).toBe("button");
    expect(row?.getAttribute("tabindex")).toBe("0");
    expect(link).not.toBeNull();
    expect(link?.getAttribute("href")).toBe("/sessions/button-session");
  });

  it("opens the same canonical href from the context menu in a new tab", async () => {
    const openSpy = vi
      .spyOn(window, "open")
      .mockReturnValue(null as unknown as Window);
    sessions.sessions = [
      makeSession({
        id: "native-open-session",
        display_name: "Open in new tab target",
        is_index_only: false,
      }),
    ];
    vi.spyOn(sessions, "hydrateVisibleSessions").mockResolvedValue(
      undefined,
    );

    component = mount(SessionList, { target: document.body });
    await tick();

    const row = document.querySelector<HTMLElement>(".session-item");
    expect(row).not.toBeNull();
    row!.dispatchEvent(
      new MouseEvent("contextmenu", {
        bubbles: true,
        cancelable: true,
        clientX: 7,
        clientY: 8,
      }),
    );
    await tick();

    const openInNewTab = Array.from(
      document.querySelectorAll<HTMLButtonElement>(".context-menu-item"),
    ).find((button) => button.textContent === "Open in new tab");
    expect(openInNewTab).not.toBeNull();
    openInNewTab!.click();

    expect(openSpy).toHaveBeenCalledWith(
      "/sessions/native-open-session",
      "_blank",
      "noopener",
    );
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

  it("selects only rendered session rows when selecting all visible", async () => {
    sessions.sessions = [
      makeSession({
        id: "parent",
        display_name: "Parent session",
        started_at: "2024-01-01T00:00:00Z",
        ended_at: "2024-01-01T00:01:00Z",
        is_index_only: false,
      }),
      makeSession({
        id: "child",
        parent_session_id: "parent",
        display_name: "Visible continuation",
        started_at: "2024-01-01T00:02:00Z",
        ended_at: "2024-01-01T00:03:00Z",
        is_index_only: false,
      }),
    ];
    vi.spyOn(sessions, "hydrateVisibleSessions").mockResolvedValue(undefined);

    component = mount(SessionList, { target: document.body });
    await tick();

    expect(
      document.querySelector<HTMLElement>('[data-session-id="child"]'),
    ).not.toBeNull();
    expect(
      document.querySelector<HTMLElement>('[data-session-id="parent"]'),
    ).toBeNull();

    const selectModeButton = document.querySelector<HTMLButtonElement>(
      ".select-toggle-btn",
    );
    expect(selectModeButton).not.toBeNull();
    selectModeButton!.click();
    await tick();

    const selectAllButton = document.querySelector<HTMLButtonElement>(
      ".batch-select-all-btn",
    );
    expect(selectAllButton).not.toBeNull();
    selectAllButton!.click();
    await tick();

    expect([...sessions.selectedIds]).toEqual(["child"]);
  });

  it("deletes only selected rows that are still rendered", async () => {
    sessions.sessions = [
      makeSession({
        id: "visible",
        display_name: "Visible session",
        is_index_only: false,
      }),
      makeSession({
        id: "hidden",
        display_name: "Hidden session",
        is_index_only: false,
      }),
    ];
    vi.spyOn(sessions, "hydrateVisibleSessions").mockResolvedValue(undefined);
    const batchDelete = vi
      .spyOn(sessions, "batchDeleteSessions")
      .mockResolvedValue(undefined);

    component = mount(SessionList, { target: document.body });
    await tick();

    const selectModeButton = document.querySelector<HTMLButtonElement>(
      ".select-toggle-btn",
    );
    expect(selectModeButton).not.toBeNull();
    selectModeButton!.click();
    await tick();

    const selectAllButton = document.querySelector<HTMLButtonElement>(
      ".batch-select-all-btn",
    );
    expect(selectAllButton).not.toBeNull();
    selectAllButton!.click();
    await tick();
    expect([...sessions.selectedIds]).toEqual(["visible", "hidden"]);

    sessions.sessions = sessions.sessions.filter((s) => s.id !== "hidden");
    await tick();

    expect(document.querySelector('[data-session-id="hidden"]')).toBeNull();
    expect(document.body.textContent).toContain("1 selected");

    const deleteButton = document.querySelector<HTMLButtonElement>(
      ".batch-delete-btn",
    );
    expect(deleteButton).not.toBeNull();
    deleteButton!.click();
    await tick();

    expect(batchDelete).toHaveBeenCalledWith(["visible"]);
  });

  it("does not auto-page when saved grouping starts collapsed", async () => {
    localStorage.setItem(STORAGE_KEY_GROUP, "agent");
    sessions.sessions = Array.from({ length: 30 }, (_, i) =>
      makeSession({
        id: `s${i}`,
        agent: i % 2 === 0 ? "claude" : "codex",
        display_name: `Session ${i}`,
        is_index_only: true,
      }),
    );
    sessions.nextCursor = "next-page";
    vi.spyOn(sessions, "hydrateVisibleSessions").mockResolvedValue(undefined);
    const loadMore = vi
      .spyOn(sessions, "loadMore")
      .mockResolvedValue(undefined);

    component = mount(SessionList, { target: document.body });
    await tick();
    await tick();

    expect(loadMore).not.toHaveBeenCalled();
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
