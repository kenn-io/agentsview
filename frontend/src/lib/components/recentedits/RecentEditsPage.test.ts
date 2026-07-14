// @vitest-environment jsdom
import {
  describe,
  it,
  expect,
  vi,
  beforeEach,
  afterEach,
} from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import { setLocale } from "../../i18n/index.js";

const mocks = vi.hoisted(() => ({
  scrollToOrdinal: vi.fn(),
  navigateToSession: vi.fn(),
  navigate: vi.fn(),
  setProjectFilter: vi.fn(),
  getApiV1RecentEdits: vi.fn(async () => ({
    files: [
      {
        project: "agentsview",
        file_path: "/a/b/config.go",
        edit_count: 3,
        last_edited_at: "2026-06-24T12:00:00Z",
        last_session_id: "s1",
        edits_truncated: true,
        edits: [
          {
            session_id: "s1",
            ordinal: 12,
            call_index: 0,
            tool_name: "Edit",
            category: "Edit",
            timestamp: "2026-06-24T12:00:00Z",
          },
        ],
      },
    ],
    has_more: false,
  })),
  signals: [] as AbortSignal[],
}));

vi.mock("../../api/generated/index", () => ({
  RecentEditsService: {
    getApiV1RecentEdits: mocks.getApiV1RecentEdits,
  },
}));

vi.mock("../../api/runtime.js", () => ({
  callGenerated: (fn: () => unknown, signal?: AbortSignal) => {
    if (signal) mocks.signals.push(signal);
    return fn();
  },
  isAbortError: vi.fn(() => false),
  configureGeneratedClient: () => {},
}));

vi.mock("../../stores/ui.svelte.js", () => ({
  ui: { scrollToOrdinal: mocks.scrollToOrdinal },
}));

vi.mock("../../stores/router.svelte.js", () => ({
  router: {
    route: "recent-edits",
    navigate: mocks.navigate,
    navigateToSession: mocks.navigateToSession,
  },
}));

vi.mock("../../stores/sessions.svelte.js", () => ({
  sessions: {
    filters: { project: "" },
    projects: [],
    setProjectFilter: mocks.setProjectFilter,
  },
}));

// @ts-ignore
import RecentEditsPage from "./RecentEditsPage.svelte";

describe("RecentEditsPage", () => {
  let component: ReturnType<typeof mount> | undefined;

  beforeEach(() => {
    vi.clearAllMocks();
    mocks.signals.length = 0;
    setLocale("en");
  });

  it("aborts the visible page read when unmounted", async () => {
    mocks.getApiV1RecentEdits.mockImplementationOnce(
      () => new Promise(() => {}),
    );
    component = mount(RecentEditsPage, { target: document.body });
    await tick();
    const signal = mocks.signals[0];

    unmount(component);
    component = undefined;

    expect(signal?.aborted).toBe(true);
  });

  afterEach(() => {
    if (component) {
      unmount(component);
      component = undefined;
    }
    document.body.innerHTML = "";
  });

  it("renders a file row with project and shortened path", async () => {
    component = mount(RecentEditsPage, { target: document.body });
    await tick();
    await tick(); // allow async load to settle

    expect(document.body.textContent).toContain("agentsview");
    expect(document.body.textContent).toContain("b/config.go");
  });

  it("expands to show edits on file row click", async () => {
    component = mount(RecentEditsPage, { target: document.body });
    await tick();
    await tick();

    const fileRowBtn = document.querySelector<HTMLButtonElement>(
      ".re-file-row",
    );
    expect(fileRowBtn).not.toBeNull();

    fileRowBtn!.click();
    await tick();

    const editsContainer = document.querySelector(".re-edits");
    expect(editsContainer).not.toBeNull();
  });

  it("shows truncation hint when edits_truncated is true", async () => {
    component = mount(RecentEditsPage, { target: document.body });
    await tick();
    await tick();

    const fileRowBtn = document.querySelector<HTMLButtonElement>(
      ".re-file-row",
    );
    fileRowBtn!.click();
    await tick();

    const truncated = document.querySelector(".re-truncated");
    expect(truncated).not.toBeNull();
  });

  it("calls scrollToOrdinal and navigateToSession on edit click", async () => {
    component = mount(RecentEditsPage, { target: document.body });
    await tick();
    await tick();

    const fileRowBtn = document.querySelector<HTMLButtonElement>(
      ".re-file-row",
    );
    fileRowBtn!.click();
    await tick();

    const editBtn = document.querySelector<HTMLButtonElement>(".re-edit");
    expect(editBtn).not.toBeNull();
    editBtn!.click();
    await tick();

    expect(mocks.scrollToOrdinal).toHaveBeenCalledWith(12, "s1");
    expect(mocks.navigateToSession).toHaveBeenCalledWith("s1");
  });

  it("derives the load-more offset from the loaded file count", async () => {
    const fileAt = (path: string, ts: string, sid: string) => ({
      project: "p",
      file_path: path,
      edit_count: 1,
      last_edited_at: ts,
      last_session_id: sid,
      edits_truncated: false,
      edits: [],
    });
    mocks.getApiV1RecentEdits
      .mockResolvedValueOnce({
        files: [
          fileAt("/x/a.go", "2026-06-24T12:00:00Z", "s1"),
          fileAt("/x/b.go", "2026-06-24T11:00:00Z", "s2"),
        ],
        has_more: true,
      })
      .mockResolvedValueOnce({
        files: [fileAt("/x/c.go", "2026-06-24T10:00:00Z", "s3")],
        has_more: false,
      });

    component = mount(RecentEditsPage, { target: document.body });
    await tick();
    await tick();

    const loadMoreBtn =
      document.querySelector<HTMLButtonElement>(".re-load-more");
    expect(loadMoreBtn).not.toBeNull();
    loadMoreBtn!.click();
    await tick();
    await tick();

    // The second request must start at the loaded file count (2), not a
    // blindly pre-incremented offset, so a failed page can't skip rows.
    const calls = mocks.getApiV1RecentEdits.mock.calls as unknown as Array<
      [{ offset: number }]
    >;
    expect(calls).toHaveLength(2);
    expect(calls[0]![0].offset).toBe(0);
    expect(calls[1]![0].offset).toBe(2);
  });

  it("debounces the search box and sends the term as a reset request", async () => {
    vi.useFakeTimers();
    try {
      component = mount(RecentEditsPage, { target: document.body });
      await tick();
      await tick(); // initial load (call #1, no search)

      const searchInput = document.querySelector<HTMLInputElement>(
        'input[aria-label="Filter by file path"]',
      );
      expect(searchInput).not.toBeNull();
      searchInput!.value = "config";
      searchInput!.dispatchEvent(new Event("input", { bubbles: true }));

      // Within the debounce window, no extra request fires.
      const before = mocks.getApiV1RecentEdits.mock.calls.length;
      vi.advanceTimersByTime(200);
      expect(mocks.getApiV1RecentEdits.mock.calls.length).toBe(before);

      // After the window elapses, exactly one request fires with the trimmed
      // term and a reset offset (0), so paging restarts from the top.
      vi.advanceTimersByTime(60);
      await tick();
      await tick();
      const calls = mocks.getApiV1RecentEdits.mock.calls as unknown as Array<
        [{ offset: number; search?: string }]
      >;
      expect(calls.length).toBe(before + 1);
      const last = calls[calls.length - 1]![0];
      expect(last.search).toBe("config");
      expect(last.offset).toBe(0);
    } finally {
      vi.useRealTimers();
    }
  });
});
