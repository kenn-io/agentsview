<script lang="ts">
  import { Chart, Layer, Line, Rect, Spline, Text } from "layerchart";
  import { scaleLinear } from "d3-scale";
  import { formatDateTime, getLocale, m } from "../../i18n/index.js";
  import type { Report } from "../../api/types.js";
  import { Button, Typeahead, type TypeaheadOption } from "@kenn-io/kit-ui";
  import type { ActivityBucket } from "../../api/generated/index";
  import { formatMoney, moneyFromMicrodollars } from "../../money.js";

  let {
    report,
    selectedRange = null,
    onSelectRange,
  }: {
    report: Report;
    selectedRange?: { start: number; end: number } | null;
    onSelectRange?: (
      sel: { start: number; end: number; label: string } | null,
    ) => void;
  } = $props();

  const TRACK_HEADER_H = 24;
  const TRACK_GAP = 10;
  const X_LABEL_H = 18;
  const STRIP_H = 14;
  const STRIP_GAP = 6;
  const Y_LABEL_W = 32;
  const RIGHT_PAD = 16;
  const OVERLAY_AXIS_W = 48;
  const TOP_PAD = TRACK_HEADER_H;
  const TICK_TARGET = 4;

  const buckets = $derived(report.buckets ?? []);

  let tooltip = $state<{ x: number; y: number; bucket: ActivityBucket } | null>(null);
  let tooltipEl = $state<HTMLDivElement>();
  let tooltipPos = $state<{ left: number; top: number } | null>(null);
  let keyboardAnchorIndex = $state<number | null>(null);

  const TIP_PAD = 8;

  // Clamp the measured tooltip box inside the viewport so it stays fully
  // visible when the hovered bucket sits near a chart or window edge. The
  // tooltip renders hidden for one frame while this measures it.
  $effect(() => {
    if (!tooltip || !tooltipEl) {
      tooltipPos = null;
      return;
    }
    const w = tooltipEl.offsetWidth;
    const h = tooltipEl.offsetHeight;
    const left = Math.min(
      Math.max(tooltip.x - w / 2, TIP_PAD),
      Math.max(window.innerWidth - w - TIP_PAD, TIP_PAD),
    );
    const top = Math.max(tooltip.y - h, TIP_PAD);
    tooltipPos = { left, top };
  });

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

  function showSlotTip(e: MouseEvent, b: ActivityBucket) {
    const rect = (e.currentTarget as Element).getBoundingClientRect();
    tooltip = {
      x: rect.left + rect.width / 2,
      y: rect.top - 4,
      bucket: b,
    };
  }

  function hideTip() {
    tooltip = null;
  }

  function fmtSelectionRange(start: number, end: number): string {
    const first = buckets[start];
    const last = buckets[end - 1];
    if (!first || !last) return "";
    if (end - start === 1) return fmtBucketRange(first);
    const startMs = Date.parse(first.start);
    const endMs = Date.parse(last.end);
    if (Number.isNaN(startMs) || Number.isNaN(endMs)) return "";
    if (report.bucket_unit === "minute") return fmtMinuteRange(startMs, endMs);
    if (report.bucket_unit === "hour") return fmtHourRange(startMs, endMs);
    return fmtWeekRange(startMs, endMs);
  }

  function sameRange(start: number, end: number): boolean {
    return selectedRange?.start === start && selectedRange.end === end;
  }

  // Bucket membership is computed by the shared backend aggregator. The chart
  // emits a half-open range; ActivityPage requests that page asynchronously.
  function selectRange(startIndex: number, endIndex: number) {
    if (!onSelectRange) return;
    const start = Math.min(startIndex, endIndex);
    const end = Math.max(startIndex, endIndex) + 1;
    if (sameRange(start, end)) {
      onSelectRange(null);
      return;
    }
    onSelectRange({ start, end, label: fmtSelectionRange(start, end) });
  }

  function onSlotKey(e: KeyboardEvent, idx: number) {
    if (e.key === "Escape" && selectedRange) {
      e.preventDefault();
      onSelectRange?.(null);
      keyboardAnchorIndex = null;
      return;
    }
    if (e.key === "Enter" || e.key === " ") {
      e.preventDefault();
      keyboardAnchorIndex = idx;
      selectRange(idx, idx);
      return;
    }
    if (e.key !== "ArrowLeft" && e.key !== "ArrowRight") return;
    e.preventDefault();
    const next = Math.max(
      0,
      Math.min(
        liveBars.length - 1,
        idx + (e.key === "ArrowRight" ? 1 : -1),
      ),
    );
    if (e.shiftKey) {
      const anchor = keyboardAnchorIndex ?? idx;
      keyboardAnchorIndex = anchor;
      selectRange(anchor, next);
    } else {
      keyboardAnchorIndex = next;
    }
    queueMicrotask(() => {
      document.querySelector<SVGElement>(
        `[data-concurrency-bucket-index="${next}"]`,
      )?.focus();
    });
  }

  let dragStart = $state<number | null>(null);
  let dragEnd = $state<number | null>(null);

  const activeRange = $derived.by(() => {
    if (dragStart !== null) {
      const end = dragEnd ?? dragStart;
      return {
        start: Math.min(dragStart, end),
        end: Math.max(dragStart, end) + 1,
      };
    }
    return selectedRange;
  });

  function beginRangeDrag(event: PointerEvent, idx: number) {
    if (event.button !== 0) return;
    event.preventDefault();
    dragStart = idx;
    dragEnd = idx;
  }

  function extendRangeDrag(idx: number) {
    if (dragStart === null) return;
    dragEnd = idx;
  }

  function moveRangeDrag(event: PointerEvent) {
    if (dragStart === null || !containerEl || liveBars.length === 0) return;
    const x = event.clientX - containerEl.getBoundingClientRect().left;
    const first = liveBars[0]!;
    const last = liveBars.at(-1)!;
    if (x <= first.cellX) {
      dragEnd = 0;
      return;
    }
    if (x >= last.cellX + last.cellW) {
      dragEnd = liveBars.length - 1;
      return;
    }
    const idx = liveBars.findIndex((bar) => x < bar.cellX + bar.cellW);
    if (idx >= 0) dragEnd = idx;
  }

  function finishRangeDrag() {
    if (dragStart === null) return;
    const start = dragStart;
    const end = dragEnd ?? dragStart;
    dragStart = null;
    dragEnd = null;
    selectRange(start, end);
  }

  function cancelRangeDrag() {
    dragStart = null;
    dragEnd = null;
  }

  // Combined usage overlays the interactive track with its own right axis.
  let overlayMetric = $state<"none" | "tokens" | "cost">("none");
  const overlayOptions: TypeaheadOption[] = $derived([
    { name: "none", label: m.activity_overlay_none(), displayLabel: m.activity_overlay_none() },
    { name: "tokens", label: m.activity_tokens(), displayLabel: m.activity_tokens() },
    { name: "cost", label: m.activity_cost(), displayLabel: m.activity_cost() },
  ]);

  function bucketOverlayValue(b: ActivityBucket): number {
    return overlayMetric === "cost" ? b.cost.microdollars : b.output_tokens;
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

  function fmtCompactValue(v: number): string {
    return new Intl.NumberFormat(getLocale(), {
      notation: "compact",
      compactDisplay: "short",
      maximumFractionDigits: 1,
    }).format(v);
  }

  function trackPeakLabel(peak: Report["peak"]): string {
    const count = peak.agents.toLocaleString(getLocale());
    return peak.at
      ? m.activity_peak_at({ count, time: timeLabel(Date.parse(peak.at)) })
      : m.activity_peak_label({ count });
  }

  function fmtOverlayTick(v: number): string {
    if (overlayMetric === "cost") return formatMoney(moneyFromMicrodollars(v));
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

  // Class maxima occur independently; a busy subagent track must not flatten
  // the interactive track or hide a class peak outside the combined peak.
  const tracks = $derived.by(() => {
    const definitions = [
      {
        kind: "interactive",
        label: m.activity_interactive(),
        peakLabel: m.activity_interactive_peak(),
        field: "max_interactive_agents" as const,
        peak: report.interactive_peak,
        height: 120,
      },
      {
        kind: "subagent",
        label: m.activity_subagents(),
        peakLabel: m.activity_subagent_peak(),
        field: "max_subagent_agents" as const,
        peak: report.subagent_peak,
        height: 72,
      },
      {
        kind: "automated",
        label: m.activity_automated(),
        peakLabel: m.activity_automated_peak(),
        field: "max_automated_agents" as const,
        peak: report.automated_peak,
        height: 72,
      },
    ];
    let offset = 0;
    return definitions.map((track) => {
      const top = offset + TRACK_HEADER_H;
      const bottom = top + track.height;
      offset = bottom + TRACK_GAP;
      const scale = niceScale(
        Math.max(0, ...buckets.map((bucket) => bucket[track.field])),
      );
      const ticks = Array.from(
        { length: Math.round(scale.max / scale.step) + 1 },
        (_, i) => ({
          y: bottom - ((i * scale.step) / scale.max) * track.height,
          label: (i * scale.step).toLocaleString(getLocale()),
        }),
      );
      return { ...track, top, bottom, scale, ticks };
    });
  });
  const chartH = $derived(tracks[2]!.bottom);

  // All three tracks, the activity strip, and selection share bucket bounds.
  const bars = $derived(buckets.map((bucket, idx) => {
    const start = Date.parse(bucket.start);
    const end = Date.parse(bucket.end);
    const cellX = xForMs(start);
    const cellW = Math.max(((end - start) / rangeSpanMs) * plotWidth, 1);
    const gap = Math.min(cellW * 0.2, 2);
    return { x: cellX + gap / 2, w: Math.max(cellW - gap, 1), cellX, cellW, idx };
  }));

  const selectionBounds = $derived.by(() => {
    if (!activeRange) return null;
    const first = bars[activeRange.start];
    const last = bars[activeRange.end - 1];
    if (!first || !last) return null;
    return {
      x: first.cellX,
      width: last.cellX + last.cellW - first.cellX,
    };
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

  const overlayPoints = $derived(
    overlayMetric === "none"
      ? []
      : buckets.map((bucket) => ({
          time: (Date.parse(bucket.start) + Date.parse(bucket.end)) / 2,
          value: bucketOverlayValue(bucket) / overlayMax,
        })),
  );

  const overlayTicks = $derived.by(() => {
    if (overlayMetric === "none") return [];
    const values =
      overlayDataMax <= 0 ? [0] : [0, overlayDataMax / 2, overlayDataMax];
    return values.map((val) => ({
      y: tracks[0]!.bottom - (val / overlayMax) * tracks[0]!.height,
      label: fmtOverlayTick(val),
    }));
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

  // Buckets that start at or after the effective end are entirely in the
  // future. They get no hit target, so they cannot be hovered, focused,
  // tooltipped, or selected. Buckets are chronological, so this is a prefix
  // of bars and slot indexes stay aligned.
  const liveBars = $derived(
    bars.filter((bar) => Date.parse(buckets[bar.idx]!.start) < futureStartMs),
  );

  const svgH = $derived(chartH + STRIP_GAP + STRIP_H + X_LABEL_H);
  const stripY = $derived(chartH + STRIP_GAP);

  function setOverlayMetric(value: string) {
    overlayMetric = value as "none" | "tokens" | "cost";
  }
</script>

<svelte:window
  onpointerup={finishRangeDrag}
  onpointercancel={cancelRangeDrag}
/>

<div class="timeline">
  <div class="timeline-header">
    <h3 class="timeline-title">{m.activity_concurrency()}</h3>
    <div class="panel-actions">
      {#if selectedRange}
        <Button
          size="sm"
          surface="soft"
          label={m.sidebar_clear_selection()}
          onclick={() => onSelectRange?.(null)}
        />
      {/if}
      <div class="overlay-toggle">
        <span>{m.activity_all_sessions_overlay()}</span>
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

  <p class="scale-note">{m.activity_independent_scales()}</p>

  <div
    class="timeline-body"
    role="group"
    aria-label={m.activity_concurrency()}
    bind:this={containerEl}
    onpointermove={moveRangeDrag}
  >
    <Chart
      data={overlayPoints}
      x="time"
      y="value"
      xScale={scaleLinear()}
      xDomain={[rangeStartMs, rangeEndMs]}
      yDomain={[0, 1]}
      xRange={[Y_LABEL_W, Y_LABEL_W + plotWidth]}
      yRange={[tracks[0]!.bottom, tracks[0]!.top]}
      padding={0}
      height={svgH}
    >
      <Layer class="timeline-svg">
        {#if futureW > 0}
          <Rect
            class="concurrency-future"
            data-future
            x={futureX}
            y={TOP_PAD}
            width={futureW}
            height={chartH - TOP_PAD}
          />
        {/if}

        {#each tracks as track (track.kind)}
          <g class="concurrency-track" data-track={track.kind}>
            <Text
              class={`track-label ${track.kind}`}
              value={track.label}
              x={Y_LABEL_W}
              y={track.top - 8}
            />
            <Text
              class="track-peak"
              value={trackPeakLabel(track.peak)}
              x={Y_LABEL_W + plotWidth}
              y={track.top - 8}
              textAnchor="end"
            />
            {#each track.ticks as tick}
              <Line
                x1={Y_LABEL_W}
                y1={tick.y}
                x2={Y_LABEL_W + plotWidth}
                y2={tick.y}
                class="grid-line"
              />
              <Text
                value={tick.label}
                x={Y_LABEL_W - 4}
                y={tick.y + 3}
                class="y-label"
                textAnchor="end"
              />
            {/each}
            {#each bars as bar (bar.idx)}
              {@const height = buckets[bar.idx]![track.field] / track.scale.max * track.height}
              <Rect
                class={`concurrency-seg ${track.kind}${activeRange && bar.idx >= activeRange.start && bar.idx < activeRange.end ? " selected" : ""}`}
                x={bar.x}
                y={track.bottom - height}
                width={bar.w}
                {height}
              />
            {/each}
          </g>
        {/each}

        {#if overlayMetric !== "none" && overlayPoints.length > 0}
          <Spline
            class="overlay-line"
            data={overlayPoints}
            x="time"
            y="value"
            fill="none"
          />
          <Line
            class="overlay-axis-line"
            x1={Y_LABEL_W + plotWidth}
            y1={TOP_PAD}
            x2={Y_LABEL_W + plotWidth}
            y2={tracks[0]!.bottom}
          />
          {#each overlayTicks as tick}
            <Line
              class="overlay-axis-tick"
              x1={Y_LABEL_W + plotWidth}
              y1={tick.y}
              x2={Y_LABEL_W + plotWidth + 4}
              y2={tick.y}
            />
            <Text
              value={tick.label}
              x={Y_LABEL_W + plotWidth + 6}
              y={tick.y + 3}
              class="overlay-y-label"
              textAnchor="start"
            />
          {/each}
        {/if}

        {#each xTicks as tick}
          <Text
            value={tick.label}
            x={tick.x}
            y={svgH - 4}
            class="x-label"
            textAnchor={tick.x <= Y_LABEL_W + 1
              ? "start"
              : tick.x >= Y_LABEL_W + plotWidth - 1
                ? "end"
                : "middle"}
          />
        {/each}

        {#each bars as bar (bar.idx)}
          {@const b = buckets[bar.idx]}
          <Rect
            class={`strip-cell${b !== undefined && b.max_agents > 0 ? " active" : ""}`}
            x={bar.cellX}
            y={stripY}
            width={bar.cellW}
            height={STRIP_H}
          />
        {/each}
        {#if futureW > 0}
          <Rect
            class="strip-future"
            x={futureX}
            y={stripY}
            width={futureW}
            height={STRIP_H}
          />
        {/if}

        {#if selectionBounds}
          <Rect
            class="range-selection"
            x={selectionBounds.x}
            y={TOP_PAD}
            width={selectionBounds.width}
            height={stripY + STRIP_H - TOP_PAD}
          />
        {/if}

        {#each liveBars as bar (bar.idx)}
          {@const b = buckets[bar.idx]}
          <Rect
            class="slot-hit"
            data-bucket-bar
            data-concurrency-bucket-index={bar.idx}
            x={bar.cellX}
            y={TOP_PAD}
            width={bar.cellW}
            height={stripY + STRIP_H - TOP_PAD}
            role="button"
            tabindex={0}
            aria-pressed={activeRange !== null && bar.idx >= activeRange.start && bar.idx < activeRange.end}
            aria-label={m.activity_filter_active_in_range()}
            onmouseenter={(event) => b && showSlotTip(event, b)}
            onmouseleave={hideTip}
            onpointerdown={(event) => beginRangeDrag(event, bar.idx)}
            onpointerenter={() => extendRangeDrag(bar.idx)}
            onkeydown={(event) => onSlotKey(event, bar.idx)}
          />
        {/each}
      </Layer>
    </Chart>

    {#if tooltip}
      <div
        bind:this={tooltipEl}
        class="tooltip"
        style={tooltipPos
          ? `left: ${tooltipPos.left}px; top: ${tooltipPos.top}px;`
          : "visibility: hidden;"}
      >
        <div class="tooltip-date">{fmtBucketRange(tooltip.bucket)}</div>
        <dl class="tooltip-metrics">
          {#each tracks as track (track.kind)}
            <div>
              <dt>{track.peakLabel}</dt>
              <dd>{tooltip.bucket[track.field].toLocaleString(getLocale())}</dd>
            </div>
          {/each}
          <div>
            <dt>{m.activity_combined_peak()}</dt>
            <dd>{tooltip.bucket.max_agents.toLocaleString(getLocale())}</dd>
          </div>
          <div>
            <dt>{m.activity_agent_min()}</dt>
            <dd>{fmtCompactValue(tooltip.bucket.agent_minutes)}</dd>
          </div>
          <div>
            <dt>{m.usage_input_tokens()}</dt>
            <dd>{fmtCompactValue(tooltip.bucket.input_tokens ?? 0)}</dd>
          </div>
          <div>
            <dt>{m.usage_output_tokens()}</dt>
            <dd>{fmtCompactValue(tooltip.bucket.output_tokens)}</dd>
          </div>
          <div>
            <dt>{m.activity_cost()}</dt>
            <dd>{formatMoney(tooltip.bucket.cost)}</dd>
          </div>
        </dl>
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
    flex-wrap: wrap;
    gap: 8px;
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

  .panel-actions :global(.kit-button.kit-button--sm) {
    height: 22px;
    min-height: 22px;
    padding: 0 8px;
    font-size: 10px;
  }

  .scale-note {
    margin: 0 0 8px;
    font-size: 10px;
    color: var(--text-muted);
  }

  .timeline :global(.track-label) {
    font-size: 11px;
    font-weight: 600;
    fill: var(--text-secondary);
  }

  .timeline :global(.track-label.interactive) {
    fill: var(--accent-blue);
  }

  .timeline :global(.track-label.subagent) {
    fill: var(--accent-violet);
  }

  .timeline :global(.track-label.automated) {
    fill: var(--accent-orange);
  }

  .timeline :global(.track-peak) {
    font-size: 10px;
    fill: var(--text-muted);
  }

  .overlay-toggle {
    display: flex;
    flex-shrink: 0;
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

  .overlay-toggle :global(.kit-typeahead) {
    flex: 0 0 96px;
    width: 96px;
  }

  .timeline-body {
    width: 100%;
  }

  .timeline :global(.timeline-svg) {
    display: block;
  }

  .timeline :global(.grid-line) {
    stroke: var(--border-muted);
    stroke-width: 1;
    stroke-dasharray: 2 2;
  }

  .timeline :global(.y-label) {
    font-size: 9px;
    fill: var(--text-muted);
    font-family: var(--font-mono);
  }

  .timeline :global(.x-label) {
    font-size: 9px;
    fill: var(--text-muted);
    font-family: var(--font-mono);
  }

  .timeline :global(.concurrency-seg) {
    opacity: 0.75;
  }

  .timeline :global(.concurrency-seg.interactive) {
    fill: var(--accent-blue);
  }

  .timeline :global(.concurrency-seg.subagent) {
    fill: var(--accent-violet);
  }

  .timeline :global(.concurrency-seg.automated) {
    fill: var(--accent-orange);
  }

  .timeline :global(.concurrency-seg.selected) {
    opacity: 1;
  }

  .timeline :global(.range-selection) {
    fill: var(--accent-blue);
    fill-opacity: 0.16;
    stroke: var(--accent-blue);
    stroke-opacity: 1;
    stroke-width: 1.5;
    pointer-events: none;
  }

  .timeline :global(.concurrency-future) {
    fill: var(--bg-inset);
    opacity: 0.5;
  }

  .timeline :global(.overlay-line) {
    fill: none;
    stroke: var(--accent-amber);
    stroke-width: 1.5;
    opacity: 0.85;
  }

  .timeline :global(.overlay-axis-line),
  .timeline :global(.overlay-axis-tick) {
    stroke: var(--accent-amber);
    stroke-width: 1;
    opacity: 0.55;
  }

  .timeline :global(.overlay-y-label) {
    font-size: 9px;
    fill: var(--accent-amber);
    font-family: var(--font-mono);
  }

  .timeline :global(.strip-cell) {
    fill: var(--bg-inset);
    stroke: var(--bg-surface);
    stroke-width: 0.5;
  }

  .timeline :global(.strip-cell.active) {
    fill: var(--accent-blue);
    opacity: 0.55;
  }

  .timeline :global(.strip-future) {
    fill: var(--bg-inset);
    opacity: 0.5;
  }

  .timeline :global(.slot-hit) {
    fill: transparent;
    cursor: pointer;
  }

  .timeline :global(.slot-hit:hover) {
    fill: var(--accent-blue);
    opacity: 0.08;
  }

  .timeline :global(.slot-hit:focus-visible) {
    outline: 1px solid var(--accent-blue);
    outline-offset: -1px;
  }

  .tooltip {
    position: fixed;
    min-width: 168px;
    padding: 8px 10px;
    background: var(--text-primary);
    color: var(--bg-primary);
    font-size: 11px;
    border-radius: var(--radius-sm);
    white-space: nowrap;
    pointer-events: none;
    z-index: var(--z-tooltip);
  }

  .tooltip-date {
    padding-bottom: 6px;
    border-bottom: 1px solid color-mix(in srgb, currentColor 18%, transparent);
    font-weight: 600;
  }

  .tooltip-metrics {
    display: grid;
    gap: 4px;
    margin: 6px 0 0;
  }

  .tooltip-metrics > div {
    display: grid;
    grid-template-columns: 1fr auto;
    gap: 16px;
    align-items: baseline;
  }

  .tooltip-metrics dt {
    opacity: 0.7;
  }

  .tooltip-metrics dd {
    margin: 0;
    font-family: var(--font-mono);
    font-weight: 600;
    text-align: right;
  }
</style>
