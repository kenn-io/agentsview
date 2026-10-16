/** Wait for a rendered row before handing measurement tracking to the virtualizer. */
import type { ScrollAlign } from "./message-scroll.js";

interface ScrollTarget {
  options: { count: number };
  getVirtualItems(): readonly { index: number }[];
  scrollToIndex(index: number, options: { align: ScrollAlign }): void;
}

export interface StagedScrollOptions {
  getIndex(): number;
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
  const { align } = options;
  for (;;) {
    if (!options.isCurrent()) return false;
    const index = options.getIndex();
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
    if (rendered) {
      virtualizer.scrollToIndex(index, { align });
      return true;
    }

    // ResizeObserver and Svelte need two frames before retrying the rendered row.
    virtualizer.scrollToIndex(index, { align });
    if (retries >= 15) return false;
    await options.nextFrame();
    if (!options.isCurrent()) return false;
    await options.nextFrame();
    retries++;
  }
}
