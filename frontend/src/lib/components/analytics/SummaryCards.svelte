<script lang="ts">
  import { formatCount } from "../../utils/format.js";
  import { Card } from "@kenn-io/kit-ui";
  import { analytics } from "../../stores/analytics.svelte.js";
  import { m } from "../../i18n/index.js";

  function pct(n: number): string {
    return `${(n * 100).toFixed(1)}%`;
  }

  interface SummaryCard {
    label: () => string;
    value: () => string;
    sub?: () => string;
  }

  const cards: SummaryCard[] = [
    {
      label: () => m.analytics_summary_sessions(),
      value: () =>
        formatCount(analytics.summary?.total_sessions ?? 0),
    },
    {
      label: () => m.analytics_summary_messages(),
      value: () =>
        formatCount(analytics.summary?.total_messages ?? 0),
    },
    {
      label: () => m.analytics_summary_projects(),
      value: () =>
        formatCount(analytics.summary?.active_projects ?? 0),
    },
    {
      label: () => m.analytics_summary_active_days(),
      value: () =>
        formatCount(analytics.summary?.active_days ?? 0),
    },
    {
      label: () => m.analytics_summary_messages_per_session(),
      value: () => {
        const s = analytics.summary;
        if (!s) return "-";
        return formatCount(s.avg_messages);
      },
      sub: () => {
        const s = analytics.summary;
        if (!s) return "";
        return m.analytics_summary_median_p90({
          median: s.median_messages,
          p90: s.p90_messages,
        });
      },
    },
    {
      label: () => m.analytics_summary_concentration(),
      value: () => pct(analytics.summary?.concentration ?? 0),
      sub: () => analytics.summary?.most_active_project ?? "",
    },
  ];
</script>

<div class="summary-cards">
  {#each cards as card}
    <Card level="default" padding="none" class="card">
      {#if analytics.errors.summary}
        <span class="card-value error">--</span>
        <span class="card-label">{card.label()}</span>
      {:else}
        <span class="card-value">
          {card.value()}
        </span>
        <span class="card-label">{card.label()}</span>
        {#if card.sub}
          {@const subtext = card.sub()}
          {#if subtext}
            <span class="card-sub">{subtext}</span>
          {/if}
        {/if}
      {/if}
    </Card>
  {/each}
</div>

{#if analytics.errors.summary}
  <div class="error-bar">
    <span>{analytics.errors.summary}</span>
    <button
      class="retry-btn"
      onclick={() => analytics.fetchSummary()}
    >
      {m.shared_retry()}
    </button>
  </div>
{/if}

<style>
  .summary-cards {
    display: flex;
    gap: 8px;
    flex-wrap: wrap;
  }

  .summary-cards :global(.card) {
    flex: 1;
    min-width: 120px;
    padding: 12px;
    display: flex;
    flex-direction: column;
    gap: 2px;
  }

  .summary-cards :global(.card > .kit-card__body) {
    display: contents;
  }

  .card-value {
    font-size: 20px;
    font-weight: 600;
    color: var(--text-primary);
    line-height: 1.2;
  }

  .card-value.error {
    color: var(--text-muted);
  }

  .card-label {
    font-size: 11px;
    color: var(--text-muted);
    font-weight: 500;
  }

  .card-sub {
    font-size: 11px;
    color: var(--text-muted);
    margin-top: 2px;
  }

  .error-bar {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 8px 12px;
    background: var(--bg-surface);
    border: 1px solid var(--accent-red);
    border-radius: var(--radius-sm);
    font-size: 11px;
    color: var(--accent-red);
  }

  .retry-btn {
    padding: 2px 8px;
    border: 1px solid var(--accent-red);
    border-radius: var(--radius-sm);
    font-size: 11px;
    color: var(--accent-red);
    cursor: pointer;
  }

  .retry-btn:hover {
    background: var(--accent-red);
    color: var(--accent-red-foreground);
  }
</style>
