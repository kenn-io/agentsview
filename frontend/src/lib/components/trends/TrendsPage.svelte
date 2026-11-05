<script lang="ts">
  import { errorTelemetry } from "../../utils/error-telemetry.js";
  import { m } from "../../i18n/index.js";
  import { onMount } from "svelte";
  import { trends } from "../../stores/trends.svelte.js";
  import { settings } from "../../stores/settings.svelte.js";
  import { getBasePath } from "../../stores/router.svelte.js";
  import { sync } from "../../stores/sync.svelte.js";
  import {
    yokedDates,
    panelDateState,
    rangeToPanelDate,
    type PanelDateState,
  } from "../../stores/yokedDates.svelte.js";
  import { rollingRange } from "../../utils/dates.js";
  import type { SeriesStyle } from "@kenn-io/kit-ui/utils/palette";
  import { chartSeriesStyleMap } from "../../utils/chartPalette.js";
  import type { TrendsGranularity } from "../../api/types.js";
  import { Button, SegmentedControl, Spinner, Toggle } from "@kenn-io/kit-ui";
  import RangePicker from "../shared/RangePicker.svelte";
  import {
    resolveRange,
    selectionFromRange,
    type RangeSelection,
  } from "../shared/rangeSelection.js";
  import TermTable from "./TermTable.svelte";
  import TrendsLineChart from "./TrendsLineChart.svelte";

  const TREND_PALETTE = [
    "var(--trend-blue)",
    "var(--trend-gold)",
    "var(--trend-purple)",
    "var(--trend-green)",
    "var(--trend-magenta)",
    "var(--trend-slate)",
    "var(--trend-red)",
    "var(--trend-cyan)",
    "var(--trend-brown)",
    "var(--trend-lime)",
    "var(--trend-indigo)",
    "var(--trend-black)",
  ] as const;
  const TREND_WINDOW_PARAM = "window_days";
  const DEFAULT_TREND_WINDOW_DAYS = 365;

  let activeTerm: string | null = $state(null);
  let trendsWindowDays: number | null = $state(DEFAULT_TREND_WINDOW_DAYS);
  // Keep bare defaults out of history so a later mount cannot mistake them
  // for a user selection, deep link, or shared seed.
  let trendsDateIntentEstablished = false;
  const trendsPanelDate = $derived(currentTrendsPanelDate());

  const granularityOptions = $derived([
    { value: "day", label: m.analytics_granularity_day() },
    { value: "week", label: m.analytics_granularity_week() },
    { value: "month", label: m.analytics_granularity_month() },
  ]);

  function pickGranularity(value: string) {
    if (isGranularity(value) && value !== trends.granularity) {
      void setGranularity(value);
    }
  }

  const metricLabel = $derived(
    trends.normalized ? m.trends_occurrences_per1k() : m.trends_occurrences(),
  );

  const termStyleMap = $derived(chartSeriesStyleMap(
    (trends.response?.series ?? []).map((item) => item.term),
    settings.chartPalette,
    TREND_PALETTE,
  ));

  function styleFor(term: string, index: number): SeriesStyle {
    return termStyleMap.get(term) ?? {
      color: TREND_PALETTE[index % TREND_PALETTE.length]!,
      fill: "solid",
      dash: "solid",
      marker: "circle",
    };
  }

  function isGranularity(value: string | null): value is TrendsGranularity {
    return value === "day" || value === "week" || value === "month";
  }

  function parseTrendWindowDays(raw: string | null): number | null {
    if (!raw) return null;
    const n = Number.parseInt(raw, 10);
    if (!Number.isInteger(n) || n <= 0 || String(n) !== raw) {
      return null;
    }
    return n;
  }

  function applyQueryParams(): boolean {
    const q = new URLSearchParams(window.location.search);
    const from = q.get("from");
    const to = q.get("to");
    const windowDays = parseTrendWindowDays(q.get(TREND_WINDOW_PARAM));
    const granularity = q.get("granularity");
    const normalized = q.get("normalized");
    const terms = q.getAll("term").map((s) => s.trim()).filter(Boolean);
    if (windowDays !== null) {
      const range = rollingRange(windowDays);
      trends.from = range.from;
      trends.to = range.to;
      trendsWindowDays = windowDays;
    } else if (from || to) {
      if (from) trends.from = from;
      if (to) trends.to = to;
      trendsWindowDays = null;
    }
    if (isGranularity(granularity)) trends.granularity = granularity;
    trends.normalized = normalized === "true";
    if (terms.length > 0) trends.termText = terms.join("\n");
    return windowDays !== null || q.has("from") || q.has("to");
  }

  function writeUrl() {
    const q = new URLSearchParams();
    const current = new URLSearchParams(window.location.search);
    if (current.has("desktop")) {
      q.set("desktop", current.get("desktop") ?? "");
    }
    if (trendsDateIntentEstablished) {
      q.set("from", trends.from);
      q.set("to", trends.to);
      if (trendsWindowDays !== null) {
        q.set(TREND_WINDOW_PARAM, String(trendsWindowDays));
      }
    }
    q.set("granularity", trends.granularity);
    if (trends.normalized) {
      q.set("normalized", "true");
    }
    for (const term of trends.terms) {
      q.append("term", term);
    }
    const basePath = getBasePath();
    const qs = q.toString();
    const url = `${basePath}/trends${qs ? `?${qs}` : ""}`;
    window.history.replaceState(null, "", url);
  }

  function materializeRollingWindow(): void {
    if (trendsWindowDays === null) return;
    const yokeEstablished = yokedDates.range !== null;
    const range = rollingRange(trendsWindowDays);
    if (trends.from === range.from && trends.to === range.to) return;
    trends.from = range.from;
    trends.to = range.to;
    if (yokeEstablished) {
      updateYokeFromTrends(panelDateState(range.from, range.to, {
        mode: "rolling",
        windowDays: trendsWindowDays,
      }));
    }
  }

  async function refresh() {
    materializeRollingWindow();
    writeUrl();
    await trends.fetchTerms();
  }

  const earliestSession = $derived(sync.stats?.earliest_session ?? null);

  const rangeSelection = $derived.by((): RangeSelection => {
    if (trendsWindowDays !== null) {
      return { mode: "relative", days: trendsWindowDays };
    }
    return selectionFromRange(trends.from, trends.to, earliestSession);
  });

  async function applyRange(sel: RangeSelection) {
    const range = resolveRange(sel, earliestSession);
    trends.from = range.from;
    trends.to = range.to;
    trendsDateIntentEstablished = true;
    const yokeState = yokeStateForSelection(sel, range);
    trendsWindowDays = yokeState?.mode === "rolling"
      ? yokeState.windowDays ?? null
      : null;
    updateYokeFromTrends(yokeState);
    await refresh();
  }

  function yokeStateForSelection(
    sel: RangeSelection,
    range: { from: string; to: string },
  ): PanelDateState | null {
    if (sel.mode === "relative" && sel.days > 0) {
      return panelDateState(range.from, range.to, {
        mode: "rolling",
        windowDays: sel.days,
      });
    }
    return panelDateState(range.from, range.to, { mode: "fixed" });
  }

  async function resetTerms() {
    materializeRollingWindow();
    writeUrl();
    await trends.resetTerms();
    writeUrl();
  }

  async function setGranularity(value: TrendsGranularity) {
    trends.granularity = value;
    await refresh();
  }

  function currentTrendsPanelDate(): PanelDateState | null {
    if (trendsWindowDays !== null) {
      return panelDateState(trends.from, trends.to, {
        mode: "rolling",
        windowDays: trendsWindowDays,
      });
    }
    return panelDateState(trends.from, trends.to, { mode: "fixed" });
  }

  function updateYokeFromTrends(
    state: PanelDateState | null = trendsPanelDate,
  ): void {
    if (state) yokedDates.updateFromPanel(state);
  }

  function seedTrendsYoke(): void {
    const seed = yokedDates.seedForPanel();
    const state = seed ? rangeToPanelDate(seed) : null;
    if (!state) return;
    trendsDateIntentEstablished = true;
    trends.from = state.from;
    trends.to = state.to;
    trendsWindowDays = state.mode === "rolling"
      ? state.windowDays ?? null
      : null;
  }

  onMount(() => {
    const hasDateParams = applyQueryParams();
    trendsDateIntentEstablished = hasDateParams;
    if (hasDateParams) {
      updateYokeFromTrends();
    } else {
      seedTrendsYoke();
    }
    materializeRollingWindow();
    writeUrl();
    trends.fetchTerms();
    return () => {
      trends.cancelInFlightReads();
    };
  });
</script>

<section class="trends-page">
  <div class="trends-toolbar">
    <RangePicker
      selection={rangeSelection}
      busy={trends.loading.terms}
      {earliestSession}
      onSelect={applyRange}
    />
    <div class="toolbar-actions">
      <Button size="sm" label={m.trends_reset()} onclick={resetTerms} />
      <!-- Recompute reruns the term counts: the same button as Reset in
           both themes. -->
      <Button
        size="sm"
        label={trends.loading.terms ? m.trends_refreshing() : m.trends_refresh()}
        disabled={trends.loading.terms}
        onclick={refresh}
      />
    </div>
  </div>

  <div class="trends-content">
    <header class="section-head">
      <h1>{m.trends_title()}</h1>
      <p>{m.trends_date_range({ from: trends.response?.from ?? trends.from, to: trends.response?.to ?? trends.to })}</p>
    </header>

    <div class="content-grid">
      <!-- Terms in, counts out: the input and its table share one panel,
           which never grows taller than the chart beside it; the table
           scrolls on its own. -->
      <section class="panel terms-panel" aria-labelledby="trend-terms-label">
        <label class="panel-head terms-label" for="trend-terms" id="trend-terms-label">
          <span class="panel-title">{m.trends_terms()}</span>
          <span class="terms-hint">{m.trends_one_per_line()}</span>
        </label>
        <div class="terms-input">
          <textarea
            id="trend-terms"
            bind:value={trends.termText}
            rows="6"
            spellcheck="false"
          ></textarea>
          {#if trends.errors.terms}
            <div class="error" {@attach errorTelemetry("analytics")}>{trends.errors.terms}</div>
          {/if}
        </div>
        {#if (trends.response?.series.length ?? 0) > 0}
          <div class="table-panel">
            <TermTable
              series={trends.response?.series ?? []}
              {styleFor}
              {activeTerm}
              normalized={trends.normalized}
              messageCount={trends.response?.message_count ?? 0}
              onHover={(term) => (activeTerm = term)}
            />
          </div>
        {/if}
      </section>

      <section class="panel chart-panel" aria-busy={trends.loading.terms} aria-labelledby="trend-chart-title">
        <div class="panel-head chart-head">
          <h2 class="panel-title" id="trend-chart-title">{metricLabel}</h2>
          <div class="chart-options">
            <div class="group-by">
              <span class="option-label" aria-hidden="true">{m.trends_group_by()}</span>
              <SegmentedControl
                options={granularityOptions}
                value={trends.granularity}
                ariaLabel={m.trends_group_by()}
                onchange={pickGranularity}
              />
            </div>
            <Toggle
              checked={trends.normalized}
              onchange={(checked) => {
                trends.normalized = checked;
                writeUrl();
              }}
              label={m.trends_normalize()}
            />
          </div>
        </div>
        <div class="chart-body">
          <TrendsLineChart
            buckets={trends.response?.buckets ?? []}
            series={trends.response?.series ?? []}
            {styleFor}
            {activeTerm}
            normalized={trends.normalized}
            onHover={(term) => (activeTerm = term)}
          />
          {#if trends.loading.terms}
            <div class="loading-overlay" role="status" aria-live="polite">
              <span aria-hidden="true"><Spinner size={18} /></span>
              <span>{m.trends_computing()}</span>
            </div>
          {/if}
        </div>
      </section>
    </div>
  </div>
</section>

<style>
  .trends-page {
    --trend-blue: var(--accent-blue);
    --trend-gold: var(--accent-amber);
    --trend-purple: var(--accent-purple);
    --trend-green: var(--accent-green);
    --trend-magenta: var(--accent-pink);
    --trend-slate: var(--text-secondary);
    --trend-red: var(--accent-red);
    --trend-cyan: var(--accent-cyan);
    /* kit-ui-check-ignore: brown slot of the 12-hue categorical series palette; nearest token --accent-orange would collide with the amber slot */
    --trend-brown: #92400e;
    --trend-lime: var(--accent-lime);
    --trend-indigo: var(--accent-indigo);
    --trend-black: var(--text-primary);
    color: var(--text-primary);
    /* The page fills the scroll host; the chart keeps a fixed height
       (see .content-grid). */
    height: 100%;
    display: flex;
    flex-direction: column;
  }

  :global(:root.dark) .trends-page {
    /* kit-ui-check-ignore: dark-mode counterpart of the suppressed brown palette slot above */
    --trend-brown: #fb923c;
  }

  /* Same frame as Usage and Activity: a full-width toolbar strip, then
     content on the shared 16px gutter. */
  .trends-toolbar {
    display: flex;
    align-items: center;
    flex-wrap: wrap;
    gap: 8px;
    padding: 8px 16px;
    background: var(--bg-surface);
    border-bottom: 1px solid var(--border-muted);
  }

  .toolbar-actions {
    display: flex;
    align-items: center;
    gap: 8px;
    margin-left: auto;
  }

  .trends-content {
    flex: 1 1 auto;
    min-height: 0;
    padding: 20px 16px 24px;
    display: flex;
    flex-direction: column;
    gap: 16px;
  }

  /* A real page title, the same size and weight as the library and
     Friction titles, with the date range under it. */
  .section-head {
    display: flex;
    flex-direction: column;
    gap: 4px;
  }

  h1 {
    margin: 0;
    font-size: 18px;
    line-height: 1.25;
    font-weight: 600;
  }

  .section-head p {
    margin: 0;
    color: var(--text-muted);
    font-size: 12px;
  }

  /* Two panels on one row: terms (input and counts) beside the chart.
     The chart panel sets the row's height; the terms panel fills that
     height and never pushes it taller (zero height, full minimum), so a
     long term list scrolls inside its table instead of stretching the
     chart. */
  .content-grid {
    --trends-chart-height: clamp(320px, calc(100vh - 330px), 420px);
    display: grid;
    grid-template-columns: minmax(240px, 300px) minmax(0, 1fr);
    gap: 16px;
    align-items: stretch;
  }

  .panel {
    min-width: 0;
    display: flex;
    flex-direction: column;
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-lg, 8px);
    background: var(--bg-surface);
  }

  .panel-head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 12px;
    min-height: 44px;
    padding: 8px 14px;
    border-bottom: 1px solid var(--border-muted);
  }

  .panel-title {
    margin: 0;
    color: var(--text-primary);
    font-size: 13px;
    font-weight: 600;
  }

  .terms-panel {
    height: 0;
    min-height: 100%;
    overflow: hidden;
  }

  .terms-hint {
    color: var(--text-muted);
    font-size: 11px;
    font-weight: 400;
  }

  .terms-input {
    padding: 12px 14px;
    border-bottom: 1px solid var(--border-muted);
  }

  textarea {
    display: block;
    box-sizing: border-box;
    width: 100%;
    min-height: 116px;
    padding: 8px 10px;
    resize: vertical;
    border: 1px solid var(--border-default);
    border-radius: var(--radius-md);
    background: var(--bg-surface);
    color: var(--text-primary);
    line-height: 1.5;
    font-family: var(--font-mono);
    font-size: 12px;
    outline: none;
  }

  textarea:focus {
    border-color: var(--accent-blue);
  }

  .error {
    margin-top: 8px;
    color: var(--accent-rose);
    font-size: 12px;
    line-height: 1.35;
  }

  /* Takes only the rows it needs, and scrolls once they outgrow the
     panel. */
  .table-panel {
    flex: 0 1 auto;
    min-height: 0;
    overflow: auto;
  }

  .chart-head {
    flex-wrap: wrap;
  }

  .chart-options {
    display: flex;
    align-items: center;
    flex-wrap: wrap;
    gap: 16px;
  }

  .group-by {
    display: flex;
    align-items: center;
    gap: 8px;
  }

  .option-label {
    color: var(--text-muted);
    font-size: 12px;
  }

  .chart-body {
    position: relative;
    height: var(--trends-chart-height);
  }

  .loading-overlay {
    position: absolute;
    inset: 0;
    display: grid;
    place-content: center;
    justify-items: center;
    gap: 8px;
    border-radius: 0 0 var(--radius-lg, 8px) var(--radius-lg, 8px);
    background: color-mix(in srgb, var(--bg-surface) 78%, transparent);
    color: var(--text-primary);
    font-size: 13px;
    font-weight: 600;
    pointer-events: none;
  }

  /* One column: the panels stack, the chart keeps a fixed height and the
     term table a bounded one. */
  @media (max-width: 900px) {
    .content-grid {
      --trends-chart-height: 300px;
      grid-template-columns: 1fr;
    }

    .terms-panel {
      height: auto;
      min-height: 0;
    }

    .table-panel {
      max-height: 320px;
    }
  }
</style>
