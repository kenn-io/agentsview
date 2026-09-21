/** Viewport-coordinate geometry for nested, independently scrolling blocks. */
export interface SearchRect {
  top: number;
  right: number;
  bottom: number;
  left: number;
}

export function isRectWithin(target: SearchRect, viewport: SearchRect, padding = 0): boolean {
  return (
    target.top >= viewport.top + padding &&
    target.bottom <= viewport.bottom - padding &&
    target.left >= viewport.left + padding &&
    target.right <= viewport.right - padding
  );
}

/** Both rectangles use viewport coordinates; their origins need not be zero. */
export function centeredOffset(
  currentOffset: number,
  targetStart: number,
  targetEnd: number,
  viewportStart: number,
  viewportEnd: number,
  scale = 1,
): number {
  return (
    currentOffset +
    (targetStart + targetEnd - viewportStart - viewportEnd) / (2 * validScale(scale))
  );
}

function validScale(scale: number): number {
  return Number.isFinite(scale) && scale > 0 ? scale : 1;
}

function viewportFor(element: HTMLElement) {
  const rect = element.getBoundingClientRect();
  // CSS zoom and axis-aligned scale affect client rectangles, while scrolling,
  // borders, and client dimensions still use the element's layout coordinates.
  const scaleX = validScale(rect.width / element.offsetWidth);
  const scaleY = validScale(rect.height / element.offsetHeight);
  const top = rect.top + element.clientTop * scaleY;
  const left = rect.left + element.clientLeft * scaleX;
  return {
    top,
    left,
    bottom: top + element.clientHeight * scaleY,
    right: left + element.clientWidth * scaleX,
    scaleX,
    scaleY,
  };
}

/** Whether any part of the target rect intersects the container's viewport. */
export function isRectVisibleIn(element: HTMLElement, target: SearchRect): boolean {
  const viewport = viewportFor(element);
  return (
    target.bottom > viewport.top &&
    target.top < viewport.bottom &&
    target.right > viewport.left &&
    target.left < viewport.right
  );
}

/** Move only clipped axes; a visible occurrence never causes recentering. */
export function revealInContainer(
  element: HTMLElement,
  readTarget: () => SearchRect,
  vertical = true,
  horizontal = true,
  scrollVertical: (offset: number) => void = (offset) => {
    element.scrollTop = offset;
  },
): boolean {
  const viewport = viewportFor(element);
  const target = readTarget();
  let moved = false;
  if (
    vertical &&
    element.clientHeight > 0 &&
    (target.top < viewport.top || target.bottom > viewport.bottom)
  ) {
    const offset = Math.max(
      0,
      Math.min(
        element.scrollHeight - element.clientHeight,
        centeredOffset(
          element.scrollTop,
          target.top,
          target.bottom,
          viewport.top,
          viewport.bottom,
          viewport.scaleY,
        ),
      ),
    );
    if (offset !== element.scrollTop) {
      scrollVertical(offset);
      moved = true;
    }
  }
  if (
    horizontal &&
    element.clientWidth > 0 &&
    (target.left < viewport.left || target.right > viewport.right)
  ) {
    const offset = Math.max(
      0,
      Math.min(
        element.scrollWidth - element.clientWidth,
        centeredOffset(
          element.scrollLeft,
          target.left,
          target.right,
          viewport.left,
          viewport.right,
          viewport.scaleX,
        ),
      ),
    );
    if (offset !== element.scrollLeft) {
      element.scrollLeft = offset;
      moved = true;
    }
  }
  return moved;
}

/** Reveal inner panes first so their clipped text has valid outer geometry. */
export function scrollNestedContainers(
  block: HTMLElement,
  root: HTMLElement,
  readTarget: () => SearchRect,
  targetNode: Node = block,
): boolean {
  if (!root.contains(block)) return false;
  // Markdown/skill attachments can own an entire block with scrolling code or
  // tables inside it. Start at the match, otherwise those descendant panes are
  // skipped. Ignore stale or foreign nodes and retain the block fallback.
  const start = block.contains(targetNode)
    ? targetNode.nodeType === Node.ELEMENT_NODE
      ? (targetNode as HTMLElement)
      : (targetNode.parentElement ?? block)
    : block;
  let moved = false;
  for (let node: HTMLElement | null = start; node && node !== root; node = node.parentElement) {
    const style = getComputedStyle(node);
    const vertical =
      /^(auto|scroll|overlay)$/.test(style.overflowY) && node.scrollHeight > node.clientHeight;
    const horizontal =
      /^(auto|scroll|overlay)$/.test(style.overflowX) && node.scrollWidth > node.clientWidth;
    if (vertical || horizontal)
      moved = revealInContainer(node, readTarget, vertical, horizontal) || moved;
  }
  return moved;
}
