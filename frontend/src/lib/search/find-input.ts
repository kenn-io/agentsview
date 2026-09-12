/** Keep IME composition separate from search navigation and global shortcuts. */
export interface FindCompositionState {
  composing: boolean;
}

export function isComposingKey(event: KeyboardEvent): boolean {
  // WebKit can report isComposing=false on the Enter that confirms a candidate.
  return event.isComposing || event.keyCode === 229;
}

export function ignoreShortcut(event: KeyboardEvent): boolean {
  return event.defaultPrevented || isComposingKey(event);
}

/** Guard the shared FindBar before its input handles Enter or Escape. */
export function protectFindInput(node: HTMLElement, state: FindCompositionState) {
  function start() {
    state.composing = true;
  }
  function end() {
    state.composing = false;
  }
  function keydown(event: KeyboardEvent) {
    if (state.composing || ignoreShortcut(event)) {
      // Preserve the browser's candidate confirmation/cancellation behavior.
      event.stopPropagation();
    }
  }
  node.addEventListener("compositionstart", start);
  node.addEventListener("compositionend", end);
  node.addEventListener("keydown", keydown, true);
  return {
    destroy() {
      node.removeEventListener("compositionstart", start);
      node.removeEventListener("compositionend", end);
      node.removeEventListener("keydown", keydown, true);
      state.composing = false;
    },
  };
}
