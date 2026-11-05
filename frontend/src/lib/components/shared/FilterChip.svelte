<!-- Dresses a single-choice kit-ui Typeahead as an analytics filter chip so
     it matches the multi-select FilterDropdown chips ("Project: All"): a
     funnel before the label, the same 26px height, radius, border, and muted
     medium-weight text, and no chevron. Callers pass the dimension through
     the Typeahead's `triggerPrefix` ("Agent:") and give the unfiltered
     option a short `displayLabel` ("All"). Only appearance changes. -->
<script lang="ts">
  import type { Snippet } from "svelte";
  import FunnelIcon from "@lucide/svelte/icons/funnel";

  interface Props {
    /** Mirrors FilterDropdown's active state when a filter narrows results. */
    active?: boolean;
    children: Snippet;
  }

  let { active = false, children }: Props = $props();
</script>

<div class="filter-chip" class:active>
  <FunnelIcon
    class="filter-chip__icon"
    size={12}
    strokeWidth={2}
    aria-hidden="true"
  />
  {@render children()}
</div>

<style>
  .filter-chip {
    position: relative;
    display: inline-flex;
    min-width: 0;
    --typeahead-control-height: var(--filter-chip-height);
    --typeahead-control-padding: 0 10px 0 26px;
    --typeahead-min-width: 0px;
    --typeahead-max-width: var(--filter-chip-max-width, 240px);
    /* The chip is narrower than its option labels; give the open list
       its own width instead of the chip's. */
    --typeahead-panel-min-width: 240px;
  }

  .filter-chip :global(.filter-chip__icon) {
    position: absolute;
    left: 11px;
    top: 50%;
    z-index: 1;
    transform: translateY(-50%);
    color: var(--text-muted);
    pointer-events: none;
  }

  .filter-chip :global(.kit-typeahead__trigger) {
    font-weight: var(--font-weight-medium, 500);
    color: var(--text-muted);
    border-radius: var(--kit-control-radius, var(--radius-sm));
  }

  .filter-chip :global(.kit-typeahead__trigger:hover:not(:disabled)) {
    color: var(--text-secondary);
  }

  .filter-chip :global(.kit-typeahead__prefix) {
    color: inherit;
  }

  .filter-chip :global(.kit-typeahead__chevron) {
    display: none;
  }

  .filter-chip.active :global(.kit-typeahead__trigger),
  .filter-chip.active :global(.filter-chip__icon) {
    color: var(--accent-blue);
    border-color: var(--accent-blue);
  }
</style>
