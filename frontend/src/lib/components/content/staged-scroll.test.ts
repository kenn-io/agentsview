import { describe, expect, it, vi } from "vite-plus/test";
import { settleVirtualScroll, type StagedScrollOptions } from "./staged-scroll.js";

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
