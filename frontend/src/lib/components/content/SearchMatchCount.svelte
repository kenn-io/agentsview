<script lang="ts">
  import { inSessionSearch } from "../../stores/inSessionSearch.svelte.js";
  import { m } from "../../i18n/index.js";

  let { searchKey, count }: { searchKey?: string; count?: number } = $props();
  let matches = $derived(count ?? inSessionSearch.countForBlock(searchKey));
</script>

{#if matches > 0}
  <span class="search-match-count" title={m.session_find_block_count({ count: matches })}>
    {m.session_find_block_count({ count: matches })}
  </span>
{/if}

<style>
  .search-match-count {
    font-size: 10px;
    font-weight: 600;
    white-space: nowrap;
    flex-shrink: 0;
    color: var(--accent-blue);
    background: color-mix(in srgb, var(--accent-blue) 12%, transparent);
    border-radius: var(--radius-sm);
    padding: 1px 5px;
  }
</style>
