/** Cancelable, bounded reveal of an occurrence inside a virtual transcript. */
import { rangeIsDisclosed } from "./details-reveal.js";
import { currentRangeForBlock } from "./search-block.svelte.js";
import {
  isRectVisibleIn,
  revealInContainer,
  scrollNestedContainers,
  type SearchRect,
} from "./scroll-geometry.js";

export interface RevealOptions {
  ordinal: number;
  blockKey: string;
  getContainer: () => HTMLElement | undefined;
  isCurrent: () => boolean;
  ensureLoaded: (ordinal: number) => Promise<void>;
  mountMessage: () => Promise<boolean>;
  scrollToOffset: (offset: number) => void;
  afterUpdate: () => Promise<void>;
  nextFrame: () => Promise<void>;
}

/** Exact data equality avoids CSS escaping and cross-session document queries. */
export function findSearchBlock(root: HTMLElement, key: string): HTMLElement | undefined {
  return Array.from(root.querySelectorAll<HTMLElement>("[data-search-block]")).find(
    (element) => element.dataset.searchBlock === key,
  );
}

function targetRect(block: HTMLElement): SearchRect {
  const range = currentRangeForBlock(block);
  if (range && typeof range.getBoundingClientRect === "function") {
    const rect = range.getBoundingClientRect();
    if (rect.width || rect.height) return rect;
  }
  return block.getBoundingClientRect();
}

export async function revealMatch(options: RevealOptions): Promise<boolean> {
  if (!options.isCurrent()) return false;
  await options.ensureLoaded(options.ordinal);
  if (!options.isCurrent()) return false;
  await options.afterUpdate();
  if (!options.isCurrent()) return false;
  let root = options.getContainer();
  if (!root) return false;

  // Do not jump back to the row start when the next occurrence is already
  // mounted. This matters for navigation inside a single tall code/output row.
  if (!findSearchBlock(root, options.blockKey)) {
    if (!(await options.mountMessage()) || !options.isCurrent()) return false;
  }
  await options.afterUpdate();
  await options.nextFrame();
  if (!options.isCurrent()) return false;

  let block: HTMLElement | undefined;
  for (let attempt = 0; attempt < 4; attempt++) {
    root = options.getContainer();
    if (!root || !options.isCurrent()) return false;
    block = findSearchBlock(root, options.blockKey);
    if (block) break;
    if (attempt < 3) {
      await options.afterUpdate();
      await options.nextFrame();
    }
  }
  if (!root || !block) return false;

  const settle = (target: HTMLElement): void => {
    scrollNestedContainers(
      target,
      root!,
      () => targetRect(target),
      currentRangeForBlock(target)?.startContainer,
    );
    revealInContainer(root!, () => targetRect(target), true, false, options.scrollToOffset);
  };

  settle(block);

  // One recheck covers virtual-row height changes after expanding a block.
  await options.nextFrame();
  if (!options.isCurrent() || options.getContainer() !== root) return false;
  block = findSearchBlock(root, options.blockKey);
  if (!block) return false;
  settle(block);

  // An expanded block can finish measuring after the recheck, and the
  // virtualizer then adjusts the offset on its own. Confirm the occurrence is
  // still inside the transcript over a two-frame settle window and correct
  // again while the bounded budget lasts.
  let visible = false;
  for (let attempt = 0; attempt < 3; attempt++) {
    await options.nextFrame();
    await options.nextFrame();
    if (!options.isCurrent() || options.getContainer() !== root) return false;
    block = findSearchBlock(root, options.blockKey);
    if (!block) return false;
    if (isRectVisibleIn(root, targetRect(block))) {
      visible = true;
      break;
    }
    settle(block);
  }
  if (!options.isCurrent()) return false;
  const range = currentRangeForBlock(block);
  if (!range || !rangeIsDisclosed(block, range)) return false;
  return visible;
}
