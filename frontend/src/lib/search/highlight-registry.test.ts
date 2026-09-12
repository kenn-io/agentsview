// @vitest-environment jsdom
import { describe, expect, it, vi } from "vite-plus/test";
import {
  createHighlightRegistry,
  FIND_HIGHLIGHT,
  CURRENT_HIGHLIGHT,
} from "./highlight-registry.js";

class TestHighlight extends Set<AbstractRange> {
  priority = 0;

  constructor(...ranges: AbstractRange[]) {
    super(ranges);
  }
}

function setup() {
  const highlights = new Map<string, TestHighlight>();
  const clear = vi.spyOn(highlights, "clear");
  const host = { CSS: { highlights }, Highlight: TestHighlight };
  return { registry: createHighlightRegistry(host), highlights, clear };
}

describe("highlight registry", () => {
  it("maintains two highlights and removes only an owner's ranges", () => {
    const { registry, highlights, clear } = setup();
    const unrelated = new TestHighlight();
    highlights.set("other-feature", unrelated);
    const a = {},
      b = {};
    const first = document.createRange(),
      second = document.createRange();
    registry.set(a, [first], [first]);
    registry.set(b, [second]);
    expect(highlights.size).toBe(3);
    expect(highlights.get(FIND_HIGHLIGHT)?.size).toBe(2);
    expect(highlights.get(CURRENT_HIGHLIGHT)?.priority).toBe(1);
    registry.remove(a);
    expect(highlights.get(FIND_HIGHLIGHT)?.has(second)).toBe(true);
    expect(highlights.get(FIND_HIGHLIGHT)?.has(first)).toBe(false);
    expect(highlights.get(CURRENT_HIGHLIGHT)?.size).toBe(0);
    registry.remove(b);
    expect([...highlights]).toEqual([["other-feature", unrelated]]);
    expect(clear).not.toHaveBeenCalled();
  });

  it("replaces a block's ranges instead of accumulating stale nodes", () => {
    const { registry, highlights } = setup();
    const owner = {};
    const oldRange = document.createRange(),
      newRange = document.createRange();
    registry.set(owner, [oldRange]);
    registry.set(owner, [newRange], [newRange]);
    expect(highlights.get(FIND_HIGHLIGHT)?.size).toBe(1);
    expect(highlights.get(FIND_HIGHLIGHT)?.has(oldRange)).toBe(false);
    expect(highlights.get(CURRENT_HIGHLIGHT)?.has(newRange)).toBe(true);
    registry.remove(owner);
  });

  it("does not delete a highlight replaced by another owner", () => {
    const { registry, highlights } = setup();
    const owner = {};
    registry.set(owner, [document.createRange()]);
    const replacement = new TestHighlight();
    highlights.set(FIND_HIGHLIGHT, replacement);
    registry.remove(owner);
    expect(highlights.get(FIND_HIGHLIGHT)).toBe(replacement);
  });

  it("gracefully no-ops without the browser API", () => {
    const registry = createHighlightRegistry({});
    expect(registry.supported).toBe(false);
    const owner = {};
    expect(() => registry.set(owner, [document.createRange()])).not.toThrow();
    expect(() => registry.remove(owner)).not.toThrow();
  });
});
