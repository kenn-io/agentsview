<script lang="ts">
  import OptionTypeahead, {
    type TypeaheadOption,
  } from "../layout/OptionTypeahead.svelte";
  import { usage } from "../../stores/usage.svelte.js";
  import { m } from "../../i18n/index.js";
  import type { UsagePairwiseDimension } from "../../api/types/usage.js";

  function fmtCost(value: number): string {
    return `$${value.toFixed(2)}`;
  }

  function fmtSignedCost(value: number): string {
    return `${value >= 0 ? "+" : "-"}$${Math.abs(value).toFixed(2)}`;
  }

  function fmtCount(value: number): string {
    return String(value);
  }

  function fmtSignedCount(value: number): string {
    return value >= 0 ? `+${value}` : String(value);
  }

  function fmtTokens(value: number): string {
    if (value >= 1_000_000_000) {
      return `${Math.floor(value / 100_000_000) / 10}B`;
    }
    if (value >= 1_000_000) {
      return `${Math.floor(value / 100_000) / 10}M`;
    }
    if (value >= 1_000) {
      return `${Math.floor(value / 100) / 10}K`;
    }
    return String(value);
  }

  function fmtSignedTokens(value: number): string {
    const prefix = value >= 0 ? "+" : "-";
    return `${prefix}${fmtTokens(Math.abs(value))}`;
  }

  function fmtRatio(value: number | null | undefined): string {
    if (value == null) return m.shared_none();
    const prefix = value >= 0 ? "+" : "";
    return `${prefix}${(value * 100).toFixed(1)}%`;
  }

  function fmtMaybeCost(value: number | null | undefined): string {
    if (value == null) return m.shared_none();
    return fmtCost(value);
  }

  function fmtMaybeTokens(value: number | null | undefined): string {
    if (value == null) return m.shared_none();
    return fmtTokens(value);
  }

  function fmtMaybeSignedCost(value: number | null | undefined): string {
    if (value == null) return m.shared_none();
    return fmtSignedCost(value);
  }

  function fmtMaybeSignedTokens(value: number | null | undefined): string {
    if (value == null) return m.shared_none();
    return fmtSignedTokens(value);
  }

  function optionsFor(dimension: UsagePairwiseDimension): string[] {
    return dimension === "project"
      ? usage.pairwiseProjectOptions
      : usage.pairwiseModelOptions;
  }

  function typeaheadOptionsFor(
    dimension: UsagePairwiseDimension,
  ): TypeaheadOption[] {
    return optionsFor(dimension).map((option) => ({
      name: option,
      label: option,
    }));
  }

  function dimensionLabel(dimension: UsagePairwiseDimension): string {
    return dimension === "project"
      ? m.usage_project()
      : m.usage_model();
  }

  const dimensionOptions: TypeaheadOption[] = $derived([
    {
      name: "model",
      label: m.usage_model(),
    },
    {
      name: "project",
      label: m.usage_project(),
    },
  ]);

  type MetricRow = {
    label: string;
    left: string;
    right: string;
    delta: string;
    ratio: string;
  };

  const hasSelection = $derived(
    !!usage.pairwiseSelection.left.value &&
      !!usage.pairwiseSelection.right.value,
  );

  const rows = $derived.by((): MetricRow[] => {
    const comparison = usage.pairwiseComparison;
    if (!comparison) return [];
    return [
      {
        label: m.usage_total_cost(),
        left: fmtCost(comparison.left.totalCost),
        right: fmtCost(comparison.right.totalCost),
        delta: fmtSignedCost(comparison.deltas.totalCostDelta),
        ratio: fmtRatio(comparison.deltas.totalCostDeltaRatio),
      },
      {
        label: m.analytics_col_sessions(),
        left: fmtCount(comparison.left.sessionCount),
        right: fmtCount(comparison.right.sessionCount),
        delta: fmtSignedCount(comparison.deltas.sessionCountDelta),
        ratio: fmtRatio(comparison.deltas.sessionCountDeltaRatio),
      },
      {
        label: m.usage_pairwise_cost_per_session(),
        left: fmtMaybeCost(comparison.left.costPerSession),
        right: fmtMaybeCost(comparison.right.costPerSession),
        delta: fmtMaybeSignedCost(comparison.deltas.costPerSessionDelta),
        ratio: fmtRatio(comparison.deltas.costPerSessionRatio),
      },
      {
        label: m.usage_pairwise_total_tokens(),
        left: fmtTokens(comparison.left.totalTokens),
        right: fmtTokens(comparison.right.totalTokens),
        delta: fmtSignedTokens(comparison.deltas.totalTokensDelta),
        ratio: fmtRatio(comparison.deltas.totalTokensDeltaRatio),
      },
      {
        label: m.usage_pairwise_tokens_per_session(),
        left: fmtMaybeTokens(comparison.left.tokensPerSession),
        right: fmtMaybeTokens(comparison.right.tokensPerSession),
        delta: fmtMaybeSignedTokens(comparison.deltas.tokensPerSessionDelta),
        ratio: fmtRatio(comparison.deltas.tokensPerSessionRatio),
      },
      {
        label: m.usage_input_tokens(),
        left: fmtTokens(comparison.left.inputTokens),
        right: fmtTokens(comparison.right.inputTokens),
        delta: fmtSignedTokens(comparison.deltas.inputTokensDelta),
        ratio: fmtRatio(comparison.deltas.inputTokensDeltaRatio),
      },
      {
        label: m.analytics_metric_output_tokens(),
        left: fmtTokens(comparison.left.outputTokens),
        right: fmtTokens(comparison.right.outputTokens),
        delta: fmtSignedTokens(comparison.deltas.outputTokensDelta),
        ratio: fmtRatio(comparison.deltas.outputTokensDeltaRatio),
      },
    ];
  });
</script>

<section class="pairwise-panel">
  <div class="panel-header">
    <div>
      <h2>{m.usage_pairwise_title()}</h2>
      <p>{m.usage_pairwise_subtitle()}</p>
    </div>
  </div>

  <div class="selectors">
    <div class="side">
      <span class="side-label">{m.usage_pairwise_left()}</span>
      <div class="side-controls">
        <label>
          <span>{m.usage_pairwise_dimension()}</span>
          <div class="pairwise-typeahead">
            <OptionTypeahead
              options={dimensionOptions}
              value={usage.pairwiseSelection.left.dimension}
              fallbackLabel={dimensionLabel(usage.pairwiseSelection.left.dimension)}
              placeholder={m.usage_pairwise_left_dimension()}
              title={m.usage_pairwise_left_dimension()}
              emptyLabel={m.usage_pairwise_no_matching_dimensions()}
              onselect={(value) =>
                usage.setPairwiseSide("left", {
                  dimension: value as UsagePairwiseDimension,
                })}
            />
          </div>
        </label>

        <label>
          <span>{m.usage_pairwise_value()}</span>
          <div class="pairwise-typeahead">
            <OptionTypeahead
              options={typeaheadOptionsFor(usage.pairwiseSelection.left.dimension)}
              value={usage.pairwiseSelection.left.value}
              fallbackLabel={usage.pairwiseSelection.left.value || m.usage_pairwise_select_value()}
              placeholder={m.usage_pairwise_left_value()}
              title={m.usage_pairwise_left_value()}
              emptyLabel={m.usage_pairwise_no_matching_values()}
              disabled={optionsFor(usage.pairwiseSelection.left.dimension).length === 0}
              onselect={(value) =>
                usage.setPairwiseSide("left", {
                  value,
                })}
            />
          </div>
        </label>
      </div>
    </div>

    <div class="side">
      <span class="side-label">{m.usage_pairwise_right()}</span>
      <div class="side-controls">
        <label>
          <span>{m.usage_pairwise_dimension()}</span>
          <div class="pairwise-typeahead">
            <OptionTypeahead
              options={dimensionOptions}
              value={usage.pairwiseSelection.right.dimension}
              fallbackLabel={dimensionLabel(usage.pairwiseSelection.right.dimension)}
              placeholder={m.usage_pairwise_right_dimension()}
              title={m.usage_pairwise_right_dimension()}
              emptyLabel={m.usage_pairwise_no_matching_dimensions()}
              onselect={(value) =>
                usage.setPairwiseSide("right", {
                  dimension: value as UsagePairwiseDimension,
                })}
            />
          </div>
        </label>

        <label>
          <span>{m.usage_pairwise_value()}</span>
          <div class="pairwise-typeahead">
            <OptionTypeahead
              options={typeaheadOptionsFor(usage.pairwiseSelection.right.dimension)}
              value={usage.pairwiseSelection.right.value}
              fallbackLabel={usage.pairwiseSelection.right.value || m.usage_pairwise_select_value()}
              placeholder={m.usage_pairwise_right_value()}
              title={m.usage_pairwise_right_value()}
              emptyLabel={m.usage_pairwise_no_matching_values()}
              disabled={optionsFor(usage.pairwiseSelection.right.dimension).length === 0}
              onselect={(value) =>
                usage.setPairwiseSide("right", {
                  value,
                })}
            />
          </div>
        </label>
      </div>
    </div>
  </div>

  {#if usage.errors.pairwise}
    <div class="error-bar">
      <span>{usage.errors.pairwise}</span>
      <button class="retry-btn" onclick={() => usage.fetchAll()}>
        {m.shared_retry()}
      </button>
    </div>
  {:else if !hasSelection}
    <div class="empty-state">
      {m.usage_pairwise_not_enough_data()}
    </div>
  {:else if usage.loading.pairwise && rows.length === 0}
    <div class="empty-state">{m.shared_refresh()}</div>
  {:else if rows.length > 0}
    <div class="table-wrap">
      <table>
        <thead>
          <tr>
            <th>{m.usage_pairwise_metric()}</th>
            <th>
              {m.usage_pairwise_side_heading({
                dimension: dimensionLabel(usage.pairwiseSelection.left.dimension),
                value: usage.pairwiseSelection.left.value,
              })}
            </th>
            <th>
              {m.usage_pairwise_side_heading({
                dimension: dimensionLabel(usage.pairwiseSelection.right.dimension),
                value: usage.pairwiseSelection.right.value,
              })}
            </th>
            <th>{m.usage_pairwise_delta()}</th>
          </tr>
        </thead>
        <tbody>
          {#each rows as row}
            <tr>
              <th>{row.label}</th>
              <td>{row.left}</td>
              <td>{row.right}</td>
              <td>
                <div class="delta-cell">
                  <span>{row.delta}</span>
                  <span class="ratio">{row.ratio}</span>
                </div>
              </td>
            </tr>
          {/each}
        </tbody>
      </table>
    </div>
  {/if}
</section>

<style>
  .pairwise-panel {
    display: flex;
    flex-direction: column;
    gap: 12px;
  }

  .panel-header h2 {
    font-size: 14px;
    font-weight: 600;
    color: var(--text-primary);
  }

  .panel-header p {
    margin-top: 4px;
    font-size: 12px;
    color: var(--text-muted);
  }

  .selectors {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: 12px;
  }

  .side {
    padding: 12px;
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-md);
    background: color-mix(in srgb, var(--bg-surface) 92%, transparent);
  }

  .side-label {
    display: block;
    margin-bottom: 10px;
    font-size: 11px;
    font-weight: 600;
    color: var(--text-muted);
    text-transform: uppercase;
    letter-spacing: 0.04em;
  }

  .side-controls {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: 10px;
  }

  label {
    display: flex;
    flex-direction: column;
    gap: 6px;
    font-size: 11px;
    color: var(--text-muted);
  }

  .pairwise-typeahead {
    min-width: 0;
  }

  .pairwise-typeahead :global(.typeahead) {
    --typeahead-min-width: 100%;
    --typeahead-max-width: 100%;
  }

  .table-wrap {
    overflow-x: auto;
  }

  table {
    width: 100%;
    border-collapse: collapse;
    font-size: 12px;
  }

  th,
  td {
    padding: 10px 0;
    border-top: 1px solid var(--border-muted);
    text-align: left;
    vertical-align: top;
  }

  thead th {
    border-top: 0;
    padding-top: 0;
    color: var(--text-muted);
    font-size: 11px;
    font-weight: 600;
  }

  tbody th {
    color: var(--text-secondary);
    font-weight: 500;
  }

  .delta-cell {
    display: flex;
    flex-direction: column;
    gap: 2px;
  }

  .ratio {
    color: var(--text-muted);
    font-size: 11px;
  }

  .empty-state,
  .error-bar {
    padding: 12px;
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-md);
    background: var(--bg-surface);
    color: var(--text-muted);
    font-size: 12px;
  }

  .error-bar {
    display: flex;
    align-items: center;
    gap: 8px;
    border-color: var(--accent-red);
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

  @media (max-width: 800px) {
    .selectors,
    .side-controls {
      grid-template-columns: 1fr;
    }
  }
</style>
