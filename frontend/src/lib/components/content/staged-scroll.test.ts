import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { settleVirtualScroll, type StagedScrollOptions } from "./staged-scroll.js";

function setup() {
  const virtualizer = {
    options: { count: 3 },
    getVirtualItems: vi.fn(() => [] as { index: number }[]),
    scrollToIndex: vi.fn(),
  };
  const options: StagedScrollOptions = {
    getIndex: () => 1,
    align: "start",
    getCount: () => 3,
    getVirtualizer: () => virtualizer,
    isCurrent: () => true,
    nextFrame: vi.fn().mockResolvedValue(undefined),
  };
  return { options, virtualizer };
}

afterEach(() => vi.unstubAllGlobals());

describe("settleVirtualScroll", () => {
  it("completes immediately when the target row is rendered", async () => {
    const { options, virtualizer } = setup();
    virtualizer.getVirtualItems.mockReturnValue([{ index: 1 }]);
    expect(await settleVirtualScroll(options)).toBe(true);
    expect(virtualizer.scrollToIndex).toHaveBeenCalledWith(1, { align: "start" });
    expect(options.nextFrame).not.toHaveBeenCalled();
  });

  it("does not report an estimated scroll as completion", async () => {
    const { options, virtualizer } = setup();
    let frame = 0;
    options.nextFrame = vi.fn(async () => {
      frame++;
      if (frame === 2) {
        virtualizer.getVirtualItems.mockReturnValue([{ index: 1 }]);
      }
    });
    expect(await settleVirtualScroll(options)).toBe(true);
    expect(options.nextFrame).toHaveBeenCalledTimes(2);
    expect(virtualizer.scrollToIndex).toHaveBeenCalledTimes(2);
    expect(virtualizer.scrollToIndex).toHaveBeenLastCalledWith(1, { align: "start" });
  });

  it("resolves the target index again after older rows arrive", async () => {
    const { options, virtualizer } = setup();
    let index = 1;
    options.getIndex = () => index;
    options.nextFrame = vi.fn(async () => {
      index = 3;
      virtualizer.options.count = 5;
      options.getCount = () => 5;
      virtualizer.getVirtualItems.mockReturnValue([{ index: 3 }]);
    });

    expect(await settleVirtualScroll(options)).toBe(true);
    expect(virtualizer.scrollToIndex).toHaveBeenNthCalledWith(1, 1, { align: "start" });
    expect(virtualizer.scrollToIndex).toHaveBeenLastCalledWith(3, { align: "start" });
    expect(virtualizer.scrollToIndex).toHaveBeenCalledTimes(2);
  });

  it("waits for the virtual count to catch up with newly loaded messages", async () => {
    const { options, virtualizer } = setup();
    virtualizer.options.count = 0;
    options.nextFrame = vi.fn(async () => {
      virtualizer.options.count = 3;
      virtualizer.getVirtualItems.mockReturnValue([{ index: 1 }]);
    });
    expect(await settleVirtualScroll(options)).toBe(true);
    expect(options.nextFrame).toHaveBeenCalledTimes(1);
    expect(virtualizer.scrollToIndex).toHaveBeenCalledWith(1, { align: "start" });
  });

  it("expires after bounded estimate retries", async () => {
    const { options, virtualizer } = setup();
    expect(await settleVirtualScroll(options)).toBe(false);
    expect(virtualizer.scrollToIndex).toHaveBeenCalledTimes(16);
    expect(options.nextFrame).toHaveBeenCalledTimes(30);
  });

  it("cancels between estimate frames without a stale exact scroll", async () => {
    const { options } = setup();
    let active = true;
    options.isCurrent = () => active;
    options.nextFrame = vi.fn(async () => {
      active = false;
    });
    expect(await settleVirtualScroll(options)).toBe(false);
    expect(options.nextFrame).toHaveBeenCalledTimes(1);
  });

  it("bounds waits for a missing virtualizer or invalid index", async () => {
    const { options } = setup();
    options.getVirtualizer = () => undefined;
    expect(await settleVirtualScroll(options)).toBe(false);
    expect(options.nextFrame).toHaveBeenCalledTimes(5);
  });
});
