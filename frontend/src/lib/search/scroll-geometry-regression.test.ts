// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { revealInContainer, scrollNestedContainers, type SearchRect } from "./scroll-geometry.js";

/** DOM sizes use layout pixels; rectangles include the caller's scale. */
function pane(scale = 1): HTMLElement {
  const element = document.createElement("pre");
  // jsdom does not expand the overflow shorthand into longhands, and the
  // reveal path reads the computed longhands a real browser reports.
  element.style.overflowY = "auto";
  element.style.overflowX = "auto";
  Object.defineProperties(element, {
    clientWidth: { value: 200 },
    clientHeight: { value: 200 },
    clientLeft: { value: 2 },
    clientTop: { value: 2 },
    offsetWidth: { value: 204 },
    offsetHeight: { value: 204 },
    scrollWidth: { value: 1500 },
    scrollHeight: { value: 1500 },
  });
  vi.spyOn(element, "getBoundingClientRect").mockImplementation(() => ({
    x: 50 * scale,
    y: 30 * scale,
    left: 50 * scale,
    top: 30 * scale,
    right: 254 * scale,
    bottom: 234 * scale,
    width: 204 * scale,
    height: 204 * scale,
    toJSON() {
      return {};
    },
  }));
  return element;
}

function target(element: HTMLElement, scale = 1, position = 500): SearchRect {
  const top = (32 + position - element.scrollTop) * scale;
  const left = (52 + position - element.scrollLeft) * scale;
  return { top, left, bottom: top + 20 * scale, right: left + 20 * scale };
}

afterEach(() => {
  document.body.replaceChildren();
  vi.restoreAllMocks();
});

describe("scaled search geometry", () => {
  it.each([0.75, 1, 1.3, 2])("converts viewport deltas to layout offsets at scale %s", (scale) => {
    const element = pane(scale);
    document.body.append(element);
    const scroll = vi.fn((offset: number) => {
      element.scrollTop = offset;
    });
    expect(revealInContainer(element, () => target(element, scale), true, true, scroll)).toBe(true);
    expect(element.scrollTop).toBeCloseTo(410);
    expect(element.scrollLeft).toBeCloseTo(410);
    expect(scroll).toHaveBeenCalledTimes(1);
    expect(revealInContainer(element, () => target(element, scale), true, true, scroll)).toBe(
      false,
    );
    expect(scroll).toHaveBeenCalledTimes(1);
  });

  it.each([0.75, 1, 1.3, 2])("does not move an already visible match at scale %s", (scale) => {
    const element = pane(scale);
    document.body.append(element);
    const scroll = vi.fn();
    expect(revealInContainer(element, () => target(element, scale, 150), true, true, scroll)).toBe(
      false,
    );
    expect(scroll).not.toHaveBeenCalled();
    expect(element.scrollLeft).toBe(0);
  });
});

describe("scrolling inside searchable Markdown blocks", () => {
  it("starts at a matched text node inside a descendant pane", () => {
    const root = document.createElement("div");
    const block = document.createElement("div");
    const element = pane();
    const code = document.createElement("code");
    const text = document.createTextNode("needle");
    code.append(text);
    element.append(code);
    block.append(element);
    root.append(block);
    document.body.append(root);
    expect(scrollNestedContainers(block, root, () => target(element), text)).toBe(true);
    expect(element.scrollTop).toBeCloseTo(410);
    expect(element.scrollLeft).toBeCloseTo(410);
    expect(root.scrollTop).toBe(0);
  });

  it("accepts an element boundary as well as a text boundary", () => {
    const root = document.createElement("div");
    const block = document.createElement("div");
    const element = pane();
    const span = document.createElement("span");
    element.append(span);
    block.append(element);
    root.append(block);
    document.body.append(root);
    expect(scrollNestedContainers(block, root, () => target(element), span)).toBe(true);
    expect(element.scrollTop).toBeCloseTo(410);
  });

  it("retains the block fallback when a live range is not yet available", () => {
    const root = document.createElement("div");
    const block = pane();
    root.append(block);
    document.body.append(root);
    expect(scrollNestedContainers(block, root, () => target(block))).toBe(true);
    expect(block.scrollTop).toBeCloseTo(410);
  });

  it("does not follow a stale range into another transcript", () => {
    const root = document.createElement("div");
    const block = document.createElement("div");
    const foreign = pane();
    foreign.textContent = "needle";
    root.append(block);
    document.body.append(root, foreign);
    expect(scrollNestedContainers(block, root, () => target(foreign), foreign.firstChild!)).toBe(
      false,
    );
    expect(foreign.scrollTop).toBe(0);
    expect(foreign.scrollLeft).toBe(0);
  });

  it("ignores blocks outside the target transcript", () => {
    const root = document.createElement("div");
    const block = pane();
    document.body.append(root, block);
    expect(scrollNestedContainers(block, root, () => target(block), block)).toBe(false);
    expect(block.scrollTop).toBe(0);
  });
});
