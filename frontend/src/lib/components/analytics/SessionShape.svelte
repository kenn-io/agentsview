<script lang="ts">
  import { analytics } from "../../stores/analytics.svelte.js";
  import { router } from "../../stores/router.svelte.js";
  import type { DistributionBucket } from "../../api/types.js";

  type View = "length" | "duration" | "autonomy";
  let activeView: View = $state("length");

  const viewLabels: Record<View, string> = {
    length: "Messages",
    duration: "Duration",
    autonomy: "Autonomy",
  };

  const activeBuckets = $derived.by(() => {
    const shape = analytics.sessionShape;
    if (!shape) return [];
    switch (activeView) {
      case "length":
        return shape.length_distribution;
      case "duration":
        return shape.duration_distribution;
      case "autonomy":
        return shape.autonomy_distribution;
    }
  });

  const maxCount = $derived(
    Math.max(1, ...activeBuckets.map((b) => b.count)),
  );

  function barWidth(bucket: DistributionBucket): string {
    return `${(bucket.count / maxCount) * 100}%`;
  }

  function parseLengthBucket(
    label: string,
  ): { min: number; max?: number } {
    if (label.endsWith("+")) {
      return { min: parseInt(label, 10) };
    }
    const parts = label.split("-");
    return {
      min: parseInt(parts[0]!, 10),
      max: parseInt(parts[1]!, 10),
    };
  }

  function handleBucketClick(bucket: DistributionBucket) {
    if (activeView !== "length" || bucket.count === 0) return;
    const { min, max } = parseLengthBucket(bucket.label);
    const params: Record<string, string> = {
      min_messages: String(min),
    };
    if (max !== undefined) {
      params["max_messages"] = String(max);
    }
    router.navigate("sessions", params);
  }
</script>

<div class="shape-container">
  <div class="shape-header">
    <h3 class="chart-title">Session Shape</h3>
    <div class="view-toggle">
      {#each (["length", "duration", "autonomy"] as const) as v}
        <button
          class="toggle-btn"
          class:active={activeView === v}
          onclick={() => (activeView = v)}
        >
          {viewLabels[v]}
        </button>
      {/each}
    </div>
  </div>

  {#if analytics.errors.sessionShape}
    <div class="error">
      {analytics.errors.sessionShape}
      <button
        class="retry-btn"
        onclick={() => analytics.fetchSessionShape()}
      >
        Retry
      </button>
    </div>
  {:else if activeBuckets.length > 0}
    <div class="bar-chart">
      {#each activeBuckets as bucket}
        <!-- svelte-ignore a11y_click_events_have_key_events -->
        <!-- svelte-ignore a11y_no_static_element_interactions -->
        <div
          class="bar-row"
          class:clickable={activeView === "length" && bucket.count > 0}
          onclick={() => handleBucketClick(bucket)}
        >
          <span class="bar-label">{bucket.label}</span>
          <div class="bar-track">
            <div
              class="bar-fill"
              style="width: {barWidth(bucket)}"
            ></div>
          </div>
          <span class="bar-count">{bucket.count}</span>
        </div>
      {/each}
    </div>
    {#if analytics.sessionShape}
      <div class="shape-footer">
        {analytics.sessionShape.count} sessions
      </div>
    {/if}
  {:else}
    <div class="empty">No data for this period</div>
  {/if}
</div>

<style>
  .shape-container {
    position: relative;
    flex: 1;
  }

  .shape-header {
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

  .view-toggle {
    display: flex;
    gap: 2px;
  }

  .toggle-btn {
    height: 22px;
    padding: 0 8px;
    border-radius: var(--radius-sm);
    font-size: 10px;
    font-weight: 500;
    color: var(--text-muted);
    cursor: pointer;
    transition: background 0.1s, color 0.1s;
  }

  .toggle-btn:hover {
    background: var(--bg-surface-hover);
    color: var(--text-secondary);
  }

  .toggle-btn.active {
    background: var(--bg-inset);
    color: var(--text-primary);
  }

  .bar-chart {
    display: flex;
    flex-direction: column;
    gap: 6px;
  }

  .bar-row {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 2px 4px;
    border-radius: var(--radius-sm);
    transition: background 0.1s;
  }

  .bar-row.clickable {
    cursor: pointer;
  }

  .bar-row.clickable:hover {
    background: var(--bg-surface-hover);
  }

  .bar-label {
    width: 48px;
    font-size: 10px;
    color: var(--text-muted);
    text-align: right;
    flex-shrink: 0;
  }

  .bar-track {
    flex: 1;
    height: 16px;
    background: var(--bg-inset);
    border-radius: var(--radius-sm);
    overflow: hidden;
  }

  .bar-fill {
    height: 100%;
    background: var(--accent-blue, #3b82f6);
    border-radius: var(--radius-sm);
    min-width: 2px;
  }

  .bar-count {
    width: 32px;
    font-size: 10px;
    color: var(--text-secondary);
    text-align: right;
    flex-shrink: 0;
  }

  .shape-footer {
    margin-top: 8px;
    font-size: 10px;
    color: var(--text-muted);
    text-align: right;
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
