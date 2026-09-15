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

let id = 900000;
function message(ordinal: number, content: string): Message {
  return {
    id: id++,
    session_id: "find-regression",
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
  };
}
const stores: InSessionSearchStore[] = [];
function setup() {
  const ensureOrdinalLoaded = vi.fn<() => Promise<void>>().mockResolvedValue(undefined);
  const source = reactiveSource({
    sessionId: "find-regression",
    messages: [message(5, "needle needle"), message(9, "target target 中文")],
    loading: false,
    hasOlder: false,
    loadingOlder: false,
    ensureOrdinalLoaded,
  });
  const view = reactiveView({
    selectedOrdinal: null,
    sortNewestFirst: false,
    selectOrdinal: vi.fn(),
    setFollowLatest: vi.fn(),
  });
  const store = new InSessionSearchStore(source, view);
  stores.push(store);
  return { store, source, view, ensureOrdinalLoaded };
}
async function settle() {
  await tick();
  await vi.advanceTimersByTimeAsync(0);
  await tick();
}
async function search(store: InSessionSearchStore) {
  store.open();
  store.query = "needle";
  await tick();
  await vi.advanceTimersByTimeAsync(150);
  await tick();
}
beforeEach(() => vi.useFakeTimers());
afterEach(async () => {
  stores.splice(0).forEach((store) => store.destroy());
  await tick();
  vi.useRealTimers();
  vi.restoreAllMocks();
});

describe("session find interaction regressions", () => {
  it("pins the initial implicit occurrence before historical matches arrive", async () => {
    const { store, source } = setup();
    await search(store);
    expect(store.currentOrdinal).toBe(5);
    source.messages = [message(1, "needle"), ...source.messages];
    await tick();
    expect(store.currentOrdinal).toBe(5);
    expect(store.currentOccurrence("5:text:0")).toBe(0);
    expect(store.currentIndex).toBe(1);
  });

  it("applies a pending query before Enter/F3 without skipping its first hit", async () => {
    const { store } = setup();
    await search(store);
    store.query = "target";
    store.next();
    expect(store.debouncedQuery).toBe("target");
    expect(store.currentOrdinal).toBe(9);
    expect(store.currentOccurrence("9:text:0")).toBe(0);
    store.next();
    expect(store.currentOccurrence("9:text:0")).toBe(1);
  });

  it("cannot activate a stale result row while a replacement query is pending", async () => {
    const { store } = setup();
    await search(store);
    const old = store.matches[1]!;
    const sequence = store.navigationRevision;
    store.query = "target";
    store.goTo(old);
    expect(store.navigationRevision).toBe(sequence);
    expect(store.currentOccurrence("5:text:0")).toBe(0);
  });

  it("clearing the input cancels old navigation immediately", async () => {
    const { store } = setup();
    await search(store);
    store.query = " ";
    store.next();
    expect(store.total).toBe(0);
    expect(store.currentOrdinal).toBeNull();
    expect(store.isActive).toBe(false);
  });

  it("does not apply intermediate IME text or navigate until composition ends", async () => {
    const { store } = setup();
    await search(store);
    store.composing = true;
    store.query = "中文";
    await tick();
    await vi.advanceTimersByTimeAsync(500);
    store.next();
    expect(store.debouncedQuery).toBe("needle");
    expect(store.currentOrdinal).toBe(5);
    store.composing = false;
    await tick();
    await vi.advanceTimersByTimeAsync(150);
    expect(store.debouncedQuery).toBe("中文");
    expect(store.total).toBe(1);
    expect(store.currentOrdinal).toBe(9);
  });

  it("opens at the newest message while preserving in-message reading order", async () => {
    const { store, source, view } = setup();
    source.messages = [message(1, "needle needle"), message(8, "needle needle")];
    view.sortNewestFirst = true;
    await search(store);
    expect(store.currentOrdinal).toBe(8);
    expect(store.currentOccurrence("8:text:0")).toBe(0);
    store.next();
    expect(store.currentOccurrence("8:text:0")).toBe(1);
    store.next();
    expect(store.currentOrdinal).toBe(1);
    expect(store.currentOccurrence("1:text:0")).toBe(0);
  });

  it("exposes an incomplete-history result without retrying in a loop", async () => {
    const { store, source, ensureOrdinalLoaded } = setup();
    source.hasOlder = true;
    await search(store);
    await settle();
    expect(store.historyError).toBe(true);
    expect(store.loadingHistory).toBe(false);
    expect(store.total).toBe(2);
    await vi.advanceTimersByTimeAsync(1000);
    expect(ensureOrdinalLoaded).toHaveBeenCalledTimes(1);
    source.hasOlder = false;
    expect(store.historyError).toBe(false);
  });

  it("handles a synchronous loader failure and recovers on explicit retry", async () => {
    const { store, source, ensureOrdinalLoaded } = setup();
    vi.spyOn(console, "warn").mockImplementation(() => {});
    ensureOrdinalLoaded.mockImplementationOnce(() => {
      throw new Error("history unavailable");
    });
    source.hasOlder = true;
    await search(store);
    await settle();
    expect(store.historyError).toBe(true);
    ensureOrdinalLoaded.mockImplementationOnce(async () => {
      source.messages = [message(1, "needle"), ...source.messages];
      source.hasOlder = false;
    });
    store.retryHistory();
    await settle();
    expect(store.historyError).toBe(false);
    expect(store.loadingHistory).toBe(false);
    expect(store.total).toBe(3);
    expect(store.currentOrdinal).toBe(5);
    expect(ensureOrdinalLoaded).toHaveBeenCalledTimes(2);
  });

  it("ignores a rejected request from the previous session", async () => {
    const { store, source, ensureOrdinalLoaded } = setup();
    let reject!: (error: Error) => void;
    ensureOrdinalLoaded.mockImplementationOnce(
      () =>
        new Promise<void>((_resolve, fail) => {
          reject = fail;
        }),
    );
    source.hasOlder = true;
    await search(store);
    source.sessionId = "other-session";
    source.hasOlder = false;
    source.messages = [message(2, "needle")];
    await tick();
    reject(new Error("old session failed"));
    await settle();
    expect(store.historyError).toBe(false);
    expect(store.currentOrdinal).toBe(2);
  });
});
