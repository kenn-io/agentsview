import type { DisplayItem, MessageItem } from "./display-items.js";
import type { DbMessage as Message } from "../api/generated/index.js";

// System rows that arrive while the assistant is still working on the
// current prompt. The Claude parser keeps them on role "user" so analytics
// do not count them as replies, but they are not prompts: a task
// notification reports a background agent finishing, and stop hook feedback
// is injected by tooling. Treating them as turn boundaries split one
// exchange into many and promoted every "still waiting" status line before
// them to a final answer.
const MID_TURN_SYSTEM_SUBTYPES = new Set(["task_notification", "stop_hook"]);

function isMidTurnSystemMessage(m: Message): boolean {
  return (
    m.is_system === true && !!m.source_subtype && MID_TURN_SYSTEM_SUBTYPES.has(m.source_subtype)
  );
}

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

    // Mid-turn system rows are neither prompts nor answers. Focused mode
    // drops them with the rest of the intermediate work; normal mode still
    // renders them as boundary cards.
    if (isMidTurnSystemMessage(item.message)) {
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
