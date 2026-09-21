<script lang="ts">
  import { searchBlock } from "../../search/session-block.svelte.js";
  import { searchCollapsed } from "../../search/component-state.js";
  import { inSessionSearch } from "../../stores/inSessionSearch.svelte.js";
  import SearchMatchCount from "./SearchMatchCount.svelte";
  import { ChevronRightIcon } from "../../icons.js";
  import { m } from "../../i18n/index.js";

  interface Props {
    content: string;
    searchKey?: string;
  }

  let { content, searchKey }: Props = $props();
  let userCollapsed = $state(true);
  let overrideSeq = $state(-1);
  let collapsed = $derived(searchCollapsed(
    userCollapsed, inSessionSearch.isCurrentBlock(searchKey),
    inSessionSearch.navigationRevision, overrideSeq,
  ));
</script>

<div class="thinking-block">
  <button
    class="thinking-header"
    aria-expanded={!collapsed}
    onclick={() => {
      userCollapsed = !collapsed;
      overrideSeq = inSessionSearch.navigationRevision;
    }}
  >
    <span class="thinking-chevron" class:open={!collapsed}>
      <ChevronRightIcon size="10" strokeWidth="2.4" aria-hidden="true" />
    </span>
    <span class="thinking-label">{m.thinking_block_label()}</span>
    <SearchMatchCount {searchKey} />
  </button>
  {#if !collapsed}
    <div
      class="thinking-content"
      {@attach searchBlock(searchKey)}
    >{content}</div>
  {/if}
</div>

<style>
  .thinking-block {
    border-left: 2px solid var(--accent-purple);
    background: var(--thinking-bg);
    border-radius: 0 var(--radius-sm) var(--radius-sm) 0;
    margin: 0;
  }

  .thinking-header {
    display: flex;
    align-items: center;
    gap: 6px;
    padding: 6px 10px;
    width: 100%;
    text-align: left;
    font-size: 12px;
    font-weight: 600;
    color: var(--accent-purple);
    letter-spacing: 0.01em;
    border-radius: 0 var(--radius-sm) var(--radius-sm) 0;
    transition: background 0.1s;
  }

  .thinking-header:hover {
    background: var(--bg-surface-hover);
  }

  .thinking-chevron {
    display: inline-flex;
    align-items: center;
    transition: transform 0.15s;
    color: var(--text-muted);
  }

  .thinking-chevron.open {
    transform: rotate(90deg);
  }

  .thinking-content {
    padding: 8px 14px 12px;
    font-size: 13px;
    font-style: italic;
    color: var(--text-secondary);
    white-space: pre-wrap;
    word-wrap: break-word;
    line-height: 1.65;
    border-top: 1px solid var(--border-muted);
  }
</style>
