import type { DbMessage as Message } from "../api/generated/index.js";
import { isToolOnly } from "./content-parser.js";
import { isSkillInvocationMessage } from "./messages.js";

export interface MessageItem {
  kind: "message";
  message: Message;
  /** Skill expansion row that directly precedes this user message. The
   *  message renders it as a disclosure attached to its own card. */
  skill?: Message;
  ordinals: number[];
}

export interface ToolGroupItem {
  kind: "tool-group";
  messages: Message[];
  ordinals: number[];
  timestamp: string;
}

export interface EmptyGroupItem {
  kind: "empty-group";
  messages: Message[];
  ordinals: number[];
  timestamp: string;
}

export type DisplayItem = MessageItem | ToolGroupItem | EmptyGroupItem;

export interface BuildDisplayItemsOptions {
  /** When true, tool-only messages are emitted as individual
   *  MessageItems instead of being grouped into ToolGroupItems.
   *  Useful when tool blocks are filtered out — the individual
   *  MessageContent component can then skip hidden segments. */
  skipToolGrouping?: boolean;
}

/**
 * Reports whether a message has nothing for its card to show: no text, no
 * thinking, and no tool calls. Codex Cloud records one for the empty final
 * answer that closes an image generation turn and for a prompt sent with no
 * text. The records are real turns, so the transcript keeps them in a
 * compact row instead of dropping them.
 */
export function isEmptyMessage(msg: Message): boolean {
  return (
    !msg.is_system &&
    !msg.is_compact_boundary &&
    !msg.has_tool_use &&
    !msg.has_thinking &&
    (msg.tool_calls?.length ?? 0) === 0 &&
    msg.content.trim() === "" &&
    msg.thinking_text.trim() === ""
  );
}

/**
 * Groups consecutive tool-only assistant messages, and consecutive empty
 * messages, into compact display items. Other messages pass through as
 * individual items.
 */
export function buildDisplayItems(
  messages: Message[],
  options?: BuildDisplayItemsOptions,
): DisplayItem[] {
  const groupTools = !options?.skipToolGrouping;
  const items: DisplayItem[] = [];

  for (const msg of messages) {
    const kind = isEmptyMessage(msg)
      ? "empty-group"
      : groupTools && isToolOnly(msg)
        ? "tool-group"
        : null;
    if (kind === null) {
      const prev = items.at(-1);
      if (
        prev?.kind === "message" &&
        prev.skill === undefined &&
        isSkillInvocationMessage(prev.message) &&
        msg.role === "user" &&
        !msg.is_system &&
        prev.message.session_id === msg.session_id &&
        msg.ordinal === prev.message.ordinal + 1
      ) {
        items[items.length - 1] = {
          kind: "message",
          message: msg,
          skill: prev.message,
          ordinals: [prev.message.ordinal, msg.ordinal],
        };
        continue;
      }
      items.push({
        kind: "message",
        message: msg,
        ordinals: [msg.ordinal],
      });
      continue;
    }
    const last = items.at(-1);
    if (last !== undefined && last.kind === kind) {
      last.messages.push(msg);
      last.ordinals.push(msg.ordinal);
      continue;
    }
    items.push({
      kind,
      messages: [msg],
      ordinals: [msg.ordinal],
      timestamp: msg.timestamp,
    });
  }

  return items;
}
