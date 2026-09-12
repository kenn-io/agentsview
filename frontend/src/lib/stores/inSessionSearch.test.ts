// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { tick } from "svelte";
import type { Message } from "../api/types.js";
import { InSessionSearchStore } from "./inSessionSearch.svelte.js";
import { reactiveSource, reactiveView } from "./__fixtures__/search-state.svelte.js";

vi.mock("./messages.svelte.js", () => ({
  messages: {
    sessionId: null,
    messages: [],
    loading: false,
    hasOlder: false,
    loadingOlder: false,
    ensureOrdinalLoaded: vi.fn().mockResolvedValue(undefined),
  },
}));
vi.mock("./ui.svelte.js", () => ({
  ui: {
    selectedOrdinal: null,
    sortNewestFirst: false,
    selectOrdinal: vi.fn(),
    setFollowLatest: vi.fn(),
  },
}));

let nextId = 150000;
function message(ordinal: number, content: string, overrides: Partial<Message> = {}): Message {
  return {
    id: nextId++,
    session_id: "session-a",
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

const stores: InSessionSearchStore[] = [];
function setup(items = [message(0, "needle needle"), message(2, "needle")]) {
  const ensureOrdinalLoaded = vi.fn().mockResolvedValue(undefined);
  const source = reactiveSource({
    sessionId: "session-a",
    messages: items,
    loading: false,
    hasOlder: false,
    loadingOlder: false,
    ensureOrdinalLoaded,
  });
  const selectOrdinal = vi.fn((ordinal: number) => {
    view.selectedOrdinal = ordinal;
  });
  const setFollowLatest = vi.fn();
  const view = reactiveView({
    selectedOrdinal: null,
    sortNewestFirst: false,
    selectOrdinal,
    setFollowLatest,
  });
  const store = new InSessionSearchStore(source, view);
  stores.push(store);
  return { store, source, view, ensureOrdinalLoaded, selectOrdinal, setFollowLatest };
}

async function search(store: InSessionSearchStore, query = "needle") {
  store.open();
  store.query = query;
  await tick();
  await vi.advanceTimersByTimeAsync(150);
  await tick();
}

beforeEach(() => vi.useFakeTimers());
afterEach(() => {
  stores.splice(0).forEach((store) => store.destroy());
  vi.useRealTimers();
  vi.restoreAllMocks();
});

describe("local in-session search", () => {
  it("debounces text before building an occurrence index", async () => {
    const { store } = setup();
    store.open();
    store.query = "needle";
    await tick();
    expect(store.total).toBe(0);
    await vi.advanceTimersByTimeAsync(149);
    expect(store.total).toBe(0);
    await vi.advanceTimersByTimeAsync(1);
    expect(store.total).toBe(3);
    expect(store.countForOrdinal(0)).toBe(2);
    expect(store.countForBlock("0:text:0")).toBe(2);
    expect(store.currentOccurrence("0:text:0")).toBe(0);
  });

  it("cancels superseded queries and pending work on close", async () => {
    const { store } = setup();
    store.open();
    store.query = "needle";
    await tick();
    await vi.advanceTimersByTimeAsync(100);
    store.query = "missing";
    await tick();
    await vi.advanceTimersByTimeAsync(50);
    expect(store.debouncedQuery).toBe("");
    store.close();
    await tick();
    await vi.advanceTimersByTimeAsync(500);
    expect(store.query).toBe("missing");
    expect(store.isActive).toBe(false);
    expect(store.index).toBeNull();
  });

  it("anchors once on open even though navigation selects other messages", async () => {
    const { store, view } = setup([
      message(0, "alpha"),
      message(5, "alphabet"),
      message(9, "alphabet"),
    ]);
    view.selectedOrdinal = 5;
    await search(store, "alpha");
    expect(store.currentOrdinal).toBe(5);
    store.next();
    expect(view.selectedOrdinal).toBe(9);
    store.query = "alphabet";
    await tick();
    await vi.advanceTimersByTimeAsync(150);
    expect(store.anchorOrdinal).toBe(5);
    expect(store.currentOrdinal).toBe(5);
  });

  it("refocuses repeated open without resetting the cursor or anchor", async () => {
    const { store, view } = setup();
    await search(store);
    store.next();
    const cursor = store.current;
    const focus = store.focusRequest;
    view.selectedOrdinal = 99;
    store.open();
    expect(store.focusRequest).toBe(focus + 1);
    expect(store.current).toBe(cursor);
    expect(store.anchorOrdinal).toBeNull();
  });

  it("preserves a block occurrence across history prepend and same-count SSE replacement", async () => {
    const { store, source } = setup([message(5, "needle needle")]);
    await search(store);
    store.next();
    expect(store.currentOccurrence("5:text:0")).toBe(1);
    source.messages = [
      message(1, "needle"),
      { ...source.messages[0]!, content: "needle needle needle", content_length: 20 },
    ];
    await tick();
    expect(store.total).toBe(4);
    expect(store.currentOccurrence("5:text:0")).toBe(1);
    expect(store.currentIndex).toBe(2);
  });

  it("falls forward when the selected occurrence disappears", async () => {
    const { store, source } = setup();
    await search(store);
    store.next();
    source.messages = [message(0, "gone"), source.messages[1]!];
    await tick();
    expect(store.currentOrdinal).toBe(2);
    expect(store.currentIndex).toBe(0);
  });

  it("navigates in display order and wraps within the same message", async () => {
    const { store, view, setFollowLatest } = setup();
    await search(store);
    store.prev();
    expect(store.currentOrdinal).toBe(2);
    store.next();
    expect(store.currentOccurrence("0:text:0")).toBe(0);
    store.next();
    expect(store.currentOccurrence("0:text:0")).toBe(1);
    view.sortNewestFirst = true;
    expect(store.currentIndex).toBe(2);
    store.next();
    expect(store.currentOrdinal).toBe(2);
    expect(store.currentIndex).toBe(0);
    store.next();
    expect(store.currentOccurrence("0:text:0")).toBe(0);
    expect(store.currentIndex).toBe(1);
    store.next();
    expect(store.currentOccurrence("0:text:0")).toBe(1);
    expect(store.currentIndex).toBe(2);
    expect(setFollowLatest).toHaveBeenLastCalledWith(false);
  });

  it("selects the first remaining result when the opening anchor is past every match", async () => {
    const { store, view } = setup();
    view.selectedOrdinal = 99;
    await search(store);
    expect(store.currentOrdinal).toBe(0);
    expect(store.currentOccurrence("0:text:0")).toBe(0);
    expect(store.currentIndex).toBe(0);
    store.next();
    expect(store.currentOrdinal).toBe(0);
    expect(store.currentOccurrence("0:text:0")).toBe(1);
    expect(store.currentIndex).toBe(1);
  });

  it("keeps query on close and clears it only on explicit clear", async () => {
    const { store } = setup();
    await search(store);
    store.close();
    expect(store.query).toBe("needle");
    expect(store.total).toBe(0);
    await search(store, store.query);
    expect(store.total).toBe(3);
    store.clearQuery();
    expect(store.query).toBe("");
    expect(store.total).toBe(0);
  });

  it("loads missing history once while a request is pending", async () => {
    const { store, source, ensureOrdinalLoaded } = setup();
    let finish!: () => void;
    ensureOrdinalLoaded.mockImplementation(
      () =>
        new Promise<void>((resolve) => {
          finish = resolve;
        }),
    );
    source.hasOlder = true;
    await search(store);
    store.open();
    await tick();
    expect(ensureOrdinalLoaded).toHaveBeenCalledTimes(1);
    expect(ensureOrdinalLoaded).toHaveBeenCalledWith(0);
    expect(store.loadingHistory).toBe(true);
    source.hasOlder = false;
    finish();
    await tick();
    expect(store.loadingHistory).toBe(false);
  });

  it("loads history after an in-progress initial session load finishes", async () => {
    const { store, source, ensureOrdinalLoaded } = setup();
    source.loading = true;
    source.hasOlder = true;
    store.open();
    await tick();
    expect(ensureOrdinalLoaded).not.toHaveBeenCalled();
    source.loading = false;
    await tick();
    expect(ensureOrdinalLoaded).toHaveBeenCalledWith(0);
  });

  it("resets the tuple on session change and ignores an old debounce", async () => {
    const { store, source } = setup();
    await search(store);
    store.next();
    source.sessionId = "session-b";
    source.messages = [message(7, "needle")];
    await tick();
    expect(store.current).toEqual({ ordinal: 7, blockKey: "7:text:0", occurrence: 0 });
    expect(store.currentOrdinal).toBe(7);
    store.query = "missing";
    await tick();
    source.sessionId = null;
    source.messages = [];
    await tick();
    await vi.advanceTimersByTimeAsync(500);
    expect(store.isOpen).toBe(false);
    expect(store.query).toBe("missing");
    expect(store.index).toBeNull();
  });

  it("rejects stale result clicks without changing the current position", async () => {
    const { store } = setup();
    await search(store);
    const seq = store.navigationRevision;
    store.goTo({ ordinal: 123, blockKey: "123:text:0", occurrence: 0 });
    expect(store.navigationRevision).toBe(seq);
    expect(store.currentOrdinal).toBe(0);
    store.goTo(store.matches[1]!);
    expect(store.navigationRevision).toBe(seq + 1);
    expect(store.currentOccurrence("0:text:0")).toBe(1);
  });

  it("counts a replaced tool result without depending on message count", async () => {
    const original = message(1, "", {
      tool_calls: [{ tool_name: "Read", result_content: "needle" }],
    });
    const { store, source } = setup([original]);
    await search(store);
    expect(store.total).toBe(1);
    source.messages = [
      { ...original, tool_calls: [{ tool_name: "Read", result_content: "needle needle" }] },
    ];
    await tick();
    expect(store.total).toBe(2);
    expect(store.currentOrdinal).toBe(1);
  });
});

it("reindexes and navigates whole words without changing the query", async () => {
  const { store } = setup([message(0, "needles needle"), message(2, "NEEDLE")]);
  await search(store);
  expect(store.total).toBe(3);
  store.toggleWholeWord();
  await tick();
  expect(store.total).toBe(2);
  expect(store.resolvedCurrent).toMatchObject({ ordinal: 0, start: 8, end: 14 });
  store.next();
  expect(store.currentOrdinal).toBe(2);
  store.close();
  await search(store);
  expect(store.wholeWord).toBe(true);
  expect(store.total).toBe(2);
  store.toggleWholeWord();
  await tick();
  expect(store.total).toBe(3);
  expect(store.query).toBe("needle");
});
