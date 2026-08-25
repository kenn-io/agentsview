export type ArrowTarget = "sessionList" | "message" | "none";
export type ArrowInteractionTarget = Exclude<ArrowTarget, "none">;

let sessionListElement: HTMLElement | null = null;
let sessionListNavigate: ((delta: number) => void) | null = null;

function isRendered(element: HTMLElement | null): element is HTMLElement {
  if (!element?.isConnected) return false;

  const view = element.ownerDocument.defaultView;
  for (
    let current: HTMLElement | null = element;
    current;
    current = current.parentElement
  ) {
    const style = view?.getComputedStyle(current);
    if (
      current.hidden ||
      style?.display === "none" ||
      style?.visibility === "hidden" ||
      style?.visibility === "collapse"
    ) {
      return false;
    }
  }
  return true;
}

export function registerSessionList(
  element: HTMLElement,
  navigate: (delta: number) => void,
): () => void {
  sessionListElement = element;
  sessionListNavigate = navigate;
  return () => {
    if (sessionListElement === element) {
      sessionListElement = null;
      sessionListNavigate = null;
    }
  };
}

export function navigateRegisteredSessionList(delta: number): boolean {
  if (!isRendered(sessionListElement) || !sessionListNavigate) return false;
  sessionListNavigate(delta);
  return true;
}

export function resolveArrowTarget(
  activeElement: Element | null = typeof document === "undefined" ? null : document.activeElement,
  sessionList: HTMLElement | null = sessionListElement,
  lastInteraction: ArrowInteractionTarget | null = null,
): ArrowTarget {
  const active = activeElement as HTMLElement | null;
  if (
    active?.matches("input, textarea, select, [contenteditable='true']") ||
    active?.closest("[role='dialog']")
  ) {
    return "none";
  }

  if (!isRendered(sessionList)) return "message";
  if (lastInteraction) return lastInteraction;
  if (sessionList.contains(active)) return "sessionList";
  return "message";
}

export function getSessionListElement(): HTMLElement | null {
  return sessionListElement;
}
