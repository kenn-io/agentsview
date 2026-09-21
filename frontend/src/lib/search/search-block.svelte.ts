/** Attach native highlights to a mounted block without rewriting its DOM. */
import type { Attachment } from "svelte/attachments";
import { domText, findOccurrences, type TextOccurrence } from "./dom-text.js";
import { highlightRegistry } from "./highlight-registry.js";
import { createDetailsReveal } from "./details-reveal.js";
import "./search.css";

export interface SearchBlockState {
  query: string;
  wholeWord?: boolean;
  count: number;
  current: boolean;
  occurrence: number;
}

interface TextSpan {
  node: Node;
  start: number;
  end: number;
}

const currentRanges = new WeakMap<HTMLElement, Range>();

/** A live Range supplies geometry; StaticRange itself has no rectangle API. */
export function currentRangeForBlock(element: HTMLElement): Range | undefined {
  return currentRanges.get(element);
}

/** Map domText offsets through inline markup and synthetic BR newlines. */
export function mapOccurrences(
  root: HTMLElement,
  occurrences: readonly TextOccurrence[],
): StaticRangeInit[] {
  const spans: TextSpan[] = [];
  const walker = root.ownerDocument.createTreeWalker(
    root,
    NodeFilter.SHOW_ELEMENT | NodeFilter.SHOW_TEXT,
  );
  let node: Node | null = root;
  let offset = 0;
  while (node) {
    const length =
      node.nodeType === Node.TEXT_NODE
        ? (node.textContent?.length ?? 0)
        : node.nodeName === "BR"
          ? 1
          : 0;
    if (length) {
      spans.push({ node, start: offset, end: offset + length });
      offset += length;
    }
    node = walker.nextNode();
  }
  if (!spans.length) return [];

  function point(position: number, start: boolean): { node: Node; offset: number } {
    let lo = 0;
    let hi = spans.length;
    while (lo < hi) {
      const mid = (lo + hi) >>> 1;
      const end = spans[mid]!.end;
      if (end < position || (start && end === position)) lo = mid + 1;
      else hi = mid;
    }
    const span = spans[Math.min(lo, spans.length - 1)]!;
    if (span.node.nodeType === Node.TEXT_NODE) {
      return { node: span.node, offset: position - span.start };
    }
    const parent = span.node.parentNode!;
    let childIndex = 0;
    for (let sibling = span.node.previousSibling; sibling; sibling = sibling.previousSibling)
      childIndex++;
    return { node: parent, offset: childIndex + (position > span.start ? 1 : 0) };
  }

  return occurrences.map(({ start, end }) => {
    const first = point(start, true);
    const last = point(end, false);
    return {
      startContainer: first.node,
      startOffset: first.offset,
      endContainer: last.node,
      endOffset: last.offset,
    };
  });
}

function liveRange(document: Document, boundaries: StaticRangeInit): Range {
  const range = document.createRange();
  range.setStart(boundaries.startContainer, boundaries.startOffset);
  range.setEnd(boundaries.endContainer, boundaries.endOffset);
  return range;
}

/**
 * The state reader runs inside Svelte's attachment effect. Its reactive reads
 * cause teardown/repaint on query or cursor changes. DOM-only Shiki updates are
 * observed separately and coalesced to one animation frame.
 */
export function searchBlock(
  key: string | undefined,
  readState: () => SearchBlockState,
): Attachment<HTMLElement> {
  return (element) => {
    if (!key) return;
    const state = readState();
    const owner = {};
    const disclosures = createDetailsReveal(element);
    let frame: number | null = null;
    let disposed = false;
    element.dataset.searchBlock = key;
    if (state.current && state.query.trim()) element.dataset.searchCurrent = "true";
    else delete element.dataset.searchCurrent;

    function paint(): void {
      if (disposed) return;
      currentRanges.delete(element);
      if (!state.query.trim() || state.count === 0) {
        highlightRegistry.remove(owner);
        disclosures.update();
        return;
      }
      const occurrences = findOccurrences(domText(element), state.query, state.wholeWord);
      const boundaries = mapOccurrences(element, occurrences);
      const ranges = boundaries.map((boundary) =>
        typeof StaticRange === "function"
          ? new StaticRange(boundary)
          : liveRange(element.ownerDocument, boundary),
      );
      const selected = state.current ? ranges[state.occurrence] : undefined;
      highlightRegistry.set(owner, ranges, selected ? [selected] : []);
      const selectedBoundary = state.current ? boundaries[state.occurrence] : undefined;
      const selectedRange = selectedBoundary
        ? liveRange(element.ownerDocument, selectedBoundary)
        : undefined;
      if (selectedRange) currentRanges.set(element, selectedRange);
      disclosures.update(selectedRange);
      if (import.meta.env.DEV && occurrences.length !== state.count) {
        console.warn("Search block text differs from its index", key, {
          indexed: state.count,
          rendered: occurrences.length,
        });
      }
    }

    paint();
    const observer = new MutationObserver(() => {
      if (disposed || frame !== null) return;
      frame = requestAnimationFrame(() => {
        frame = null;
        paint();
      });
    });
    observer.observe(element, { childList: true, characterData: true, subtree: true });
    return () => {
      disposed = true;
      disclosures.destroy();
      observer.disconnect();
      if (frame !== null) cancelAnimationFrame(frame);
      highlightRegistry.remove(owner);
      currentRanges.delete(element);
      if (element.dataset.searchBlock === key) {
        delete element.dataset.searchBlock;
        delete element.dataset.searchCurrent;
      }
    };
  };
}
