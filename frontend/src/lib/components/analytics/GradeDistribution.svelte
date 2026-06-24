<script lang="ts">
  import { _ } from "svelte-i18n";
  import { getGradeStyle } from "../../utils/grade.js";

  interface Props {
    distribution: Record<string, number>;
  }

  let { distribution }: Props = $props();

  const grades = ["A", "B", "C", "D", "F"] as const;
  const maxCount = $derived(
    Math.max(1, ...grades.map((g) => distribution[g] ?? 0)),
  );
</script>

<div class="grade-dist">
  <div class="chart-title">{$_("analytics.gradeDistribution")}</div>
  {#each grades as grade}
    {@const count = distribution[grade] ?? 0}
    {@const style = getGradeStyle(grade)}
    {@const pct = (count / maxCount) * 100}
    <div class="bar-row">
      <span class="bar-label" style:color={style.text}>
        {grade}
      </span>
      <div class="bar-track">
        <div
          class="bar-fill"
          style:width="{pct}%"
          style:background={style.bg}
        ></div>
      </div>
      <span class="bar-count">{count}</span>
    </div>
  {/each}
</div>

<style>
  .chart-title {
    font-size: 12px;
    font-weight: 600;
    color: var(--text-primary);
    margin-bottom: 10px;
  }
  .bar-row {
    display: flex;
    align-items: center;
    gap: 8px;
    margin-bottom: 6px;
  }
  .bar-label {
    width: 16px;
    font-size: 12px;
    font-weight: 700;
  }
  .bar-track {
    flex: 1;
    height: 16px;
    background: var(--bg-inset);
    border-radius: 3px;
    overflow: hidden;
  }
  .bar-fill {
    height: 100%;
    border-radius: 3px;
    transition: width 0.3s ease;
  }
  .bar-count {
    width: 24px;
    font-size: 11px;
    color: var(--text-secondary);
    text-align: right;
  }
</style>
