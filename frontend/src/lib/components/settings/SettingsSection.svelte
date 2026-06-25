<script lang="ts">
  import type { Snippet } from "svelte";

  let {
    title,
    description = "",
    allowOverflow = false,
    children,
  }: {
    title: string;
    description?: string;
    /** Let absolutely-positioned children (e.g. a typeahead dropdown)
     * escape the section box instead of being clipped by its rounded
     * overflow. */
    allowOverflow?: boolean;
    children: Snippet;
  } = $props();
</script>

<section class="settings-section" class:allow-overflow={allowOverflow}>
  <div class="section-header">
    <h3 class="section-title">{title}</h3>
    {#if description}
      <p class="section-desc">{description}</p>
    {/if}
  </div>
  <div class="section-body">
    {@render children()}
  </div>
</section>

<style>
  .settings-section {
    border: 1px solid var(--border-default);
    border-radius: var(--radius-lg);
    background: var(--bg-surface);
    overflow: hidden;
  }

  /* Allow a child dropdown/typeahead popover to overflow the section
     box. The header keeps its own rounded clipping via border-radius. */
  .settings-section.allow-overflow {
    overflow: visible;
  }

  .section-header {
    padding: 14px 18px 10px;
    border-bottom: 1px solid var(--border-muted);
  }

  .section-title {
    font-size: 13px;
    font-weight: 600;
    color: var(--text-primary);
    margin: 0;
  }

  .section-desc {
    font-size: 11px;
    color: var(--text-muted);
    margin: 4px 0 0;
  }

  .section-body {
    padding: 14px 18px;
    display: flex;
    flex-direction: column;
    gap: 12px;
  }
</style>
