// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import Fixture from "./__fixtures__/SearchBlock.svelte";
import { reactiveProps } from "./__fixtures__/props.svelte.js";
import {
  currentRangeForBlock,
  mapOccurrences,
  searchBlock,
  type SearchBlockState,
} from "./search-block.svelte.js";
import { FIND_HIGHLIGHT, CURRENT_HIGHLIGHT } from "./highlight-registry.js";

class TestHighlight extends Set<AbstractRange> {
  priority = 0;

  constructor(...ranges: AbstractRange[]) {
    super(ranges);
  }
}

const cleanups: (() => void)[] = [];
let components: ReturnType<typeof mount>[] = [];
let highlights: Map<string, TestHighlight>;
let frames: Map<number, FrameRequestCallback>;
let frameId = 0;

beforeEach(() => {
  highlights = new Map();
  frames = new Map();
  vi.stubGlobal("CSS", { highlights });
  vi.stubGlobal("Highlight", TestHighlight);
  vi.stubGlobal("requestAnimationFrame", (callback: FrameRequestCallback) => {
    const id = ++frameId;
    frames.set(id, callback);
    return id;
  });
  vi.stubGlobal("cancelAnimationFrame", (id: number) => frames.delete(id));
});

afterEach(async () => {
  cleanups.splice(0).forEach((cleanup) => cleanup());
  for (const component of components) await unmount(component);
  components = [];
  document.body.innerHTML = "";
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

function attach(html: string, state: SearchBlockState) {
  const element = document.createElement("div");
  element.innerHTML = html;
  document.body.append(element);
  const cleanup = searchBlock("7:text:0", () => state)(element);
  if (cleanup) cleanups.push(cleanup);
  return element;
}

function flushFrames() {
  const pending = [...frames.values()];
  frames.clear();
  pending.forEach((callback) => callback(0));
}

describe("search block attachment", () => {
  it("highlights across spans without changing their text or markup", () => {
    const html = "<span>hel</span><em>lo</em> hello";
    const element = attach(html, { query: "hello", count: 2, current: true, occurrence: 1 });
    expect(element.innerHTML).toBe(html);
    expect(element.querySelector("mark")).toBeNull();
    expect(highlights.get(FIND_HIGHLIGHT)?.size).toBe(2);
    expect(highlights.get(CURRENT_HIGHLIGHT)?.size).toBe(1);
    expect(element.dataset.searchCurrent).toBe("true");
    const range = currentRangeForBlock(element)!;
    expect(range.toString()).toBe("hello");
    expect(range.startContainer).toBe(element.lastChild);
    expect(range.startOffset).toBe(1);
  });

  it("maps a synthetic BR newline to DOM boundary points", () => {
    const element = document.createElement("div");
    element.innerHTML = "a<br>b";
    const [range] = mapOccurrences(element, [{ start: 1, end: 2 }]);
    expect(range).toEqual({
      startContainer: element,
      startOffset: 1,
      endContainer: element,
      endOffset: 2,
    });
  });

  it("paints Unicode length-changing folds with the original offset", () => {
    const element = attach("İstanbul", { query: "i", count: 1, current: true, occurrence: 0 });
    const range = currentRangeForBlock(element)!;
    expect(range.toString()).toBe("İ");
    expect(range.startOffset).toBe(0);
    expect(range.endOffset).toBe(1);
  });

  it("coalesces text replacements and forgets detached Shiki nodes", async () => {
    const element = attach("<span>needle</span>", {
      query: "needle",
      count: 1,
      current: true,
      occurrence: 0,
    });
    const oldText = element.firstChild!.firstChild;
    element.innerHTML = "<em>nee</em><b>dle</b>";
    element.querySelector("em")!.textContent = "nee";
    await Promise.resolve();
    expect(frames.size).toBe(1);
    flushFrames();
    expect(currentRangeForBlock(element)?.startContainer).not.toBe(oldText);
    expect(currentRangeForBlock(element)?.toString()).toBe("needle");
    expect(highlights.get(FIND_HIGHLIGHT)?.size).toBe(1);
    expect(frames.size).toBe(0);
  });

  it("cancels pending repaint and removes only owned highlights on teardown", async () => {
    const unrelated = new TestHighlight();
    highlights.set("other-feature", unrelated);
    const element = attach("needle", { query: "needle", count: 1, current: true, occurrence: 0 });
    element.textContent = "needle!";
    await Promise.resolve();
    expect(frames.size).toBe(1);
    cleanups.pop()!();
    expect(frames.size).toBe(0);
    expect(currentRangeForBlock(element)).toBeUndefined();
    expect(element.dataset.searchBlock).toBeUndefined();
    expect([...highlights]).toEqual([["other-feature", unrelated]]);
    element.textContent = "needle again";
    await Promise.resolve();
    expect(frames.size).toBe(0);
  });

  it("retains current-block navigation without native highlight support", () => {
    vi.stubGlobal("Highlight", undefined);
    const element = attach("needle", { query: "needle", count: 1, current: true, occurrence: 0 });
    expect(element.dataset.searchCurrent).toBe("true");
    expect(currentRangeForBlock(element)?.toString()).toBe("needle");
    expect(highlights.size).toBe(0);
    expect(element.innerHTML).toBe("needle");
  });

  it("does not paint a block absent from the index", () => {
    const element = attach("needle", { query: "needle", count: 0, current: false, occurrence: -1 });
    expect(highlights.size).toBe(0);
    expect(currentRangeForBlock(element)).toBeUndefined();
  });

  it("reports an index/DOM mismatch in development", () => {
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    attach("needle", { query: "needle", count: 2, current: false, occurrence: -1 });
    expect(warn).toHaveBeenCalledWith("Search block text differs from its index", "7:text:0", {
      indexed: 2,
      rendered: 1,
    });
  });

  it("reacts to query and cursor changes through a real Svelte attachment", async () => {
    const props = reactiveProps({
      blockKey: "1:text:0",
      html: "needle other needle",
      query: "needle",
      count: 2,
      current: true,
      occurrence: 0,
    });
    components.push(mount(Fixture, { target: document.body, props }));
    await tick();
    const element = document.querySelector<HTMLElement>("[data-search-block]")!;
    expect(currentRangeForBlock(element)?.startOffset).toBe(0);
    props.occurrence = 1;
    await tick();
    expect(currentRangeForBlock(element)?.startOffset).toBe(13);
    props.query = "other";
    props.count = 1;
    props.occurrence = 0;
    await tick();
    expect(currentRangeForBlock(element)?.toString()).toBe("other");
    expect(highlights.get(FIND_HIGHLIGHT)?.size).toBe(1);
    props.query = "";
    props.count = 0;
    props.current = false;
    await tick();
    expect(highlights.size).toBe(0);
    expect(element.dataset.searchCurrent).toBeUndefined();
  });
});

it("highlights only whole words across inline markup", () => {
  const element = attach("<p>scatter <b>cat</b> catapult</p>", {
    query: "cat",
    wholeWord: true,
    count: 1,
    current: true,
    occurrence: 0,
  });
  expect(highlights.get(FIND_HIGHLIGHT)?.size).toBe(1);
  expect(currentRangeForBlock(element)?.toString()).toBe("cat");
  expect(element.querySelector("b")!.contains(currentRangeForBlock(element)!.startContainer)).toBe(
    true,
  );
});
