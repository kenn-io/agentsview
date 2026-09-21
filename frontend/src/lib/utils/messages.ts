import type { DbMessage as Message } from "../api/generated/index.js";

const SYSTEM_MSG_PREFIXES = [
  "This session is being continued",
  "[Request interrupted",
  "<task-notification>",
  "<command-message>",
  "<command-name>",
  "<local-command-",
  "Stop hook feedback:",
];

const SYSTEM_REMINDER_OPEN_TAG = "<system-reminder>";
const SYSTEM_REMINDER_CLOSE_TAG = "</system-reminder>";
// Preview-only: matches the bare open tag and the attributed form
// (`<system-reminder data-role="user-context">`) that clients write
// when they inject per-turn context as the first user message. The
// literal constants above stay the classification matcher.
const SYSTEM_REMINDER_OPEN_TAG_RE = /^<system-reminder(?:\s[^>]*)?>/;

const LEGACY_GOAL_CONTEXT_PREFIX = "<goal_context>";
const CODEX_INTERNAL_CONTEXT_TAG_PREFIX = "<codex_internal_context";
const GOAL_CONTEXT_SOURCE_ATTR_RE = /(?:^|\s)source="goal"(?:\s|\/|$)/;

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
 * Reports whether the transcript renders this message as a system boundary
 * card instead of ordinary message content. The Claude parser keeps these
 * rows on role "user" so analytics do not count them as assistant replies,
 * so the role alone cannot tell them apart.
 */
export function isSystemBoundaryMessage(m: Message): m is Message & { source_subtype: string } {
  if (m.is_compact_boundary) return false;
  if (!m.is_system) return false;
  return !!m.source_subtype && m.source_subtype !== "compact_boundary";
}

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
  const { remainder, stripped } = stripLeadingReminderBlocks(m.content);
  if (stripped && remainder.length === 0) return true;
  const trimmed = stripped ? remainder : m.content.trim();
  return isGoalContextMessage(trimmed) || SYSTEM_MSG_PREFIXES.some((p) => trimmed.startsWith(p));
}

function stripLeadingReminderBlocks(content: string): {
  remainder: string;
  stripped: boolean;
} {
  const original = content.trimStart();
  let rest = original;
  let stripped = false;
  while (rest.startsWith(SYSTEM_REMINDER_OPEN_TAG)) {
    const closeIdx = rest.indexOf(SYSTEM_REMINDER_CLOSE_TAG);
    if (closeIdx < 0) return { remainder: original, stripped: false };
    rest = rest.slice(closeIdx + SYSTEM_REMINDER_CLOSE_TAG.length).trimStart();
    stripped = true;
  }
  return { remainder: rest, stripped };
}

/**
 * Removes leading `<system-reminder>` envelopes from preview text,
 * including the attributed form the literal matcher above misses.
 * Returns `content` unchanged when nothing was stripped, when an
 * envelope is unclosed, or when the envelopes are the whole message,
 * so a truncated tag and a context-only message both keep a
 * non-empty label instead of collapsing to "".
 */
function stripLeadingReminderEnvelopes(content: string): string {
  let rest = content.trimStart();
  let stripped = false;
  for (;;) {
    const open = SYSTEM_REMINDER_OPEN_TAG_RE.exec(rest);
    if (!open) break;
    const closeIdx = rest.indexOf(SYSTEM_REMINDER_CLOSE_TAG, open[0].length);
    if (closeIdx < 0) return content;
    rest = rest.slice(closeIdx + SYSTEM_REMINDER_CLOSE_TAG.length).trimStart();
    stripped = true;
  }
  return stripped && rest.length > 0 ? rest : content;
}

function isGoalContextMessage(trimmedContent: string): boolean {
  if (trimmedContent.startsWith(LEGACY_GOAL_CONTEXT_PREFIX)) {
    return true;
  }
  if (!trimmedContent.startsWith(CODEX_INTERNAL_CONTEXT_TAG_PREFIX)) {
    return false;
  }
  const tagEnd = trimmedContent.indexOf(">");
  if (tagEnd < 0) {
    return false;
  }
  const openTag = trimmedContent.slice(0, tagEnd);
  return GOAL_CONTEXT_SOURCE_ATTR_RE.test(openTag);
}

export interface MessagePreview {
  /** Display text, with leading system-reminder envelopes removed and
   *  Claude Code shell-shortcut wrappers
   *  replaced: `<bash-input>cmd</bash-input>` becomes `!cmd`,
   *  stdout/stderr are unwrapped. */
  text: string;
  /** True when the underlying message was a shell shortcut
   *  (`<bash-input>` or `<bash-stdout>`/`<bash-stderr>`). Lets
   *  the caller style the preview as code. */
  isShell: boolean;
}

/**
 * Build a one-line preview of a session's first message.
 * A leading `<system-reminder>` envelope, bare or attributed, is dropped first.
 * Claude Code's shell-shortcut wrappers are replaced with the human-typed form
 * and flagging whether the original was a shell shortcut so the
 * caller can render the label as code.
 *
 * For message-body rendering use `renderMarkdown` instead — it
 * emits real code blocks via marked extensions.
 */
export function previewMessage(text: string | null | undefined): MessagePreview {
  if (!text) return { text: "", isShell: false };
  const source = stripLeadingReminderEnvelopes(text);
  const isShell = /<bash-(?:input|stdout|stderr)>/.test(source);
  const out = source
    .replace(/<bash-input>([\s\S]*?)<\/bash-input>/g, (_, cmd: string) => `!${cmd.trim()}`)
    .replace(/<bash-(?:stdout|stderr)>([\s\S]*?)<\/bash-(?:stdout|stderr)>/g, (_, body: string) =>
      body.trim(),
    );
  if (source !== text && !out.trim()) {
    const originalIsShell = /<bash-(?:input|stdout|stderr)>/.test(text);
    const originalOut = text
      .replace(/<bash-input>([\s\S]*?)<\/bash-input>/g, (_, cmd: string) => `!${cmd.trim()}`)
      .replace(/<bash-(?:stdout|stderr)>([\s\S]*?)<\/bash-(?:stdout|stderr)>/g, (_, body: string) =>
        body.trim(),
      );
    return { text: originalOut, isShell: originalIsShell };
  }
  return { text: out, isShell };
}

/** Plain-text variant of `previewMessage` for non-visual callers
 *  (rename input pre-fill, confirm-delete sentence, etc.). */
export function normalizeMessagePreview(text: string | null | undefined): string {
  return previewMessage(text).text;
}
