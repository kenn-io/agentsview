<script lang="ts">
  import type { Snippet } from "svelte";

  interface Props {
    /** Short lead phrase, rendered in primary text before the body. */
    title?: string;
    /** Explanation text; use `children` instead for markup. */
    description?: string;
    /** Optional small neutral glyph, typically a 16px lucide icon. */
    icon?: Snippet;
    children?: Snippet;
    role?: "status" | "note" | "alert";
    class?: string;
  }

  let {
    title = undefined,
    description = undefined,
    icon = undefined,
    children = undefined,
    role = undefined,
    class: className = "",
  }: Props = $props();
</script>

<!-- One framed inline note for read-only and scope explanations on library
     pages: a muted surface, secondary body text, and an optional neutral
     icon. It sits at the top of the content area, so every page shows its
     note in the same place. A page or panel with nothing to show uses
     kit-ui EmptyState instead. -->
<div class="state-notice {className}" {role}>
  {#if icon}
    <span class="state-notice__icon" aria-hidden="true">{@render icon()}</span>
  {/if}
  <p class="state-notice__body">
    {#if title}<strong class="state-notice__title">{title}</strong>{/if}
    {#if description}<span class="state-notice__description">{description}</span>{/if}
    {#if children}{@render children()}{/if}
  </p>
</div>

<style>
  /* kit-ui-check-ignore: the notice carries role="status|note|alert", which kit-ui Card cannot, and light theme replaces this chrome below */
  .state-notice {
    display: flex;
    align-items: flex-start;
    gap: var(--space-4);
    padding: 10px 12px;
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-sm);
    background: var(--bg-inset);
    color: var(--text-secondary);
  }

  /* In light theme the inset fill sits too close to the page and the muted
     border is lighter than the fill, so the notice vanished. A surface fill
     with the default border reads as a quiet framed note on the page and
     inside cards alike. */
  :global(:root:not(.dark)) .state-notice {
    border-color: var(--border-default);
    background: var(--bg-surface);
  }

  .state-notice__icon {
    display: inline-flex;
    flex-shrink: 0;
    margin-top: 1px;
    color: var(--text-muted);
  }

  /* A framed notice fills its box: text runs on one line when it fits,
     and pretty wrapping only rescues a single-word orphan, instead of
     balancing two half-width lines that leave the right of the frame
     empty. */
  .state-notice__body {
    margin: 0;
    min-width: 0;
    font-size: 13px;
    line-height: 1.45;
    text-wrap: pretty;
  }

  .state-notice__title {
    margin-right: 0.35em;
    color: var(--text-primary);
    font-weight: 600;
  }
</style>
