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
    /** `empty` is a quiet centered page state with no frame, kept apart
        from the framed notices for read-only or unavailable states. */
    variant?: "notice" | "empty";
    class?: string;
  }

  let {
    title = undefined,
    description = undefined,
    icon = undefined,
    children = undefined,
    role = undefined,
    variant = "notice",
    class: className = "",
  }: Props = $props();
</script>

<!-- One inline block for read-only, unavailable, and empty-with-explanation
     states on library pages: a muted surface, secondary body text, and an
     optional neutral icon. It sits at the top of the content area, so every
     page shows its state in the same place. -->
<div class="state-notice {className}" class:state-notice--empty={variant === "empty"} {role}>
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

  /* Balanced wrapping keeps a lone phrase from dangling on the last line;
     pretty wrapping only rescues single-word orphans. */
  .state-notice__body {
    margin: 0;
    min-width: 0;
    font-size: 13px;
    line-height: 1.45;
    text-wrap: balance;
  }

  .state-notice__title {
    margin-right: 0.35em;
    color: var(--text-primary);
    font-weight: 600;
  }

  /* Empty page state: no frame or fill, centered, the title stacked over
     its explanation, so it cannot be mistaken for an alert or filter bar. */
  .state-notice.state-notice--empty {
    flex-direction: column;
    align-items: center;
    gap: var(--space-3);
    padding: 48px 16px;
    border: 0;
    background: transparent;
    text-align: center;
  }

  :global(:root:not(.dark)) .state-notice.state-notice--empty {
    background: transparent;
  }

  .state-notice--empty .state-notice__icon {
    margin-top: 0;
  }

  .state-notice--empty .state-notice__icon :global(svg) {
    width: 24px;
    height: 24px;
  }

  .state-notice--empty .state-notice__body {
    display: flex;
    flex-direction: column;
    gap: 2px;
    max-width: 44ch;
  }

  .state-notice--empty .state-notice__title {
    margin-right: 0;
  }

  /* The explanation sits a clear step below the title. */
  .state-notice--empty .state-notice__description {
    color: var(--text-muted);
  }
</style>
