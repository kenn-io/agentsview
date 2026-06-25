<script lang="ts">
  import { m } from "../../i18n/index.js";
  import type { TrendsSeries } from "../../api/types.js";

  interface Props {
    series: TrendsSeries[];
    colorFor: (term: string, index: number) => string;
    activeTerm: string | null;
    normalized: boolean;
    messageCount: number;
    onHover: (term: string | null) => void;
  }

  let {
    series,
    colorFor,
    activeTerm,
    normalized,
    messageCount,
    onHover,
  }: Props = $props();

  function displayTotal(item: TrendsSeries): number {
    if (!normalized) return item.total;
    if (messageCount <= 0) return 0;
    return (item.total / messageCount) * 1000;
  }

  function formatMetric(value: number): string {
    if (!normalized) return Math.round(value).toLocaleString();
    return value.toLocaleString(undefined, {
      maximumFractionDigits: value < 10 ? 2 : 1,
    });
  }
</script>

<div class="term-table-wrap">
  <table class="term-table">
    <thead>
      <tr>
        <th>{m.trends_term()}</th>
        <th class="count-col">{normalized ? m.trends_per1k_messages() : m.trends_count()}</th>
      </tr>
    </thead>
    <tbody>
      {#each series as item, index}
        <tr
          class:dimmed={activeTerm !== null && activeTerm !== item.term}
          onmouseenter={() => onHover(item.term)}
          onmouseleave={() => onHover(null)}
        >
          <td>
            <div class="term-name">
              <span
                class="swatch"
                style:background={colorFor(item.term, index)}
              ></span>
              <span>{item.term}</span>
            </div>
            {#if item.variants.length > 1}
              <div class="variants">
                {item.variants.join(" | ")}
              </div>
            {/if}
          </td>
          <td class="count-col">{formatMetric(displayTotal(item))}</td>
        </tr>
      {/each}
    </tbody>
  </table>
</div>

<style>
  .term-table-wrap {
    overflow: auto;
    border: 1px solid var(--border-default);
    border-radius: 8px;
    background: var(--bg-surface);
  }

  .term-table {
    width: 100%;
    border-collapse: collapse;
    font-size: 12px;
  }

  th {
    padding: 9px 10px;
    color: var(--text-muted);
    font-weight: 600;
    text-align: left;
    border-bottom: 1px solid var(--border-muted);
  }

  td {
    padding: 10px;
    border-bottom: 1px solid var(--border-muted);
    vertical-align: top;
  }

  tr:last-child td {
    border-bottom: 0;
  }

  tbody tr {
    transition: opacity 120ms ease, background 120ms ease;
  }

  tbody tr:hover {
    background: var(--bg-hover);
  }

  tr.dimmed {
    opacity: 0.45;
  }

  .term-name {
    display: flex;
    align-items: center;
    gap: 8px;
    min-width: 0;
    color: var(--text-primary);
    font-weight: 600;
  }

  .swatch {
    width: 9px;
    height: 9px;
    border-radius: 999px;
    flex: 0 0 auto;
  }

  .variants {
    margin-top: 3px;
    padding-left: 17px;
    color: var(--text-muted);
    font-size: 11px;
    line-height: 1.35;
    word-break: break-word;
  }

  .count-col {
    width: 96px;
    text-align: right;
    white-space: nowrap;
    font-variant-numeric: tabular-nums;
  }
</style>
