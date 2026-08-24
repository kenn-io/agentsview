export type ArrowTarget = "sessionList" | "message" | "none";

let sessionListElement: HTMLElement | null = null;

export function registerSessionList(element: HTMLElement): () => void {
  sessionListElement = element;
  return () => {
    if (sessionListElement === element) sessionListElement = null;
  };
}

export function resolveArrowTarget(
  activeElement: Element | null = typeof document === "undefined" ? null : document.activeElement,
  sessionList: HTMLElement | null = sessionListElement,
  finePointer =
    typeof window !== "undefined" &&
    window.matchMedia("(hover: hover) and (pointer: fine)").matches,
): ArrowTarget {
  if (!sessionList?.isConnected) return "message";

  const active = activeElement as HTMLElement | null;
  if (
    active?.matches("input, textarea, select, [contenteditable='true']") ||
    active?.closest("[role='dialog']")
  ) {
    return "none";
  }

  if (sessionList.contains(active)) return "sessionList";
  if (finePointer && sessionList.matches(":hover")) return "sessionList";
  return "message";
}

export function getSessionListElement(): HTMLElement | null {
  return sessionListElement;
}
