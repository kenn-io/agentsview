import type { DisplayItem, MessageItem } from "./display-items.js";
import type { Message } from "../api/types.js";

export function filterDisplayItemsByTranscriptMode(
  items: DisplayItem[],
  mode: "normal" | "focused",
  options?: {
    isMessageVisible?: (message: Message) => boolean;
    keepAnswerBeforeTrailingTools?: boolean;
  },
): DisplayItem[] {
  if (mode === "normal") return items;

  const keepAnswerBeforeTrailingTools = options?.keepAnswerBeforeTrailingTools === true;
  const filtered: DisplayItem[] = [];
  let pendingAssistant: MessageItem | null = null;
  let toolAfterPendingAssistant = false;

  for (const item of items) {
    if (item.kind === "tool-group") {
      if (pendingAssistant && !keepAnswerBeforeTrailingTools) {
        toolAfterPendingAssistant = true;
      }
      continue;
    }

    // Compact-boundary messages are rendered as dividers, not
    // turns. Without this branch they'd be routed through the
    // assistant path (their role is "assistant") and overwrite
    // the previous pending response, so the real answer right
    // before the boundary would disappear from focused mode.
    // Treat them like a turn boundary: flush any pending
    // assistant first so it lands chronologically before the
    // divider, then push the divider itself.
    if (item.message.is_compact_boundary) {
      if (pendingAssistant && !toolAfterPendingAssistant) {
        filtered.push(pendingAssistant);
      }
      pendingAssistant = null;
      toolAfterPendingAssistant = false;
      filtered.push(item);
      continue;
    }

    if (item.message.role === "user") {
      if (pendingAssistant && !toolAfterPendingAssistant) {
        filtered.push(pendingAssistant);
      }
      pendingAssistant = null;
      toolAfterPendingAssistant = false;
      filtered.push(item);
      continue;
    }

    if (options?.isMessageVisible && !options.isMessageVisible(item.message)) {
      continue;
    }

    pendingAssistant = item;
    toolAfterPendingAssistant = false;
  }

  if (pendingAssistant && !toolAfterPendingAssistant) {
    filtered.push(pendingAssistant);
  }

  return filtered;
}

export function shouldAutoSwitchTranscriptModeToNormal(
  mode: "normal" | "focused",
  ordinal: number | null,
  visibleItems: DisplayItem[],
  normalVisibleItems: DisplayItem[],
): boolean {
  if (mode !== "focused" || ordinal === null) return false;

  const visible = visibleItems.some((item) => item.ordinals.includes(ordinal));
  if (visible) return false;

  return normalVisibleItems.some((item) => item.ordinals.includes(ordinal));
}
