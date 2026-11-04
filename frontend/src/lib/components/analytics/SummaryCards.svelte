<script lang="ts">
  import { formatCount } from "../../utils/format.js";
  import { Card } from "@kenn-io/kit-ui";
  import { analytics } from "../../stores/analytics.svelte.js";
  import PanelStatus from "./PanelStatus.svelte";
  import { formatDayRange, m } from "../../i18n/index.js";

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
      sub: () => {
        const params = analytics.summaryFilterParams;
        if (!analytics.summary || !params?.from || !params.to) return "";
        return m.analytics_summary_sessions_scope({
          range: formatDayRange(params.from, params.to),
        });
      },
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
      {#if !analytics.summary}
        <span class="card-value placeholder">{analytics.errors.summary ? "--" : "-"}</span>
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

<!-- The placeholder cards stand in for the loading message. -->
<PanelStatus
  hasData
  pending={analytics.querying.summary}
  error={analytics.errors.summary}
  onRetry={() => analytics.fetchSummary()}
/>

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

  .card-value.placeholder {
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
</style>
