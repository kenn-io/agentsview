<script lang="ts">
  import { Card } from "@kenn-io/kit-ui";
  import { analytics } from "../../stores/analytics.svelte.js";
  import { scoreToGrade } from "../../utils/grade.js";
  import GradeDistribution
    from "./GradeDistribution.svelte";
  import OutcomeDistribution
    from "./OutcomeDistribution.svelte";
  import HealthTrend from "./HealthTrend.svelte";
  import { m } from "../../i18n/index.js";

  const signals = $derived(analytics.signals);
  const visible = $derived(
    signals != null &&
    (signals.scored_sessions > 0 ||
     signals.unscored_sessions > 0),
  );

  function formatSessionCount(count: number): string {
    return m.analytics_session_shape_session_count({
      count,
      countLabel: count.toLocaleString(),
    });
  }
</script>

{#if visible && signals}
  <div class="health-section">
    <div class="section-header">
      <h3 class="section-title">{m.analytics_session_health_title()}</h3>
      <span class="section-subtitle">
        {m.analytics_session_health_scored({
          countLabel: signals.scored_sessions.toLocaleString(),
        })}
        &middot;
        {m.analytics_session_health_unscored({
          countLabel: signals.unscored_sessions.toLocaleString(),
        })}
      </span>
    </div>

    <div class="health-summary-cards">
      <Card level="default" padding="none" class="card">
        <span class="card-label">{m.analytics_session_health_avg_score()}</span>
        <span class="card-value">
          {signals.avg_health_score != null
            ? Math.round(signals.avg_health_score)
            : "--"}
        </span>
        {#if signals.avg_health_score != null}
          <span class="card-sub">
            {m.analytics_session_health_grade({ grade: scoreToGrade(signals.avg_health_score) })}
          </span>
        {/if}
      </Card>
      <Card level="default" padding="none" class="card">
        <span class="card-label">{m.analytics_session_health_completed()}</span>
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
          {formatSessionCount(signals.outcome_distribution?.completed ?? 0)}
        </span>
      </Card>
      <Card level="default" padding="none" class="card">
        <span class="card-label">{m.analytics_session_health_errored()}</span>
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
          {formatSessionCount(signals.outcome_distribution?.errored ?? 0)}
        </span>
      </Card>
      <Card level="default" padding="none" class="card">
        <span class="card-label">{m.analytics_session_health_tool_failures()}</span>
        <span class="card-value" style:color="var(--accent-amber)">
          {#if signals.scored_sessions > 0}
            {Math.round(signals.tool_health.failure_rate)}%
          {:else}
            --
          {/if}
        </span>
        <span class="card-sub">
          {formatSessionCount(signals.tool_health.sessions_with_failures)}
        </span>
      </Card>
      <Card level="default" padding="none" class="card">
        <span class="card-label">{m.analytics_session_health_compactions()}</span>
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
            {signals.context_health.sessions_with_mid_task_compaction}
            {m.analytics_session_health_mid_task()} &middot;
          {/if}
          {m.analytics_session_health_avg_per_session({
            value: signals.context_health.avg_compaction_count.toFixed(1),
          })}
        </span>
      </Card>
    </div>

    <div class="chart-grid">
      <Card level="default" padding="none" class="chart-panel">
        <GradeDistribution
          distribution={signals.grade_distribution}
        />
      </Card>
      <Card level="default" padding="none" class="chart-panel">
        <OutcomeDistribution
          distribution={signals.outcome_distribution}
        />
      </Card>
      <Card level="default" padding="none" class="chart-panel wide">
        <HealthTrend trend={signals.trend} />
      </Card>
      <Card level="default" padding="none" class="chart-panel">
        <div class="mini-table">
          <div class="table-title">{m.analytics_by_agent()}</div>
          <table>
            <thead>
              <tr>
                <th>{m.analytics_col_agent()}</th>
                <th class="num">{m.analytics_col_sessions()}</th>
                <th class="num">{m.analytics_session_health_avg_score()}</th>
                <th class="num">{m.analytics_session_health_completed()}</th>
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
      </Card>
      <Card level="default" padding="none" class="chart-panel">
        <div class="mini-table">
          <div class="table-title">{m.analytics_by_project()}</div>
          <table>
            <thead>
              <tr>
                <th>{m.analytics_col_project()}</th>
                <th class="num">{m.analytics_col_sessions()}</th>
                <th class="num">{m.analytics_session_health_avg_score()}</th>
                <th class="num">{m.analytics_session_health_completed()}</th>
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
      </Card>
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
  .health-summary-cards :global(.card) {
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
  .chart-grid :global(.chart-panel) {
    padding: 12px;
  }
  .chart-grid :global(.chart-panel.wide) {
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
  @media (max-width: 760px) {
    .health-summary-cards {
      grid-template-columns: repeat(2, 1fr);
    }
    .chart-grid {
      grid-template-columns: 1fr;
    }
    .chart-grid :global(.chart-panel.wide) {
      grid-column: 1;
    }
  }
</style>
