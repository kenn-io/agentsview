<script lang="ts">
  import { onDestroy, tick, untrack } from "svelte";
  import { copyToClipboard } from "../../utils/clipboard.js";
  import { applyHighlight, applyMarks, clearMarks, escapeHTML } from "../../utils/highlight.js";
  import { highlightToHtml } from "../../utils/syntax-highlight.js";
  import CopyButton from "../shared/CopyButton.svelte";

  interface Props {
    content: string;
    language?: string;
    highlightQuery?: string;
    isCurrentHighlight?: boolean;
  }

  let { content, language, highlightQuery = "", isCurrentHighlight = false }: Props = $props();
  let copied = $state(false);
  let copyTimer: ReturnType<typeof setTimeout> | undefined;

  let highlighted = $state<string | null>(null);
  let preEl = $state<HTMLElement | undefined>(undefined);

  $effect(() => {
    highlighted = null;
    if (!language) return;

    const effectContent = content;
    const effectLang = language;
    let cancelled = false;

    highlightToHtml(effectContent, effectLang).then(async (html) => {
      if (cancelled) return;
      highlighted = html;
      // Flush the {@html} swap to the DOM before re-applying marks.
      await tick();
      if (cancelled) return;
      // Read current prop values after the await — intentionally untracked
      // because we are inside an async continuation, not during the sync
      // reactive evaluation.
      const q = untrack(() => highlightQuery);
      const current = untrack(() => isCurrentHighlight);
      const el = untrack(() => preEl);
      if (el && q.trim()) {
        clearMarks(el);
        applyMarks(el, q, current);
      }
    });

    return () => {
      cancelled = true;
    };
  });

  async function handleCopy() {
    const ok = await copyToClipboard(content);
    if (!ok) return;

    clearTimeout(copyTimer);
    copied = true;
    copyTimer = setTimeout(() => {
      copied = false;
    }, 1500);
  }

  onDestroy(() => {
    clearTimeout(copyTimer);
  });
</script>

<div class="code-block">
  <CopyButton
    class="code-copy"
    {copied}
    ariaLabel="Copy code block"
    copiedAriaLabel="Copied code block"
    title="Copy code"
    copiedTitle="Copied!"
    onclick={handleCopy}
  />
  {#if language}
    <div class="code-lang">{language}</div>
  {/if}
  <pre
    class="code-content"
    bind:this={preEl}
    use:applyHighlight={{ q: highlightQuery, current: isCurrentHighlight, content }}
  ><code>{@html highlighted ?? escapeHTML(content)}</code></pre>
</div>

<style>
  .code-block {
    position: relative;
    background: var(--code-bg);
    border-radius: var(--radius-md);
    margin: 4px 0;
    overflow: hidden;
  }

  :global(.code-copy.copy-btn) {
    position: absolute;
    top: 6px;
    right: 6px;
    z-index: 1;
  }

  .code-block:hover :global(.code-copy.copy-btn) {
    opacity: 1;
  }

  .code-lang {
    padding: 4px 12px;
    font-family: var(--font-mono);
    font-size: 11px;
    font-weight: 500;
    color: var(--code-text);
    opacity: 0.5;
    border-bottom: 1px solid rgba(255, 255, 255, 0.06);
  }

  .code-content {
    padding: 12px 16px;
    font-family: var(--font-mono);
    font-size: 13px;
    line-height: 1.55;
    color: var(--code-text);
    overflow-x: auto;
  }

  .code-content code {
    font-family: inherit;
  }

  @media (max-width: 767px) {
    .code-content {
      max-width: calc(100vw - 32px);
    }
  }
</style>
