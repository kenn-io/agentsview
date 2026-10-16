// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import { Virtualizer, type VirtualizerOptions } from "@tanstack/virtual-core";
import { controlFrames } from "../../test/frames.js";
import { realVirtualizer } from "../../test/virtualizer.js";
import type { DbMessage as Message } from "../../api/generated/index.js";
import { inSessionSearch } from "../../stores/inSessionSearch.svelte.js";
import { messages } from "../../stores/messages.svelte.js";
import { readProgress } from "../../stores/read-progress.svelte.js";
import { sessions } from "../../stores/sessions.svelte.js";
import { ui, type ScrollCall } from "../../stores/ui.svelte.js";
import { toolCallFingerprint } from "../../utils/tool-call-fingerprint.js";
import { setLocale } from "../../i18n/index.js";

const virtualizerMock = vi.hoisted(() => ({
  real: undefined as Virtualizer<HTMLElement, HTMLElement> | undefined,
  options: { count: 0 },
  scrollOffset: 0,
  notify: () => {},
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

vi.mock("../../virtual/createVirtualizer.svelte.js", async (importOriginal) => {
  const original =
    await importOriginal<typeof import("../../virtual/createVirtualizer.svelte.js")>();
  const { createSubscriber } = await import("svelte/reactivity");
  return {
    ...original,
    createVirtualizer: (optsFn: Parameters<typeof original.createVirtualizer>[0]) => {
      if (virtualizerMock.real) {
        const instance = virtualizerMock.real;
        const {
          getScrollElement,
          observeElementRect,
          observeElementOffset,
          scrollToFn,
          measureElement,
        } = instance.options;
        return original.createVirtualizer(() => ({
          ...optsFn(),
          getScrollElement,
          observeElementRect,
          observeElementOffset,
          scrollToFn,
          measureElement,
        }));
      }
      const subscribe = createSubscriber((update) => {
        virtualizerMock.notify = update;
      });
      return {
        get instance() {
          subscribe();
          virtualizerMock.options.count = optsFn().count;
          return virtualizerMock;
        },
      };
    },
  };
});

vi.mock("@tanstack/virtual-core", async (importOriginal) => {
  const original = await importOriginal<typeof import("@tanstack/virtual-core")>();
  return {
    ...original,
    Virtualizer: class extends original.Virtualizer<HTMLElement, HTMLElement> {
      constructor(options: VirtualizerOptions<HTMLElement, HTMLElement>) {
        super(options);
        if (virtualizerMock.real) {
          // Let the component own the same real instance whose scroll element the test observes.
          virtualizerMock.real.setOptions(options);
          return virtualizerMock.real;
        }
      }
    },
  };
});

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

function realJumpScroll() {
  const fixture = realVirtualizer(Virtualizer, {
    count: messages.messages.length,
    onChange: () => virtualizerMock.notify(),
  });
  virtualizerMock.real = fixture.instance;
  return fixture;
}

async function flushScrollUpdates() {
  await tick();
  if (vi.isFakeTimers()) await vi.advanceTimersByTimeAsync(0);
  else await new Promise((resolve) => setTimeout(resolve, 0));
  await tick();
}

describe("MessageList follow cancellation", () => {
  let component: ReturnType<typeof mount> | undefined;
  let rafSpy: ReturnType<typeof vi.spyOn>;

  beforeEach(() => {
    vi.clearAllMocks();
    virtualizerMock.getOffsetForIndex.mockReset();
    virtualizerMock.scrollOffset = 0;
    virtualizerMock.scrollToIndex.mockReset();
    virtualizerMock.scrollRect.height = 200;
    inSessionSearch.close();
    inSessionSearch.clearQuery();
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
    ui.showAllBlocks();
    ui.setTranscriptMode("normal");
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

  afterEach(async () => {
    setLocale("en");
    if (component) {
      await unmount(component);
      component = undefined;
    }
    virtualizerMock.real?._didMount()();
    virtualizerMock.real = undefined;
    vi.useRealTimers();
    rafSpy.mockRestore();
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
    inSessionSearch.close();
    inSessionSearch.clearQuery();
    messages.clear();
    sessions.activeSessionId = null;
    ui.followLatest = false;
    readProgress.reset();
    document.body.innerHTML = "";
  });

  it("keeps an ordinal jump on its message through an older load past five seconds", async () => {
    const { flushFrames } = controlFrames();
    vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout"] });
    let now = 0;
    vi.spyOn(performance, "now").mockImplementation(() => now);
    ui.followLatest = false;
    messages.messages = [makeMessage(10), makeMessage(11)];
    const { element } = realJumpScroll();
    component = mount(MessageList, { target: document.body });
    await flushScrollUpdates();

    component.scrollToOrdinal(11);
    expect(element.scrollTop).toBe(120);
    flushFrames();
    await flushScrollUpdates();
    messages.loadingOlder = true;
    await flushScrollUpdates();
    now = 6000;
    await vi.advanceTimersByTimeAsync(6000);
    messages.messages = [makeMessage(8), makeMessage(9), ...messages.messages];
    messages.loadingOlder = false;
    await flushScrollUpdates();

    flushFrames();
    await flushScrollUpdates();
    expect(element.scrollTop).toBe(300);
  });

  it.each([false, true])(
    "corrects an upward ordinal jump when an already measured preceding row grows with loadingOlder=%s",
    async (loadingOlder) => {
      const { flushFrames } = controlFrames();
      vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout"] });
      ui.followLatest = false;
      messages.messages = Array.from({ length: 10 }, (_, index) => makeMessage(index));
      const { instance, element, measureRow } = realJumpScroll();
      component = mount(MessageList, { target: document.body });
      await flushScrollUpdates();
      measureRow(0, 110);
      element.scrollTop = 500;
      await vi.advanceTimersByTimeAsync(0);
      await flushScrollUpdates();
      component.scrollToOrdinal(1);
      flushFrames();
      await flushScrollUpdates();
      flushFrames();
      expect(element.scrollTop).toBe(110);
      expect(instance.scrollDirection).toBe("backward");

      messages.loadingOlder = loadingOlder;
      await flushScrollUpdates();
      measureRow(0, 300);
      instance.getVirtualItems();
      await flushScrollUpdates();

      expect(element.scrollTop).toBe(300);
    },
  );

  it.each([125, 450])(
    "ends the hold when scrolling to %s without a preceding input event",
    async (offset) => {
      const { flushFrames } = controlFrames();
      vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout"] });
      ui.followLatest = false;
      messages.hasOlder = false;
      messages.messages = Array.from({ length: 10 }, (_, index) => makeMessage(index));
      const { instance, element, measureRow } = realJumpScroll();
      component = mount(MessageList, { target: document.body });
      await flushScrollUpdates();
      // The fixture owns geometry; forward its scrolling to the component's real event handler.
      const container = document.querySelector<HTMLElement>(".message-list-scroll")!;
      Object.defineProperty(container, "scrollTop", { get: () => element.scrollTop });
      element.addEventListener("scroll", () => container.dispatchEvent(new Event("scroll")));
      component.scrollToOrdinal(1);
      flushFrames();
      await flushScrollUpdates();
      flushFrames();
      await flushScrollUpdates();
      expect(element.scrollTop).toBe(120);

      element.scrollTop = offset;
      await flushScrollUpdates();
      flushFrames();
      expect(element.scrollTop).toBe(offset);

      // Isolate the expired hold from TanStack's normal compensation for row growth.
      instance.shouldAdjustScrollPositionOnItemSizeChange = () => false;
      measureRow(0, 300);
      instance.getVirtualItems();
      await flushScrollUpdates();
      flushFrames();
      expect(element.scrollTop).toBe(offset);
    },
  );

  it("cancels reconciliation when the jump expires between newest-first appends", async () => {
    const { flushFrames } = controlFrames();
    vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout"] });
    let now = 0;
    vi.spyOn(performance, "now").mockImplementation(() => now);
    ui.followLatest = false;
    ui.sortNewestFirst = true;
    messages.messages = [makeMessage(10), makeMessage(11), makeMessage(12), makeMessage(13)];
    const { instance, element, measureRow } = realJumpScroll();
    component = mount(MessageList, { target: document.body });
    await flushScrollUpdates();
    measureRow(0, 200);
    measureRow(1, 300);
    measureRow(2, 100);
    measureRow(3, 150);
    component.scrollToOrdinal(11);
    flushFrames();
    await flushScrollUpdates();
    flushFrames();
    expect(element.scrollTop).toBe(500);

    now = 4999;
    await vi.advanceTimersByTimeAsync(4999);
    messages.messages = [...messages.messages, makeMessage(14)];
    await flushScrollUpdates();
    measureRow(0, 300);
    await flushScrollUpdates();
    expect(element.scrollTop).toBe(800);
    now = 5000;
    await vi.advanceTimersByTimeAsync(1);
    messages.messages = [...messages.messages, makeMessage(15)];
    await flushScrollUpdates();
    instance.getVirtualItems();
    flushFrames();
    await flushScrollUpdates();
    flushFrames();

    expect(element.scrollTop).toBe(800);
  });

  it.each(["search closes", "match disappears", "ordinal target is hidden"])(
    "ends scrolling when the %s",
    async (change) => {
      const { frames, flushFrames } = controlFrames();
      vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout"] });
      ui.followLatest = false;
      messages.hasOlder = false;
      const ordinalJump = change === "ordinal target is hidden";
      const ordinal = ordinalJump ? 4 : 11;
      const offset = ordinalJump ? 400 : 1200;
      messages.messages = Array.from({ length: ordinalJump ? 12 : 40 }, (_, index) => ({
        ...makeMessage(index),
        content: index === 11 ? "needle" : `msg ${index}`,
      }));
      const { instance, element, measureRow } = realJumpScroll();
      const scrollSpy = vi.spyOn(instance, "scrollToIndex");
      const offsetSpy = vi.spyOn(instance, "scrollToOffset");
      component = mount(MessageList, { target: document.body });
      await flushScrollUpdates();
      if (ordinalJump) {
        ui.selectedOrdinal = ordinal;
        component.scrollToOrdinal(ordinal);
      } else {
        measureRow(0, 200);
        inSessionSearch.open();
        inSessionSearch.query = "needle";
        await vi.advanceTimersByTimeAsync(200);
        expect(inSessionSearch.resolvedCurrent?.ordinal).toBe(ordinal);
      }
      await flushScrollUpdates();
      expect(element.scrollTop).toBe(offset);
      expect(frames.size).toBeGreaterThan(0);
      expect(ui.selectedOrdinal).toBe(ordinal);

      if (change === "search closes") inSessionSearch.close();
      else ui.setBlockVisible(ordinalJump ? "user" : "assistant", false);
      await flushScrollUpdates();
      scrollSpy.mockClear();
      offsetSpy.mockClear();
      // Isolate queued index correction from ordinary size anchoring.
      instance.shouldAdjustScrollPositionOnItemSizeChange = () => false;
      measureRow(0, ordinalJump ? 300 : 500);
      instance.getVirtualItems();
      flushFrames();
      await flushScrollUpdates();
      flushFrames();
      if (change === "search closes") {
        await vi.advanceTimersByTimeAsync(100);
        expect(scrollSpy).not.toHaveBeenCalled();
        expect(offsetSpy).not.toHaveBeenCalled();
      }

      expect(element.scrollTop).toBe(offset);
      expect(ui.selectedOrdinal).toBe(ordinal);
    },
  );

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
    const ensureSpy = vi.spyOn(messages, "ensureOrdinalLoaded").mockImplementation(async () => {
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

  function withCall(message: Message, toolUseId: string, inputJson?: string): Message {
    return {
      ...message,
      tool_calls: [
        { tool_name: "Grep", category: "Search", tool_use_id: toolUseId, input_json: inputJson },
      ],
    } as Message;
  }

  type JumpList = ReturnType<typeof mount> & {
    scrollToOrdinal: (ordinal: number, call?: ScrollCall) => void;
  };

  it("cancels a queued jump before the first frame of a new session", async () => {
    const { frames, flushFrames } = controlFrames();
    ui.followLatest = false;
    messages.messages = Array.from({ length: 40 }, (_, index) => makeMessage(index));
    const { instance, element, measureRow } = realJumpScroll();
    component = mount(MessageList, { target: document.body });
    await flushScrollUpdates();
    expect(instance.getVirtualItems().some((item) => item.index === 30)).toBe(false);
    ui.selectedOrdinal = 30;
    component.scrollToOrdinal(30);
    expect(element.scrollTop).toBeGreaterThan(0);
    expect(frames.size).toBeGreaterThan(0);

    sessions.activeSessionId = "s2";
    messages.sessionId = "s2";
    messages.messages = Array.from({ length: 40 }, (_, index) => ({
      ...makeMessage(index),
      session_id: "s2",
    }));
    await flushScrollUpdates();
    expect(element.scrollTop).toBe(0);
    expect(instance.options.getItemKey(30)).toBe("s2-m-30");
    measureRow(0, 300);
    instance.getVirtualItems();
    flushFrames();
    // Check before the staged jump's promise resumes and can cancel itself.
    expect(element.scrollTop).toBe(0);
    await flushScrollUpdates();
    flushFrames();
    expect(element.scrollTop).toBe(0);
    expect(ui.selectedOrdinal).toBe(30);
  });

  it("keeps following latest when the previous tool target is rewritten", async () => {
    const { flushFrames } = controlFrames();
    ui.followLatest = false;
    messages.messages = [makeMessage(10), withCall(makeMessage(11), "grep-call"), makeMessage(12)];
    const { instance, element, measureRow } = realJumpScroll();
    component = mount(MessageList, { target: document.body });
    await flushScrollUpdates();
    component.scrollToOrdinal(11, { index: 0, toolUseId: "grep-call" });
    await flushScrollUpdates();
    expect(element.scrollTop).toBe(100);

    component.scrollToLatest();
    messages.messages = [makeMessage(10), withCall(makeMessage(11), "other-call"), makeMessage(12)];
    await flushScrollUpdates();
    measureRow(0, 300);
    instance.getVirtualItems();
    await flushScrollUpdates();
    flushFrames();
    await flushScrollUpdates();
    flushFrames();

    expect(element.scrollTop).toBe(400);
  });

  it.each([
    "tool rewrite",
    "manual scroll",
    "new ordinal request",
    "tool rewrite with newer selection",
    "tool rewrite between estimate frames",
  ])("cancels a queued jump after a %s", async (change) => {
    const { frames, flushFrames } = controlFrames();
    // Advance the staged wait while leaving TanStack's earlier reconciliation queued.
    const flushEstimateFrame = () => {
      const [id, callback] = [...frames.entries()].at(-1)!;
      frames.delete(id);
      callback(0);
    };
    ui.followLatest = false;
    messages.messages = [makeMessage(10), withCall(makeMessage(11), "grep-call")];
    const { instance, element, measureRow } = realJumpScroll();
    component = mount(MessageList, { target: document.body });
    await flushScrollUpdates();

    if (change === "tool rewrite between estimate frames") {
      instance.setOptions({ ...instance.options, overscan: 0 });
      expect(instance.getVirtualItems().some((item) => item.index === 1)).toBe(false);
    }
    ui.selectedOrdinal = 11;
    component.scrollToOrdinal(11, { index: 0, toolUseId: "grep-call" });
    await flushScrollUpdates();
    expect(element.scrollTop).toBe(change === "tool rewrite between estimate frames" ? 120 : 100);

    if (change === "tool rewrite between estimate frames") {
      flushEstimateFrame();
      await flushScrollUpdates();
    }
    const loaded = deferred<void>();
    if (change.startsWith("tool rewrite")) {
      if (change === "tool rewrite with newer selection") ui.selectedOrdinal = 10;
      messages.messages = [makeMessage(10), withCall(makeMessage(11), "other-call")];
    } else if (change === "manual scroll") {
      document
        .querySelector<HTMLElement>(".message-list-scroll")!
        .dispatchEvent(new Event("wheel"));
    } else {
      vi.spyOn(messages, "ensureOrdinalLoaded").mockReturnValue(loaded.promise);
      component.scrollToOrdinal(0);
      expect(messages.ensureOrdinalLoaded).toHaveBeenCalledWith(0);
    }
    await flushScrollUpdates();
    if (change === "tool rewrite between estimate frames") {
      flushEstimateFrame();
      await flushScrollUpdates();
    }
    // At the top, default size anchoring cannot mask a stale index reconcile.
    instance.options.scrollToFn(0, {}, instance);
    measureRow(0, 300);
    instance.getVirtualItems();
    await flushScrollUpdates();
    flushFrames();
    await flushScrollUpdates();
    flushFrames();

    expect(element.scrollTop).toBe(0);
    expect(ui.selectedOrdinal).toBe(
      change.startsWith("tool rewrite")
        ? change === "tool rewrite with newer selection"
          ? 10
          : null
        : 11,
    );
    loaded.resolve();
    await flushScrollUpdates();
    flushFrames();
    await flushScrollUpdates();
    flushFrames();
    await flushScrollUpdates();
  });

  it.each([
    { newestFirst: false, align: "end" },
    { newestFirst: true, align: "start" },
  ])("follows the latest row with newest-first=$newestFirst", async ({ newestFirst, align }) => {
    const { flushFrames } = controlFrames();
    ui.sortNewestFirst = newestFirst;
    messages.messages = [makeMessage(10), makeMessage(11), makeMessage(12), makeMessage(13)];
    const { instance } = realJumpScroll();
    const scrollSpy = vi.spyOn(instance, "scrollToIndex");
    component = mount(MessageList, { target: document.body });
    await flushScrollUpdates();
    flushFrames();
    await flushScrollUpdates();
    flushFrames();
    await flushScrollUpdates();

    expect(scrollSpy.mock.lastCall?.[1]?.align).toBe(align);
  });

  it("drops a jump to a tool call once its ordinal holds a different message", async () => {
    vi.spyOn(messages, "ensureOrdinalLoaded").mockImplementation(async () => {
      // A resync renumbered the transcript, so ordinal 0 now holds another call.
      messages.messages = [withCall(makeMessage(0), "other-call"), makeMessage(10)];
    });

    component = mount(MessageList, { target: document.body });
    await tick();

    ui.setFollowLatest(false);
    ui.selectedOrdinal = 0;
    (component as JumpList).scrollToOrdinal(0, { index: 0, toolUseId: "grep-call" });
    await vi.waitFor(() => expect(ui.selectedOrdinal).toBeNull());
    expect(virtualizerMock.scrollToIndex).not.toHaveBeenCalled();
  });

  // Roo Code builds tool IDs from the tool name and message ordinal, so a rewrite can reuse one for another call.
  it("drops a jump when a rewrite reuses the tool ID for a call with other input", async () => {
    vi.spyOn(messages, "ensureOrdinalLoaded").mockImplementation(async () => {
      messages.messages = [
        withCall(makeMessage(0), "roocode:Grep:0", '{"pattern":"other"}'),
        makeMessage(10),
      ];
    });

    component = mount(MessageList, { target: document.body });
    await tick();

    ui.setFollowLatest(false);
    ui.selectedOrdinal = 0;
    (component as JumpList).scrollToOrdinal(0, {
      index: 0,
      toolUseId: "roocode:Grep:0",
      fingerprint: toolCallFingerprint("Grep", '{"pattern":"config"}'),
    });
    await vi.waitFor(() => expect(ui.selectedOrdinal).toBeNull());
    expect(virtualizerMock.scrollToIndex).not.toHaveBeenCalled();
  });

  it("keeps a fingerprinted jump to the same call after messages are appended", async () => {
    messages.loadedRevision = "r2";
    vi.spyOn(messages, "ensureOrdinalLoaded").mockImplementation(async () => {
      messages.messages = [
        withCall(makeMessage(0), "roocode:Grep:0", '{"pattern":"config"}'),
        makeMessage(10),
      ];
    });

    component = mount(MessageList, { target: document.body });
    await tick();

    ui.setFollowLatest(false);
    ui.selectedOrdinal = 0;
    (component as JumpList).scrollToOrdinal(0, {
      index: 0,
      toolUseId: "roocode:Grep:0",
      fingerprint: toolCallFingerprint("Grep", '{"pattern":"config"}'),
    });
    await vi.waitFor(() => {
      expect(virtualizerMock.scrollToIndex).toHaveBeenCalledWith(0, { align: "start" });
    });
    expect(ui.selectedOrdinal).toBe(0);
  });

  it("drops a jump to a call with no tool ID once a rewrite moves the transcript revision", async () => {
    messages.loadedRevision = "r1";
    vi.spyOn(messages, "ensureOrdinalLoaded").mockImplementation(async () => {
      // A resync replaced the call at this position with another one that also has no tool ID.
      messages.messages = [withCall(makeMessage(0), ""), makeMessage(10)];
      messages.loadedRevision = "r2";
    });

    component = mount(MessageList, { target: document.body });
    await tick();

    ui.setFollowLatest(false);
    ui.selectedOrdinal = 0;
    (component as JumpList).scrollToOrdinal(0, { index: 0, toolUseId: "", revision: "r1" });
    await vi.waitFor(() => expect(ui.selectedOrdinal).toBeNull());
    expect(virtualizerMock.scrollToIndex).not.toHaveBeenCalled();
  });

  it("drops a jump to a whole message once a rewrite moves the transcript revision", async () => {
    messages.loadedRevision = "r1";
    vi.spyOn(messages, "ensureOrdinalLoaded").mockImplementation(async () => {
      messages.messages = [makeMessage(0), makeMessage(10)];
      messages.loadedRevision = "r2";
    });

    component = mount(MessageList, { target: document.body });
    await tick();

    ui.setFollowLatest(false);
    ui.selectedOrdinal = 0;
    (component as JumpList).scrollToOrdinal(0, { toolUseId: "", revision: "r1" });
    await vi.waitFor(() => expect(ui.selectedOrdinal).toBeNull());
    expect(virtualizerMock.scrollToIndex).not.toHaveBeenCalled();
  });

  it("keeps a jump to a whole message while the revision still matches", async () => {
    messages.loadedRevision = "r1";
    vi.spyOn(messages, "ensureOrdinalLoaded").mockImplementation(async () => {
      messages.messages = [makeMessage(0), makeMessage(10)];
    });

    component = mount(MessageList, { target: document.body });
    await tick();

    ui.setFollowLatest(false);
    ui.selectedOrdinal = 0;
    (component as JumpList).scrollToOrdinal(0, { toolUseId: "", revision: "r1" });
    await vi.waitFor(() => {
      expect(virtualizerMock.scrollToIndex).toHaveBeenCalledWith(0, { align: "start" });
    });
    expect(ui.selectedOrdinal).toBe(0);
  });

  it("keeps a jump to a call with no tool ID while the revision still matches", async () => {
    messages.loadedRevision = "r1";
    vi.spyOn(messages, "ensureOrdinalLoaded").mockImplementation(async () => {
      messages.messages = [withCall(makeMessage(0), ""), makeMessage(10)];
    });

    component = mount(MessageList, { target: document.body });
    await tick();

    ui.setFollowLatest(false);
    ui.selectedOrdinal = 0;
    (component as JumpList).scrollToOrdinal(0, { index: 0, toolUseId: "", revision: "r1" });
    await vi.waitFor(() => {
      expect(virtualizerMock.scrollToIndex).toHaveBeenCalledWith(0, { align: "start" });
    });
    expect(ui.selectedOrdinal).toBe(0);
  });

  it("keeps a jump to a tool call when new messages arrive under it", async () => {
    messages.loadedRevision = "r1";
    vi.spyOn(messages, "ensureOrdinalLoaded").mockImplementation(async () => {
      messages.messages = [withCall(makeMessage(0), "grep-call"), makeMessage(10)];
      // A sync appends a message while the list re-renders the loaded page.
      requestAnimationFrame(() => {
        messages.messages = [...messages.messages, makeMessage(11)];
        messages.loadedRevision = "r2";
      });
    });

    component = mount(MessageList, { target: document.body });
    await tick();

    ui.setFollowLatest(false);
    ui.selectedOrdinal = 0;
    (component as JumpList).scrollToOrdinal(0, { index: 0, toolUseId: "grep-call" });
    await vi.waitFor(() => {
      expect(virtualizerMock.scrollToIndex).toHaveBeenCalledWith(0, { align: "start" });
    });
    expect(ui.selectedOrdinal).toBe(0);
  });

  it("renders an unknown revision divider at the earliest message", async () => {
    messages.messages = [makeMessage(0), makeMessage(1), makeMessage(2), makeMessage(3)];
    messages.messageCount = 4;
    messages.activeSessionToken = "current";
    setVirtualRows(4);
    readProgress.baseline("s1", "previous", 1);

    component = mount(MessageList, { target: document.body });
    await tick();

    const divider = document.querySelector(".read-progress-divider");
    expect(divider?.textContent).toContain("New messages");
    expect(divider?.closest(".virtual-row")?.getAttribute("data-index")).toBe("0");
  });

  it("suppresses the newest-first divider when no read history exists", async () => {
    messages.messages = [makeMessage(0), makeMessage(1), makeMessage(2), makeMessage(3)];
    messages.messageCount = 4;
    messages.activeSessionToken = "current";
    ui.sortNewestFirst = true;
    setVirtualRows(4);
    readProgress.baseline("s1", "previous", 1);

    component = mount(MessageList, { target: document.body });
    await tick();

    const divider = document.querySelector(".read-progress-divider");
    expect(divider).toBeNull();
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
    document.querySelector<HTMLElement>(".message-list-scroll")?.dispatchEvent(new Event("scroll"));

    await new Promise((resolve) => window.setTimeout(resolve, 20));
    expect(readProgress.get("s1")?.token).toBe("previous");
  });

  it("requires conservative unread endpoints after a direct boundary jump", async () => {
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
      { index: 4, key: "row-4", start: 100, end: 200 },
    ]);
    document.querySelector<HTMLElement>(".message-list-scroll")?.dispatchEvent(new Event("scroll"));
    await new Promise((resolve) => window.setTimeout(resolve, 20));

    expect(readProgress.get("s1")?.token).toBe("current");
  });

  it("acknowledges focused-mode traversal when the raw boundary is hidden", async () => {
    messages.messages = [
      { ...makeMessage(0), role: "user" },
      { ...makeMessage(1), role: "assistant" },
      { ...makeMessage(2), role: "assistant" },
      { ...makeMessage(3), role: "user" },
      { ...makeMessage(4), role: "assistant" },
    ];
    messages.messageCount = 5;
    messages.activeSessionToken = "current";
    ui.sortNewestFirst = true;
    ui.setTranscriptMode("focused");
    setVirtualRows(4);
    readProgress.baseline("s1", "previous", 0);

    component = mount(MessageList, { target: document.body });
    await tick();
    await new Promise((resolve) => window.setTimeout(resolve, 20));

    expect(readProgress.get("s1")?.token).toBe("previous");

    virtualizerMock.scrollOffset = 200;
    document.querySelector<HTMLElement>(".message-list-scroll")?.dispatchEvent(new Event("scroll"));
    await new Promise((resolve) => window.setTimeout(resolve, 20));

    expect(readProgress.get("s1")?.token).toBe("current");
  });

  it("hides system boundary cards when the system block is filtered out", async () => {
    messages.messages = [
      {
        ...makeMessage(0),
        role: "user",
        content: "<task-notification>\n<status>completed</status>\n</task-notification>",
        is_system: true,
        source_subtype: "task_notification",
      },
    ];
    messages.messageCount = 1;
    setVirtualRows(1);

    component = mount(MessageList, { target: document.body });
    await tick();
    expect(document.querySelector(".system-boundary")).not.toBeNull();

    ui.setBlockVisible("system", false);
    await tick();

    expect(document.querySelector(".system-boundary")).toBeNull();
  });

  it("keeps a code-only message visible as a collapsed placeholder when Code is filtered", async () => {
    const content = ["```latex", "\\subsection{Deployment Considerations}", "```"].join("\n");
    messages.messages = [
      {
        ...makeMessage(0),
        role: "assistant",
        content,
        content_length: content.length,
      },
    ];
    messages.messageCount = 1;
    ui.setBlockVisible("code", false);
    setVirtualRows(1);

    component = mount(MessageList, { target: document.body });
    await tick();

    const toggle = document.querySelector<HTMLButtonElement>(".code-fence-toggle");
    expect(toggle).not.toBeNull();
    expect(toggle?.textContent?.replace(/\s+/g, " ").trim()).toBe(
      "Code block collapsed · latex · Expand",
    );
    expect(document.querySelector(".code-content")).toBeNull();
  });

  it("acknowledges traversal when a block filter hides the raw boundary", async () => {
    messages.messages = [
      { ...makeMessage(0), role: "assistant" },
      { ...makeMessage(1), role: "user" },
      { ...makeMessage(2), role: "user" },
    ];
    messages.messageCount = 3;
    messages.activeSessionToken = "current";
    ui.setBlockVisible("assistant", false);
    setVirtualRows(2);
    readProgress.baseline("s1", "previous", 0);

    component = mount(MessageList, { target: document.body });
    await tick();
    await new Promise((resolve) => window.setTimeout(resolve, 20));

    expect(readProgress.get("s1")?.token).toBe("current");
  });

  it("rechecks visible progress when filters change after mounting", async () => {
    messages.messages = [
      { ...makeMessage(0), role: "user" },
      { ...makeMessage(1), role: "assistant" },
      { ...makeMessage(2), role: "user" },
    ];
    messages.messageCount = 3;
    messages.activeSessionToken = "current";
    ui.sortNewestFirst = true;
    setVirtualRows(2);
    readProgress.baseline("s1", "previous", 0);

    component = mount(MessageList, { target: document.body });
    await tick();
    await new Promise((resolve) => window.setTimeout(resolve, 20));
    expect(readProgress.get("s1")?.token).toBe("previous");

    ui.setBlockVisible("assistant", false);
    await tick();
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
    messages.messages = [makeMessage(0), makeMessage(1), makeMessage(2)];
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
    document.querySelector<HTMLElement>(".message-list-scroll")?.dispatchEvent(new Event("scroll"));
    await new Promise((resolve) => window.setTimeout(resolve, 20));

    expect(readProgress.get("s1")?.token).toBe("current");
  });

  it("conservatively traverses history when a reopened revision also appends", async () => {
    messages.messages = [makeMessage(0), makeMessage(1), makeMessage(2), makeMessage(3)];
    messages.messageCount = 4;
    messages.activeSessionToken = "current";
    messages.activeSessionUnreadOrdinal = null;
    ui.sortNewestFirst = true;
    virtualizerMock.getVirtualItems.mockReturnValue([
      { index: 0, key: "row-0", start: 0, end: 100 },
    ]);
    readProgress.baseline("s1", "previous", 2);

    component = mount(MessageList, { target: document.body });
    await tick();
    await new Promise((resolve) => window.setTimeout(resolve, 20));

    expect(readProgress.get("s1")?.token).toBe("previous");

    virtualizerMock.getVirtualItems.mockReturnValue([
      { index: 3, key: "row-3", start: 0, end: 100 },
    ]);
    document.querySelector<HTMLElement>(".message-list-scroll")?.dispatchEvent(new Event("scroll"));
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
    messages.messages = [makeMessage(0), { ...makeMessage(1), is_system: true }, makeMessage(2)];
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
