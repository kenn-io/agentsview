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

interface AnchorScrollTarget {
  getOffsetForIndex(index: number, align: "start"): readonly [number, unknown] | undefined;
  getOffsetForAlignment(toOffset: number, align: "start"): number;
  scrollToOffset(offset: number, options: { align: "start" }): void;
}

export interface AnchorSettleOptions {
  /** Display index of the anchor row now, or -1 once it is gone. */
  resolveIndex(): number;
  /** Pixels the viewport top sits below the anchor row's top. */
  offsetPx: number;
  getVirtualizer(): AnchorScrollTarget | undefined;
  getScrollTop(): number;
  isCurrent(): boolean;
  nextFrame(): Promise<void>;
  stableFrames?: number;
  maxFrames?: number;
}

/** Hold the viewport top `offsetPx` below the anchor row's top while row
 *  measurements land. Rows measured for the first time near the viewport move
 *  scrollTop, because TanStack compensates them against estimated starts, so a
 *  single scroll can end rows away from the anchor. A true result means the
 *  position held for `stableFrames` frames in a row; false means cancellation,
 *  a missing anchor, or no settled position within `maxFrames`. */
export async function settleScrollAnchor(options: AnchorSettleOptions): Promise<boolean> {
  const stableFrames = options.stableFrames ?? 5;
  const maxFrames = options.maxFrames ?? 90;
  let stable = 0;
  for (let frame = 0; frame < maxFrames; frame++) {
    if (!options.isCurrent()) return false;
    const virtualizer = options.getVirtualizer();
    const index = options.resolveIndex();
    const row =
      virtualizer && index >= 0 ? virtualizer.getOffsetForIndex(index, "start") : undefined;
    if (!virtualizer || !row) return false;
    const target = Math.round(
      virtualizer.getOffsetForAlignment(row[0] + options.offsetPx, "start"),
    );
    if (Math.abs(options.getScrollTop() - target) > 1) {
      virtualizer.scrollToOffset(target, { align: "start" });
      stable = 0;
    } else if (++stable >= stableFrames) {
      return true;
    }
    await options.nextFrame();
  }
  return false;
}
