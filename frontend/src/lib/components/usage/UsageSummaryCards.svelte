<script lang="ts">
  import { Card } from "@kenn-io/kit-ui";
  import { usage } from "../../stores/usage.svelte.js";
  import { m } from "../../i18n/index.js";

  function fmtCost(v: number): string {
    return `$${v.toFixed(2)}`;
  }

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

  function fmtPct(v: number): string {
    return `${(v * 100).toFixed(1)}%`;
  }

  const inputTokens = $derived(
    usage.summary?.totals.inputTokens ?? 0,
  );

  const outputTokens = $derived(
    usage.summary?.totals.outputTokens ?? 0,
  );

  // "cached" here means input tokens that were actually
  // served from cache, i.e. cacheReadTokens. Cache-creation
  // tokens are cache writes — fresh input paying the
  // cache-write surcharge rather than being replayed from
  // cache — so folding them in would overstate cache usage
  // on workloads that only warm the cache.
  const cachedTokens = $derived(
    usage.summary?.totals.cacheReadTokens ?? 0,
  );

  const dailyBurn = $derived.by(() => {
    const s = usage.summary;
    if (!s || !s.daily || s.daily.length === 0) return 0;
    return s.totals.totalCost / s.daily.length;
  });

  const peak = $derived.by(() => {
    const s = usage.summary;
    if (!s || !s.daily || s.daily.length === 0) {
      return { date: "", cost: 0 };
    }
    let best = s.daily[0]!;
    for (const d of s.daily) {
      if (d.totalCost > best.totalCost) best = d;
    }
    return { date: best.date, cost: best.totalCost };
  });

  const activeDays = $derived(
    usage.summary?.daily?.filter(
      (d) => d.totalCost > 0,
    ).length ?? 0,
  );

  const vsPrior = $derived.by(() => {
    const c = usage.summary?.comparison;
    if (!c) return null;
    const sign = c.deltaPct >= 0 ? "+" : "";
    return m.usage_summary_vs_prior({
      value: `${sign}${(c.deltaPct * 100).toFixed(0)}%`,
    });
  });

  interface Card {
    label: () => string;
    value: () => string;
    sub?: () => string;
    featured?: boolean;
  }

  const cards = $derived.by(() => {
    const baseCards: Card[] = [
      {
        label: () => m.usage_summary_total_cost(),
        value: () => fmtCost(usage.summary?.totals.totalCost ?? 0),
        sub: () => vsPrior ?? "",
        featured: true,
      },
      {
        label: () => m.usage_summary_input_tokens(),
        value: () => fmtTokens(inputTokens),
        sub: () =>
          cachedTokens > 0
            ? m.usage_summary_cached_tokens({
                value: `+${fmtTokens(cachedTokens)}`,
              })
            : "",
      },
      {
        label: () => m.analytics_metric_output_tokens(),
        value: () => fmtTokens(outputTokens),
      },
      {
        label: () => m.usage_summary_daily_burn(),
        value: () => fmtCost(dailyBurn),
        sub: () => m.usage_summary_avg_day(),
      },
      {
        label: () => m.usage_summary_peak_day(),
        value: () => fmtCost(peak.cost),
        sub: () => peak.date,
      },
      {
        label: () => m.usage_summary_cache_hit(),
        value: () =>
          fmtPct(usage.summary?.cacheStats.hitRate ?? 0),
      },
      {
        label: () => m.analytics_summary_projects(),
        value: () =>
          String(
            Object.keys(
              usage.summary?.sessionCounts.byProject ?? {},
            ).length,
          ),
      },
      {
        label: () => m.usage_models(),
        value: () =>
          String(usage.summary?.modelTotals.length ?? 0),
      },
      {
        label: () => m.analytics_summary_active_days(),
        value: () => String(activeDays),
      },
    ];
    return baseCards;
  });
</script>

<div class="summary-cards">
  {#each cards as card}
    <Card
      level="default"
      padding="none"
      class={card.featured ? "card featured" : "card"}
    >
      {#if usage.errors.summary}
        <span class="card-value error">--</span>
        <span class="card-label">{card.label()}</span>
      {:else}
        <span class="card-value">{card.value()}</span>
        <span class="card-label">{card.label()}</span>
        {#if card.sub}
          {@const subtext = card.sub()}
          {#if subtext}
            <span class="card-sub">{subtext}</span>
          {/if}
        {/if}
      {/if}
    </Card>
  {/each}
</div>

{#if usage.errors.summary}
  <div class="error-bar">
    <span>{usage.errors.summary}</span>
    <button
      class="retry-btn"
      onclick={() => usage.fetchSummary()}
    >
      {m.shared_retry()}
    </button>
  </div>
{/if}

<style>
  .summary-cards {
    display: flex;
    gap: 8px;
    flex-wrap: wrap;
  }

  .summary-cards :global(.card) {
    flex: 1;
    min-width: 120px;
    padding: 12px;
    display: flex;
    flex-direction: column;
    gap: 2px;
  }

  .summary-cards :global(.card > .kit-card__body) {
    display: contents;
  }

  .summary-cards :global(.card.featured) {
    border-width: 2px;
    border-color: var(--accent-blue);
  }

  .card-value {
    font-size: 20px;
    font-weight: 600;
    color: var(--text-primary);
    line-height: 1.2;
  }

  .card-value.error {
    color: var(--text-muted);
  }

  .card-label {
    font-size: 11px;
    color: var(--text-muted);
    font-weight: 500;
  }

  .card-sub {
    font-size: 10px;
    color: var(--text-muted);
    margin-top: 2px;
  }

  .error-bar {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 8px 12px;
    background: var(--bg-surface);
    border: 1px solid var(--accent-red);
    border-radius: var(--radius-sm);
    font-size: 11px;
    color: var(--accent-red);
  }

  .retry-btn {
    padding: 2px 8px;
    border: 1px solid var(--accent-red);
    border-radius: var(--radius-sm);
    font-size: 11px;
    color: var(--accent-red);
    cursor: pointer;
  }

  .retry-btn:hover {
    background: var(--accent-red);
    color: var(--accent-red-foreground);
  }
</style>
