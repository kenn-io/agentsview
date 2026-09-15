<script lang="ts">
  import { onDestroy } from "svelte";
  import { copyToClipboard } from "../../utils/clipboard.js";
  import { searchBlock } from "../../search/session-block.svelte.js";
  import { highlightToHtml } from "../../utils/syntax-highlight.js";
  import { CopyButton } from "@kenn-io/kit-ui";
  import { m } from "../../i18n/index.js";

  interface Props {
    content: string;
    language?: string;
    searchKey?: string;
  }

  let { content, language, searchKey }: Props = $props();
  let copied = $state(false);
  let copyTimer: ReturnType<typeof setTimeout> | undefined;
  let highlighted = $state<string | null>(null);

  $effect(() => {
    const source = content;
    const lang = language;
    highlighted = null;
    if (!lang) return;
    let cancelled = false;
    void highlightToHtml(source, lang).then((html) => {
      if (!cancelled) highlighted = html;
    }).catch(() => {
      // Keep the original text if the optional syntax highlighter fails.
    });
    return () => { cancelled = true; };
  });

  // Keep copy controlled through the application's clipboard utility.
  async function handleCopy() {
    const ok = await copyToClipboard(content);
    if (!ok) return;
    clearTimeout(copyTimer);
    copied = true;
    copyTimer = setTimeout(() => { copied = false; }, 1500);
  }

  onDestroy(() => { clearTimeout(copyTimer); });
</script>

<!-- kit-ui-check-ignore: controlled clipboard behavior and a search attachment on pre require the app-owned code block. -->
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
  <pre class="code-content" {@attach searchBlock(searchKey)}><code>{#if highlighted !== null}{@html highlighted}{:else}{content}{/if}</code></pre>
</div>

<style>
  /* kit-ui-check-ignore: app-owned code block, see markup note above */
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
  /* kit-ui-check-ignore: app-owned code block, see markup note above */
  .code-block:hover :global(.code-copy.kit-copy-btn) { opacity: 1; }
  .code-lang {
    padding: 4px 12px;
    font-family: var(--font-mono);
    font-size: 11px;
    font-weight: 500;
    color: var(--code-text);
    opacity: 0.5;
    border-bottom: 1px solid color-mix(in srgb, var(--code-text) 8%, transparent);
  }
  .code-content {
    padding: 12px 16px;
    font-family: var(--font-mono);
    font-size: 13px;
    line-height: 1.55;
    color: var(--code-text);
    overflow-x: auto;
  }
  .code-content code { font-family: inherit; }
  @media (max-width: 760px) {
    .code-content { max-width: calc(100vw - 32px); }
  }
</style>
