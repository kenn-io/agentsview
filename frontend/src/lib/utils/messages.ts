import type { Message } from "../api/types.js";

const SYSTEM_MSG_PREFIXES = [
  "This session is being continued",
  "[Request interrupted",
  "<task-notification>",
  "<command-message>",
  "<command-name>",
  "<local-command-",
  "Stop hook feedback:",
];

// Subtypes the Claude parser promotes into visible system messages
// that the SPA renders via SystemBoundaryCard. These must pass
// through the MessageList filter even though is_system=true.
const VISIBLE_SYSTEM_SUBTYPES = new Set([
  "continuation",
  "resume",
  "interrupted",
  "task_notification",
  "stop_hook",
]);

/**
 * Returns true if the message is system-injected and should be
 * hidden from the UI. Checks the backend is_system flag first,
 * then falls back to prefix detection for parsers that don't set it.
 *
 * Compact boundary messages and promoted system-subtype messages
 * (continuation, resume, interrupted, task_notification, stop_hook)
 * are system-flagged but rendered as dividers/cards, so they are
 * kept visible here.
 */
export function isSystemMessage(m: Message): boolean {
  if (m.is_compact_boundary) return false;
  if (m.source_subtype && VISIBLE_SYSTEM_SUBTYPES.has(m.source_subtype)) {
    return false;
  }
  if (m.is_system) return true;
  if (m.role !== "user") return false;
  const trimmed = m.content.trim();
  return SYSTEM_MSG_PREFIXES.some((p) => trimmed.startsWith(p));
}

/**
 * Returns true when a message represents an explicit compact
 * boundary inserted by the agent runtime.
 */
export function isCompactBoundary(m: Message): boolean {
  return Boolean(m.is_compact_boundary);
}

/**
 * Replace Claude Code shell-shortcut wrappers with the form a user
 * typed (or saw output as): `<bash-input>cmd</bash-input>` becomes
 * `!cmd`, and `<bash-stdout>` / `<bash-stderr>` are unwrapped.
 *
 * Use for plain-text labels (sidebar, breadcrumbs, modals). For
 * message-body rendering use `renderMarkdown`, which emits real
 * code blocks via marked extensions.
 */
export function normalizeMessagePreview(
  text: string | null | undefined,
): string {
  if (!text) return "";
  return text
    .replace(
      /<bash-input>([\s\S]*?)<\/bash-input>/g,
      (_, cmd: string) => `!${cmd.trim()}`,
    )
    .replace(
      /<bash-(?:stdout|stderr)>([\s\S]*?)<\/bash-(?:stdout|stderr)>/g,
      (_, out: string) => out.trim(),
    );
}
