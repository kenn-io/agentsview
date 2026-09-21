// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { tick } from "svelte";
import { InSessionSearchStore } from "./inSessionSearch.svelte.js";
import { reactiveSource, reactiveView } from "./__fixtures__/search-state.svelte.js";
const stores: InSessionSearchStore[] = [];
afterEach(() => {
  stores.splice(0).forEach((store) => store.destroy());
  vi.restoreAllMocks();
});
async function settle() {
  await tick();
  await Promise.resolve();
  await tick();
  await Promise.resolve();
  await tick();
}
function setup() {
  const load = vi.fn(async () => {});
  const source = reactiveSource({
    sessionId: "history",
    messages: [],
    loading: false,
    hasOlder: false,
    loadingOlder: false,
    historyComplete: false,
    ensureHistoryLoaded: load,
    ensureOrdinalLoaded: vi.fn(async () => {}),
  });
  const view = reactiveView({
    selectedOrdinal: null,
    sortNewestFirst: false,
    selectOrdinal: vi.fn(),
    setFollowLatest: vi.fn(),
  });
  const store = new InSessionSearchStore(source, view);
  stores.push(store);
  return { source, store, load };
}
describe("search history completeness", () => {
  it("shows partial results for a failed tail even when hasOlder is false", async () => {
    const { source, store, load } = setup();
    store.open();
    await settle();
    expect(load).toHaveBeenCalledTimes(1);
    expect(source.hasOlder).toBe(false);
    expect(store.historyError).toBe(true);
    expect(store.loadingHistory).toBe(false);
    await settle();
    expect(load).toHaveBeenCalledTimes(1);
  });
  it("clears the partial status only after a successful explicit retry", async () => {
    const { source, store, load } = setup();
    store.open();
    await settle();
    expect(store.historyError).toBe(true);
    load.mockImplementation(async () => {
      source.historyComplete = true;
    });
    store.retryHistory();
    await settle();
    expect(store.historyError).toBe(false);
    expect(store.loadingHistory).toBe(false);
    expect(load).toHaveBeenCalledTimes(2);
  });
  it("ignores late failure from another session", async () => {
    const { source, store, load } = setup();
    let release!: () => void;
    load.mockImplementation(
      () =>
        new Promise<void>((resolve) => {
          release = resolve;
        }),
    );
    store.open();
    await settle();
    source.sessionId = "other";
    source.historyComplete = true;
    await tick();
    release();
    await settle();
    expect(store.historyError).toBe(false);
    expect(store.loadingHistory).toBe(false);
  });
});
