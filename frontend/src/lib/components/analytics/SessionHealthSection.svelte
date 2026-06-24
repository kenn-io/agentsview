<script lang="ts">
  import { _ } from "svelte-i18n";
  import { analytics } from "../../stores/analytics.svelte.js";
  import { scoreToGrade } from "../../utils/grade.js";
  import GradeDistribution
    from "./GradeDistribution.svelte";
  import OutcomeDistribution
    from "./OutcomeDistribution.svelte";
  import HealthTrend from "./HealthTrend.svelte";

  const signals = $derived(analytics.signals);
  const visible = $derived(
    signals != null &&
    (signals.scored_sessions > 0 ||
     signals.unscored_sessions > 0),
  );
</script>

{#if visible && signals}
  <div class="health-section">
    <div class="section-header">
      <h3 class="section-title">{$_("analytics.sessionHealth")}</h3>
      <span class="section-subtitle">
        {$_("analytics.scoredCount", { values: { count: signals.scored_sessions } })}
        &middot;
        {$_("analytics.unscoredCount", { values: { count: signals.unscored_sessions } })}
      </span>
    </div>

    <div class="health-summary-cards">
      <div class="card">
        <span class="card-label">{$_("analytics.avgScore")}</span>
        <span class="card-value">
          {signals.avg_health_score != null
            ? Math.round(signals.avg_health_score)
            : "--"}
        </span>
        {#if signals.avg_health_score != null}
          <span class="card-sub">
            {$_("analytics.gradeBadge", { values: { grade: scoreToGrade(signals.avg_health_score) } })}
          </span>
        {/if}
      </div>
      <div class="card">
        <span class="card-label">{$_("analytics.completed")}</span>
        <span class="card-value" style:color="var(--accent-green)">
          {#if signals.scored_sessions > 0}
            {Math.round(
              ((signals.outcome_distribution?.completed ?? 0) /
                (signals.scored_sessions +
                  signals.unscored_sessions)) *
                100,
            )}%
          {:else}
            --
          {/if}
        </span>
        <span class="card-sub">
          {$_("analytics.sessionCount", { values: { count: signals.outcome_distribution?.completed ?? 0 } })}
        </span>
      </div>
      <div class="card">
        <span class="card-label">{$_("analytics.errored")}</span>
        <span class="card-value" style:color="var(--accent-red)">
          {#if signals.scored_sessions > 0}
            {Math.round(
              ((signals.outcome_distribution?.errored ?? 0) /
                (signals.scored_sessions +
                  signals.unscored_sessions)) *
                100,
            )}%
          {:else}
            --
          {/if}
        </span>
        <span class="card-sub">
          {$_("analytics.sessionCount", { values: { count: signals.outcome_distribution?.errored ?? 0 } })}
        </span>
      </div>
      <div class="card">
        <span class="card-label">{$_("analytics.toolFailures")}</span>
        <span class="card-value" style:color="var(--accent-amber)">
          {#if signals.scored_sessions > 0}
            {Math.round(signals.tool_health.failure_rate)}%
          {:else}
            --
          {/if}
        </span>
        <span class="card-sub">
          {$_("analytics.sessionCount", { values: { count: signals.tool_health.sessions_with_failures } })}
        </span>
      </div>
      <div class="card">
        <span class="card-label">{$_("analytics.compactions")}</span>
        <span
          class="card-value"
          style:color={signals.context_health
            .sessions_with_mid_task_compaction > 0
            ? "var(--accent-red)"
            : "var(--accent-amber)"}
        >
          {signals.context_health.sessions_with_compaction}
        </span>
        <span class="card-sub">
          {#if signals.context_health.sessions_with_mid_task_compaction > 0}
            {$_("analytics.midTaskCount", { values: { count: signals.context_health.sessions_with_mid_task_compaction } })} &middot;
          {/if}
          {$_("analytics.avgPerSession", { values: { avg: signals.context_health.avg_compaction_count.toFixed(1) } })}
        </span>
      </div>
    </div>

    <div class="chart-grid">
      <div class="chart-panel">
        <GradeDistribution
          distribution={signals.grade_distribution}
        />
      </div>
      <div class="chart-panel">
        <OutcomeDistribution
          distribution={signals.outcome_distribution}
        />
      </div>
      <div class="chart-panel wide">
        <HealthTrend trend={signals.trend} />
      </div>
      <div class="chart-panel">
        <div class="mini-table">
          <div class="table-title">{$_("analytics.byAgent")}</div>
          <table>
            <thead>
              <tr>
                <th>{$_("analytics.agent")}</th>
                <th class="num">{$_("analytics.sessions")}</th>
                <th class="num">{$_("analytics.avgScore")}</th>
                <th class="num">{$_("analytics.completed")}</th>
              </tr>
            </thead>
            <tbody>
              {#each [...signals.by_agent].sort(
                (a, b) => b.session_count - a.session_count,
              ) as row}
                <tr>
                  <td>{row.agent}</td>
                  <td class="num">{row.session_count}</td>
                  <td class="num">
                    {row.avg_health_score != null
                      ? Math.round(row.avg_health_score)
                      : "--"}
                  </td>
                  <td class="num">
                    {Math.round(row.completed_rate)}%
                  </td>
                </tr>
              {/each}
            </tbody>
          </table>
        </div>
      </div>
      <div class="chart-panel">
        <div class="mini-table">
          <div class="table-title">{$_("analytics.byProject")}</div>
          <table>
            <thead>
              <tr>
                <th>{$_("analytics.project")}</th>
                <th class="num">{$_("analytics.sessions")}</th>
                <th class="num">{$_("analytics.avgScore")}</th>
                <th class="num">{$_("analytics.completed")}</th>
              </tr>
            </thead>
            <tbody>
              {#each [...signals.by_project].sort(
                (a, b) => b.session_count - a.session_count,
              ) as row}
                <tr>
                  <td>{row.project}</td>
                  <td class="num">{row.session_count}</td>
                  <td class="num">
                    {row.avg_health_score != null
                      ? Math.round(row.avg_health_score)
                      : "--"}
                  </td>
                  <td class="num">
                    {Math.round(row.completed_rate)}%
                  </td>
                </tr>
              {/each}
            </tbody>
          </table>
        </div>
      </div>
    </div>
  </div>
{/if}

<style>
  .health-section {
    margin-top: 16px;
  }
  .section-header {
    margin-bottom: 12px;
  }
  .section-title {
    font-size: 15px;
    font-weight: 700;
    color: var(--text-primary);
    margin: 0 0 2px;
  }
  .section-subtitle {
    font-size: 12px;
    color: var(--text-muted);
  }
  .health-summary-cards {
    display: grid;
    grid-template-columns: repeat(5, 1fr);
    gap: 12px;
    margin-bottom: 12px;
  }
  .card {
    background: var(--bg-surface);
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-md);
    padding: 12px;
  }
  .card-label {
    display: block;
    font-size: 11px;
    color: var(--text-muted);
    text-transform: uppercase;
    letter-spacing: 0.5px;
    margin-bottom: 4px;
  }
  .card-value {
    display: block;
    font-size: 24px;
    font-weight: 700;
    color: var(--text-primary);
  }
  .card-sub {
    display: block;
    font-size: 12px;
    color: var(--text-secondary);
  }
  .chart-grid {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: 12px;
  }
  .chart-panel {
    background: var(--bg-surface);
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-md);
    padding: 12px;
  }
  .chart-panel.wide {
    grid-column: 1 / -1;
  }
  .mini-table {
    font-size: 12px;
  }
  .table-title {
    font-weight: 600;
    color: var(--text-primary);
    margin-bottom: 8px;
  }
  table {
    width: 100%;
    border-collapse: collapse;
  }
  th {
    text-align: left;
    padding: 4px 0;
    color: var(--text-muted);
    font-weight: 500;
    border-bottom: 1px solid var(--border-muted);
  }
  th.num, td.num {
    text-align: right;
  }
  td {
    padding: 6px 0;
    color: var(--text-primary);
    border-bottom: 1px solid var(--bg-inset);
  }
  @media (max-width: 767px) {
    .health-summary-cards {
      grid-template-columns: repeat(2, 1fr);
    }
    .chart-grid {
      grid-template-columns: 1fr;
    }
    .chart-panel.wide {
      grid-column: 1;
    }
  }
</style>
