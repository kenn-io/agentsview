<script lang="ts">
  import type { SignalsTrendBucket } from "../../api/types/analytics.js";
  import { getGradeStyle, scoreToGrade } from "../../utils/grade.js";

  interface Props {
    trend: SignalsTrendBucket[];
  }

  let { trend }: Props = $props();

  const maxScore = 100;
</script>

<div class="health-trend">
  <div class="chart-title">Health Trend</div>
  {#if trend.length > 0}
    <div class="chart-area">
      <div class="y-axis">
        <span>100</span>
        <span>50</span>
        <span>0</span>
      </div>
      <div class="bars">
        {#each trend as bucket}
          {@const score = bucket.avg_health_score}
          {@const height = score != null
            ? (score / maxScore) * 100
            : 50}
          {@const style = score != null
            ? getGradeStyle(scoreToGrade(score))
            : getGradeStyle(null)}
          <div
            class="bar"
            style:height="{height}%"
            style:background={style.bg}
            title="{bucket.date}: {score != null
              ? Math.round(score)
              : 'no scored sessions'} ({bucket.session_count} sessions)"
          ></div>
        {/each}
      </div>
    </div>
    <div class="x-axis">
      {#if trend.length > 0}
        <span>{trend[0]!.date}</span>
      {/if}
      {#if trend.length > 1}
        <span>{trend[trend.length - 1]!.date}</span>
      {/if}
    </div>
    <div class="chart-caption">
      Daily average health score &middot; bar color = grade
    </div>
  {:else}
    <div class="empty">No trend data</div>
  {/if}
</div>

<style>
  .chart-title {
    font-size: 12px;
    font-weight: 600;
    color: var(--text-primary);
    margin-bottom: 10px;
  }
  .chart-area {
    display: flex;
    gap: 4px;
    height: 100px;
  }
  .y-axis {
    display: flex;
    flex-direction: column;
    justify-content: space-between;
    font-size: 10px;
    color: var(--text-muted);
    width: 24px;
    text-align: right;
    padding-right: 4px;
  }
  .bars {
    flex: 1;
    display: flex;
    align-items: flex-end;
    gap: 2px;
    border-bottom: 1px solid var(--border-muted);
    border-left: 1px solid var(--border-muted);
    padding: 0 2px;
  }
  .bar {
    flex: 1;
    border-radius: 2px 2px 0 0;
    min-width: 4px;
    transition: height 0.3s ease;
  }
  .x-axis {
    display: flex;
    justify-content: space-between;
    padding-left: 28px;
    padding-top: 4px;
    font-size: 10px;
    color: var(--text-muted);
  }
  .chart-caption {
    font-size: 11px;
    color: var(--text-muted);
    margin-top: 6px;
  }
  .empty {
    color: var(--text-muted);
    font-size: 12px;
  }
</style>
