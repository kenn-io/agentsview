<script lang="ts">
  import { applyHighlight, escapeHTML } from "../../utils/highlight.js";

  interface Props {
    content: string;
    language?: string;
    highlightQuery?: string;
    isCurrentHighlight?: boolean;
  }

  let { content, language, highlightQuery = "", isCurrentHighlight = false }: Props = $props();
</script>

<div class="code-block">
  {#if language}
    <div class="code-lang">{language}</div>
  {/if}
  <pre
    class="code-content"
    use:applyHighlight={{ q: highlightQuery, current: isCurrentHighlight, content }}
  ><code>{@html escapeHTML(content)}</code></pre>
</div>

<style>
  .code-block {
    background: var(--code-bg);
    border-radius: var(--radius-md);
    margin: 4px 0;
    overflow: hidden;
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
</style>
