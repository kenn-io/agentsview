<script lang="ts">
  import type { Snippet } from "svelte";

  let {
    title,
    description = "",
    children,
  }: {
    title: string;
    description?: string;
    children: Snippet;
  } = $props();
</script>

<section class="settings-section">
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
