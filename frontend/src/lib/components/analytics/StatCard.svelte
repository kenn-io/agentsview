<script lang="ts">
  import { Card } from "@kenn-io/kit-ui";

  /**
   * One KPI card for every analytics summary row: value above label, an
   * optional one-line sublabel, and three value sizes. `lead` is the one or
   * two figures a row is about, `standard` the supporting metrics, and
   * `context` the plain counts (projects, models, days) that read quieter.
   */
  type Tier = "lead" | "standard" | "context";

  let {
    value,
    label,
    sub = "",
    tier = "standard",
    featured = false,
    error = false,
    class: className = "",
  }: {
    value: string;
    label: string;
    sub?: string;
    tier?: Tier;
    featured?: boolean;
    error?: boolean;
    class?: string;
  } = $props();

  const classes = $derived(
    [
      "card",
      "stat-card",
      tier,
      featured ? "featured" : "",
      className,
    ]
      .filter(Boolean)
      .join(" "),
  );
</script>

<Card level="default" padding="none" class={classes}>
  <span class="card-value" class:error>{error ? "--" : value}</span>
  <span class="card-label" title={label}>{label}</span>
  {#if sub && !error}
    <span class="card-sub" title={sub}>{sub}</span>
  {/if}
</Card>

<style>
  /* Fixed line boxes keep every card 64px tall, with or without a
     sublabel, so wrapped rows line up. Three classes outrank kit-ui's
     scoped padding and surface rules. */
  :global(.kit-card.stat-card.card) {
    flex: 1;
    min-width: 120px;
    min-height: 64px;
    box-sizing: border-box;
    padding: 5px 10px;
    display: flex;
    flex-direction: column;
    gap: 1px;
  }

  :global(.stat-card > .kit-card__body) {
    display: contents;
  }

  /* The featured total keeps the neutral selected surface, not an
     accent outline that reads as a clickable choice. */
  :global(.kit-card.stat-card.featured) {
    background: var(--selected-bg);
    box-shadow: var(--selected-edge);
  }

  :global(.kit-card.stat-card.context) {
    border-color: var(--border-muted);
    box-shadow: none;
  }

  .card-value {
    font-size: 18px;
    line-height: 22px;
    font-weight: 600;
    color: var(--text-primary);
    font-variant-numeric: tabular-nums;
    letter-spacing: -0.01em;
    white-space: nowrap;
  }

  :global(.stat-card.lead) .card-value {
    font-size: 20px;
  }

  :global(.stat-card.context) .card-value {
    font-size: 16px;
  }

  .card-value.error {
    color: var(--text-muted);
  }

  .card-label,
  .card-sub {
    font-size: 11px;
    line-height: 14px;
    min-width: 0;
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .card-label {
    font-weight: 500;
    color: var(--text-secondary);
  }

  :global(.stat-card.context) .card-label,
  .card-sub {
    color: var(--text-muted);
  }
</style>
