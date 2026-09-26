<script lang="ts">
  import { Card } from "@kenn-io/kit-ui";
  import type { DbStatsOutcomeStats } from "../../api/generated/index.js";
  import { m } from "../../i18n/index.js";

  interface Props {
    stats: DbStatsOutcomeStats | null;
    loading: boolean;
    error: string | null;
    /** True once the caller has asked for the GitHub lookups too. */
    includePullRequests: boolean;
    onIncludePullRequests: () => void;
  }

  let {
    stats,
    loading,
    error,
    includePullRequests,
    onIncludePullRequests,
  }: Props = $props();

  interface Metric {
    label: string;
    value: number;
  }

  function formatNum(value: number): string {
    return value.toLocaleString();
  }

  const metrics = $derived.by((): Metric[] => {
    if (!stats) return [];
    const out: Metric[] = [
      { label: m.analytics_outcome_repositories(), value: stats.repos_active },
      { label: m.analytics_outcome_commits(), value: stats.commits },
      { label: m.analytics_outcome_loc_added(), value: stats.loc_added },
      { label: m.analytics_outcome_loc_removed(), value: stats.loc_removed },
      { label: m.analytics_outcome_files_changed(), value: stats.files_changed },
    ];
    // Absent pull-request counts mean "not asked for, or gh not configured",
    // which is not the same as zero, so the cards stay off the page entirely.
    if (stats.prs_opened !== undefined) {
      out.push({
        label: m.analytics_outcome_prs_opened(),
        value: stats.prs_opened,
      });
    }
    if (stats.prs_merged !== undefined) {
      out.push({
        label: m.analytics_outcome_prs_merged(),
        value: stats.prs_merged,
      });
    }
    return out;
  });

  const skipped = $derived(stats?.skipped ?? []);
</script>

<div class="outcome-container">
  <div class="outcome-header">
    <h3 class="chart-title">{m.analytics_outcome_title()}</h3>
    {#if !includePullRequests}
      <button
        class="outcome-load-prs"
        title={m.analytics_outcome_include_prs_hint()}
        onclick={onIncludePullRequests}
      >
        {m.analytics_outcome_include_prs()}
      </button>
    {/if}
  </div>

  {#if error}
    <div class="outcome-error">{error}</div>
  {:else if loading && !stats}
    <div class="outcome-loading">{m.analytics_outcome_loading()}</div>
  {:else if !stats}
    <div class="outcome-empty">{m.analytics_outcome_empty()}</div>
  {:else}
    <div class="outcome-cards">
      {#each metrics as item (item.label)}
        <Card level="default" padding="none" class="outcome-card">
          <span class="outcome-value">{formatNum(item.value)}</span>
          <span class="outcome-label">{item.label}</span>
        </Card>
      {/each}
    </div>
    {#if skipped.length > 0}
      <div class="outcome-partial">
        <span class="outcome-partial-title">{m.analytics_outcome_partial()}</span>
        <ul class="outcome-partial-list">
          {#each skipped as entry (entry.repo + entry.op)}
            <li>
              {m.analytics_outcome_partial_entry({
                repo: entry.repo,
                op: entry.op,
                reason: entry.reason,
              })}
            </li>
          {/each}
        </ul>
      </div>
    {/if}
  {/if}
</div>

<style>
  .outcome-container {
    display: flex;
    flex-direction: column;
    gap: 8px;
  }

  .outcome-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 8px;
  }

  .chart-title {
    font-size: 13px;
    font-weight: 600;
    color: var(--text-primary);
    margin: 0;
  }

  .outcome-cards {
    display: flex;
    gap: 8px;
    flex-wrap: wrap;
  }

  .outcome-cards :global(.outcome-card) {
    flex: 1;
    min-width: 120px;
    padding: 12px;
    display: flex;
    flex-direction: column;
    gap: 2px;
  }

  .outcome-cards :global(.outcome-card > .kit-card__body) {
    display: contents;
  }

  .outcome-value {
    font-size: 20px;
    font-weight: 600;
    color: var(--text-primary);
    line-height: 1.2;
  }

  .outcome-label {
    font-size: 11px;
    color: var(--text-muted);
    font-weight: 500;
  }

  .outcome-empty,
  .outcome-loading {
    font-size: 11px;
    color: var(--text-muted);
  }

  .outcome-error {
    font-size: 11px;
    color: var(--accent-red);
  }

  .outcome-partial {
    font-size: 11px;
    color: var(--accent-orange);
  }

  .outcome-partial-title {
    font-weight: 600;
  }

  .outcome-partial-list {
    margin: 2px 0 0;
    padding-left: 16px;
  }
</style>
