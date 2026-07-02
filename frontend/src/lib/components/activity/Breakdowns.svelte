<script lang="ts">
  import { m } from "../../i18n/index.js";
  import type { Report } from "../../api/types.js";
  import type { ActivityKeyMinutes } from "../../api/generated/index";

  let { report }: { report: Report } = $props();

  type Metric = "minutes" | "cost";
  let metric = $state<Metric>("minutes");

  // by_* fields are typed `any[] | null` by the codegen; cast each
  // to the generated element model for field-level type safety.
  function asKeyMinutes(arr: any[] | null): ActivityKeyMinutes[] {
    return (arr ?? []) as ActivityKeyMinutes[];
  }

  function rowValue(row: ActivityKeyMinutes): number {
    return metric === "cost" ? row.cost : row.agent_minutes;
  }

  // Per-row automation split for the active metric. Interactive + automated
  // sum to rowValue, so the two bar segments stack to the full bar width.
  function interactiveValue(row: ActivityKeyMinutes): number {
    return metric === "cost"
      ? row.interactive_cost
      : row.interactive_agent_minutes;
  }

  function automatedValue(row: ActivityKeyMinutes): number {
    return metric === "cost" ? row.automated_cost : row.automated_agent_minutes;
  }

  // Rank by the selected metric and drop rows that are zero for it: an untimed
  // cost-only row contributes nothing to the minutes view (and would otherwise
  // render as an empty "0" bar), and a zero-cost row drops from the cost view.
  // The backend pre-sorts by minutes, so re-sort for the cost view.
  function rankedRows(arr: any[] | null): ActivityKeyMinutes[] {
    return asKeyMinutes(arr)
      .filter((r) => rowValue(r) > 0)
      .sort((a, b) => rowValue(b) - rowValue(a));
  }

  const byProject = $derived(rankedRows(report.by_project));
  const byModel = $derived(rankedRows(report.by_model));
  const byAgent = $derived(rankedRows(report.by_agent));

  interface Panel {
    title: string;
    rows: ActivityKeyMinutes[];
  }

  const panels = $derived.by((): Panel[] => [
    { title: m.activity_project(), rows: byProject },
    { title: m.activity_model(), rows: byModel },
    { title: m.activity_agent(), rows: byAgent },
  ]);

  function maxValue(rows: ActivityKeyMinutes[]): number {
    if (rows.length === 0) return 1;
    const m = Math.max(...rows.map(rowValue));
    // Fall back to 1 only when the max is non-positive, so the largest bar
    // reaches 100% even when every value is under one unit.
    return m > 0 ? m : 1;
  }

  function barWidth(value: number, max: number): number {
    return (value / max) * 100;
  }

  function fmtMinutes(v: number): string {
    return Math.round(v).toLocaleString();
  }

  function fmtCost(v: number): string {
    return `$${v.toFixed(2)}`;
  }

  function fmtValue(row: ActivityKeyMinutes): string {
    return metric === "cost" ? fmtCost(row.cost) : fmtMinutes(row.agent_minutes);
  }

  function fmtSeg(v: number): string {
    return metric === "cost" ? fmtCost(v) : fmtMinutes(v);
  }

  function truncate(name: string, max: number): string {
    if (name.length <= max) return name;
    return name.slice(0, max - 1) + "…";
  }

  let tooltip = $state<{ x: number; y: number; text: string } | null>(null);

  function sumValue(rows: ActivityKeyMinutes[]): number {
    let total = 0;
    for (const r of rows) total += rowValue(r);
    return total;
  }

  function showTip(e: MouseEvent, row: ActivityKeyMinutes, total: number) {
    const rect = (e.currentTarget as HTMLElement).getBoundingClientRect();
    const pct = total > 0 ? Math.round((rowValue(row) / total) * 100) : 0;
    const unit = metric === "cost" ? "" : m.activity_min_unit();
    const split = m.activity_int_auto_split({ int: fmtSeg(interactiveValue(row)), auto: fmtSeg(automatedValue(row)) });
    tooltip = {
      x: rect.left + rect.width / 2,
      y: rect.top - 4,
      text: `${row.key} · ${fmtValue(row)}${unit} · ${pct}% · ${split}`,
    };
  }

  function hideTip() {
    tooltip = null;
  }
</script>

<div class="breakdowns">
  <div class="breakdowns-header">
    <h3 class="breakdowns-title">{m.activity_breakdown()}</h3>
    <div class="panel-actions">
      <div class="legend" aria-hidden="true">
        <span class="legend-item">
          <span class="swatch interactive"></span>{m.activity_interactive()}
        </span>
        <span class="legend-item">
          <span class="swatch automated"></span>{m.activity_automated()}
        </span>
      </div>
      <div class="metric-toggle" role="group" aria-label={m.activity_breakdown_metric()}>
      <button
        type="button"
        class="metric-btn"
        class:active={metric === "minutes"}
        aria-pressed={metric === "minutes"}
        onclick={() => (metric = "minutes")}
      >
        {m.activity_agent_min()}
      </button>
      <button
        type="button"
        class="metric-btn"
        class:active={metric === "cost"}
        aria-pressed={metric === "cost"}
        onclick={() => (metric = "cost")}
      >
        {m.activity_cost()}
      </button>
      </div>
    </div>
  </div>

  <div class="breakdown-grid">
    {#each panels as panel (panel.title)}
      {@const max = maxValue(panel.rows)}
      {@const total = sumValue(panel.rows)}
      <div class="breakdown-panel">
        <h4 class="panel-title">{panel.title}</h4>
        {#if panel.rows.length > 0}
          <div class="bar-list">
            {#each panel.rows as row (row.key)}
              <!-- svelte-ignore a11y_no_static_element_interactions -->
              <div
                class="bar-row"
                onmouseenter={(e) => showTip(e, row, total)}
                onmouseleave={hideTip}
              >
                <span class="bar-label" title={row.key}>
                  {truncate(row.key, 22)}
                </span>
                <div class="bar-track">
                  <div
                    class="bar-seg interactive"
                    style="width: {barWidth(interactiveValue(row), max)}%"
                  ></div>
                  <div
                    class="bar-seg automated"
                    style="width: {barWidth(automatedValue(row), max)}%"
                  ></div>
                </div>
                <span class="bar-value">
                  {fmtValue(row)}
                </span>
              </div>
            {/each}
          </div>
        {:else}
          <div class="empty">{m.activity_none_lower()}</div>
        {/if}
      </div>
    {/each}
  </div>

  {#if tooltip}
    <div class="tooltip" style="left: {tooltip.x}px; top: {tooltip.y}px;">
      {tooltip.text}
    </div>
  {/if}
</div>

<style>
  .breakdowns {
    display: flex;
    flex-direction: column;
  }

  .breakdowns-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    margin-bottom: 12px;
  }

  .breakdowns-title {
    font-size: 12px;
    font-weight: 600;
    color: var(--text-primary);
  }

  .panel-actions {
    display: flex;
    align-items: center;
    gap: 12px;
  }

  .legend {
    display: flex;
    align-items: center;
    gap: 10px;
  }

  .legend-item {
    display: inline-flex;
    align-items: center;
    gap: 4px;
    font-size: 10px;
    color: var(--text-muted);
  }

  .swatch {
    width: 8px;
    height: 8px;
    border-radius: 2px;
  }

  .swatch.interactive {
    background: var(--accent-blue);
  }

  .swatch.automated {
    background: var(--accent-orange);
  }

  .metric-toggle {
    display: inline-flex;
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-sm);
    overflow: hidden;
  }

  .metric-btn {
    padding: 3px 10px;
    font-size: 10px;
    font-weight: 600;
    color: var(--text-muted);
    background: var(--bg-inset);
    cursor: pointer;
    border: none;
  }

  .metric-btn:hover {
    color: var(--text-secondary);
  }

  .metric-btn.active {
    background: var(--accent-blue);
    color: var(--accent-blue-foreground);
  }

  .metric-btn + .metric-btn {
    border-left: 1px solid var(--border-muted);
  }

  .breakdown-grid {
    display: grid;
    grid-template-columns: repeat(
      auto-fit,
      minmax(220px, 1fr)
    );
    gap: 16px;
  }

  .breakdown-panel {
    min-width: 0;
  }

  .panel-title {
    font-size: 11px;
    font-weight: 600;
    color: var(--text-primary);
    margin-bottom: 8px;
  }

  .bar-list {
    display: flex;
    flex-direction: column;
    gap: 3px;
  }

  .bar-row {
    display: flex;
    align-items: center;
    gap: 8px;
  }

  .bar-label {
    flex-shrink: 0;
    width: 96px;
    font-size: 11px;
    color: var(--text-secondary);
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .bar-track {
    flex: 1;
    display: flex;
    height: 14px;
    background: var(--bg-inset);
    border-radius: 2px;
    overflow: hidden;
  }

  .bar-seg {
    height: 100%;
  }

  .bar-seg.interactive {
    background: var(--accent-blue);
  }

  .bar-seg.automated {
    background: var(--accent-orange);
  }

  .bar-value {
    flex-shrink: 0;
    width: 56px;
    text-align: right;
    font-size: 10px;
    font-family: var(--font-mono);
    color: var(--text-muted);
  }

  .empty {
    color: var(--text-muted);
    font-size: 11px;
    padding: 8px 0;
  }

  .tooltip {
    position: fixed;
    transform: translateX(-50%) translateY(-100%);
    padding: 4px 8px;
    background: var(--text-primary);
    color: var(--bg-primary);
    font-size: 10px;
    border-radius: var(--radius-sm);
    white-space: nowrap;
    pointer-events: none;
    z-index: 100;
  }
</style>
