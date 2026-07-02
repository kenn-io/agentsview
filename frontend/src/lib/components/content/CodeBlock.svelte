<script lang="ts">
  import { onDestroy, tick, untrack } from "svelte";
  import { copyToClipboard } from "../../utils/clipboard.js";
  import { applyHighlight, applyMarks, clearMarks, escapeHTML } from "../../utils/highlight.js";
  import { highlightToHtml } from "../../utils/syntax-highlight.js";
  import { CopyButton } from "@kenn-io/kit-ui";
  import { m } from "../../i18n/index.js";

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

  // Copy stays controlled (rather than kit-ui CopyButton's self-managed
  // text mode) so it goes through the app's clipboard util.
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
    revealOnHover
    {copied}
    ariaLabel={m.code_block_copy_code_block()}
    copiedAriaLabel={m.code_block_copied_code_block()}
    title={m.code_block_copy_code()}
    copiedTitle={m.code_block_copied()}
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

  :global(.code-copy.kit-copy-btn) {
    position: absolute;
    top: 6px;
    right: 6px;
    z-index: 1;
  }

  .code-block:hover :global(.code-copy.kit-copy-btn) {
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
