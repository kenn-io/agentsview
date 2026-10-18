import { describe, expect, it, vi } from "vite-plus/test";
import {
  settleScrollAnchor,
  settleVirtualScroll,
  type AnchorSettleOptions,
  type StagedScrollOptions,
} from "./staged-scroll.js";

function setup() {
  const virtualizer = {
    options: { count: 3 },
    getVirtualItems: vi.fn(() => [] as { index: number }[]),
    getOffsetForIndex: vi.fn(() => undefined as [number, "start" | "end"] | undefined),
    scrollToOffset: vi.fn(),
    scrollToIndex: vi.fn(),
  };
  const options: StagedScrollOptions = {
    index: 1,
    align: "start",
    getCount: () => 3,
    getVirtualizer: () => virtualizer,
    isCurrent: () => true,
    nextFrame: vi.fn().mockResolvedValue(undefined),
  };
  return { options, virtualizer };
}

describe("settleVirtualScroll", () => {
  it("completes immediately when an exact rendered offset exists", async () => {
    const { options, virtualizer } = setup();
    virtualizer.getVirtualItems.mockReturnValue([{ index: 1 }]);
    virtualizer.getOffsetForIndex.mockReturnValue([123.6, "start"]);
    expect(await settleVirtualScroll(options)).toBe(true);
    expect(virtualizer.scrollToOffset).toHaveBeenCalledWith(124, { align: "start" });
    expect(options.nextFrame).not.toHaveBeenCalled();
    expect(virtualizer.scrollToIndex).not.toHaveBeenCalled();
  });

  it("does not report an estimated scroll as completion", async () => {
    const { options, virtualizer } = setup();
    let frame = 0;
    options.nextFrame = vi.fn(async () => {
      frame++;
      if (frame === 2) {
        virtualizer.getVirtualItems.mockReturnValue([{ index: 1 }]);
        virtualizer.getOffsetForIndex.mockReturnValue([240, "start"]);
      }
    });
    expect(await settleVirtualScroll(options)).toBe(true);
    expect(options.nextFrame).toHaveBeenCalledTimes(2);
    expect(virtualizer.scrollToIndex).toHaveBeenCalledTimes(1);
    expect(virtualizer.scrollToOffset).toHaveBeenCalledWith(240, { align: "start" });
  });

  it("waits for the virtual count to catch up with newly loaded messages", async () => {
    const { options, virtualizer } = setup();
    virtualizer.options.count = 0;
    options.nextFrame = vi.fn(async () => {
      virtualizer.options.count = 3;
      virtualizer.getVirtualItems.mockReturnValue([{ index: 1 }]);
      virtualizer.getOffsetForIndex.mockReturnValue([100, "start"]);
    });
    expect(await settleVirtualScroll(options)).toBe(true);
    expect(options.nextFrame).toHaveBeenCalledTimes(1);
    expect(virtualizer.scrollToIndex).not.toHaveBeenCalled();
  });

  it("expires after bounded estimate retries", async () => {
    const { options, virtualizer } = setup();
    expect(await settleVirtualScroll(options)).toBe(false);
    expect(virtualizer.scrollToIndex).toHaveBeenCalledTimes(16);
    expect(options.nextFrame).toHaveBeenCalledTimes(30);
  });

  it("cancels between estimate frames without a stale exact scroll", async () => {
    const { options, virtualizer } = setup();
    let active = true;
    options.isCurrent = () => active;
    options.nextFrame = vi.fn(async () => {
      active = false;
    });
    expect(await settleVirtualScroll(options)).toBe(false);
    expect(options.nextFrame).toHaveBeenCalledTimes(1);
    expect(virtualizer.scrollToOffset).not.toHaveBeenCalled();
  });

  it("does not double-apply end alignment to an already aligned offset", async () => {
    const { options, virtualizer } = setup();
    options.align = "end";
    virtualizer.getVirtualItems.mockReturnValue([{ index: 1 }]);
    virtualizer.getOffsetForIndex.mockReturnValue([200, "end"]);
    expect(await settleVirtualScroll(options)).toBe(true);
    expect(virtualizer.getOffsetForIndex).toHaveBeenCalledWith(1, "end");
    expect(virtualizer.scrollToOffset).toHaveBeenCalledWith(200, { align: "start" });
  });

  it("bounds waits for a missing virtualizer or invalid index", async () => {
    const { options } = setup();
    options.getVirtualizer = () => undefined;
    expect(await settleVirtualScroll(options)).toBe(false);
    expect(options.nextFrame).toHaveBeenCalledTimes(5);
  });
});

/** A scroller whose row starts and scrollTop the test moves between frames,
 *  the way TanStack does when it measures rows near the viewport. */
function anchorSetup(rowStart: number, scrollTop: number, maxOffset = 100_000) {
  const list = { rowStart, scrollTop, maxOffset };
  const clamp = (offset: number) => Math.max(0, Math.min(list.maxOffset, offset));
  const virtualizer = {
    getOffsetForIndex: vi.fn((index: number) =>
      index === 41 ? ([clamp(list.rowStart), "start"] as const) : undefined,
    ),
    getOffsetForAlignment: vi.fn((offset: number) => clamp(offset)),
    scrollToOffset: vi.fn((offset: number) => {
      list.scrollTop = clamp(offset);
    }),
  };
  const options: AnchorSettleOptions = {
    resolveIndex: () => 41,
    offsetPx: 26,
    getVirtualizer: () => virtualizer,
    getScrollTop: () => list.scrollTop,
    isCurrent: () => true,
    nextFrame: vi.fn().mockResolvedValue(undefined),
  };
  return { list, options, virtualizer };
}

describe("settleScrollAnchor", () => {
  it("re-anchors after first measurements push the viewport past the anchor row", async () => {
    const { list, options, virtualizer } = anchorSetup(4920, 4920);
    let frame = 0;
    options.nextFrame = vi.fn(async () => {
      frame++;
      if (frame === 1) {
        // Rows above the anchor grew and the batch also compensated rows
        // below it, as observed in the browser.
        list.rowStart = 5886;
        list.scrollTop = 8061;
      }
    });

    expect(await settleScrollAnchor(options)).toBe(true);
    expect(virtualizer.scrollToOffset.mock.calls).toEqual([
      [4946, { align: "start" }],
      [5912, { align: "start" }],
    ]);
    expect(list.scrollTop).toBe(5912);
  });

  it("settles on the last screen when the anchor offset is past the end", async () => {
    const { list, options } = anchorSetup(9800, 9800, 9900);
    options.offsetPx = 300;

    expect(await settleScrollAnchor(options)).toBe(true);
    expect(list.scrollTop).toBe(9900);
  });

  it("stops correcting once a newer scroll replaces the restore", async () => {
    const { list, options, virtualizer } = anchorSetup(4920, 4920);
    let current = true;
    options.isCurrent = () => current;
    options.nextFrame = vi.fn(async () => {
      list.rowStart = 5886;
      list.scrollTop = 8061;
      current = false;
    });

    expect(await settleScrollAnchor(options)).toBe(false);
    expect(virtualizer.scrollToOffset).toHaveBeenCalledTimes(1);
    expect(list.scrollTop).toBe(8061);
  });

  it("does not scroll when the anchor row is gone", async () => {
    const { options, virtualizer } = anchorSetup(4920, 4920);
    options.resolveIndex = () => -1;

    expect(await settleScrollAnchor(options)).toBe(false);
    expect(virtualizer.scrollToOffset).not.toHaveBeenCalled();
  });

  it("gives up when measurements keep moving the anchor row", async () => {
    const { list, options, virtualizer } = anchorSetup(4920, 4920);
    options.maxFrames = 6;
    options.nextFrame = vi.fn(async () => {
      list.rowStart += 100;
    });

    expect(await settleScrollAnchor(options)).toBe(false);
    expect(virtualizer.scrollToOffset).toHaveBeenCalledTimes(6);
  });
});
