import type { DbMessage as Message } from "../../api/generated/index.js";
import { isSystemBoundaryMessage, isSystemMessage, previewMessage } from "../../utils/messages.js";

/** DOM id of the outline panel, which its toggle controls. */
export const SESSION_TOC_ID = "session-toc";

/** One prompt in the session outline. */
export interface TocEntry {
  ordinal: number;
  /** First line of the prompt, whitespace collapsed; empty for an empty prompt. */
  label: string;
  /** The prompt was a shell shortcut, so the label reads as code. */
  isShell: boolean;
}

/** The part of the transcript on screen, in message ordinals. */
export interface TocView {
  /** Ordinal of the row at the top of the viewport. */
  topOrdinal: number;
  minOrdinal: number;
  maxOrdinal: number;
  /** The oldest-first transcript is scrolled to its last row. */
  atEnd: boolean;
}

// Long enough to fill the panel's widest line; CSS adds the ellipsis.
const LABEL_MAX_CHARS = 160;

/**
 * Reports whether a message is a prompt someone typed: a user row the
 * transcript renders as a user card. Injected context, boundary cards, and
 * tool output that providers store on the user role are not prompts.
 */
export function isPromptMessage(m: Message): boolean {
  if (m.role !== "user" || m.is_compact_boundary || m.is_system) return false;
  if (m.source_subtype === "tool_result") return false;
  return !isSystemMessage(m) && !isSystemBoundaryMessage(m);
}

export function tocEntry(m: Message): TocEntry {
  const preview = previewMessage(m.content);
  const firstLine = preview.text.trim().split("\n", 1)[0] ?? "";
  const label = firstLine.replace(/\s+/g, " ").trim();
  return {
    ordinal: m.ordinal,
    label: label.length > LABEL_MAX_CHARS ? label.slice(0, LABEL_MAX_CHARS) : label,
    isShell: preview.isShell,
  };
}

/** Prompt entries for messages, oldest first. */
export function promptEntries(messages: readonly Message[]): TocEntry[] {
  return messages
    .filter(isPromptMessage)
    .map(tocEntry)
    .sort((a, b) => a.ordinal - b.ordinal);
}

/**
 * Joins prompts fetched from before the loaded window with the prompts in
 * it. The loaded window is current, so it wins wherever the two overlap.
 */
export function mergeTocEntries(older: readonly TocEntry[], loaded: readonly Message[]): TocEntry[] {
  const loadedEntries = promptEntries(loaded);
  const oldestLoaded = loaded.reduce(
    (min, m) => Math.min(min, m.ordinal),
    Number.POSITIVE_INFINITY,
  );
  return [...older.filter((e) => e.ordinal < oldestLoaded), ...loadedEntries];
}

/**
 * Index in `entries` (oldest first) of the prompt whose turn the reader is
 * in: the last prompt at or before the top row. At the end of the
 * transcript it is the last prompt on screen, which the top row may never
 * reach. A prompt the reader just picked stays current while it is on
 * screen. Returns -1 when no prompt starts at or before that point.
 */
export function currentEntryIndex(
  entries: readonly TocEntry[],
  view: TocView | null,
  picked: number | null,
): number {
  if (!view || entries.length === 0) return -1;
  if (picked !== null && picked >= view.minOrdinal && picked <= view.maxOrdinal) {
    const index = entries.findIndex((e) => e.ordinal === picked);
    if (index >= 0) return index;
  }
  const anchor = view.atEnd ? view.maxOrdinal : view.topOrdinal;
  let low = 0;
  let high = entries.length - 1;
  let found = -1;
  while (low <= high) {
    const mid = (low + high) >> 1;
    if (entries[mid]!.ordinal <= anchor) {
      found = mid;
      low = mid + 1;
    } else {
      high = mid - 1;
    }
  }
  return found;
}
