/** Navigation order shared by the session cursor and result presentation. */
import type { Match } from "./session-index.js";

export type SearchCursor = Pick<Match, "ordinal" | "blockKey" | "occurrence">;

export function sameCursor(a: SearchCursor, b: SearchCursor): boolean {
  return a.ordinal === b.ordinal && a.blockKey === b.blockKey && a.occurrence === b.occurrence;
}

export function cursorFor(match: SearchCursor): SearchCursor {
  return {
    ordinal: match.ordinal,
    blockKey: match.blockKey,
    occurrence: match.occurrence,
  };
}

/**
 * The index is chronological. Newest-first reverses message groups only:
 * blocks and occurrences inside each message still render from top to bottom.
 * Keep match identity intact so the cursor, result list, and overview agree.
 */
export function matchesInDisplayOrder(
  matches: readonly Match[],
  newestFirst: boolean,
): readonly Match[] {
  if (!newestFirst) return matches;
  const ordered: Match[] = [];
  for (let end = matches.length; end > 0;) {
    let start = end - 1;
    const ordinal = matches[start]!.ordinal;
    while (start > 0 && matches[start - 1]!.ordinal === ordinal) start--;
    for (let index = start; index < end; index++) ordered.push(matches[index]!);
    end = start;
  }
  return ordered;
}

/**
 * Resolve a stable tuple, falling forward from its message in display order and
 * then to the first remaining occurrence. A filter change can remove the cursor
 * while other matches stay; search must never report an empty current then.
 */
export function resolveSearchMatch(
  ordered: readonly Match[],
  cursor: SearchCursor | null,
  anchorOrdinal: number | null,
  newestFirst: boolean,
): Match | null {
  if (cursor) {
    const exact = ordered.find((match) => sameCursor(match, cursor));
    if (exact) return exact;
  }
  const anchor = cursor?.ordinal ?? anchorOrdinal;
  if (anchor === null) return ordered[0] ?? null;
  return (
    ordered.find((match) => (newestFirst ? match.ordinal <= anchor : match.ordinal >= anchor)) ??
    ordered[0] ??
    null
  );
}

/**
 * Navigate one occurrence, wrapping only on an explicit next/previous command.
 * A freshly committed query first reveals its anchor, avoiding a skipped first
 * hit when Enter is pressed before the debounce timer has fired.
 */
export function stepSearchMatch(
  ordered: readonly Match[],
  current: SearchCursor | null,
  delta: 1 | -1,
  freshQuery = false,
): Match | null {
  if (!ordered.length) return null;
  const index = current ? ordered.findIndex((match) => sameCursor(match, current)) : -1;
  if (freshQuery && index >= 0) return ordered[index]!;
  const next =
    index < 0
      ? delta > 0
        ? 0
        : ordered.length - 1
      : (index + delta + ordered.length) % ordered.length;
  return ordered[next]!;
}
