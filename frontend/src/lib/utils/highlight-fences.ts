import { highlightToHtml } from "./syntax-highlight.js";
import { applyMarks } from "./highlight.js";

export interface HighlightCodeFencesParams {
  /** Markdown source rendered via {@html}; passing it makes Svelte call
   * update() when {@html} replaces the DOM children. */
  q?: string;
  content: string;
  current?: boolean;
}

/**
 * Svelte action that applies Shiki highlighting to labeled fenced code
 * blocks in rendered markdown, after DOMPurify sanitization. Unlabeled or
 * unsupported fences are left as plain escaped text. Re-applies search
 * <mark> nodes after each swap since innerHTML replacement wipes them.
 */
export function highlightCodeFences(
  node: HTMLElement,
  params: HighlightCodeFencesParams,
) {
  // Per-element cancel functions that mark in-flight highlights as stale.
  const cancels = new Map<HTMLElement, () => void>();

  function cancelAll() {
    for (const cancel of cancels.values()) cancel();
    cancels.clear();
  }

  function highlightNode(
    codeEl: HTMLElement,
    lang: string,
    q: string,
    isCurrent: boolean,
  ) {
    cancels.get(codeEl)?.();

    let stale = false;
    cancels.set(codeEl, () => { stale = true; });

    // Read plain text BEFORE any innerHTML swap so we capture the
    // DOMPurify-sanitized text content, not previous Shiki spans.
    const code = codeEl.textContent ?? "";

    highlightToHtml(code, lang).then((html) => {
      if (stale) return;
      cancels.delete(codeEl);
      if (html === null) return;

      codeEl.innerHTML = html;

      // Re-apply search marks to this code element after the innerHTML swap
      // wiped any <mark> nodes that applyHighlight had placed inside it.
      if (q.trim()) applyMarks(codeEl, q, isCurrent);
    }).catch(() => {
      // Any error: leave the plain escaped text as-is.
      cancels.delete(codeEl);
    });
  }

  function run(p: HighlightCodeFencesParams) {
    cancelAll();

    const q = p.q ?? "";
    const isCurrent = p.current ?? false;

    node
      .querySelectorAll<HTMLElement>("pre > code[class*='language-']")
      .forEach((codeEl) => {
        const cls = codeEl.className;
        const match = /\blanguage-(\S+)/.exec(cls);
        const lang = match?.[1] ?? "";
        if (!lang) return;
        highlightNode(codeEl, lang, q, isCurrent);
      });
  }

  run(params);

  return {
    update(p: HighlightCodeFencesParams) {
      run(p);
    },
    destroy() {
      cancelAll();
    },
  };
}
