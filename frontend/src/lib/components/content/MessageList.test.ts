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
import type { Message } from "../../api/types.js";
import { messages } from "../../stores/messages.svelte.js";
import { readProgress } from "../../stores/read-progress.svelte.js";
import { sessions } from "../../stores/sessions.svelte.js";
import { ui } from "../../stores/ui.svelte.js";
import { setLocale } from "../../i18n/index.js";

const virtualizerMock = vi.hoisted(() => ({
  options: { count: 0 },
  scrollOffset: 0,
  scrollRect: { height: 200 },
  getVirtualItems: vi.fn<
    () => Array<{
      index: number;
      key: string;
      start: number;
      end: number;
    }>
  >(() => []),
  getTotalSize: vi.fn(() => 120),
  measureElement: vi.fn(),
  scrollToIndex: vi.fn(),
  scrollToOffset: vi.fn(),
  getOffsetForIndex: vi.fn(),
}));

vi.mock("../../virtual/createVirtualizer.svelte.js", () => ({
  createVirtualizer: (
    optsFn: () => { count: number },
  ) => ({
    get instance() {
      virtualizerMock.options.count = optsFn().count;
      return virtualizerMock;
    },
  }),
}));

// @ts-ignore
import MessageList from "./MessageList.svelte";

function makeMessage(ordinal: number): Message {
  return {
    id: ordinal + 1,
    session_id: "s1",
    ordinal,
    role: ordinal % 2 === 0 ? "user" : "assistant",
    content: `msg ${ordinal}`,
    timestamp: new Date(ordinal * 1000).toISOString(),
    has_thinking: false,
    thinking_text: "",
    has_tool_use: false,
    content_length: 6,
    model: "",
    token_usage: null,
    context_tokens: 0,
    output_tokens: 0,
    has_context_tokens: false,
    has_output_tokens: false,
    is_system: false,
  };
}

function deferred<T>() {
  let resolve!: (value: T | PromiseLike<T>) => void;
  const promise = new Promise<T>((res) => {
    resolve = res;
  });
  return { promise, resolve };
}

function setVirtualRows(count: number) {
  virtualizerMock.getVirtualItems.mockReturnValue(
    Array.from({ length: count }, (_, index) => ({
      index,
      key: `row-${index}`,
      start: index * 100,
      end: index * 100 + 100,
    })),
  );
}

describe("MessageList follow cancellation", () => {
  let component: ReturnType<typeof mount> | undefined;
  let rafSpy: ReturnType<typeof vi.spyOn>;

  beforeEach(() => {
    vi.clearAllMocks();
    virtualizerMock.scrollOffset = 0;
    virtualizerMock.scrollRect.height = 200;
    messages.clear();
    sessions.activeSessionId = "s1";
    messages.sessionId = "s1";
    messages.messages = [makeMessage(10)];
    messages.messageCount = 11;
    messages.activeSessionToken = "current";
    messages.hasOlder = true;
    ui.followLatest = true;
    ui.followLatestRequest = 1;
    ui.sortNewestFirst = false;
    ui.selectedOrdinal = null;
    ui.pendingScrollOrdinal = null;
    ui.pendingScrollSession = null;
    readProgress.reset();
    setVirtualRows(1);
    rafSpy = vi
      .spyOn(window, "requestAnimationFrame")
      .mockImplementation((cb: FrameRequestCallback) => {
        window.setTimeout(() => cb(performance.now()), 0);
        return 1;
      });
  });

  afterEach(() => {
    setLocale("en");
    if (component) {
      unmount(component);
      component = undefined;
    }
    rafSpy.mockRestore();
    messages.clear();
    sessions.activeSessionId = null;
    ui.followLatest = false;
    readProgress.reset();
    document.body.innerHTML = "";
  });

  it("renders empty and loading states in Simplified Chinese", async () => {
    setLocale("zh-CN");
    sessions.activeSessionId = null;
    messages.clear();

    component = mount(MessageList, { target: document.body });
    await tick();

    expect(document.body.textContent).toContain("选择一个会话查看消息");

    unmount(component);
    component = undefined;
    document.body.innerHTML = "";

    sessions.activeSessionId = "s1";
    messages.sessionId = "s1";
    messages.messages = [];
    messages.loading = true;

    component = mount(MessageList, { target: document.body });
    await tick();

    expect(document.body.textContent).toContain("正在加载消息...");
  });

  it("keeps delayed ordinal navigation alive after follow latest is disabled", async () => {
    const loaded = deferred<void>();
    const ensureSpy = vi
      .spyOn(messages, "ensureOrdinalLoaded")
      .mockImplementation(async () => {
        await loaded.promise;
        messages.messages = [makeMessage(0), makeMessage(10)];
      });

    component = mount(MessageList, { target: document.body });
    await tick();

    ui.setFollowLatest(false);
    (
      component as ReturnType<typeof mount> & {
        scrollToOrdinal: (ordinal: number) => void;
      }
    ).scrollToOrdinal(0);
    await tick();

    loaded.resolve();
    await tick();
    await vi.waitFor(() => {
      expect(virtualizerMock.scrollToIndex).toHaveBeenCalled();
    });

    expect(ensureSpy).toHaveBeenCalledWith(0);
    expect(virtualizerMock.scrollToIndex).toHaveBeenCalledWith(0, {
      align: "start",
    });
  });

  it("renders the unread divider before the first unread message", async () => {
    messages.messages = [
      makeMessage(0),
      makeMessage(1),
      makeMessage(2),
      makeMessage(3),
    ];
    messages.messageCount = 4;
    messages.activeSessionToken = "current";
    setVirtualRows(4);
    readProgress.baseline("s1", "previous", 1);

    component = mount(MessageList, { target: document.body });
    await tick();

    const divider = document.querySelector(".read-progress-divider");
    expect(divider?.textContent).toContain("New messages");
    expect(
      divider?.closest(".virtual-row")?.getAttribute("data-index"),
    ).toBe("2");
  });

  it("keeps the newest-first divider on the first already-read row", async () => {
    messages.messages = [
      makeMessage(0),
      makeMessage(1),
      makeMessage(2),
      makeMessage(3),
    ];
    messages.messageCount = 4;
    messages.activeSessionToken = "current";
    ui.sortNewestFirst = true;
    setVirtualRows(4);
    readProgress.baseline("s1", "previous", 1);

    component = mount(MessageList, { target: document.body });
    await tick();

    const divider = document.querySelector(".read-progress-divider");
    expect(divider?.textContent).toContain("Earlier messages");
    expect(
      divider?.closest(".virtual-row")?.getAttribute("data-index"),
    ).toBe("2");
  });

  it("does not mark newest-first updates read while only older rows are visible", async () => {
    messages.messages = [
      makeMessage(0),
      makeMessage(1),
      makeMessage(2),
      makeMessage(3),
      makeMessage(4),
    ];
    messages.messageCount = 5;
    messages.activeSessionToken = "current";
    ui.sortNewestFirst = true;
    setVirtualRows(5);
    virtualizerMock.scrollOffset = 300;
    readProgress.baseline("s1", "previous", 1);

    component = mount(MessageList, { target: document.body });
    await tick();
    document.querySelector<HTMLElement>(".message-list-scroll")
      ?.dispatchEvent(new Event("scroll"));

    await new Promise((resolve) => window.setTimeout(resolve, 20));
    expect(readProgress.get("s1")?.token).toBe("previous");
  });

  it("requires the newest unread endpoint before acknowledging a direct boundary jump", async () => {
    messages.messages = [
      makeMessage(0),
      makeMessage(1),
      makeMessage(2),
      makeMessage(3),
      makeMessage(4),
    ];
    messages.messageCount = 5;
    messages.activeSessionToken = "current";
    ui.sortNewestFirst = true;
    virtualizerMock.getVirtualItems.mockReturnValue([
      { index: 2, key: "row-2", start: 0, end: 100 },
    ]);
    readProgress.baseline("s1", "previous", 1);

    component = mount(MessageList, { target: document.body });
    await tick();
    await new Promise((resolve) => window.setTimeout(resolve, 20));

    expect(readProgress.get("s1")?.token).toBe("previous");

    virtualizerMock.getVirtualItems.mockReturnValue([
      { index: 0, key: "row-0", start: 0, end: 100 },
    ]);
    document.querySelector<HTMLElement>(".message-list-scroll")
      ?.dispatchEvent(new Event("scroll"));
    await new Promise((resolve) => window.setTimeout(resolve, 20));

    expect(readProgress.get("s1")?.token).toBe("current");
  });

  it("marks a short newest-first transcript read when its unread boundary is initially visible", async () => {
    messages.messages = [makeMessage(0), makeMessage(1)];
    messages.messageCount = 2;
    messages.activeSessionToken = "current";
    ui.sortNewestFirst = true;
    setVirtualRows(2);
    readProgress.baseline("s1", "previous", 0);

    component = mount(MessageList, { target: document.body });
    await tick();
    await new Promise((resolve) => window.setTimeout(resolve, 20));

    expect(readProgress.get("s1")?.token).toBe("current");
  });

  it("does not acknowledge an earlier edit from the unchanged newest row", async () => {
    messages.messages = [
      makeMessage(0),
      makeMessage(1),
      makeMessage(2),
    ];
    messages.messageCount = 3;
    messages.activeSessionToken = "current";
    messages.activeSessionUnreadOrdinal = 0;
    ui.sortNewestFirst = true;
    setVirtualRows(1);
    readProgress.baseline("s1", "previous", 2);

    component = mount(MessageList, { target: document.body });
    await tick();
    await new Promise((resolve) => window.setTimeout(resolve, 20));

    expect(readProgress.get("s1")?.token).toBe("previous");

    virtualizerMock.getVirtualItems.mockReturnValue([
      { index: 2, key: "row-2", start: 0, end: 100 },
    ]);
    document.querySelector<HTMLElement>(".message-list-scroll")
      ?.dispatchEvent(new Event("scroll"));
    await new Promise((resolve) => window.setTimeout(resolve, 20));

    expect(readProgress.get("s1")?.token).toBe("current");
  });

  it("keeps an appended unread boundary immutable before traversal", async () => {
    messages.messages = [
      makeMessage(0),
      makeMessage(1),
      makeMessage(2),
      makeMessage(3),
      makeMessage(4),
    ];
    messages.messageCount = 5;
    messages.activeSessionToken = "current";
    messages.activeSessionUnreadOrdinal = null;
    virtualizerMock.getVirtualItems.mockReturnValue([
      { index: 4, key: "row-4", start: 0, end: 100 },
    ]);
    readProgress.baseline("s1", "previous", 1);

    component = mount(MessageList, { target: document.body });
    await tick();
    await new Promise((resolve) => window.setTimeout(resolve, 20));

    expect(readProgress.get("s1")).toMatchObject({
      token: "previous",
      ordinal: 1,
    });
  });

  it("does not acknowledge a boundary hidden by a visible ordinal gap", async () => {
    messages.messages = [makeMessage(0), makeMessage(1), makeMessage(2)];
    messages.messageCount = 3;
    messages.activeSessionToken = "current";
    messages.activeSessionUnreadOrdinal = 1;
    virtualizerMock.getVirtualItems.mockReturnValue([
      { index: 0, key: "row-0", start: 0, end: 100 },
      { index: 2, key: "row-2", start: 100, end: 200 },
    ]);
    readProgress.baseline("s1", "previous", 2);

    component = mount(MessageList, { target: document.body });
    await tick();
    await new Promise((resolve) => window.setTimeout(resolve, 20));

    expect(readProgress.get("s1")?.token).toBe("previous");
  });

  it("skips a hidden system ordinal when inferring an appended boundary", async () => {
    messages.messages = [
      makeMessage(0),
      { ...makeMessage(1), is_system: true },
      makeMessage(2),
    ];
    messages.messageCount = 3;
    messages.activeSessionToken = "current";
    messages.activeSessionUnreadOrdinal = null;
    setVirtualRows(2);
    readProgress.baseline("s1", "previous", 0);

    component = mount(MessageList, { target: document.body });
    await tick();
    await new Promise((resolve) => window.setTimeout(resolve, 20));

    expect(readProgress.get("s1")?.token).toBe("current");
  });

  it("acknowledges a revised transcript with only system messages", async () => {
    messages.messages = [{ ...makeMessage(0), is_system: true }];
    messages.messageCount = 1;
    messages.activeSessionToken = "current";
    setVirtualRows(0);
    readProgress.baseline("s1", "previous", 0);

    component = mount(MessageList, { target: document.body });
    await tick();
    await new Promise((resolve) => window.setTimeout(resolve, 20));

    expect(readProgress.get("s1")?.token).toBe("current");
  });
});
