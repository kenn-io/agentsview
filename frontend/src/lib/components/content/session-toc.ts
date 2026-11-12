import type { DbMessage as Message } from "../../api/generated/index.js";
import type { BlockType } from "../../stores/ui.svelte.js";
import { hasVisibleSegments } from "../../utils/content-parser.js";
import { isSystemBoundaryMessage, isSystemMessage, previewMessage } from "../../utils/messages.js";

/** Most rows the prompt picker renders at once. */
export const PICKER_LIMIT = 100;

/** One prompt in the session prompt picker. */
export interface TocEntry {
  ordinal: number;
  timestamp: string;
  /** First line of the prompt, whitespace collapsed; empty for an empty prompt. */
  label: string;
  /** The prompt's text, lowercased and whitespace collapsed, for search. */
  search: string;
  /** The prompt was a shell shortcut, so the label reads as code. */
  isShell: boolean;
  /** Block types the prompt's card renders; the block filter hides the
   *  card only when it hides all of them. */
  blocks: BlockType[];
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

// Long enough to fill the picker's widest row; CSS adds the ellipsis.
const LABEL_MAX_CHARS = 160;
// Search covers the start of each prompt. A prompt with a pasted log can
// run to megabytes, and thousands of them must stay cheap to filter on
// every keystroke.
const SEARCH_MAX_CHARS = 4000;

// Claude Code records a shell shortcut's output as its own user row that
// opens with its stdout block (then stderr); the row holding the typed
// command opens with <bash-input>. The opening tag alone decides, because
// the output itself can print any text, closing tags included.
const SHELL_OUTPUT_START_RE = /^\s*<bash-(?:stdout|stderr)>/;

/**
 * Reports whether a message is a prompt someone typed: a user row the
 * transcript renders as a user card. Injected context, boundary cards, and
 * tool output that providers store on the user role are not prompts.
 */
export function isPromptMessage(m: Message): boolean {
  if (m.role !== "user" || m.is_compact_boundary || m.is_system) return false;
  if (m.source_subtype === "tool_result") return false;
  if (SHELL_OUTPUT_START_RE.test(previewSource(m.content))) return false;
  return !isSystemMessage(m) && !isSystemBoundaryMessage(m);
}

// The content a preview starts from, without leading reminder envelopes.
function previewSource(content: string): string {
  return content.replace(/^(?:\s*<system-reminder(?:\s[^>]*)?>[\s\S]*?<\/system-reminder>)+/, "");
}

/** Block types a message's card renders, as the transcript's filter sees them. */
function renderedBlocks(m: Message): BlockType[] {
  const blocks = new Set<BlockType>();
  // A predicate that never matches makes the check visit every segment.
  hasVisibleSegments(m, (type) => {
    blocks.add(type);
    return false;
  });
  return [...blocks];
}

/**
 * Reports whether the transcript shows an entry's prompt under the block
 * filter. A filtered code fence still renders its placeholder, so code
 * keeps a prompt on screen, as it does in the transcript.
 */
export function entryVisible(entry: TocEntry, isVisible: (type: BlockType) => boolean): boolean {
  return entry.blocks.some((type) => type === "code" || isVisible(type));
}

export function tocEntry(m: Message): TocEntry {
  const preview = previewMessage(m.content);
  const firstLine = preview.text.trim().split("\n", 1)[0] ?? "";
  const label = firstLine.replace(/\s+/g, " ").trim();
  return {
    ordinal: m.ordinal,
    timestamp: m.timestamp,
    label: label.length > LABEL_MAX_CHARS ? label.slice(0, LABEL_MAX_CHARS) : label,
    search: preview.text.slice(0, SEARCH_MAX_CHARS).replace(/\s+/g, " ").toLowerCase(),
    isShell: preview.isShell,
    blocks: renderedBlocks(m),
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
export function mergeTocEntries(
  older: readonly TocEntry[],
  loaded: readonly Message[],
): TocEntry[] {
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

/** The prompts the picker lists for a query. */
export interface PromptPick {
  /** At most `limit` prompts, oldest first. */
  shown: TocEntry[];
  /** How many prompts match the query; all of them when it is empty. */
  matched: number;
  /** Index in the matches of the first shown prompt. */
  start: number;
}

/**
 * Chooses the prompts the picker lists. A query keeps prompts whose text
 * contains every word of it, oldest first, up to `limit`. With no query,
 * the list is a window of `limit` prompts around the current one, so the
 * picker opens where the reader is.
 */
export function pickPrompts(
  entries: readonly TocEntry[],
  query: string,
  currentIndex: number,
  limit: number = PICKER_LIMIT,
): PromptPick {
  const words = query.toLowerCase().split(/\s+/).filter(Boolean);
  if (words.length === 0) {
    const centered = currentIndex < 0 ? 0 : currentIndex - Math.floor(limit / 2);
    const start = Math.max(0, Math.min(centered, entries.length - limit));
    return { shown: entries.slice(start, start + limit), matched: entries.length, start };
  }
  const shown: TocEntry[] = [];
  let matched = 0;
  for (const entry of entries) {
    if (!words.every((word) => entry.search.includes(word))) continue;
    matched += 1;
    if (shown.length < limit) shown.push(entry);
  }
  return { shown, matched, start: 0 };
}
