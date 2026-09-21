/** A half-open character range in the original UTF-16 string. */
export interface TextOccurrence {
  start: number;
  end: number;
}

const SHOW_ELEMENT = 0x1;
const SHOW_TEXT = 0x4;

function appendNodeText(node: Node, parts: string[]): void {
  if (node.nodeType === 3) {
    parts.push(node.nodeValue ?? "");
    return;
  }
  if (node.nodeType === 1 && (node as Element).tagName.toLowerCase() === "br") {
    parts.push("\n");
  }
}

/**
 * Return the searchable text represented by a rendered DOM subtree.
 *
 * Text nodes are concatenated in document order and line break elements are
 * represented by a newline. Element boundaries add no implicit separator.
 */
export function domText(root: Node): string {
  const document = root.nodeType === 9 ? (root as Document) : root.ownerDocument;
  if (!document) return root.textContent ?? "";

  const parts: string[] = [];
  appendNodeText(root, parts);

  const walker = document.createTreeWalker(root, SHOW_ELEMENT | SHOW_TEXT);
  let node = walker.nextNode();
  while (node) {
    appendNodeText(node, parts);
    node = walker.nextNode();
  }
  return parts.join("");
}

function isCodePointBoundary(value: string, index: number): boolean {
  if (index <= 0 || index >= value.length) return true;
  const previous = value.charCodeAt(index - 1);
  const current = value.charCodeAt(index);
  return !(previous >= 0xd800 && previous <= 0xdbff && current >= 0xdc00 && current <= 0xdfff);
}

const WORD_END = /[\p{L}\p{M}\p{N}\p{Pc}]$/u;
const WORD_START = /^[\p{L}\p{M}\p{N}\p{Pc}]/u;

function findFoldedOffsets(text: string, query: string, wholeWord: boolean): TextOccurrence[] {
  const occurrences: TextOccurrence[] = [];
  let cursor = 0;
  while (cursor <= text.length - query.length) {
    const start = text.indexOf(query, cursor);
    if (start < 0) break;
    const end = start + query.length;
    if (
      isCodePointBoundary(text, start) &&
      isCodePointBoundary(text, end) &&
      (!wholeWord ||
        (!WORD_END.test(text.slice(Math.max(0, start - 2), start)) &&
          !WORD_START.test(text.slice(end, end + 2))))
    ) {
      occurrences.push({ start, end });
      cursor = end;
    } else {
      cursor = start + 1;
    }
  }
  return occurrences;
}

/** Prepared text is immutable and can be retained with a stable content block. */
export interface PreparedSearchText {
  readonly value: string;
  /** Packed expansion boundaries: folded start/end, then original start/end. */
  readonly expansions?: Uint32Array;
}

/** Fold ASCII runs natively and non-ASCII code points independently. */
export function prepareSearchText(text: string): PreparedSearchText {
  if (/^[\x00-\x7f]*$/.test(text)) return { value: text.toLowerCase() };
  const parts: string[] = [];
  const expansions: number[] = [];
  let previous = 0;
  let delta = 0;
  for (const match of text.matchAll(/[^\x00-\x7f]/gu)) {
    const start = match.index;
    if (start > previous) parts.push(text.slice(previous, start).toLowerCase());
    const point = match[0];
    const lower = point.toLowerCase();
    parts.push(lower);
    previous = start + point.length;
    if (lower.length !== point.length) {
      expansions.push(start + delta, start + delta + lower.length, start, previous);
      delta += lower.length - point.length;
    }
  }
  if (previous < text.length) parts.push(text.slice(previous).toLowerCase());
  return {
    value: parts.join(""),
    // A single expanding character must not allocate maps for the entire block.
    expansions: expansions.length ? new Uint32Array(expansions) : undefined,
  };
}

function originalPosition(position: number, end: boolean, expansions: Uint32Array): number {
  let lo = 0;
  let hi = expansions.length / 4;
  while (lo < hi) {
    const mid = (lo + hi) >>> 1;
    if (expansions[mid * 4]! <= position) lo = mid + 1;
    else hi = mid;
  }
  if (lo === 0) return position;
  const offset = (lo - 1) * 4;
  const foldedStart = expansions[offset]!;
  const foldedEnd = expansions[offset + 1]!;
  const sourceStart = expansions[offset + 2]!;
  const sourceEnd = expansions[offset + 3]!;
  if (position === foldedStart) return sourceStart;
  if (position < foldedEnd || (end && position === foldedEnd)) return end ? sourceEnd : sourceStart;
  return position - (foldedEnd - sourceEnd);
}

/** Compile the query once for a scan across many independently cached blocks. */
export function createOccurrenceMatcher(
  query: string,
  wholeWord = false,
): (text: PreparedSearchText) => TextOccurrence[] {
  const foldedQuery = query.trim() ? prepareSearchText(query).value : "";
  return (text) => {
    if (!foldedQuery) return [];
    const matches = findFoldedOffsets(text.value, foldedQuery, wholeWord);
    if (!text.expansions) return matches;
    const occurrences: TextOccurrence[] = [];
    let previousEnd = -1;
    for (const match of matches) {
      const start = originalPosition(match.start, false, text.expansions);
      const end = originalPosition(match.end, true, text.expansions);
      if (start < previousEnd) continue;
      occurrences.push({ start, end });
      previousEnd = end;
    }
    return occurrences;
  };
}

/**
 * Find non-overlapping, case-insensitive occurrences from left to right.
 *
 * Matching lowercases each Unicode code point. Returned offsets always refer
 * to the original UTF-16 string and never divide a surrogate pair.
 */
export function findOccurrences(text: string, query: string, wholeWord = false): TextOccurrence[] {
  if (!query.trim()) return [];
  return createOccurrenceMatcher(query, wholeWord)(prepareSearchText(text));
}
