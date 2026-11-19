<!-- Dresses a single-choice kit-ui Typeahead as an analytics filter chip so
     it matches the multi-select FilterDropdown chips ("Project: All"): the
     same 26px height, radius, border, and muted medium-weight text, and a small
     muted chevron after the label that says the chip opens a list. Chips carry no funnel: the "Project:" prefix already says what
     each one filters, and a funnel on every chip in a row repeated the same
     mark without telling them apart. Callers pass the dimension through
     the Typeahead's `triggerPrefix` ("Agent:") and give the unfiltered
     option a short `displayLabel` ("All"). Only appearance changes. -->
<script lang="ts">
  import type { Snippet } from "svelte";

  interface Props {
    /** Mirrors FilterDropdown's active state when a filter narrows results. */
    active?: boolean;
    children: Snippet;
  }

  let { active = false, children }: Props = $props();
</script>

<div class="filter-chip" class:active>
  {@render children()}
</div>

<style>
  .filter-chip {
    position: relative;
    display: inline-flex;
    min-width: 0;
    --typeahead-control-height: var(--filter-chip-height);
    --typeahead-control-padding: 0 10px;
    --typeahead-min-width: 0px;
    --typeahead-max-width: var(--filter-chip-max-width, 240px);
    /* The chip is narrower than its option labels; give the open list
       its own width instead of the chip's. */
    --typeahead-panel-min-width: 240px;
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

  /* The same 12px muted chevron as the FilterDropdown chips, and it
     stays muted when the chip is active. */
  .filter-chip :global(.kit-typeahead__chevron) {
    color: var(--text-muted);
    opacity: 0.8;
  }

  .filter-chip.active :global(.kit-typeahead__trigger) {
    color: var(--accent-blue);
    border-color: var(--accent-blue);
  }
</style>
