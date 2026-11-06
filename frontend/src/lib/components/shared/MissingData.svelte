<script lang="ts">
  import type { Snippet } from "svelte";

  interface Props {
    /** What is missing, as a short phrase ("No pinned messages"). */
    title: string;
    /** Why it is missing or how to fill it, in one or two sentences. */
    description?: string;
    /** A neutral glyph for the page or feature, typically a lucide icon. */
    icon?: Snippet;
    /** Controls that fill the gap, such as Retry or Build now. */
    action?: Snippet;
    role?: "status" | "note";
    class?: string;
  }

  let {
    title,
    description = undefined,
    icon = undefined,
    action = undefined,
    role = undefined,
    class: className = "",
  }: Props = $props();
</script>

<!-- The one empty or unavailable state for a page or panel: nothing to
     show yet, or a feature this server does not offer. It has no frame or
     fill, so it never reads as an alert, and it sits centered a fixed step
     below the page header, so every page shows it in the same place. -->
<div class="missing-data {className}" {role}>
  {#if icon}
    <span class="missing-data__icon" aria-hidden="true">{@render icon()}</span>
  {/if}
  <p class="missing-data__title">{title}</p>
  {#if description}
    <p class="missing-data__description">{description}</p>
  {/if}
  {#if action}
    <div class="missing-data__action">{@render action()}</div>
  {/if}
</div>

<style>
  .missing-data {
    display: flex;
    flex-direction: column;
    align-items: center;
    align-self: stretch;
    width: 100%;
    box-sizing: border-box;
    padding: 48px 16px;
    text-align: center;
  }

  .missing-data__icon {
    display: inline-flex;
    margin-bottom: var(--space-3);
    color: var(--text-muted);
  }

  .missing-data__icon :global(svg) {
    width: 24px;
    height: 24px;
  }

  /* Centered text keeps balanced lines so a lone word does not dangle. */
  .missing-data__title,
  .missing-data__description {
    margin: 0;
    max-width: 52ch;
    font-size: 13px;
    line-height: 1.45;
    text-wrap: balance;
  }

  .missing-data__title {
    color: var(--text-primary);
    font-weight: 600;
  }

  .missing-data__description {
    margin-top: 2px;
    color: var(--text-muted);
  }

  .missing-data__action {
    display: flex;
    flex-wrap: wrap;
    justify-content: center;
    gap: var(--space-4);
    margin-top: var(--space-5);
  }
</style>
