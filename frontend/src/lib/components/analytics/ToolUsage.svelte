<script lang="ts">
  import { onDestroy, untrack } from "svelte";
  import { Button, Tooltip } from "@kenn-io/kit-ui";
  import { AnalyticsService, type DbToolUsageAnalysis as ToolUsageAnalysis, type DbSignalSessionExample as SignalSessionExample } from "../../api/generated/index.js";
  import { agentLabel } from "../../utils/agents.js";
  import { LatestRead } from "../../utils/latest-read.js";
  import { isAbortError } from "../../api/runtime.js";
  import { router } from "../../stores/router.svelte.js";
  import { ui } from "../../stores/ui.svelte.js";
  import { analytics } from "../../stores/analytics.svelte.js";
  import type { DbToolCategoryCount as ToolCategoryCount } from "../../api/generated/index.js";
  import { getLocale, m } from "../../i18n/index.js";

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
  const toolRows = $derived(analytics.tools?.by_tool ?? []);

  let expanded = $state(false);
  let selectedRate = $state<{ tool: string; category: string; metric: string } | null>(null);
  let evidence = $state<SignalSessionExample[]>([]);
  let evidenceTotal = $state<number | undefined>();
  let nextOffset = $state<number | null>(null);
  let evidenceLoading = $state(false);
  let evidenceError = $state<string | null>(null);
  const evidenceRead = new LatestRead();
  const evidenceParams = $derived(analytics.filterParams());
  const evidenceInputs = $derived({ params: evidenceParams, rows: toolRows });
  let loadedEvidenceKey: string | null = null;
  let loadedEvidenceRows: ToolUsageAnalysis[] | null = null;

  function metricCount(tool: ToolUsageAnalysis, metric: string): number {
    switch (metric) {
      case "tool_empty_rate": return tool.empty_calls ?? 0;
      case "tool_repeat_rate": return tool.repeated_calls ?? 0;
      default: return tool.recovered_sequences ?? 0;
    }
  }

  function evidenceKeyFor(selection: { tool: string; category: string; metric: string }, inputs: typeof evidenceInputs): string | null {
    const row = inputs.rows.find((tool) => tool.tool_name === selection.tool && tool.category === selection.category);
    return row ? JSON.stringify({ params: inputs.params, count: metricCount(row, selection.metric) }) : null;
  }

  $effect(() => {
    const inputs = evidenceInputs;
    untrack(() => {
      const selection = selectedRate;
      if (!selection) return;
      const key = evidenceKeyFor(selection, inputs);
      if (key === null) {
        evidenceRead.cancel();
        selectedRate = null;
        evidenceLoading = false;
        loadedEvidenceKey = null;
      } else if (key !== loadedEvidenceKey || inputs.rows !== loadedEvidenceRows) {
        void loadEvidence(selection.tool, selection.category, selection.metric);
      }
    });
  });
  onDestroy(() => evidenceRead.cancel());

  async function loadEvidence(tool: string, category: string, metric: string, offset = 0) {
    const signal = evidenceRead.begin();
    selectedRate = { tool, category, metric };
    evidenceLoading = true;
    evidenceError = null;
    if (offset === 0) {
      evidence = []; evidenceTotal = undefined; nextOffset = null;
      loadedEvidenceKey = evidenceKeyFor(selectedRate, evidenceInputs);
      loadedEvidenceRows = toolRows;
    }
    try {
      const response = await AnalyticsService.getApiV1AnalyticsSignalSessions({ ...evidenceParams, signal: metric, tool_name: tool, tool_category: category, limit: 10, offset }, { signal });
      if (!evidenceRead.isCurrent(signal)) return;
      evidence = offset ? [...evidence, ...response.sessions] : response.sessions;
      evidenceTotal = response.total;
      nextOffset = response.next_offset ?? null;
    } catch (error) {
      if (isAbortError(error) || !evidenceRead.isCurrent(signal)) return;
      evidenceError = error instanceof Error ? error.message : m.insights_page_could_not_load_examples();
    } finally {
      if (evidenceRead.finish(signal)) evidenceLoading = false;
    }
  }

  function evidenceParamsFor(example: SignalSessionExample): Record<string, string> {
    return example.message_ordinal == null ? {} : { msg: String(example.message_ordinal) };
  }

  function openSession(event: MouseEvent, example: SignalSessionExample) {
    if (event.ctrlKey || event.metaKey || event.shiftKey || event.altKey) return;
    event.preventDefault();
    router.navigateToSession(example.session_id, evidenceParamsFor(example));
    if (example.message_ordinal != null) ui.scrollToOrdinal(example.message_ordinal, example.session_id);
  }

  function formatCount(value: number): string {
    return value.toLocaleString(getLocale());
  }

  // A nonzero rate below 0.05% keeps one significant digit so it never reads as zero.
  function formatRate(rate: number): string {
    const options: Intl.NumberFormatOptions = rate > 0 && rate < 0.0005
      ? { style: "percent", maximumSignificantDigits: 1 }
      : { style: "percent", minimumFractionDigits: 1, maximumFractionDigits: 1 };
    return new Intl.NumberFormat(getLocale(), options).format(rate);
  }

  function rateTitle(tool: ToolUsageAnalysis, metric: string): string {
    switch (metric) {
      case "tool_empty_rate": return m.analytics_tool_empty_explanation({ count: formatCount(tool.empty_calls ?? 0), total: formatCount(tool.known_outcome_calls ?? 0) });
      case "tool_repeat_rate": return m.analytics_tool_repeat_explanation({ count: formatCount(tool.repeated_calls ?? 0), total: formatCount(tool.analyzed_calls ?? 0) });
      default: return m.analytics_tool_recovery_explanation({ count: formatCount(tool.recovered_sequences ?? 0), total: formatCount((tool.recovered_sequences ?? 0) + (tool.abandoned_sequences ?? 0)), open: formatCount(tool.open_sequences ?? 0), unknown: formatCount(tool.unknown_sequences ?? 0) });
    }
  }

  const rateColumns = $derived([
    { metric: "tool_empty_rate", label: m.analytics_tool_empty_rate(), field: "empty_rate" as const },
    { metric: "tool_repeat_rate", label: m.analytics_tool_repeat_rate(), field: "repeat_rate" as const },
    { metric: "tool_recovery_rate", label: m.analytics_tool_recovery_rate(), field: "recovery_rate" as const },
  ]);

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
      text: `${cat.category}: ${formatCount(cat.count)} (${cat.pct}%)`,
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
      text: m.analytics_tool_usage_trend_tooltip({
        date: entry.date,
        total,
        parts: parts.join(", "),
      }),
    };
  }

  function handleLeave() {
    tooltip = null;
  }
</script>

<div class="tool-container">
  <div class="tool-header">
    <h3 class="chart-title">{m.analytics_tool_usage_title()}</h3>
    {#if analytics.tools}
      <span class="count">
        {m.analytics_tool_usage_call_count({
          count: analytics.tools.total_calls,
          countLabel: formatCount(analytics.tools.total_calls),
        })}
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
        {m.shared_retry()}
      </button>
    </div>
  {:else if analytics.loading.tools}
    <div class="empty">{m.analytics_tool_usage_loading()}</div>
  {:else if categories.length > 0}
    <div class="sections">
      {#if toolRows.length > 0}
        <div class="section">
          <h4 class="section-title">
            {m.analytics_tool_usage_top_tools()}
          </h4>
          <div class="tool-table">
            <div class="tool-row tool-labels">
              <span></span><span>{m.analytics_tool_name()}</span><span>{m.analytics_tool_coverage()}</span><span>{m.analytics_tool_calls()}</span><span>{m.analytics_tool_sessions()}</span><span>{m.analytics_tool_share()}</span>
              {#each rateColumns as column}<span>{column.label}</span>{/each}
            </div>
            {#each expanded ? toolRows : toolRows.slice(0, 8) as tool}
              <div class="tool-row">
                <span
                  class="tool-dot"
                  style="background: {colorFor(tool.category)}"
                ></span>
                <span class="tool-name" title={tool.tool_name}>
                  {tool.tool_name}<small class="tool-kind">{tool.category}</small>
                </span>
                <span class="tool-category"><Tooltip text={m.analytics_tool_coverage_explanation()} focusable>{formatCount(tool.analyzed_calls ?? 0)}/{formatCount(tool.call_count)}</Tooltip></span>
                <span class="tool-count">
                  {formatCount(tool.call_count)}
                </span>
                <span class="tool-sessions">
                  {m.analytics_tool_usage_sessions({
                    count: tool.session_count,
                    countLabel: formatCount(tool.session_count),
                  })}
                </span>
                <span class="tool-pct">{tool.pct}%</span>
                {#each rateColumns as column}
                  <span class="tool-rate" data-label={column.label}>
                    {#if tool[column.field] == null}
                      <Tooltip text={rateTitle(tool, column.metric)} focusable>{m.analytics_tool_unavailable()}</Tooltip>
                    {:else}
                      {@const value = formatRate(tool[column.field]!)}
                      <Tooltip text={rateTitle(tool, column.metric)} focusable={tool[column.field] === 0}>
                        {#if tool[column.field] === 0}
                          {value}
                        {:else}
                          <Button surface="soft" size="sm" onclick={() => loadEvidence(tool.tool_name, tool.category, column.metric)} ariaLabel={m.analytics_tool_rate_button_label({ tool: tool.tool_name, rate: column.label, value })}>
                            {value}
                          </Button>
                        {/if}
                      </Tooltip>
                    {/if}
                  </span>
                {/each}
              </div>
            {/each}
          </div>
          <p class="rate-note">{m.analytics_tool_recovery_note()}</p>
          {#if toolRows.some((tool) => tool.missing_calls > 0)}<p class="rate-note">{m.analytics_tool_history_note()}</p>{/if}
          {#if toolRows.length > 8}<Button surface="soft" size="sm" onclick={() => expanded = !expanded}>{expanded ? m.analytics_tool_show_less() : m.analytics_tool_show_all()}</Button>{/if}
          {#if selectedRate}
            <section class="tool-evidence" aria-live="polite">
              <h4 class="section-title">{selectedRate.tool} ({selectedRate.category}) · {rateColumns.find((c) => c.metric === selectedRate?.metric)?.label}</h4>
              {#if evidenceTotal != null}<p class="rate-note">{m.analytics_tool_evidence_count({ count: evidenceTotal, countLabel: formatCount(evidenceTotal) })}</p>{/if}
              {#if evidenceLoading}<p class="rate-note">{m.insights_page_loading_examples()}</p>{/if}
              {#if evidenceError}<p class="error">{evidenceError}</p><Button surface="soft" size="sm" onclick={() => loadEvidence(selectedRate!.tool, selectedRate!.category, selectedRate!.metric)}>{m.shared_retry()}</Button>{/if}
              <div class="evidence-list">
                {#each evidence as example}
                  {@const project = example.project || m.insights_page_unassigned_project()}
                  <a class="evidence-row" href={router.buildSessionHref(example.session_id, evidenceParamsFor(example))} onclick={(event) => openSession(event, example)}>
                    <span class="evidence-main">
                      <span class="evidence-project" title={project}>{project}</span>
                      <span class="evidence-meta">{agentLabel(example.agent)} · {m.analytics_tool_latest_matching_call({ date: example.date })}</span>
                    </span>
                    <span class="evidence-count">{m.analytics_tool_matching_calls({ count: formatCount(example.signal_total) })}</span>
                  </a>
                {/each}
              </div>
              {#if nextOffset != null}<Button surface="soft" size="sm" disabled={evidenceLoading} onclick={() => loadEvidence(selectedRate!.tool, selectedRate!.category, selectedRate!.metric, nextOffset!)}>{m.analytics_tool_more_sessions()}</Button>{/if}
            </section>
          {/if}
        </div>
      {/if}

      <div class="section">
        <h4 class="section-title">{m.analytics_by_category()}</h4>
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
                {formatCount(cat.count)}
              </span>
              <span class="bar-pct">{cat.pct}%</span>
            </div>
          {/each}
        </div>
      </div>

      {#if trendEntries.length > 1}
        <div class="section">
          <h4 class="section-title">{m.analytics_weekly_trend()}</h4>
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
        <span>{tooltip.text}</span>
      </div>
    {/if}
  {:else}
    <div class="empty">{m.analytics_tool_usage_empty()}</div>
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

  .tool-table {
    display: grid;
    overflow-x: auto;
    grid-template-columns: minmax(720px, 1fr);
    gap: 2px;
  }

  .tool-row {
    display: grid;
    grid-template-columns: 8px minmax(80px, 1fr) 64px 48px 72px 40px repeat(3, 76px);
    align-items: center;
    gap: 8px;
    min-height: 28px;
    padding: 4px;
    border-radius: var(--radius-sm);
    color: var(--text-secondary);
    font-size: 11px;
  }

  .tool-row:hover {
    background: var(--bg-surface-hover);
  }

  .tool-dot {
    width: 8px;
    height: 8px;
    border-radius: 50%;
  }

  .tool-kind { display: block; font-size: 10px; color: var(--text-muted); }

  .tool-name {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    color: var(--text-primary);
    font-weight: 500;
  }

  .tool-category {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    color: var(--text-muted);
  }

  .tool-count,
  .tool-sessions,
  .tool-pct {
    text-align: right;
    font-family: var(--font-mono);
    color: var(--text-muted);
  }

  .tool-labels { font-size: 10px; color: var(--text-muted); }
  .tool-rate { text-align: right; font-family: var(--font-mono); font-size: 10px; }
  .rate-note { font-size: 11px; color: var(--text-muted); margin-top: 8px; }
  .tool-evidence { margin-top: 12px; }
  .evidence-list { display: grid; gap: 2px; margin: 6px 0 8px; }
  .evidence-row {
    display: grid;
    grid-template-columns: minmax(0, 1fr) auto;
    align-items: center;
    gap: 12px;
    padding: 6px 4px;
    border-radius: var(--radius-sm);
    color: inherit;
    text-decoration: none;
  }
  .evidence-row:hover { background: var(--bg-surface-hover); }
  .evidence-row:hover .evidence-project { color: var(--accent-blue); }
  .evidence-main { display: grid; gap: 2px; min-width: 0; }
  .evidence-project {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    color: var(--text-primary);
    font-size: 12px;
    font-weight: 500;
  }
  .evidence-meta { color: var(--text-muted); font-size: 11px; font-variant-numeric: tabular-nums; }
  .evidence-count {
    white-space: nowrap;
    text-align: right;
    font-family: var(--font-mono);
    font-size: 11px;
    color: var(--text-secondary);
  }

  .bar-list {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
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
    gap: var(--space-2);
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
    z-index: var(--z-tooltip);
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
  @media (max-width: 640px) {
    .evidence-row { grid-template-columns: minmax(0, 1fr); gap: 2px; }
    .evidence-count { text-align: left; }
    .tool-table { display: block; overflow: visible; }
    .tool-labels { display: none; }
    .tool-row { grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 8px; padding: 10px 4px; }
    .tool-dot, .tool-count, .tool-sessions, .tool-pct { display: none; }
    .tool-name { grid-column: 1 / 3; }
    .tool-category { text-align: right; }
    .tool-rate { grid-column: auto; display: flex; flex-direction: column; gap: 4px; font-family: inherit; align-items: center; }
    .tool-rate::before { content: attr(data-label); font-size: 10px; }
    .tool-row > .tool-rate:nth-of-type(7) { grid-column: 1; }
    .tool-rate { min-width: 0; }
  }
</style>
