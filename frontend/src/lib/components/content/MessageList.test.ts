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
import { sessions } from "../../stores/sessions.svelte.js";
import { ui } from "../../stores/ui.svelte.js";
import { readProgress } from "../../stores/read-progress.svelte.js";
import { setLocale } from "../../i18n/index.js";

const virtualizerMock = vi.hoisted(() => ({
  options: { count: 0 },
  scrollOffset: 0,
  getVirtualItems: vi.fn<() => unknown[]>(() => []),
  getTotalSize: vi.fn(() => 120),
  measureElement: vi.fn(),
  scrollToIndex: vi.fn(),
  scrollToOffset: vi.fn(),
  getOffsetForIndex: vi.fn(),
  scrollRect: { height: 300 },
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

describe("MessageList follow cancellation", () => {
  let component: ReturnType<typeof mount> | undefined;
  let rafSpy: ReturnType<typeof vi.spyOn>;

  beforeEach(() => {
    vi.clearAllMocks();
    messages.clear();
    sessions.activeSessionId = "s1";
    messages.sessionId = "s1";
    messages.messages = [makeMessage(10)];
    messages.messageCount = 11;
    messages.hasOlder = true;
    ui.followLatest = true;
    ui.followLatestRequest = 1;
    ui.sortNewestFirst = false;
    ui.selectedOrdinal = null;
    ui.pendingScrollOrdinal = null;
    ui.pendingScrollSession = null;
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
});
describe("MessageList read progress", () => {
  let component: ReturnType<typeof mount> | undefined;
  let rafSpy: ReturnType<typeof vi.spyOn>;

  beforeEach(() => {
    vi.clearAllMocks();
    messages.clear();
    sessions.activeSessionId = "s1";
    messages.sessionId = "s1";
    messages.messages = [1, 2, 3, 4, 5].map(makeMessage);
    messages.messageCount = 5;
    ui.sortNewestFirst = false;
    readProgress.clear("s1");
    readProgress.baseline("s1", 3, 3);
    virtualizerMock.scrollOffset = 0;
    virtualizerMock.scrollRect.height = 300;
    virtualizerMock.getVirtualItems.mockReturnValue(
      [0, 1, 2, 3, 4].map((index) => ({
        index,
        key: `row-${index}`,
        start: index * 100,
        end: (index + 1) * 100,
      })),
    );
    rafSpy = vi
      .spyOn(window, "requestAnimationFrame")
      .mockImplementation((cb: FrameRequestCallback) => {
        window.setTimeout(() => cb(performance.now()), 0);
        return 1;
      });
  });

  afterEach(() => {
    if (component) unmount(component);
    component = undefined;
    rafSpy.mockRestore();
    readProgress.clear("s1");
    messages.clear();
    sessions.activeSessionId = null;
    ui.sortNewestFirst = false;
    ui.showAllBlocks();
    document.body.innerHTML = "";
  });

  it("places one divider at the next ordinal and clears it after it is visible", async () => {
    component = mount(MessageList, { target: document.body });
    await tick();

    expect(document.querySelectorAll(".read-progress-divider")).toHaveLength(1);
    expect(document.body.textContent).toContain("New messages");

    const scroller = document.querySelector<HTMLElement>(".message-list-scroll");
    expect(scroller).not.toBeNull();
    virtualizerMock.scrollOffset = 300;
    scroller!.dispatchEvent(new Event("scroll"));
    await vi.waitFor(() => {
      expect(readProgress.get("s1")).toEqual({ ordinal: 5, messageCount: 5 });
    });
    await tick();

    expect(document.querySelectorAll(".read-progress-divider")).toHaveLength(0);
  });

  it("separates unread and read ranges in newest first and filtered views", async () => {
    messages.loading = true;
    ui.sortNewestFirst = true;
    component = mount(MessageList, { target: document.body });
    await tick();

    expect(document.querySelectorAll(".read-progress-divider")).toHaveLength(1);
    expect(document.querySelectorAll(".virtual-row")[2]?.textContent).toContain("Earlier messages");
    expect(document.querySelectorAll(".virtual-row")[2]?.textContent).toContain("msg 3");

    ui.sortNewestFirst = false;
    ui.setBlockVisible("user", false);
    await tick();

    expect(document.querySelectorAll(".read-progress-divider")).toHaveLength(1);
    expect(document.body.textContent).toContain("msg 5");
  });

  it("clears a session visible count when the list switches sessions", async () => {
    messages.messages = [0, 1, 2, 3].map(makeMessage);
    messages.messageCount = 4;
    readProgress.clear("s1");
    readProgress.baseline("s1", 3, 4);
    component = mount(MessageList, { target: document.body });
    await tick();

    messages.sessionId = "s2";
    await tick();

    expect(readProgress.get("s1")).toEqual({ ordinal: 3, messageCount: 4 });
    expect(readProgress.hasUnread("s1", 5)).toBe(true);
  });

  it("hides the newest-first divider for a fully read transcript", async () => {
    ui.sortNewestFirst = true;
    readProgress.clear("s1");
    readProgress.baseline("s1", 5, 6);
    component = mount(MessageList, { target: document.body });
    await tick();

    expect(document.querySelectorAll(".read-progress-divider")).toHaveLength(0);
  });

  it("hides the newest-first divider after new output becomes visible", async () => {
    ui.sortNewestFirst = true;
    component = mount(MessageList, { target: document.body });
    await tick();

    expect(document.querySelectorAll(".read-progress-divider")).toHaveLength(1);
    document.querySelector<HTMLElement>(".message-list-scroll")?.dispatchEvent(
      new Event("scroll"),
    );
    await vi.waitFor(() => {
      expect(readProgress.get("s1")).toEqual({ ordinal: 5, messageCount: 5 });
    });
    await tick();

    expect(document.querySelectorAll(".read-progress-divider")).toHaveLength(0);
  });

  it("places the divider inside a tool group and does not mark every submessage read", async () => {
    messages.messages = [4, 5].map((ordinal) => ({
      ...makeMessage(ordinal),
      role: "assistant",
      content: "",
      content_length: 0,
      has_tool_use: true,
    }));
    messages.messageCount = 6;
    readProgress.clear("s1");
    readProgress.baseline("s1", 3, 4);
    virtualizerMock.getVirtualItems.mockReturnValue([{
      index: 0,
      key: "tool-group",
      start: 0,
      end: 100,
    }]);
    component = mount(MessageList, { target: document.body });
    await tick();

    const boundary = document.querySelector(".read-progress-divider");
    expect(boundary?.nextElementSibling?.getAttribute("data-message-ordinal")).toBe("4");

    document.querySelector<HTMLElement>(".message-list-scroll")?.dispatchEvent(
      new Event("scroll"),
    );
    await tick();

    expect(readProgress.get("s1")).toEqual({ ordinal: 3, messageCount: 4 });
  });

  it("records a normal row when its observer reports it visible", async () => {
    const originalObserver = globalThis.IntersectionObserver;
    const callbacks: IntersectionObserverCallback[] = [];
    class ObserverMock {
      constructor(callback: IntersectionObserverCallback) {
        callbacks.push(callback);
      }

      observe() {}
      disconnect() {}
      unobserve() {}
      takeRecords() { return []; }
      root = null;
      rootMargin = "0px";
      thresholds = [];
    }
    Object.defineProperty(globalThis, "IntersectionObserver", {
      configurable: true,
      writable: true,
      value: ObserverMock,
    });
    try {
      component = mount(MessageList, { target: document.body });
      await tick();

      callbacks[3]!([
        { isIntersecting: true } as IntersectionObserverEntry,
      ], {} as IntersectionObserver);

      expect(readProgress.get("s1")).toEqual({ ordinal: 4, messageCount: 5 });
    } finally {
      Object.defineProperty(globalThis, "IntersectionObserver", {
        configurable: true,
        writable: true,
        value: originalObserver,
      });
    }
  });

  it("acknowledges a trailing system message after its last displayable row is visible", async () => {
    const originalObserver = globalThis.IntersectionObserver;
    const callbacks: IntersectionObserverCallback[] = [];
    class ObserverMock {
      constructor(callback: IntersectionObserverCallback) {
        callbacks.push(callback);
      }

      observe() {}
      disconnect() {}
      unobserve() {}
      takeRecords() { return []; }
      root = null;
      rootMargin = "0px";
      thresholds = [];
    }
    Object.defineProperty(globalThis, "IntersectionObserver", {
      configurable: true,
      writable: true,
      value: ObserverMock,
    });
    try {
      messages.messages = [
        makeMessage(0),
        makeMessage(1),
        { ...makeMessage(2), is_system: true },
      ];
      messages.messageCount = 3;
      readProgress.clear("s1");
      readProgress.baseline("s1", 0, 1);
      virtualizerMock.getVirtualItems.mockReturnValue([0, 1].map((index) => ({
        index,
        key: `row-${index}`,
        start: index * 100,
        end: (index + 1) * 100,
      })));
      component = mount(MessageList, { target: document.body });
      await tick();

      callbacks[1]!([
        { isIntersecting: true } as IntersectionObserverEntry,
      ], {} as IntersectionObserver);

      expect(readProgress.get("s1")).toEqual({
        ordinal: 1,
        messageCount: 2,
        totalMessageCount: 3,
      });
      messages.sessionId = "s2";
      await tick();
      expect(readProgress.hasUnread("s1", 3)).toBe(false);
    } finally {
      Object.defineProperty(globalThis, "IntersectionObserver", {
        configurable: true,
        writable: true,
        value: originalObserver,
      });
    }
  });
});
