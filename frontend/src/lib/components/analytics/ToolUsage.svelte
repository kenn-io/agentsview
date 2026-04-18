<script lang="ts">
  import { analytics } from "../../stores/analytics.svelte.js";
  import type { ToolCategoryCount } from "../../api/types.js";

  const CATEGORY_COLORS: Record<string, string> = {
    Read: "#3b82f6",
    Edit: "#f59e0b",
    Write: "#10b981",
    Bash: "#ef4444",
    Grep: "#8b5cf6",
    Glob: "#06b6d4",
    Task: "#ec4899",
    Other: "#6b7280",
  };

  function colorFor(category: string): string {
    return CATEGORY_COLORS[category] ?? "#6b7280";
  }

  const categories = $derived(
    analytics.tools?.by_category ?? [],
  );

  const maxCount = $derived(
    categories.length > 0
      ? Math.max(...categories.map((c) => c.count), 1)
      : 1,
  );

  const trendEntries = $derived(analytics.tools?.trend ?? []);

  const trendMax = $derived.by(() => {
    let max = 1;
    for (const entry of trendEntries) {
      let total = 0;
      for (const v of Object.values(entry.by_category)) {
        total += v;
      }
      if (total > max) max = total;
    }
    return max;
  });

  function barWidth(count: number): number {
    return (count / maxCount) * 100;
  }

  function trendBarHeight(total: number): number {
    return Math.max((total / trendMax) * 100, 2);
  }

  function trendTotal(byCat: Record<string, number>): number {
    let total = 0;
    for (const v of Object.values(byCat)) {
      total += v;
    }
    return total;
  }

  function formatWeek(date: string): string {
    if (date.length < 10) return date;
    return date.slice(5);
  }

  let tooltip = $state<{
    x: number;
    y: number;
    text: string;
  } | null>(null);

  function handleCatHover(
    e: MouseEvent,
    cat: ToolCategoryCount,
  ) {
    const rect = (
      e.currentTarget as HTMLElement
    ).getBoundingClientRect();
    tooltip = {
      x: rect.left + rect.width / 2,
      y: rect.top - 4,
      text: `${cat.category}: ${cat.count.toLocaleString()} (${cat.pct}%)`,
    };
  }

  function handleTrendHover(
    e: MouseEvent,
    entry: { date: string; by_category: Record<string, number> },
  ) {
    const rect = (
      e.currentTarget as HTMLElement
    ).getBoundingClientRect();
    const total = trendTotal(entry.by_category);
    const parts = Object.entries(entry.by_category)
      .sort(([, a], [, b]) => b - a)
      .slice(0, 4)
      .map(([cat, count]) => `${cat}: ${count}`);
    tooltip = {
      x: rect.left + rect.width / 2,
      y: rect.top - 4,
      text: `${entry.date} | ${total} total | ${parts.join(", ")}`,
    };
  }

  function handleLeave() {
    tooltip = null;
  }
</script>

<div class="tool-container">
  <div class="tool-header">
    <h3 class="chart-title">Tool Usage</h3>
    {#if analytics.tools}
      <span class="count">
        {analytics.tools.total_calls.toLocaleString()} calls
      </span>
    {/if}
  </div>

  {#if analytics.errors.tools}
    <div class="error">
      {analytics.errors.tools}
      <button
        class="retry-btn"
        onclick={() => analytics.fetchTools()}
      >
        Retry
      </button>
    </div>
  {:else if categories.length > 0}
    <div class="sections">
      <div class="section">
        <h4 class="section-title">By Category</h4>
        <div class="bar-list">
          {#each categories as cat}
            <!-- svelte-ignore a11y_no_static_element_interactions -->
            <div
              class="bar-row"
              onmouseenter={(e) => handleCatHover(e, cat)}
              onmouseleave={handleLeave}
            >
              <span class="cat-name">{cat.category}</span>
              <div class="bar-track">
                <div
                  class="bar-fill"
                  style="width: {barWidth(cat.count)}%;
                    background: {colorFor(cat.category)}"
                ></div>
              </div>
              <span class="bar-value">
                {cat.count.toLocaleString()}
              </span>
              <span class="bar-pct">{cat.pct}%</span>
            </div>
          {/each}
        </div>
      </div>

      {#if trendEntries.length > 1}
        <div class="section">
          <h4 class="section-title">Weekly Trend</h4>
          <div class="trend-chart">
            {#each trendEntries as entry}
              <!-- svelte-ignore a11y_no_static_element_interactions -->
              <div
                class="trend-bar-wrapper"
                onmouseenter={(e) => handleTrendHover(e, entry)}
                onmouseleave={handleLeave}
              >
                <div
                  class="trend-bar"
                  style="height: {trendBarHeight(trendTotal(entry.by_category))}%"
                ></div>
                <span class="trend-label">
                  {formatWeek(entry.date)}
                </span>
              </div>
            {/each}
          </div>
        </div>
      {/if}
    </div>

    {#if tooltip}
      <div
        class="tooltip"
        style="left: {tooltip.x}px; top: {tooltip.y}px;"
      >
        {tooltip.text}
      </div>
    {/if}
  {:else}
    <div class="empty">No tool usage data</div>
  {/if}
</div>

<style>
  .tool-container {
    position: relative;
    flex: 1;
  }

  .tool-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    margin-bottom: 8px;
  }

  .chart-title {
    font-size: 12px;
    font-weight: 600;
    color: var(--text-primary);
  }

  .count {
    font-size: 10px;
    color: var(--text-muted);
  }

  .sections {
    display: flex;
    flex-direction: column;
    gap: 16px;
  }

  .section-title {
    font-size: 10px;
    font-weight: 600;
    color: var(--text-muted);
    text-transform: uppercase;
    letter-spacing: 0.05em;
    margin-bottom: 6px;
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
    padding: 2px 4px;
    border-radius: var(--radius-sm);
    transition: background 0.1s;
  }

  .bar-row:hover {
    background: var(--bg-surface-hover);
  }

  .cat-name {
    flex-shrink: 0;
    width: 60px;
    font-size: 11px;
    color: var(--text-secondary);
    white-space: nowrap;
  }

  .bar-track {
    flex: 1;
    height: 14px;
    background: var(--bg-inset);
    border-radius: 2px;
    overflow: hidden;
  }

  .bar-fill {
    height: 100%;
    border-radius: 2px;
    min-width: 2px;
  }

  .bar-value {
    flex-shrink: 0;
    width: 48px;
    text-align: right;
    font-size: 10px;
    font-family: var(--font-mono);
    color: var(--text-muted);
  }

  .bar-pct {
    flex-shrink: 0;
    width: 36px;
    text-align: right;
    font-size: 10px;
    font-family: var(--font-mono);
    color: var(--text-muted);
  }

  .trend-chart {
    display: flex;
    align-items: flex-end;
    gap: 3px;
    height: 80px;
    padding-top: 4px;
  }

  .trend-bar-wrapper {
    flex: 1;
    display: flex;
    flex-direction: column;
    align-items: center;
    height: 100%;
    justify-content: flex-end;
    cursor: default;
  }

  .trend-bar {
    width: 100%;
    max-width: 32px;
    background: var(--accent-blue, #3b82f6);
    border-radius: 2px 2px 0 0;
    min-height: 2px;
  }

  .trend-bar-wrapper:hover .trend-bar {
    opacity: 0.8;
  }

  .trend-label {
    font-size: 8px;
    color: var(--text-muted);
    margin-top: 2px;
    white-space: nowrap;
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

  .empty {
    color: var(--text-muted);
    font-size: 12px;
    padding: 24px;
    text-align: center;
  }

  .error {
    color: var(--accent-red);
    font-size: 12px;
    padding: 12px;
    display: flex;
    align-items: center;
    gap: 8px;
  }

  .retry-btn {
    padding: 2px 8px;
    border: 1px solid currentColor;
    border-radius: var(--radius-sm);
    font-size: 11px;
    color: inherit;
    cursor: pointer;
  }
</style>
