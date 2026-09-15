// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import type { Message } from "../../api/types.js";
import { inSessionSearch } from "../../stores/inSessionSearch.svelte.js";
import { messages } from "../../stores/messages.svelte.js";
import { sessions } from "../../stores/sessions.svelte.js";
import { ui } from "../../stores/ui.svelte.js";

const virtualizerMock = vi.hoisted(() => ({
  options: { count: 0 },
  scrollOffset: 0,
  scrollRect: { height: 500 },
  getVirtualItems: vi.fn(() => [] as { index: number; key: string; start: number; end: number }[]),
  getTotalSize: vi.fn(() => 1000),
  measureElement: vi.fn(),
  scrollToIndex: vi.fn(),
  scrollToOffset: vi.fn(),
  getOffsetForIndex: vi.fn((index: number) => [index * 120, "start"]),
}));
vi.mock("../../virtual/createVirtualizer.svelte.js", () => ({
  createVirtualizer: (read: () => { count: number }) => ({
    get instance() {
      virtualizerMock.options.count = read().count;
      virtualizerMock.getVirtualItems.mockReturnValue(
        Array.from({ length: read().count }, (_, index) => ({
          index,
          key: `search-row-${index}`,
          start: index * 120,
          end: (index + 1) * 120,
        })),
      );
      return virtualizerMock;
    },
  }),
}));
import MessageList from "./MessageList.svelte";

let component: ReturnType<typeof mount> | undefined;
let nextId = 180000;
function message(ordinal: number, content: string, overrides: Partial<Message> = {}): Message {
  return {
    id: nextId++,
    session_id: "search-list",
    ordinal,
    role: "assistant",
    content,
    content_length: content.length,
    timestamp: "2026-01-01T00:00:00Z",
    has_thinking: false,
    thinking_text: "",
    has_tool_use: false,
    model: "",
    context_tokens: 0,
    output_tokens: 0,
    is_system: false,
    ...overrides,
  };
}

beforeEach(() => {
  vi.useFakeTimers();
  vi.clearAllMocks();
  inSessionSearch.close();
  inSessionSearch.clearQuery();
  messages.clear();
  messages.sessionId = "search-list";
  sessions.activeSessionId = "search-list";
  messages.hasOlder = false;
  messages.loading = false;
  messages.messages = [
    message(0, "needle in visible user", { role: "user" }),
    message(1, "[Thinking]\nneedle\n[/Thinking]", { has_thinking: true }),
    message(2, "", {
      has_tool_use: true,
      tool_calls: [{ tool_name: "Read", result_content: "needle" }],
    }),
  ];
  messages.messageCount = 3;
  ui.visibleBlocks = new Set(["user"]);
  ui.setTranscriptMode("focused");
  ui.messageLayout = "skim";
  ui.sortNewestFirst = false;
  ui.followLatest = false;
  ui.selectedOrdinal = null;
  vi.spyOn(window, "requestAnimationFrame").mockImplementation((callback) =>
    window.setTimeout(() => callback(performance.now()), 1),
  );
  vi.spyOn(window, "cancelAnimationFrame").mockImplementation((id) => window.clearTimeout(id));
});

afterEach(async () => {
  if (component) await unmount(component);
  component = undefined;
  inSessionSearch.close();
  inSessionSearch.clearQuery();
  messages.clear();
  sessions.activeSessionId = null;
  ui.showAllBlocks();
  ui.setTranscriptMode("normal");
  ui.messageLayout = "default";
  ui.sortNewestFirst = false;
  document.body.innerHTML = "";
  vi.restoreAllMocks();
  vi.useRealTimers();
});

describe("MessageList search visibility", () => {
  it("respects block filters and focused mode while searching without changing preferences", async () => {
    component = mount(MessageList, { target: document.body });
    await tick();
    const before = document.querySelectorAll(".virtual-row").length;
    const filters = [...ui.visibleBlocks];
    expect(document.querySelector(".layout-skim")).not.toBeNull();
    inSessionSearch.open();
    inSessionSearch.query = "needle";
    await tick();
    await vi.advanceTimersByTimeAsync(200);
    await tick();
    // Only the visible user text is searchable and rendered; hidden thinking
    // and tool blocks contribute no count and no DOM.
    expect(inSessionSearch.total).toBe(1);
    expect(inSessionSearch.countForOrdinal(0)).toBe(1);
    expect(inSessionSearch.countForBlock("1:thinking:0")).toBe(0);
    expect(document.querySelectorAll(".virtual-row")).toHaveLength(before);
    expect(document.querySelectorAll(".thinking-header")).toHaveLength(0);
    expect(document.querySelector(".tool-block")).toBeNull();
    // Layout is not a filter: skim may lift temporarily to reveal a match.
    expect(document.querySelector(".layout-skim")).toBeNull();
    expect([...ui.visibleBlocks]).toEqual(filters);
    expect(ui.transcriptMode).toBe("focused");
    expect(ui.messageLayout).toBe("skim");
    inSessionSearch.close();
    await tick();
    expect(document.querySelectorAll(".virtual-row")).toHaveLength(before);
    expect(document.querySelector(".layout-skim")).not.toBeNull();
  });

  it("applies a filter change during an active query to counts, DOM, and the current result", async () => {
    component = mount(MessageList, { target: document.body });
    await tick();
    inSessionSearch.open();
    inSessionSearch.query = "needle";
    await tick();
    await vi.advanceTimersByTimeAsync(200);
    await tick();
    expect(inSessionSearch.total).toBe(1);
    expect(inSessionSearch.resolvedCurrent?.blockKey).toBe("0:text:0");

    ui.setBlockVisible("thinking", true);
    await tick();
    await vi.advanceTimersByTimeAsync(50);
    await tick();
    expect(inSessionSearch.total).toBe(2);
    expect(inSessionSearch.countForBlock("1:thinking:0")).toBe(1);
    expect(document.querySelectorAll(".thinking-header").length).toBeGreaterThan(0);

    ui.setBlockVisible("thinking", false);
    await tick();
    await vi.advanceTimersByTimeAsync(50);
    await tick();
    expect(inSessionSearch.total).toBe(1);
    expect(inSessionSearch.countForBlock("1:thinking:0")).toBe(0);
    expect(document.querySelectorAll(".thinking-header")).toHaveLength(0);
    // The removed occurrence is replaced deterministically, not left dangling.
    expect(inSessionSearch.resolvedCurrent?.blockKey).toBe("0:text:0");
    expect(
      document.querySelector('[data-search-current="true"]')?.getAttribute("data-search-block"),
    ).toBe("0:text:0");
  });

  it("clears the current highlight and pending reveal when filters leave no match", async () => {
    component = mount(MessageList, { target: document.body });
    await tick();
    inSessionSearch.open();
    inSessionSearch.query = "needle";
    await tick();
    await vi.advanceTimersByTimeAsync(200);
    await tick();
    expect(inSessionSearch.total).toBe(1);
    expect(document.querySelector('[data-search-current="true"]')).not.toBeNull();

    for (const type of ["user"] as const) ui.setBlockVisible(type, false);
    await tick();
    await vi.advanceTimersByTimeAsync(200);
    await tick();
    expect(inSessionSearch.total).toBe(0);
    expect(inSessionSearch.resolvedCurrent).toBeNull();
    expect(document.querySelector("[data-search-current]")).toBeNull();

    // Restoring the filter restores the searchable range without retyping.
    ui.setBlockVisible("user", true);
    await tick();
    await vi.advanceTimersByTimeAsync(200);
    await tick();
    expect(inSessionSearch.total).toBe(1);
    expect(inSessionSearch.query).toBe("needle");
    expect(inSessionSearch.resolvedCurrent?.blockKey).toBe("0:text:0");
  });

  it("cancels stale virtual scrolling after closing search", async () => {
    component = mount(MessageList, { target: document.body });
    await tick();
    inSessionSearch.open();
    inSessionSearch.query = "needle";
    await tick();
    await vi.advanceTimersByTimeAsync(150);
    await tick();
    inSessionSearch.close();
    await tick();
    virtualizerMock.scrollToOffset.mockClear();
    virtualizerMock.scrollToIndex.mockClear();
    await vi.advanceTimersByTimeAsync(100);
    expect(virtualizerMock.scrollToOffset).not.toHaveBeenCalled();
    expect(virtualizerMock.scrollToIndex).not.toHaveBeenCalled();
  });
});
