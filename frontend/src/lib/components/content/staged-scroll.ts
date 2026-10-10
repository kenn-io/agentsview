/** Settle virtual-list scrolling after measured row offsets stop changing. */
import { getAlignedOffsetScrollAlign, type ScrollAlign } from "./message-scroll.js";

interface ScrollTarget {
  options: { count: number };
  getVirtualItems(): readonly { index: number }[];
  getOffsetForIndex(index: number, align: ScrollAlign): readonly [number, unknown] | undefined;
  scrollToOffset(offset: number, options: { align: "start" }): void;
  scrollToIndex(index: number, options: { align: ScrollAlign }): void;
}

export interface StagedScrollOptions {
  index: number;
  getIndex?(): number;
  align: ScrollAlign;
  getVirtualizer(): ScrollTarget | undefined;
  getCount(): number;
  isCurrent(): boolean;
  nextFrame(this: void): Promise<void>;
  waitFrames?: number;
  scrollRetries?: number;
}

/** A false result means cancellation, invalid index, or bounded retry expiry. */
export async function settleVirtualScroll(options: StagedScrollOptions): Promise<boolean> {
  let waitFrames = options.waitFrames ?? 0;
  let retries = options.scrollRetries ?? 0;
  let previousOffset: number | undefined;
  let previousIndex: number | undefined;
  let stablePasses = 0;
  const { align } = options;
  for (;;) {
    if (!options.isCurrent()) return false;
    const index = options.getIndex?.() ?? options.index;
    if (index !== previousIndex) {
      stablePasses = 0;
      previousOffset = undefined;
      previousIndex = index;
    }
    const virtualizer = options.getVirtualizer();
    const count = options.getCount();
    if (
      waitFrames < 5 &&
      (!virtualizer || virtualizer.options.count !== count || index >= virtualizer.options.count)
    ) {
      await options.nextFrame();
      waitFrames++;
      continue;
    }
    if (!virtualizer || index < 0 || index >= virtualizer.options.count) return false;

    const rendered = virtualizer.getVirtualItems().some((item) => item.index === index);
    const offset = rendered ? virtualizer.getOffsetForIndex(index, align) : undefined;
    if (offset) {
      const measuredOffset = Math.round(offset[0]);
      virtualizer.scrollToOffset(measuredOffset, {
        align: getAlignedOffsetScrollAlign(align),
      });
      stablePasses = measuredOffset === previousOffset ? stablePasses + 1 : 0;
      previousOffset = measuredOffset;
      if (stablePasses >= 2) return true;
    } else {
      stablePasses = 0;
      previousOffset = undefined;
      virtualizer.scrollToIndex(index, { align });
    }

    // Being rendered does not mean preceding row heights are final. Recheck
    // after ResizeObserver and Svelte have settled, correcting delayed shifts.
    // Manual scrolling and newer navigation still cancel between frames.
    if (retries >= 15) return false;
    await options.nextFrame();
    if (!options.isCurrent()) return false;
    await options.nextFrame();
    retries++;
  }
}
