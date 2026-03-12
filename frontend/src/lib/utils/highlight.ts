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
 * Svelte action that wraps all occurrences of a query string within
 * the text nodes of an element in <mark class="search-highlight"> tags.
 * Pass `content` as a param so the action re-runs when content changes.
 */
export function applyHighlight(
  node: HTMLElement,
  params: { q: string; current: boolean; content: string },
) {
  function clearMarks(el: HTMLElement) {
    el.querySelectorAll("mark.search-highlight").forEach((m) => {
      const p = m.parentNode!;
      while (m.firstChild) p.insertBefore(m.firstChild, m);
      p.removeChild(m);
    });
    el.normalize();
  }

  function applyMarks(el: HTMLElement, q: string, isCurrent: boolean) {
    const lq = q.toLowerCase();
    const walker = document.createTreeWalker(el, NodeFilter.SHOW_TEXT);
    const nodes: Text[] = [];
    let n: Node | null;
    while ((n = walker.nextNode())) nodes.push(n as Text);

    for (const tn of nodes) {
      const txt = tn.textContent ?? "";
      const lower = txt.toLowerCase();
      if (!lower.includes(lq)) continue;
      const frag = document.createDocumentFragment();
      let last = 0;
      let i = lower.indexOf(lq);
      while (i !== -1) {
        if (i > last)
          frag.appendChild(document.createTextNode(txt.slice(last, i)));
        const mark = document.createElement("mark");
        mark.className =
          "search-highlight" +
          (isCurrent ? " search-highlight--current" : "");
        mark.textContent = txt.slice(i, i + q.length);
        frag.appendChild(mark);
        last = i + q.length;
        i = lower.indexOf(lq, last);
      }
      if (last < txt.length)
        frag.appendChild(document.createTextNode(txt.slice(last)));
      tn.parentNode!.replaceChild(frag, tn);
    }
  }

  function run(p: { q: string; current: boolean }) {
    clearMarks(node);
    if (p.q.trim()) applyMarks(node, p.q, p.current);
  }

  run(params);
  return { update: (p: { q: string; current: boolean; content: string }) => run(p) };
}
