<script lang="ts">
  import { formatDateTime, m } from "../../i18n/index.js";
  import type { Report } from "../../api/types.js";
  import { activeSessionsInSlot } from "./activeSessions.js";
  import { Typeahead, type TypeaheadOption } from "@kenn-io/kit-ui";
  import type {
    ActivityBucket,
    ActivityReportInterval,
    ActivitySessionRow,
  } from "../../api/generated/index";

  let {
    report,
    selectedBucket = null,
    onSelectBucket,
  }: {
    report: Report;
    selectedBucket?: number | null;
    onSelectBucket?: (
      sel: { idx: number; label: string; sessionIds: string[] } | null,
    ) => void;
  } = $props();

  const CHART_H = 160;
  const X_LABEL_H = 18;
  const STRIP_H = 14;
  const STRIP_GAP = 6;
  const Y_LABEL_W = 32;
  const RIGHT_PAD = 8;
  const OVERLAY_AXIS_W = 48;
  // Reserved headroom so the tallest bar, its grid line, and
  // the top y-axis label do not clip against the viewBox edge.
  const TOP_PAD = 10;
  const TICK_TARGET = 4;

  // buckets/by_* are typed `any[] | null` by the codegen, so cast
  // to the generated element model for field-level type safety.
  const buckets = $derived(
    (report.buckets ?? []) as ActivityBucket[],
  );

  let tooltip = $state<{ x: number; y: number; text: string } | null>(null);

  // Format bucket boundaries in the report's own timezone. Bucket start/end are
  // UTC instants of local calendar boundaries, so rendering them in the report
  // timezone keeps a "day" bucket on its intended calendar date.
  function timeLabel(ms: number): string {
    return formatDateTime(ms, {
      hour: "2-digit",
      minute: "2-digit",
      hourCycle: "h23",
      timeZone: report.timezone,
    });
  }

  function weekdayLabel(ms: number): string {
    return formatDateTime(ms, {
      weekday: "short",
      timeZone: report.timezone,
    });
  }

  function dateLabel(ms: number): string {
    return formatDateTime(ms, {
      month: "short",
      day: "numeric",
      timeZone: report.timezone,
    });
  }

  function fmtMinuteRange(startMs: number, endMs: number): string {
    return `${timeLabel(startMs)}–${timeLabel(endMs)}`;
  }

  function fmtHourRange(startMs: number, endMs: number): string {
    return `${weekdayLabel(startMs)} ${timeLabel(startMs)}–${timeLabel(endMs)}`;
  }

  function fmtDayRange(startMs: number): string {
    return dateLabel(startMs);
  }

  // The bucket end is exclusive; the last included instant is 1ms before it,
  // which formats to the inclusive last day (DST-safe, unlike subtracting 24h).
  function fmtWeekRange(startMs: number, endMs: number): string {
    return `${dateLabel(startMs)}–${dateLabel(endMs - 1)}`;
  }

  function fmtBucketRange(b: ActivityBucket): string {
    const startMs = Date.parse(b.start);
    const endMs = Date.parse(b.end);
    if (Number.isNaN(startMs) || Number.isNaN(endMs)) return "";
    if (report.bucket_unit === "hour") return fmtHourRange(startMs, endMs);
    if (report.bucket_unit === "day") return fmtDayRange(startMs);
    if (report.bucket_unit === "week") return fmtWeekRange(startMs, endMs);
    return fmtMinuteRange(startMs, endMs);
  }

  // Only the peak count splits by automation; the bucket's agent-minutes and
  // cost stay combined (the API does not break those down per bucket), so the
  // split annotation sits on "peak" alone and shows only when an automated
  // agent was running at the peak.
  function showSlotTip(e: MouseEvent, b: ActivityBucket) {
    const rect = (e.currentTarget as Element).getBoundingClientRect();
    const peakSplit =
      b.automated_at_peak > 0
        ? ` (${m.activity_int_auto_short({ int: b.interactive_at_peak, auto: b.automated_at_peak })})`
        : "";
    tooltip = {
      x: rect.left + rect.width / 2,
      y: rect.top - 4,
      text:
        `${fmtBucketRange(b)} · ${m.activity_peak_label({ count: b.max_agents })}${peakSplit} · ` +
        `${m.activity_agent_min_value({ value: b.agent_minutes.toFixed(1) })} · ` +
        `${m.activity_output_tokens_value({
          count: b.output_tokens,
          countLabel: b.output_tokens.toLocaleString(),
        })} · ` +
        `$${b.cost.toFixed(2)}`,
    };
  }

  function hideTip() {
    tooltip = null;
  }

  const intervals = $derived(
    (report.intervals ?? []) as ActivityReportInterval[],
  );
  const bySession = $derived(
    new Map(
      ((report.by_session ?? []) as ActivitySessionRow[]).map(
        (r) => [r.session_id, r],
      ),
    ),
  );

  // Half-open [startMs, endMs) for a slot, taken straight from the bucket's own
  // bounds (variable-width across presets). A missing bucket yields NaN bounds;
  // NaN comparisons are all false in activeSessionsInSlot, so the slot resolves
  // to an empty membership rather than throwing. In practice idx always maps to
  // a rendered bucket.
  function slotBounds(idx: number): { startMs: number; endMs: number } {
    const b = buckets[idx];
    if (!b) return { startMs: NaN, endMs: NaN };
    return { startMs: Date.parse(b.start), endMs: Date.parse(b.end) };
  }

  // Clicking a bucket hands its active-session membership to the parent, which
  // owns the page-local sessions-table filter. Clicking the already selected
  // bucket clears the filter. The parent resets `selectedBucket` to null
  // whenever the report reloads, so a stale slot never points at a wrong bucket.
  function selectSlot(idx: number) {
    if (!onSelectBucket) return;
    if (selectedBucket === idx) {
      onSelectBucket(null);
      return;
    }
    const b = buckets[idx];
    const { startMs, endMs } = slotBounds(idx);
    const sessionIds = activeSessionsInSlot(
      intervals,
      startMs,
      endMs,
      bySession,
    ).map((r) => r.session_id);
    onSelectBucket({ idx, label: b ? fmtBucketRange(b) : "", sessionIds });
  }

  function onSlotKey(e: KeyboardEvent, idx: number) {
    if (e.key === "Enter" || e.key === " ") {
      e.preventDefault();
      selectSlot(idx);
    }
  }

  // Optional secondary series overlaid on the bars: none, output tokens, or
  // cost. Each metric scales to its own max so the line reads as a shape over
  // the concurrency bars, not an absolute count on the agent axis.
  let overlayMetric = $state<"none" | "tokens" | "cost">("none");
  const overlayOptions: TypeaheadOption[] = $derived([
    { name: "none", label: m.activity_overlay_none(), displayLabel: m.activity_overlay_none() },
    { name: "tokens", label: m.activity_tokens(), displayLabel: m.activity_tokens() },
    { name: "cost", label: m.activity_cost(), displayLabel: m.activity_cost() },
  ]);

  function bucketOverlayValue(b: ActivityBucket): number {
    return overlayMetric === "cost" ? b.cost : b.output_tokens;
  }

  function trimDecimal(v: number, digits: number): string {
    return v.toFixed(digits).replace(/\.0+$/, "").replace(/(\.\d*?)0+$/, "$1");
  }

  function fmtCompact(v: number): string {
    const abs = Math.abs(v);
    if (abs >= 1_000_000) return `${trimDecimal(v / 1_000_000, 1)}M`;
    if (abs >= 1_000) return `${trimDecimal(v / 1_000, 1)}k`;
    if (Number.isInteger(v)) return String(v);
    return trimDecimal(v, 1);
  }

  function fmtOverlayTick(v: number): string {
    if (overlayMetric === "cost") return `$${v.toFixed(2)}`;
    return fmtCompact(v);
  }

  let containerEl: HTMLDivElement | undefined = $state();
  let containerWidth = $state(600);

  $effect(() => {
    if (!containerEl) return;
    const ro = new ResizeObserver((entries) => {
      const entry = entries[0];
      if (entry) {
        containerWidth = Math.floor(entry.contentRect.width);
      }
    });
    ro.observe(containerEl);
    return () => ro.disconnect();
  });

  const rightAxisW = $derived(
    overlayMetric === "none" ? RIGHT_PAD : OVERLAY_AXIS_W,
  );
  const plotWidth = $derived(
    Math.max(containerWidth - Y_LABEL_W - rightAxisW, 100),
  );

  // The plot maps the full [range_start, range_end) window onto plotWidth; every
  // bucket positions itself by its real bounds within that span.
  const rangeStartMs = $derived(Date.parse(report.range_start));
  const rangeEndMs = $derived(Date.parse(report.range_end));
  const rangeSpanMs = $derived(Math.max(rangeEndMs - rangeStartMs, 1));

  // x pixel for a given instant within the range.
  function xForMs(ms: number): number {
    return Y_LABEL_W + ((ms - rangeStartMs) / rangeSpanMs) * plotWidth;
  }

  function niceScale(maxY: number): { step: number; max: number } {
    if (!Number.isFinite(maxY) || maxY <= 0) {
      return { step: 1, max: 1 };
    }
    const rough = maxY / TICK_TARGET;
    const exp = Math.floor(Math.log10(rough));
    const base = Math.pow(10, exp);
    const normalized = rough / base;
    let mult: number;
    if (normalized <= 1) mult = 1;
    else if (normalized <= 2) mult = 2;
    else if (normalized <= 5) mult = 5;
    else mult = 10;
    const step = Math.max(mult * base, 1);
    const max = Math.ceil(maxY / step) * step;
    return { step, max };
  }

  const maxAgents = $derived.by(() => {
    let m = 0;
    for (const b of buckets) {
      if (b.max_agents > m) m = b.max_agents;
    }
    return m;
  });

  const scale = $derived(niceScale(maxAgents));

  function scaleY(val: number, max: number, h: number): number {
    const plotH = h - TOP_PAD;
    return h - (val / max) * plotH;
  }

  // Each bucket owns a full contiguous cell [cellX, cellX+cellW); the visible
  // bar is inset by a small gap. Strip cells and hit targets reuse the cell.
  const bars = $derived.by(() => {
    const out: Array<{
      x: number;
      y: number;
      w: number;
      h: number;
      interactiveY: number;
      interactiveH: number;
      automatedY: number;
      automatedH: number;
      cellX: number;
      cellW: number;
      idx: number;
    }> = [];
    for (let i = 0; i < buckets.length; i++) {
      const b = buckets[i]!;
      const bStart = Date.parse(b.start);
      const bEnd = Date.parse(b.end);
      const cellX = xForMs(bStart);
      const cellW = Math.max(((bEnd - bStart) / rangeSpanMs) * plotWidth, 1);
      const barGap = Math.min(cellW * 0.2, 2);
      const top = scaleY(b.max_agents, scale.max, CHART_H);
      // Split the peak bar into a blue interactive base and an orange automated
      // cap. interactive_at_peak + automated_at_peak == max_agents, so the two
      // segments stack to the full bar; interactiveTop is the seam between them.
      const interactiveTop = scaleY(b.interactive_at_peak, scale.max, CHART_H);
      out.push({
        x: cellX + barGap / 2,
        y: top,
        w: Math.max(cellW - barGap, 1),
        h: Math.max(CHART_H - top, 0),
        interactiveY: interactiveTop,
        interactiveH: Math.max(CHART_H - interactiveTop, 0),
        automatedY: top,
        automatedH: Math.max(interactiveTop - top, 0),
        cellX,
        cellW,
        idx: i,
      });
    }
    return out;
  });

  const overlayDataMax = $derived.by(() => {
    let m = 0;
    for (const b of buckets) {
      const v = bucketOverlayValue(b);
      if (v > m) m = v;
    }
    return m;
  });
  const overlayMax = $derived(overlayDataMax || 1);

  const overlayPath = $derived.by(() => {
    if (overlayMetric === "none" || buckets.length === 0) return "";
    let d = "";
    for (let i = 0; i < buckets.length; i++) {
      const b = buckets[i]!;
      const center = (Date.parse(b.start) + Date.parse(b.end)) / 2;
      const x = xForMs(center);
      const y = scaleY(bucketOverlayValue(b), overlayMax, CHART_H);
      d += i === 0 ? `M${x},${y}` : `L${x},${y}`;
    }
    return d;
  });

  const overlayTicks = $derived.by(() => {
    if (overlayMetric === "none") return [];
    const values =
      overlayDataMax <= 0 ? [0] : [0, overlayDataMax / 2, overlayDataMax];
    return values.map((val) => ({
      y: scaleY(val, overlayMax, CHART_H),
      label: fmtOverlayTick(val),
    }));
  });

  const yTicks = $derived.by(() => {
    const { step, max } = scale;
    if (max <= 0 || step <= 0) return [];
    const ticks: Array<{ y: number; label: string }> = [];
    const count = Math.round(max / step);
    for (let i = 0; i <= count; i++) {
      const val = step * i;
      ticks.push({
        y: scaleY(val, max, CHART_H),
        label: String(val),
      });
    }
    return ticks;
  });

  // Local clock fields in the report timezone, used to pick tick boundaries.
  function localHour(ms: number): number {
    return Number(
      new Date(ms).toLocaleString("en-US", {
        hour: "2-digit",
        hourCycle: "h23",
        timeZone: report.timezone,
      }),
    );
  }

  function localWeekday(ms: number): string {
    return new Date(ms).toLocaleDateString("en-US", {
      weekday: "short",
      timeZone: report.timezone,
    });
  }

  function localDayOfMonth(ms: number): number {
    return Number(
      new Date(ms).toLocaleDateString("en-US", {
        day: "numeric",
        timeZone: report.timezone,
      }),
    );
  }

  // Five ticks at even fractions of the range, each labelled with the actual
  // local time (report timezone) at that position. A full day reads as
  // 00:00/06:00/12:00/18:00/00:00; custom sub-day ranges track their real
  // bounds instead of fixed 0/6/12/18/24 hour marks.
  function minuteUnitTicks(): Array<{ x: number; label: string }> {
    const out: Array<{ x: number; label: string }> = [];
    for (const frac of [0, 0.25, 0.5, 0.75, 1]) {
      const ms = rangeStartMs + frac * rangeSpanMs;
      out.push({ x: xForMs(ms), label: timeLabel(ms) });
    }
    return out;
  }

  // One tick per bucket whose start satisfies the boundary predicate, labelled
  // with its short date (used for hour/day/week presets).
  function bucketBoundaryTicks(
    isBoundary: (ms: number) => boolean,
  ): Array<{ x: number; label: string }> {
    const out: Array<{ x: number; label: string }> = [];
    for (const b of buckets) {
      const ms = Date.parse(b.start);
      if (Number.isNaN(ms) || !isBoundary(ms)) continue;
      out.push({ x: xForMs(ms), label: dateLabel(ms) });
    }
    return out;
  }

  const xTicks = $derived.by(() => {
    if (report.bucket_unit === "hour") {
      return bucketBoundaryTicks((ms) => localHour(ms) === 0);
    }
    if (report.bucket_unit === "day") {
      return bucketBoundaryTicks((ms) => localWeekday(ms) === "Mon");
    }
    if (report.bucket_unit === "week") {
      return bucketBoundaryTicks((ms) => localDayOfMonth(ms) === 1);
    }
    return minuteUnitTicks();
  });

  // Partial future region: shade from effective_end to range_end.
  const effEndMs = $derived(Date.parse(report.effective_end));
  const futureStartMs = $derived(Math.min(effEndMs, rangeEndMs));
  const futureX = $derived(xForMs(futureStartMs));
  const futureW = $derived(
    Math.max(((rangeEndMs - futureStartMs) / rangeSpanMs) * plotWidth, 0),
  );

  const svgW = $derived(plotWidth + Y_LABEL_W + rightAxisW);
  const svgH = $derived(CHART_H + STRIP_GAP + STRIP_H + X_LABEL_H);
  const stripY = $derived(CHART_H + STRIP_GAP);

  function setOverlayMetric(value: string) {
    overlayMetric = value as "none" | "tokens" | "cost";
  }
</script>

<div class="timeline">
  <div class="timeline-header">
    <h3 class="timeline-title">{m.activity_concurrency()}</h3>
    <div class="panel-actions">
      <div class="legend" aria-hidden="true">
        <span class="legend-item">
          <span class="swatch interactive"></span>{m.activity_interactive()}
        </span>
        <span class="legend-item">
          <span class="swatch automated"></span>{m.activity_automated()}
        </span>
      </div>
      <div class="overlay-toggle">
        <span>{m.activity_overlay()}</span>
        <Typeahead
          options={overlayOptions}
          value={overlayMetric}
          fallbackLabel={m.activity_overlay_none()}
          placeholder={m.activity_overlay_placeholder()}
          title={m.activity_overlay_metric()}
          emptyLabel={m.activity_no_metrics()}
          onselect={setOverlayMetric}
        />
      </div>
    </div>
  </div>

  <div class="timeline-body" bind:this={containerEl}>
    <svg
      width="100%"
      height={svgH}
      viewBox="0 0 {svgW} {svgH}"
      preserveAspectRatio="xMidYMid meet"
      class="timeline-svg"
    >
      {#if futureW > 0}
        <rect
          class="concurrency-future"
          data-future
          x={futureX}
          y={TOP_PAD}
          width={futureW}
          height={CHART_H - TOP_PAD}
        />
      {/if}

      {#each yTicks as tick}
        <line
          x1={Y_LABEL_W}
          y1={tick.y}
          x2={Y_LABEL_W + plotWidth}
          y2={tick.y}
          class="grid-line"
        />
        <text
          x={Y_LABEL_W - 4}
          y={tick.y + 3}
          class="y-label"
          text-anchor="end"
        >
          {tick.label}
        </text>
      {/each}

      {#each bars as bar (bar.idx)}
        <rect
          class="concurrency-seg interactive"
          class:selected={selectedBucket === bar.idx}
          x={bar.x}
          y={bar.interactiveY}
          width={bar.w}
          height={bar.interactiveH}
        />
        <rect
          class="concurrency-seg automated"
          class:selected={selectedBucket === bar.idx}
          x={bar.x}
          y={bar.automatedY}
          width={bar.w}
          height={bar.automatedH}
        />
        {#if selectedBucket === bar.idx}
          <rect
            class="concurrency-outline"
            x={bar.x}
            y={bar.y}
            width={bar.w}
            height={bar.h}
          />
        {/if}
      {/each}

      {#if overlayMetric !== "none" && overlayPath}
        <path class="overlay-line" d={overlayPath} />
        <line
          class="overlay-axis-line"
          x1={Y_LABEL_W + plotWidth}
          y1={TOP_PAD}
          x2={Y_LABEL_W + plotWidth}
          y2={CHART_H}
        />
        {#each overlayTicks as tick}
          <line
            class="overlay-axis-tick"
            x1={Y_LABEL_W + plotWidth}
            y1={tick.y}
            x2={Y_LABEL_W + plotWidth + 4}
            y2={tick.y}
          />
          <text
            x={Y_LABEL_W + plotWidth + 6}
            y={tick.y + 3}
            class="overlay-y-label"
            text-anchor="start"
          >
            {tick.label}
          </text>
        {/each}
      {/if}

      {#each xTicks as tick}
        <text
          x={tick.x}
          y={svgH - 4}
          class="x-label"
          text-anchor="middle"
        >
          {tick.label}
        </text>
      {/each}

      <!-- Active/idle strip: one full-width cell per elapsed bucket. -->
      {#each bars as bar (bar.idx)}
        {@const b = buckets[bar.idx]}
        <rect
          class="strip-cell"
          class:active={b !== undefined && b.max_agents > 0}
          x={bar.cellX}
          y={stripY}
          width={bar.cellW}
          height={STRIP_H}
        />
      {/each}
      {#if futureW > 0}
        <rect
          class="strip-future"
          x={futureX}
          y={stripY}
          width={futureW}
          height={STRIP_H}
        />
      {/if}

      <!-- Transparent full-cell per-bucket hover/click target (one per bucket).
           data-bucket-bar lives here, not on the visible bar, because the hover
           handler that drives the tooltip is on this interactive rect. -->
      {#each bars as bar (bar.idx)}
        {@const b = buckets[bar.idx]}
        <rect
          class="slot-hit"
          data-bucket-bar
          x={bar.cellX}
          y={TOP_PAD}
          width={bar.cellW}
          height={stripY + STRIP_H - TOP_PAD}
          role="button"
          tabindex="0"
          aria-pressed={selectedBucket === bar.idx}
          aria-label={m.activity_filter_active_in_slot()}
          onmouseenter={(e) => b && showSlotTip(e, b)}
          onmouseleave={hideTip}
          onclick={() => selectSlot(bar.idx)}
          onkeydown={(e) => onSlotKey(e, bar.idx)}
        />
      {/each}
    </svg>

    {#if tooltip}
      <div class="tooltip" style="left: {tooltip.x}px; top: {tooltip.y}px;">
        {tooltip.text}
      </div>
    {/if}
  </div>
</div>

<style>
  .timeline {
    display: flex;
    flex-direction: column;
  }

  .timeline-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    margin-bottom: 8px;
  }

  .timeline-title {
    font-size: 12px;
    font-weight: 600;
    color: var(--text-primary);
  }

  .panel-actions {
    display: flex;
    align-items: center;
    gap: 12px;
  }

  .legend {
    display: flex;
    align-items: center;
    gap: 10px;
  }

  .legend-item {
    display: inline-flex;
    align-items: center;
    gap: 4px;
    font-size: 10px;
    color: var(--text-muted);
  }

  .swatch {
    width: 8px;
    height: 8px;
    border-radius: 2px;
  }

  .swatch.interactive {
    background: var(--accent-blue);
  }

  .swatch.automated {
    background: var(--accent-orange);
  }

  .overlay-toggle {
    display: flex;
    align-items: center;
    gap: 4px;
    font-size: 10px;
    color: var(--text-muted);
    /* kit-ui Typeahead sizing knobs; custom properties inherit into the
       child .kit-typeahead. */
    --typeahead-min-width: 86px;
    --typeahead-max-width: 96px;
    --typeahead-control-height: 22px;
    --typeahead-control-padding: 0 6px;
    --typeahead-control-font-size: 10px;
  }

  .timeline-body {
    width: 100%;
  }

  .timeline-svg {
    display: block;
  }

  .grid-line {
    stroke: var(--border-muted);
    stroke-width: 1;
    stroke-dasharray: 2 2;
  }

  .y-label {
    font-size: 9px;
    fill: var(--text-muted);
    font-family: var(--font-mono);
  }

  .x-label {
    font-size: 9px;
    fill: var(--text-muted);
    font-family: var(--font-mono);
  }

  .concurrency-seg {
    opacity: 0.75;
  }

  .concurrency-seg.interactive {
    fill: var(--accent-blue);
  }

  .concurrency-seg.automated {
    fill: var(--accent-orange);
  }

  .concurrency-seg.selected {
    opacity: 1;
  }

  .concurrency-outline {
    fill: none;
    stroke: var(--text-primary);
    stroke-width: 1;
  }

  .concurrency-future {
    fill: var(--bg-inset);
    opacity: 0.5;
  }

  .overlay-line {
    fill: none;
    stroke: var(--accent-amber);
    stroke-width: 1.5;
    opacity: 0.85;
  }

  .overlay-axis-line,
  .overlay-axis-tick {
    stroke: var(--accent-amber);
    stroke-width: 1;
    opacity: 0.55;
  }

  .overlay-y-label {
    font-size: 9px;
    fill: var(--accent-amber);
    font-family: var(--font-mono);
  }

  .strip-cell {
    fill: var(--bg-inset);
    stroke: var(--bg-surface);
    stroke-width: 0.5;
  }

  .strip-cell.active {
    fill: var(--accent-blue);
    opacity: 0.55;
  }

  .strip-future {
    fill: var(--bg-inset);
    opacity: 0.5;
  }

  .slot-hit {
    fill: transparent;
    cursor: pointer;
  }

  .slot-hit:hover {
    fill: var(--accent-blue);
    opacity: 0.08;
  }

  .slot-hit:focus-visible {
    outline: 1px solid var(--accent-blue);
    outline-offset: -1px;
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
</style>
