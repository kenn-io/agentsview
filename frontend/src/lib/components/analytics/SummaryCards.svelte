<script lang="ts">
  import { formatCount } from "../../utils/format.js";
  import StatCard from "./StatCard.svelte";
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
    tier?: "lead" | "standard" | "context";
  }

  const cards: SummaryCard[] = [
    {
      label: () => m.analytics_summary_sessions(),
      tier: "lead",
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
      tier: "lead",
      value: () =>
        formatCount(analytics.summary?.total_messages ?? 0),
    },
    {
      label: () => m.analytics_summary_projects(),
      tier: "context",
      value: () =>
        formatCount(analytics.summary?.active_projects ?? 0),
    },
    {
      label: () => m.analytics_summary_active_days(),
      tier: "context",
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
    <StatCard
      value={analytics.summary ? card.value() : "-"}
      label={card.label()}
      sub={card.sub?.() ?? ""}
      tier={card.tier}
      error={!!analytics.errors.summary}
    />
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
</style>
