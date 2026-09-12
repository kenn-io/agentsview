/** Settle virtual-list scrolling only after the exact-offset phase succeeds. */
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
  align: ScrollAlign;
  getVirtualizer(): ScrollTarget | undefined;
  getCount(): number;
  isCurrent(): boolean;
  nextFrame(): Promise<void>;
  waitFrames?: number;
  scrollRetries?: number;
}

/** A false result means cancellation, invalid index, or bounded retry expiry. */
export async function settleVirtualScroll(options: StagedScrollOptions): Promise<boolean> {
  let waitFrames = options.waitFrames ?? 0;
  let retries = options.scrollRetries ?? 0;
  const { index, align } = options;
  for (;;) {
    if (!options.isCurrent()) return false;
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
      virtualizer.scrollToOffset(Math.round(offset[0]), {
        align: getAlignedOffsetScrollAlign(align),
      });
      return true;
    }

    // Estimated scrolling is not completion. ResizeObserver and Svelte need
    // two frames before the next measured-offset attempt can be trusted.
    virtualizer.scrollToIndex(index, { align });
    if (retries >= 15) return false;
    await options.nextFrame();
    if (!options.isCurrent()) return false;
    await options.nextFrame();
    retries++;
  }
}
