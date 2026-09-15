// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import {
  centeredOffset,
  isRectWithin,
  revealInContainer,
  scrollNestedContainers,
} from "./scroll-geometry.js";

function pane(top: number, left: number, height: number, width: number) {
  const element = document.createElement("div");
  for (const [key, value] of Object.entries({
    clientHeight: height,
    clientWidth: width,
    clientTop: 0,
    clientLeft: 0,
    scrollHeight: 2000,
    scrollWidth: 2000,
  }))
    Object.defineProperty(element, key, { configurable: true, value });
  vi.spyOn(element, "getBoundingClientRect").mockReturnValue({
    top,
    left,
    bottom: top + height,
    right: left + width,
    width,
    height,
    x: left,
    y: top,
    toJSON: () => ({}),
  });
  return element;
}

afterEach(() => {
  document.body.innerHTML = "";
  vi.restoreAllMocks();
});

describe("search scroll geometry", () => {
  it("uses the viewport origin when centering a clipped target", () => {
    expect(centeredOffset(400, 650, 670, 200, 600)).toBe(660);
    expect(centeredOffset(90, 150, 170, 200, 600)).toBe(-150);
  });

  it("tests every edge, including a requested inset", () => {
    const viewport = { top: 100, bottom: 300, left: 50, right: 250 };
    expect(isRectWithin({ top: 110, bottom: 280, left: 60, right: 240 }, viewport, 10)).toBe(true);
    expect(isRectWithin({ top: 99, bottom: 280, left: 60, right: 240 }, viewport)).toBe(false);
    expect(isRectWithin({ top: 110, bottom: 280, left: 60, right: 260 }, viewport)).toBe(false);
  });

  it("leaves a fully visible result at the user's scroll position", () => {
    const root = pane(100, 50, 200, 200);
    root.scrollTop = 400;
    root.scrollLeft = 200;
    expect(revealInContainer(root, () => ({ top: 120, bottom: 140, left: 80, right: 120 }))).toBe(
      false,
    );
    expect(root.scrollTop).toBe(400);
    expect(root.scrollLeft).toBe(200);
  });

  it("moves only clipped axes and clamps at the start and end", () => {
    const root = pane(100, 50, 200, 200);
    root.scrollTop = 50;
    root.scrollLeft = 10;
    expect(revealInContainer(root, () => ({ top: -50, bottom: -30, left: 80, right: 120 }))).toBe(
      true,
    );
    expect(root.scrollTop).toBe(0);
    expect(root.scrollLeft).toBe(10);
    revealInContainer(root, () => ({ top: 4000, bottom: 4020, left: 5000, right: 5020 }));
    expect(root.scrollTop).toBe(1800);
    expect(root.scrollLeft).toBe(1800);
  });

  it("scrolls the inner output before considering the outer transcript", () => {
    const root = pane(100, 50, 400, 400);
    const output = pane(150, 70, 100, 200);
    output.style.overflowY = "auto";
    root.append(output);
    document.body.append(root);
    const read = () => ({
      top: 700 - output.scrollTop,
      bottom: 720 - output.scrollTop,
      left: 90,
      right: 110,
    });
    expect(scrollNestedContainers(output, root, read)).toBe(true);
    expect(output.scrollTop).toBe(510);
    expect(root.scrollTop).toBe(0);
    expect(revealInContainer(root, read, true, false)).toBe(false);
  });

  it("does not scroll an element outside the owning transcript", () => {
    const root = pane(0, 0, 100, 100);
    const other = pane(0, 0, 100, 100);
    other.style.overflowY = "auto";
    expect(
      scrollNestedContainers(other, root, () => ({ top: 700, bottom: 720, left: 0, right: 10 })),
    ).toBe(false);
    expect(other.scrollTop).toBe(0);
  });
});
