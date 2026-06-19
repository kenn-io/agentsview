<script lang="ts">
  import { onMount } from "svelte";
  import { trends } from "../../stores/trends.svelte.js";
  import { getBasePath } from "../../stores/router.svelte.js";
  import { sync } from "../../stores/sync.svelte.js";
  import type { TrendsGranularity } from "../../api/types.js";
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

  let activeTerm: string | null = $state(null);

  function colorFor(_term: string, index: number): string {
    return TREND_PALETTE[index % TREND_PALETTE.length]!;
  }

  function isGranularity(value: string | null): value is TrendsGranularity {
    return value === "day" || value === "week" || value === "month";
  }

  function applyQueryParams() {
    const q = new URLSearchParams(window.location.search);
    const from = q.get("from");
    const to = q.get("to");
    const granularity = q.get("granularity");
    const normalized = q.get("normalized");
    const terms = q.getAll("term").map((s) => s.trim()).filter(Boolean);
    if (from) trends.from = from;
    if (to) trends.to = to;
    if (isGranularity(granularity)) trends.granularity = granularity;
    trends.normalized = normalized === "true";
    if (terms.length > 0) trends.termText = terms.join("\n");
  }

  function writeUrl() {
    const q = new URLSearchParams();
    const current = new URLSearchParams(window.location.search);
    if (current.has("desktop")) {
      q.set("desktop", current.get("desktop") ?? "");
    }
    q.set("from", trends.from);
    q.set("to", trends.to);
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

  async function refresh() {
    writeUrl();
    await trends.fetchTerms();
  }

  const earliestSession = $derived(sync.stats?.earliest_session ?? null);

  const rangeSelection = $derived(
    selectionFromRange(trends.from, trends.to, earliestSession),
  );

  async function applyRange(sel: RangeSelection) {
    const range = resolveRange(sel, earliestSession);
    trends.from = range.from;
    trends.to = range.to;
    await refresh();
  }

  function setNormalized(event: Event) {
    trends.normalized = (event.currentTarget as HTMLInputElement).checked;
    writeUrl();
  }

  async function resetTerms() {
    await trends.resetTerms();
    writeUrl();
  }

  async function setGranularity(value: TrendsGranularity) {
    trends.granularity = value;
    await refresh();
  }

  onMount(() => {
    applyQueryParams();
    writeUrl();
    trends.fetchTerms();
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
    <div class="granularity" aria-label="Granularity">
      {#each ["day", "week", "month"] as value}
        <button
          class:active={trends.granularity === value}
          onclick={() => setGranularity(value as TrendsGranularity)}
        >
          {value}
        </button>
      {/each}
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
  .toolbar,
  .granularity {
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

  .granularity {
    align-self: end;
    height: 32px;
    padding: 2px;
    border: 1px solid var(--border-default);
    border-radius: 7px;
    background: var(--bg-surface);
  }

  .granularity button {
    height: 26px;
    min-width: 54px;
    padding: 0 10px;
    border: 0;
    background: transparent;
    color: var(--text-muted);
    text-transform: capitalize;
    font-size: 12px;
  }

  .granularity button.active {
    background: var(--bg-hover);
    color: var(--text-primary);
  }

  .normalize-toggle {
    align-self: end;
    height: 32px;
    display: flex;
    align-items: center;
    gap: 7px;
    color: var(--text-primary);
    font-size: 12px;
    font-weight: 500;
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
