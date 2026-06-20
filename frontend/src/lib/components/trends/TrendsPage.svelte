<script lang="ts">
  import { onMount } from "svelte";
  import { trends } from "../../stores/trends.svelte.js";
  import { getBasePath } from "../../stores/router.svelte.js";
  import { sync } from "../../stores/sync.svelte.js";
  import {
    yokedDates,
    panelDateState,
    rangeToPanelDate,
    type PanelDateState,
  } from "../../stores/yokedDates.svelte.js";
  import { rollingRange } from "../../utils/dates.js";
  import type { TrendsGranularity } from "../../api/types.js";
  import { ChartColumnIcon, ChevronDownIcon } from "../../icons.js";
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

  let activeTerm: string | null = $state(null);
  let trendsWindowDays: number | null = $state(null);
  const trendsPanelDate = $derived(currentTrendsPanelDate());

  const GRANULARITIES: TrendsGranularity[] = ["day", "week", "month"];
  let groupByOpen = $state(false);
  let groupByEl: HTMLDivElement | undefined = $state();

  function pickGranularity(g: TrendsGranularity) {
    groupByOpen = false;
    if (g !== trends.granularity) void setGranularity(g);
  }

  function onGroupByDocClick(e: MouseEvent) {
    if (groupByEl && !groupByEl.contains(e.target as Node)) {
      groupByOpen = false;
    }
  }

  function onGroupByKey(e: KeyboardEvent) {
    if (e.key === "Escape") groupByOpen = false;
  }

  function colorFor(_term: string, index: number): string {
    return TREND_PALETTE[index % TREND_PALETTE.length]!;
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
    } else {
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
    q.set("from", trends.from);
    q.set("to", trends.to);
    if (trendsWindowDays !== null) {
      q.set(TREND_WINDOW_PARAM, String(trendsWindowDays));
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
    const range = rollingRange(trendsWindowDays);
    if (trends.from === range.from && trends.to === range.to) return;
    trends.from = range.from;
    trends.to = range.to;
    updateYokeFromTrends(panelDateState(range.from, range.to, {
      mode: "rolling",
      windowDays: trendsWindowDays,
    }));
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

  function setNormalized(event: Event) {
    trends.normalized = (event.currentTarget as HTMLInputElement).checked;
    writeUrl();
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
    trends.from = state.from;
    trends.to = state.to;
    trendsWindowDays = state.mode === "rolling"
      ? state.windowDays ?? null
      : null;
  }

  onMount(() => {
    const hasDateParams = applyQueryParams();
    if (hasDateParams) {
      updateYokeFromTrends();
    } else {
      seedTrendsYoke();
    }
    writeUrl();
    trends.fetchTerms();
    document.addEventListener("click", onGroupByDocClick);
    document.addEventListener("keydown", onGroupByKey);
    return () => {
      document.removeEventListener("click", onGroupByDocClick);
      document.removeEventListener("keydown", onGroupByKey);
    };
  });
</script>

<section class="trends-page">
  <div class="page-head">
    <div>
      <h1>Trends</h1>
      <p>{trends.response?.from ?? trends.from} to {trends.response?.to ?? trends.to}</p>
    </div>
    <div class="head-actions">
      <button class="secondary" onclick={resetTerms}>Reset</button>
      <button class="primary" onclick={refresh} disabled={trends.loading.terms}>
        {trends.loading.terms ? "Refreshing" : "Refresh"}
      </button>
    </div>
  </div>

  <div class="toolbar">
    <RangePicker
      selection={rangeSelection}
      busy={trends.loading.terms}
      {earliestSession}
      onSelect={applyRange}
    />
  </div>

  <div class="content-grid">
    <div class="query-panel">
      <label class="terms-label" for="trend-terms">
        <span>Terms</span>
        <span class="terms-hint">one per line</span>
      </label>
      <textarea
        id="trend-terms"
        bind:value={trends.termText}
        rows="9"
        spellcheck="false"
      ></textarea>
      {#if trends.errors.terms}
        <div class="error">{trends.errors.terms}</div>
      {/if}
    </div>

    <div class="chart-panel" aria-busy={trends.loading.terms}>
      <div class="chart-options">
        <div class="group-by" bind:this={groupByEl}>
          <button
            class="group-trigger"
            onclick={() => (groupByOpen = !groupByOpen)}
            aria-haspopup="menu"
            aria-expanded={groupByOpen}
          >
            <ChartColumnIcon size="13" strokeWidth="2" aria-hidden="true" />
            Group by <span class="gval">{trends.granularity}</span>
            <ChevronDownIcon
              class={groupByOpen ? "g-chev open" : "g-chev"}
              size="11"
              strokeWidth="2.2"
              aria-hidden="true"
            />
          </button>
          {#if groupByOpen}
            <div class="group-menu" role="menu">
              {#each GRANULARITIES as g (g)}
                <button
                  class="group-item"
                  class:active={trends.granularity === g}
                  role="menuitemradio"
                  aria-checked={trends.granularity === g}
                  onclick={() => pickGranularity(g)}
                >
                  {g}
                </button>
              {/each}
            </div>
          {/if}
        </div>
        <label class="normalize-toggle">
          <input
            type="checkbox"
            bind:checked={trends.normalized}
            onchange={setNormalized}
          />
          <span>Normalize by number of messages</span>
        </label>
      </div>
      <TrendsLineChart
        buckets={trends.response?.buckets ?? []}
        series={trends.response?.series ?? []}
        {colorFor}
        {activeTerm}
        normalized={trends.normalized}
        onHover={(term) => (activeTerm = term)}
      />
      {#if trends.loading.terms}
        <div class="loading-overlay" role="status" aria-live="polite">
          <span class="loading-spinner" aria-hidden="true"></span>
          <span>Computing trends...</span>
        </div>
      {/if}
    </div>

    <div class="table-panel">
      <TermTable
        series={trends.response?.series ?? []}
        {colorFor}
        {activeTerm}
        normalized={trends.normalized}
        messageCount={trends.response?.message_count ?? 0}
        onHover={(term) => (activeTerm = term)}
      />
    </div>
  </div>
</section>

<style>
  .trends-page {
    --trend-blue: #2563eb;
    --trend-gold: #d97706;
    --trend-purple: #7c3aed;
    --trend-green: #059669;
    --trend-magenta: #db2777;
    --trend-slate: #475569;
    --trend-red: #dc2626;
    --trend-cyan: #0891b2;
    --trend-brown: #92400e;
    --trend-lime: #65a30d;
    --trend-indigo: #4338ca;
    --trend-black: #111827;
    max-width: 1180px;
    margin: 0 auto;
    padding: 22px;
    color: var(--text-primary);
  }

  :global(:root.dark) .trends-page {
    --trend-blue: #60a5fa;
    --trend-gold: #fbbf24;
    --trend-purple: #c084fc;
    --trend-green: #4ade80;
    --trend-magenta: #f472b6;
    --trend-slate: #cbd5e1;
    --trend-red: #f87171;
    --trend-cyan: #22d3ee;
    --trend-brown: #fb923c;
    --trend-lime: #a3e635;
    --trend-indigo: #818cf8;
    --trend-black: #f8fafc;
  }

  .page-head {
    display: flex;
    align-items: flex-start;
    justify-content: space-between;
    gap: 16px;
    margin-bottom: 16px;
  }

  h1 {
    margin: 0;
    font-size: 24px;
    line-height: 1.2;
    font-weight: 650;
    letter-spacing: 0;
  }

  p {
    margin: 4px 0 0;
    color: var(--text-muted);
    font-size: 13px;
  }

  .head-actions,
  .toolbar {
    display: flex;
    align-items: center;
    gap: 8px;
  }

  button,
  input,
  textarea {
    font: inherit;
  }

  button {
    border: 1px solid var(--border-default);
    border-radius: 6px;
    background: var(--bg-surface);
    color: var(--text-primary);
    cursor: pointer;
  }

  button:hover:not(:disabled) {
    background: var(--bg-hover);
  }

  button:disabled {
    opacity: 0.65;
    cursor: default;
  }

  .primary,
  .secondary {
    height: 32px;
    padding: 0 12px;
    font-size: 12px;
    font-weight: 600;
  }

  .primary {
    background: var(--accent-blue);
    border-color: var(--accent-blue);
    color: white;
  }

  .primary:hover:not(:disabled) {
    filter: brightness(0.95);
    background: var(--accent-blue);
  }

  .toolbar {
    flex-wrap: wrap;
    padding: 12px 0 18px;
    border-top: 1px solid var(--border-muted);
  }

  label {
    display: grid;
    gap: 5px;
    color: var(--text-muted);
    font-size: 11px;
    font-weight: 600;
  }

  input,
  textarea {
    border: 1px solid var(--border-default);
    border-radius: 6px;
    background: var(--bg-surface);
    color: var(--text-primary);
  }

  input {
    height: 32px;
    padding: 0 8px;
    font-size: 12px;
  }

  .chart-options {
    display: flex;
    align-items: center;
    justify-content: flex-end;
    gap: 14px;
    padding: 2px 2px 10px;
  }

  .group-by {
    position: relative;
  }

  .group-trigger {
    height: 26px;
    padding: 0 8px;
    display: inline-flex;
    align-items: center;
    gap: 6px;
    border: 0;
    border-radius: 6px;
    background: transparent;
    color: var(--text-muted);
    font-size: 12px;
  }

  .group-trigger:hover:not(:disabled) {
    background: var(--bg-surface-hover);
    color: var(--text-secondary);
  }

  .group-trigger .gval {
    color: var(--text-secondary);
    font-weight: 500;
    text-transform: capitalize;
  }

  :global(.g-chev) {
    color: var(--text-muted);
    transition: transform 0.15s;
  }

  :global(.g-chev.open) {
    transform: rotate(180deg);
  }

  .group-menu {
    position: absolute;
    top: calc(100% + 4px);
    right: 0;
    z-index: 20;
    min-width: 124px;
    padding: 4px;
    background: var(--bg-surface);
    border: 1px solid var(--border-muted);
    border-radius: 7px;
    box-shadow: var(--shadow-md);
  }

  .group-item {
    width: 100%;
    height: 28px;
    padding: 0 9px;
    display: flex;
    align-items: center;
    border: 0;
    border-radius: 5px;
    background: transparent;
    color: var(--text-secondary);
    font-size: 12px;
    text-align: left;
    text-transform: capitalize;
  }

  .group-item:hover:not(:disabled) {
    background: var(--bg-surface-hover);
  }

  .group-item.active {
    color: var(--accent-blue);
    font-weight: 500;
  }

  .normalize-toggle {
    display: flex;
    align-items: center;
    gap: 6px;
    color: var(--text-muted);
    font-size: 12px;
    font-weight: 500;
    cursor: pointer;
  }

  .normalize-toggle input {
    width: 14px;
    height: 14px;
    padding: 0;
  }

  .content-grid {
    display: grid;
    grid-template-columns: minmax(220px, 280px) minmax(0, 1fr);
    grid-template-areas:
      "query chart"
      "table chart";
    gap: 14px;
    align-items: start;
  }

  .query-panel {
    grid-area: query;
  }

  .chart-panel {
    grid-area: chart;
    min-width: 0;
    position: relative;
  }

  .table-panel {
    grid-area: table;
  }

  .terms-label {
    display: flex;
    align-items: baseline;
    justify-content: space-between;
    gap: 8px;
    margin-bottom: 6px;
  }

  .terms-hint {
    color: var(--text-muted);
    font-size: 11px;
    font-weight: 500;
  }

  textarea {
    width: 100%;
    min-height: 188px;
    padding: 10px;
    resize: vertical;
    line-height: 1.45;
    font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace;
    font-size: 12px;
  }

  .error {
    margin-top: 8px;
    color: var(--accent-rose);
    font-size: 12px;
    line-height: 1.35;
  }

  .loading-overlay {
    position: absolute;
    inset: 1px;
    display: grid;
    place-items: center;
    gap: 8px;
    border-radius: 8px;
    background: color-mix(
      in srgb,
      var(--bg-surface) 78%,
      transparent
    );
    color: var(--text-primary);
    font-size: 13px;
    font-weight: 600;
    pointer-events: none;
  }

  .loading-spinner {
    width: 18px;
    height: 18px;
    border: 2px solid var(--border-default);
    border-top-color: var(--accent-blue);
    border-radius: 999px;
    animation: trends-spin 800ms linear infinite;
  }

  @keyframes trends-spin {
    to {
      transform: rotate(360deg);
    }
  }

  @media (max-width: 820px) {
    .trends-page {
      padding: 16px;
    }

    .page-head {
      flex-direction: column;
    }

    .content-grid {
      grid-template-columns: 1fr;
      grid-template-areas:
        "query"
        "chart"
        "table";
    }
  }
</style>
