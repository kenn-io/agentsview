export type ArrowTarget = "sessionList" | "message" | "none";
export type ArrowInteractionTarget = Exclude<ArrowTarget, "none">;

let sessionListElement: HTMLElement | null = null;
let sessionListNavigate: ((delta: number) => void) | null = null;

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
  if (!sessionListElement?.isConnected || !sessionListNavigate) return false;
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

  if (!sessionList?.isConnected) return "message";
  if (lastInteraction) return lastInteraction;
  if (sessionList.contains(active)) return "sessionList";
  return "message";
}

export function getSessionListElement(): HTMLElement | null {
  return sessionListElement;
}
