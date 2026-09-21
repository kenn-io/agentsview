/** Temporary native-disclosure state owned by one current search attachment. */
function detailsForRange(root: HTMLElement, range: Range): HTMLDetailsElement[] {
  if (!root.contains(range.startContainer) || !root.contains(range.endContainer)) return [];
  const details = Array.from(root.querySelectorAll<HTMLDetailsElement>("details"));
  if (root.tagName === "DETAILS") details.unshift(root as HTMLDetailsElement);
  return details.filter((element) => {
    if (!range.intersectsNode(element)) return false;
    const summary = Array.from(element.children).find((child) => child.tagName === "SUMMARY");
    // The first summary is already visible. Finding its label must not expand it.
    return (
      !summary || !summary.contains(range.startContainer) || !summary.contains(range.endContainer)
    );
  });
}

/** Geometry alone cannot detect text hidden by a closed native details element. */
export function rangeIsDisclosed(root: HTMLElement, range: Range): boolean {
  if (range.collapsed || !root.contains(range.startContainer) || !root.contains(range.endContainer))
    return false;
  if (detailsForRange(root, range).some((element) => !element.open)) return false;
  for (const boundary of [range.startContainer, range.endContainer]) {
    let element =
      boundary.nodeType === Node.ELEMENT_NODE ? (boundary as Element) : boundary.parentElement;
    if (element) {
      const visibility = getComputedStyle(element).visibility;
      if (visibility === "hidden" || visibility === "collapse") return false;
    }
    for (; element; element = element.parentElement) {
      if (getComputedStyle(element).display === "none") return false;
      if (element === root) break;
    }
  }
  return true;
}

/** Restore only changes made by search, never an explicit user disclosure choice. */
export function createDetailsReveal(root: HTMLElement) {
  const owned = new Map<HTMLDetailsElement, string | null>();
  const manual = new WeakSet<HTMLDetailsElement>();
  let disposed = false;

  function restoreName(element: HTMLDetailsElement, name: string | null): void {
    if (name !== null) element.setAttribute("name", name);
  }

  function restore(element: HTMLDetailsElement): void {
    const name = owned.get(element);
    if (name === undefined) return;
    owned.delete(element);
    element.open = false;
    restoreName(element, name);
  }

  function relinquish(element: HTMLDetailsElement): void {
    const name = owned.get(element);
    if (name === undefined) return;
    owned.delete(element);
    manual.add(element);
    // A summary click toggles after the listener. Restore group membership only
    // after that default action, so closing cannot close another group's item.
    queueMicrotask(() => restoreName(element, name));
  }

  function onClick(event: Event): void {
    const target = event.target;
    if (!(target instanceof Element)) return;
    const summary = target.closest("summary");
    const details = summary?.parentElement;
    if (details?.tagName === "DETAILS") relinquish(details as HTMLDetailsElement);
  }

  function onToggle(event: Event): void {
    const element = event.target;
    if (element instanceof HTMLDetailsElement && !element.open) relinquish(element);
  }

  root.addEventListener("click", onClick, true);
  root.addEventListener("toggle", onToggle, true);
  return {
    update(range?: Range): void {
      if (disposed) return;
      const required = new Set(range ? detailsForRange(root, range) : []);
      for (const element of owned.keys()) {
        if (!required.has(element)) restore(element);
      }
      for (const element of required) {
        if (owned.has(element) && !element.open) {
          relinquish(element);
        } else if (!element.open && !manual.has(element)) {
          // Avoid the browser closing a previously open same-name sibling.
          // Membership and closed state are both restored on navigation/close.
          const name = element.getAttribute("name");
          owned.set(element, name);
          if (name !== null) element.removeAttribute("name");
          element.open = true;
        }
      }
    },
    destroy(): void {
      if (disposed) return;
      disposed = true;
      root.removeEventListener("click", onClick, true);
      root.removeEventListener("toggle", onToggle, true);
      for (const element of owned.keys()) restore(element);
    },
  };
}
