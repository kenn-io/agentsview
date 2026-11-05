<script lang="ts">
  import type { Snippet } from "svelte";
  import type { DisplayItem } from "../../utils/display-items.js";
  import { inSessionSearch } from "../../stores/inSessionSearch.svelte.js";
  import FindResultsList from "./FindResultsList.svelte";
  import FindOverviewRail from "./FindOverviewRail.svelte";
  import SessionFindBar from "./SessionFindBar.svelte";

  interface Props {
    children: Snippet;
    items: DisplayItem[];
    totalSize: number;
    newestFirst: boolean;
    rowOffset: (index: number) => number;
  }
  let { children, items, totalSize, newestFirst, rowOffset }: Props = $props();
</script>

<SessionFindBar />
{#if inSessionSearch.isOpen && inSessionSearch.resultsOpen}
  <FindResultsList />
{/if}
<div class="find-viewport" class:has-find={inSessionSearch.isActive}>
  {@render children()}
  {#if inSessionSearch.isActive}
    <FindOverviewRail {items} {totalSize} {newestFirst} {rowOffset} />
  {/if}
</div>

<style>
  .find-viewport { position: relative; display: flex; flex-direction: column; flex: 1; min-height: 0; }
  /* The overview rail needs 12px plus a 10px gap past the reading column.
     Wide panes already have that in the side gutter, so the column stays
     centered on the same edges as the session header; only a pane within
     44px of the column reserves up to 12px and shifts it. */
  .has-find {
    --find-rail: clamp(0px, calc(var(--reading-column) + 44px - 100%), 12px);
  }
  .has-find :global(.message-list-scroll) { padding-inline-end: var(--find-rail); }
</style>
