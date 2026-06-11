/**
 * Escapes a plain-text string for safe insertion via {@html}.
 * Use this on raw content inside elements that also use applyHighlight,
 * so Svelte replaces innerHTML on update instead of patching a retained
 * text node reference (which becomes detached when applyHighlight splits it).
 */
export function escapeHTML(s: string): string {
  return s
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;")
    .replace(/'/g, "&#39;");
}

/**
 * Remove all <mark class="search-highlight"> wrappers from `el`,
 * restoring the original text nodes.
 */
export function clearMarks(el: HTMLElement): void {
  el.querySelectorAll("mark.search-highlight").forEach((m) => {
    const p = m.parentNode!;
    while (m.firstChild) p.insertBefore(m.firstChild, m);
    p.removeChild(m);
  });
  el.normalize();
}

/**
 * Wrap every occurrence of `q` in `el`'s text nodes with
 * <mark class="search-highlight"> (and optionally the --current variant).
 *
 * Matches that span Shiki token boundaries (separate sibling text nodes) are
 * handled by concatenating all text-node content into a single string, finding
 * match ranges there, then mapping ranges back to individual text nodes. Each
 * text node that overlaps a match range gets its own <mark> fragment so the
 * marks visually abut across span boundaries without crossing element
 * boundaries.
 */
export function applyMarks(
  el: HTMLElement,
  q: string,
  isCurrent: boolean,
): void {
  if (!q) return;
  const lq = q.toLowerCase();

  // --- Phase 1: collect all text nodes BEFORE any DOM mutation ---
  interface Segment {
    node: Text;
    start: number; // offset in `full` where this node's text begins
    end: number;   // exclusive end offset
  }

  const walker = document.createTreeWalker(el, NodeFilter.SHOW_TEXT);
  const segments: Segment[] = [];
  let full = "";
  let n: Node | null;
  while ((n = walker.nextNode())) {
    const tn = n as Text;
    const txt = tn.textContent ?? "";
    if (txt.length === 0) continue;
    segments.push({ node: tn, start: full.length, end: full.length + txt.length });
    full += txt;
  }

  if (segments.length === 0) return;

  const lowerFull = full.toLowerCase();

  // --- Phase 2: find all non-overlapping match ranges in the concatenated string ---
  interface MatchRange {
    start: number;
    end: number; // exclusive
  }
  const matches: MatchRange[] = [];
  let pos = 0;
  while (pos <= lowerFull.length - lq.length) {
    const idx = lowerFull.indexOf(lq, pos);
    if (idx === -1) break;
    matches.push({ start: idx, end: idx + lq.length });
    pos = idx + lq.length;
  }

  if (matches.length === 0) return;

  // --- Phase 3: for each segment, collect the per-node pieces that overlap any match ---
  interface Piece {
    localStart: number;
    localEnd: number;
  }
  // Map from segment index to list of pieces within that node
  const segPieces: Map<number, Piece[]> = new Map();

  let segIdx = 0; // monotonic outer cursor over segments
  for (const match of matches) {
    // Skip segments that end at or before this match starts. Safe to advance
    // permanently: matches are ascending and non-overlapping, so a segment that
    // ends <= match.start cannot overlap this or any later match.
    while (segIdx < segments.length && segments[segIdx]!.end <= match.start) {
      segIdx++;
    }
    // Walk segments overlapping [match.start, match.end) using a LOCAL index so a
    // segment shared by a later match is not skipped by the outer cursor.
    for (let j = segIdx; j < segments.length && segments[j]!.start < match.end; j++) {
      const seg = segments[j]!;
      const overlapStart = Math.max(match.start, seg.start);
      const overlapEnd = Math.min(match.end, seg.end);
      if (overlapStart >= overlapEnd) continue;
      const localStart = overlapStart - seg.start;
      const localEnd = overlapEnd - seg.start;
      if (!segPieces.has(j)) segPieces.set(j, []);
      segPieces.get(j)!.push({ localStart, localEnd });
    }
  }

  // --- Phase 4: rebuild each affected text node into a DocumentFragment ---
  const markClass =
    "search-highlight" + (isCurrent ? " search-highlight--current" : "");

  for (const [si, pieces] of segPieces) {
    const seg = segments[si as number];
    if (!seg) continue; // defensive: si is always a valid index, but satisfies TS
    const txt = seg.node.textContent ?? "";
    const frag = document.createDocumentFragment();
    let cursor = 0;

    for (const piece of pieces) {
      if (piece.localStart > cursor)
        frag.appendChild(document.createTextNode(txt.slice(cursor, piece.localStart)));
      const mark = document.createElement("mark");
      mark.className = markClass;
      mark.textContent = txt.slice(piece.localStart, piece.localEnd);
      frag.appendChild(mark);
      cursor = piece.localEnd;
    }

    if (cursor < txt.length)
      frag.appendChild(document.createTextNode(txt.slice(cursor)));

    seg.node.parentNode!.replaceChild(frag, seg.node);
  }
}

/**
 * Svelte action that wraps all occurrences of a query string within
 * the text nodes of an element in <mark class="search-highlight"> tags.
 * Pass `content` as a param so the action re-runs when content changes.
 */
export function applyHighlight(
  node: HTMLElement,
  params: { q: string; current: boolean; content: string },
) {
  function run(p: { q: string; current: boolean }) {
    clearMarks(node);
    if (p.q.trim()) applyMarks(node, p.q, p.current);
  }

  run(params);
  return { update: (p: { q: string; current: boolean; content: string }) => run(p) };
}
