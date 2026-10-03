<script lang="ts">
  import { inSessionSearch } from "../../stores/inSessionSearch.svelte.js";
  import { ui } from "../../stores/ui.svelte.js";
  import { searchBlock } from "../../search/session-block.svelte.js";
  import { highlightCodeFences } from "../../utils/highlight-fences.js";
  import { loadAssetImages, renderMarkdown } from "../../utils/markdown.js";
  import { displayToolResult, hasAssetImageMarkdown } from "../../utils/toolDisplay.js";
  import { m } from "../../i18n/index.js";

  interface Props {
    content: string;
    searchKey?: string;
  }

  let { content, searchKey }: Props = $props();
  let outputContent = $derived(displayToolResult(content));
  let hasAssetImage = $derived(hasAssetImageMarkdown(outputContent));
  let rawForSearch = $derived(searchKey !== undefined && inSessionSearch.isActive);
</script>

{#if hasAssetImage && !rawForSearch}
  <div
    class="tool-content output-content formatted-output"
    {@attach searchBlock(searchKey)}
    use:highlightCodeFences={{ content: outputContent }}
    use:loadAssetImages={outputContent}
  >
    {@html renderMarkdown(outputContent, {
      renderUnknownXmlBlocksAsPreformatted: ui.renderUnknownXmlBlocksAsPreformatted,
    })}
  </div>
{:else}
  {#if hasAssetImage && rawForSearch}
    <span class="search-output-hint">{m.session_find_raw_output()}</span>
  {/if}
  <pre class="tool-content output-content" {@attach searchBlock(searchKey)}>{outputContent}</pre>
{/if}

<style>
  .search-output-hint { color: var(--text-muted); font-size: 10px; margin-block-end: 0.25rem; }
  .formatted-output { overflow-wrap: anywhere; }
  .formatted-output :global(pre) { white-space: pre-wrap; }
  .formatted-output :global(img) { max-width: 100%; height: auto; }
  .formatted-output :global(p) { margin: 0.5em 0; overflow-wrap: anywhere; }
  .formatted-output :global(p:first-child) { margin-top: 0; }
  .formatted-output :global(p:last-child) { margin-bottom: 0; }
</style>
