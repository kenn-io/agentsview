<script lang="ts">
  import { applyHighlight, escapeHTML } from "../../utils/highlight.js";

  interface Props {
    content: string;
    highlightQuery?: string;
    isCurrentHighlight?: boolean;
  }

  let { content, highlightQuery = "", isCurrentHighlight = false }: Props = $props();
  let userCollapsed: boolean = $state(true);
  let userOverride: boolean = $state(false);
  let searchExpanded: boolean = $state(false);
  let prevQuery: string = "";

  // Auto-expand when a search match exists in this block.
  // Only reset the user override when the query itself changes,
  // not when content updates (e.g. during streaming).
  $effect(() => {
    const q = highlightQuery;
    const hasMatch =
      q.trim() !== "" &&
      content.toLowerCase().includes(q.toLowerCase());
    searchExpanded = hasMatch;
    if (q !== prevQuery) {
      userOverride = false;
      prevQuery = q;
    }
  });

  let collapsed = $derived(
    userOverride ? userCollapsed
      : searchExpanded ? false
      : userCollapsed,
  );
</script>

<div class="thinking-block">
  <button
    class="thinking-header"
    onclick={() => { userCollapsed = !userCollapsed; userOverride = true; }}
  >
    <span class="thinking-chevron" class:open={!collapsed}>
      &#9656;
    </span>
    <span class="thinking-label">Thinking</span>
  </button>
  {#if !collapsed}
    <div
      class="thinking-content"
      use:applyHighlight={{ q: highlightQuery, current: isCurrentHighlight, content }}
    >{@html escapeHTML(content)}</div>
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
    display: inline-block;
    font-size: 10px;
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
