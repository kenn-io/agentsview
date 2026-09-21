// @vitest-environment jsdom
import { describe, expect, it, vi } from "vite-plus/test";
import { centeredOffset, revealInContainer, type SearchRect } from "./scroll-geometry.js";

function scroller(scale: number) {
  const element = document.createElement("div");
  Object.defineProperties(element, {
    clientHeight: { value: 200 },
    clientWidth: { value: 300 },
    offsetHeight: { value: 210 },
    offsetWidth: { value: 310 },
    clientTop: { value: 5 },
    clientLeft: { value: 5 },
    scrollHeight: { value: 2000 },
    scrollWidth: { value: 2000 },
  });
  element.getBoundingClientRect = () => ({
    top: 40,
    left: 20,
    bottom: 40 + 210 * scale,
    right: 20 + 310 * scale,
    width: 310 * scale,
    height: 210 * scale,
    x: 20,
    y: 40,
    toJSON: () => ({}),
  });
  return element;
}

describe("search reveal under CSS zoom", () => {
  it.each([0.67, 0.9, 1, 1.2, 1.3, 2])(
    "converts viewport deltas to scroll coordinates at %s",
    (scale) => {
      const element = scroller(scale);
      const readTarget = (): SearchRect => ({
        top: 40 + (5 + 250 - element.scrollTop) * scale,
        bottom: 40 + (5 + 270 - element.scrollTop) * scale,
        left: 20 + (5 + 350 - element.scrollLeft) * scale,
        right: 20 + (5 + 370 - element.scrollLeft) * scale,
      });
      const scroll = vi.fn((offset: number) => {
        element.scrollTop = offset;
      });
      expect(revealInContainer(element, readTarget, true, true, scroll)).toBe(true);
      expect(element.scrollTop).toBeCloseTo(160);
      expect(element.scrollLeft).toBeCloseTo(210);
      expect(scroll).toHaveBeenCalledTimes(1);
      expect(revealInContainer(element, readTarget, true, true, scroll)).toBe(false);
      expect(scroll).toHaveBeenCalledTimes(1);
    },
  );

  it.each([0, -1, Number.NaN, Number.POSITIVE_INFINITY])(
    "uses a finite fallback for invalid scale %s",
    (scale) => {
      expect(centeredOffset(50, 400, 420, 100, 300, scale)).toBe(260);
    },
  );
});
