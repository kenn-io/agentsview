/** Occurrence-level search over message data, independent of mounted DOM. */
import type { MarkdownRenderOptions } from "../utils/markdown.js";
import type { Message } from "../api/types.js";
import { collectSearchBlocks, type SearchBlock } from "./block-text.js";
import { createOccurrenceMatcher, prepareSearchText, type PreparedSearchText } from "./dom-text.js";

// Weak ownership releases folded text when a message version is discarded.
// Retain the source too so accidental in-place block updates cannot use stale text.
const preparedBlocks = new WeakMap<SearchBlock, { source: string; text: PreparedSearchText }>();

function preparedText(block: SearchBlock): PreparedSearchText {
  const cached = preparedBlocks.get(block);
  if (cached?.source === block.text) return cached.text;
  const text = prepareSearchText(block.text);
  preparedBlocks.set(block, { source: block.text, text });
  return text;
}

export interface Match {
  ordinal: number;
  blockKey: string;
  occurrence: number;
  start: number;
  end: number;
}

export interface SessionIndex {
  matches: Match[];
  byBlock: Map<string, number>;
  byOrdinal: Map<number, number>;
  total: number;
}

/**
 * Return chronological, non-overlapping matches using the shared text matcher.
 * Offsets are UTF-16 offsets in the block's rendered text. The input array and
 * its messages are never mutated; transcript visibility enters only through the
 * optional predicate, so callers pass the messages and blocks the current
 * filters actually render.
 */
export function buildSessionIndex(
  messages: readonly Message[],
  query: string,
  allowsBlock?: (message: Message, block: SearchBlock) => boolean,
  renderOptions: MarkdownRenderOptions = {},
  wholeWord = false,
): SessionIndex {
  const index: SessionIndex = {
    matches: [],
    byBlock: new Map(),
    byOrdinal: new Map(),
    total: 0,
  };
  if (!query.trim()) return index;
  const matchText = createOccurrenceMatcher(query, wholeWord);

  // The message store is normally ordered already. Keep that path linear.
  const ordered = messages.every(
    (message, i) => i === 0 || messages[i - 1]!.ordinal <= message.ordinal,
  )
    ? messages
    : [...messages].sort((a, b) => a.ordinal - b.ordinal);

  for (const message of ordered) {
    for (const block of collectSearchBlocks(message, renderOptions)) {
      if (allowsBlock && !allowsBlock(message, block)) continue;
      const occurrences = matchText(preparedText(block));
      if (!occurrences.length) continue;
      index.byBlock.set(block.key, occurrences.length);
      index.byOrdinal.set(
        block.ordinal,
        (index.byOrdinal.get(block.ordinal) ?? 0) + occurrences.length,
      );
      occurrences.forEach(({ start, end }, occurrence) => {
        index.matches.push({
          ordinal: block.ordinal,
          blockKey: block.key,
          occurrence,
          start,
          end,
        });
      });
    }
  }
  index.total = index.matches.length;
  return index;
}
