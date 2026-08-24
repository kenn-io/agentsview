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
    typeof window.matchMedia === "function" &&
    window.matchMedia("(hover: hover) and (pointer: fine)").matches,
): ArrowTarget {
  const active = activeElement as HTMLElement | null;
  if (
    active?.matches("input, textarea, select, [contenteditable='true']") ||
    active?.closest("[role='dialog']")
  ) {
    return "none";
  }

  if (!sessionList?.isConnected) return "message";

  // A fine-pointer hover inside the document is fresher than focus left on a
  // row, so moving into the message pane restores message navigation.
  const pointerIsInsideDocument =
    finePointer &&
    typeof document !== "undefined" &&
    document.documentElement.matches(":hover");
  if (pointerIsInsideDocument) {
    return sessionList.matches(":hover") ? "sessionList" : "message";
  }

  if (sessionList.contains(active)) return "sessionList";
  if (finePointer && sessionList.matches(":hover")) return "sessionList";
  return "message";
}

export function getSessionListElement(): HTMLElement | null {
  return sessionListElement;
}
