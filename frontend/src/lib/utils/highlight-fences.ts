import { highlightToHtml } from "./syntax-highlight.js";

export interface HighlightCodeFencesParams {
  /** Markdown source: changes cause Svelte to update the action after rendering. */
  content: string;
}

/** Apply optional syntax coloring without owning search state or rewriting text. */
export function highlightCodeFences(node: HTMLElement, _params: HighlightCodeFencesParams) {
  const cancels = new Map<HTMLElement, () => void>();
  function cancelAll() {
    for (const cancel of cancels.values()) cancel();
    cancels.clear();
  }
  function highlightNode(codeEl: HTMLElement, lang: string) {
    cancels.get(codeEl)?.();
    let stale = false;
    cancels.set(codeEl, () => {
      stale = true;
    });
    const code = codeEl.textContent ?? "";
    void highlightToHtml(code, lang)
      .then((html) => {
        if (stale) return;
        cancels.delete(codeEl);
        if (html !== null) codeEl.innerHTML = html;
        // The block attachment observes this DOM replacement independently.
      })
      .catch(() => {
        if (!stale) cancels.delete(codeEl);
      });
  }
  function run() {
    cancelAll();
    // HTML parsing splits large text into adjacent nodes. Merging them avoids
    // slow WebKit line wrapping for large tool-image fallback payloads.
    node.normalize();
    node.querySelectorAll<HTMLElement>("pre > code[class*='language-']").forEach((codeEl) => {
      const lang = /\blanguage-(\S+)/.exec(codeEl.className)?.[1];
      if (lang) highlightNode(codeEl, lang);
    });
  }
  run();
  return {
    update(_next: HighlightCodeFencesParams) {
      run();
    },
    destroy: cancelAll,
  };
}
