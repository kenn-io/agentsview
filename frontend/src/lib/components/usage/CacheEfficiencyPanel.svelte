<script lang="ts">
  import { usage } from "../../stores/usage.svelte.js";
  import { savingsState } from "../../utils/usageSavings.js";

  function fmtTokens(v: number): string {
    if (v >= 1_000_000_000) {
      const g = Math.floor(v / 100_000_000) / 10;
      return `${g}B`;
    }
    if (v >= 1_000_000) {
      const m = Math.floor(v / 100_000) / 10;
      return `${m}M`;
    }
    if (v >= 1_000) {
      const k = Math.floor(v / 100) / 10;
      return `${k}K`;
    }
    return String(v);
  }

  function fmtCost(v: number): string {
    return `$${v.toFixed(2)}`;
  }

  interface Bar {
    label: string;
    value: number;
    pct: number;
    color: string;
  }

  const bars = $derived.by((): Bar[] => {
    const cs = usage.summary?.cacheStats;
    if (!cs) return [];
    const total =
      cs.cacheReadTokens +
      cs.cacheCreationTokens +
      cs.uncachedInputTokens +
      cs.outputTokens;
    if (total === 0) return [];
    return [
      {
        label: "Cache Reads",
        value: cs.cacheReadTokens,
        pct: cs.cacheReadTokens / total,
        color: "var(--accent-green)",
      },
      {
        label: "Cache Writes",
        value: cs.cacheCreationTokens,
        pct: cs.cacheCreationTokens / total,
        color: "var(--accent-teal)",
      },
      {
        label: "Uncached Input",
        value: cs.uncachedInputTokens,
        pct: cs.uncachedInputTokens / total,
        color: "var(--accent-amber)",
      },
      {
        label: "Output",
        value: cs.outputTokens,
        pct: cs.outputTokens / total,
        color: "var(--accent-blue)",
      },
    ];
  });

  const savings = $derived(
    usage.summary?.cacheStats?.savingsVsUncached ?? 0,
  );
  const savingsLabel = $derived(savingsState(savings));
</script>

<div class="cache-panel">
  <h3 class="chart-title">Cache Efficiency</h3>

  {#if bars.length === 0}
    <div class="empty">No token data</div>
  {:else}
    <div class="bar-list">
      {#each bars as bar}
        <div class="bar-row">
          <span class="bar-label">{bar.label}</span>
          <div class="bar-track">
            <div
              class="bar-fill"
              style="width: {Math.max(bar.pct * 100, 1)}%;
                     background: {bar.color};"
            ></div>
          </div>
          <span class="bar-value">
            {fmtTokens(bar.value)}
          </span>
        </div>
      {/each}
    </div>

    {#if savingsLabel === "saved"}
      <div class="savings-callout saved">
        {fmtCost(savings)} saved vs uncached
      </div>
    {:else if savingsLabel === "costlier"}
      <div class="savings-callout costlier">
        {fmtCost(Math.abs(savings))} more than uncached
      </div>
    {/if}
  {/if}
</div>

<style>
  .cache-panel {
    flex: 1;
    display: flex;
    flex-direction: column;
  }

  .chart-title {
    font-size: 12px;
    font-weight: 600;
    color: var(--text-primary);
    margin-bottom: 8px;
  }

  .bar-list {
    display: flex;
    flex-direction: column;
    gap: 8px;
  }

  .bar-row {
    display: flex;
    align-items: center;
    gap: 8px;
  }

  .bar-label {
    flex-shrink: 0;
    width: 100px;
    font-size: 11px;
    color: var(--text-secondary);
  }

  .bar-track {
    flex: 1;
    height: 12px;
    background: var(--bg-inset);
    border-radius: var(--radius-sm);
    overflow: hidden;
  }

  .bar-fill {
    height: 100%;
    border-radius: var(--radius-sm);
    transition: width 0.3s ease;
  }

  .bar-value {
    flex-shrink: 0;
    min-width: 48px;
    text-align: right;
    font-size: 10px;
    font-family: var(--font-mono);
    color: var(--text-muted);
  }

  .savings-callout {
    margin-top: 12px;
    padding: 6px 10px;
    border-radius: var(--radius-sm);
    font-size: 11px;
    font-weight: 500;
  }

  .savings-callout.saved {
    background: color-mix(
      in srgb, var(--accent-green) 10%, transparent
    );
    color: var(--accent-green);
  }

  .savings-callout.costlier {
    background: color-mix(
      in srgb, var(--accent-amber) 12%, transparent
    );
    color: var(--accent-amber);
  }

  .empty {
    color: var(--text-muted);
    font-size: 12px;
    padding: 24px;
    text-align: center;
  }
</style>
