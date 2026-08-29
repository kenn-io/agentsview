<script lang="ts">
  import { Button } from "@kenn-io/kit-ui";
  import type { Message } from "../../api/types.js";
  import type { SessionTiming } from "../../api/types/timing.js";
  import { formatDateTime, m } from "../../i18n/index.js";
  import { liveTick } from "../../stores/liveTick.svelte.js";
  import { categoryToken } from "../../utils/categoryToken.js";
  import { formatDuration } from "../../utils/duration.js";
  import { displayToolName } from "../../utils/toolDisplay.js";
  import {
    deriveTimingOverview,
    nearestTimingOverviewSpan,
    orderedTimingOverviewRange,
    timingOverviewFocusOrdinals,
    timingOverviewSpanIntersects,
    type TimingOverviewLane,
    type TimingOverviewRange,
    type TimingOverviewSpan,
  } from "../../utils/timingOverview.js";

  interface Props {
    messages: readonly Message[];
    timing: SessionTiming;
    sessionStartedAt?: string | null;
    sessionEndedAt?: string | null;
    hasEarlierMessages?: boolean;
    loadingEarlierMessages?: boolean;
    categoryFilter?: string | null;
    onLoadEarlier?: () => void | Promise<void>;
    onNavigate: (ordinal: number) => void;
  }

  let {
    messages,
    timing,
    sessionStartedAt,
    sessionEndedAt,
    hasEarlierMessages = false,
    loadingEarlierMessages = false,
    categoryFilter = null,
    onLoadEarlier,
    onNavigate,
  }: Props = $props();

  const MINIMUM_DRAG_PX = 3;
  const MINIMUM_VISIBLE_SPAN_PX = 2;
  const laneOrder: TimingOverviewLane[] = ["input", "model", "tools"];

  let trackEl: HTMLDivElement | undefined = $state(undefined);
  let selection = $state<TimingOverviewRange | null>(null);
  let draftSelection = $state<TimingOverviewRange | null>(null);
  let viewport = $state<TimingOverviewRange | null>(null);
  let hoverFraction = $state<number | null>(null);
  let gesture = $state<{
    pointerId: number;
    button: number;
    anchorClientX: number;
    anchorTimeMs: number;
    viewportStartMs: number;
    moved: boolean;
  } | null>(null);
  let activeSessionId: string | null = null;

  let model = $derived(deriveTimingOverview(messages, timing, {
    sessionStartedAt,
    sessionEndedAt,
    hasEarlierMessages,
  }));

  $effect(() => {
    const sessionId = timing.session_id;
    if (activeSessionId === null) {
      activeSessionId = sessionId;
      return;
    }
    if (sessionId === activeSessionId) return;
    activeSessionId = sessionId;
    selection = null;
    draftSelection = null;
    viewport = null;
  });

  let modelEndMs = $derived(
    model
      ? timing.running
        ? Math.max(model.endMs, liveTick.now)
        : model.endMs
      : 1,
  );

  $effect(() => {
    if (!model || !viewport) return;
    if (viewport.endMs <= model.startMs || viewport.startMs >= modelEndMs) {
      viewport = null;
    }
  });

  let fullDurationMs = $derived(
    model ? Math.max(1, modelEndMs - model.startMs) : 1,
  );
  let domainStartMs = $derived(viewport?.startMs ?? model?.startMs ?? 0);
  let domainEndMs = $derived(viewport?.endMs ?? modelEndMs);
  let domainDurationMs = $derived(Math.max(1, domainEndMs - domainStartMs));
  let activeSelection = $derived(draftSelection ?? selection);
  let selectedOrdinals = $derived(
    model && selection
      ? timingOverviewFocusOrdinals(model, selection)
      : [],
  );
  let visibleSpans = $derived.by(() => {
    if (!model) return [];
    return model.spans.filter((span) =>
      span.endMs >= domainStartMs && span.startMs <= domainEndMs
    );
  });
  let lanes = $derived([
    { id: "input" as const, label: m.session_vitals_timeline_input() },
    { id: "model" as const, label: m.session_vitals_timeline_model() },
    { id: "tools" as const, label: m.session_vitals_timeline_tools() },
  ]);

  function laneLabel(lane: TimingOverviewLane): string {
    return lanes.find((item) => item.id === lane)?.label ?? lane;
  }

  function fractionAt(clientX: number): number {
    if (!trackEl) return 0;
    const rect = trackEl.getBoundingClientRect();
    return Math.min(1, Math.max(0, (clientX - rect.left) / Math.max(1, rect.width)));
  }

  function timeAt(clientX: number): number {
    return domainStartMs + fractionAt(clientX) * domainDurationMs;
  }

  function spanGeometry(span: TimingOverviewSpan): {
    leftPct: number;
    widthPct: number;
  } {
    const clippedStart = Math.max(domainStartMs, span.startMs);
    const clippedEnd = Math.min(domainEndMs, span.endMs);
    return {
      leftPct: ((clippedStart - domainStartMs) / domainDurationMs) * 100,
      widthPct: Math.max(0, ((clippedEnd - clippedStart) / domainDurationMs) * 100),
    };
  }

  function rangeGeometry(range: TimingOverviewRange): {
    leftPct: number;
    widthPct: number;
  } {
    const start = Math.max(domainStartMs, Math.min(domainEndMs, range.startMs));
    const end = Math.max(domainStartMs, Math.min(domainEndMs, range.endMs));
    return {
      leftPct: ((start - domainStartMs) / domainDurationMs) * 100,
      widthPct: Math.max(0, ((end - start) / domainDurationMs) * 100),
    };
  }

  function spanColor(span: TimingOverviewSpan): string {
    if (span.errored) return "var(--accent-red)";
    if (span.lane === "input") return "var(--accent-blue)";
    if (span.lane === "model") return "var(--accent-purple)";
    return categoryToken(span.category ?? "Other");
  }

  function spanTitle(span: TimingOverviewSpan): string {
    const lane = laneLabel(span.lane);
    const label = span.lane === "tools"
      ? displayToolName({
        tool_name: span.label,
        category: span.category,
      })
      : span.label;
    const heading = label ? `${lane} · ${label}` : lane;
    const start = formatDateTime(span.startMs, {
      hour: "2-digit",
      minute: "2-digit",
      second: "2-digit",
      fractionalSecondDigits: 3,
    });
    if (span.running) {
      return `${heading}\n${start} · ${m.session_vitals_running()}`;
    }
    if (span.endMs <= span.startMs) return `${heading}\n${start}`;
    const end = formatDateTime(span.endMs, {
      hour: "2-digit",
      minute: "2-digit",
      second: "2-digit",
      fractionalSecondDigits: 3,
    });
    const duration = `${span.approximate ? "≤" : ""}${formatDuration(
      span.endMs - span.startMs,
    )}`;
    return `${heading}\n${start} → ${end} · ${duration}`;
  }

  function clearSelection(): void {
    selection = null;
    draftSelection = null;
  }

  function resetView(): void {
    viewport = null;
  }

  function focusRange(range: TimingOverviewRange): void {
    if (!model) return;
    let nextRange = range;
    let ordinals = timingOverviewFocusOrdinals(model, nextRange);
    if (ordinals.length === 0) {
      const midpoint = (nextRange.startMs + nextRange.endMs) / 2;
      const nearest = nearestTimingOverviewSpan(model, midpoint);
      nextRange = { startMs: nearest.startMs, endMs: nearest.endMs };
      ordinals = [nearest.ordinal];
    }
    selection = nextRange;
    onNavigate(ordinals[0]!);
  }

  function beginPointer(event: PointerEvent): void {
    if (!model || (event.button !== 0 && event.button !== 2)) return;
    const target = event.target instanceof Element ? event.target : null;
    if (target?.closest("[data-overview-control]")) return;
    const anchorTimeMs = timeAt(event.clientX);
    gesture = {
      pointerId: event.pointerId,
      button: event.button,
      anchorClientX: event.clientX,
      anchorTimeMs,
      viewportStartMs: domainStartMs,
      moved: false,
    };
    if (event.button === 0) {
      draftSelection = { startMs: anchorTimeMs, endMs: anchorTimeMs };
    }
    if (typeof trackEl?.setPointerCapture === "function") {
      trackEl.setPointerCapture(event.pointerId);
    }
  }

  function movePointer(event: PointerEvent): void {
    hoverFraction = fractionAt(event.clientX);
    if (!gesture || gesture.pointerId !== event.pointerId || !model) return;
    const deltaX = event.clientX - gesture.anchorClientX;
    if (Math.abs(deltaX) >= MINIMUM_DRAG_PX) gesture.moved = true;

    if (gesture.button === 2) {
      if (!viewport || !trackEl) return;
      const rect = trackEl.getBoundingClientRect();
      const nextStart = Math.min(
        Math.max(
          gesture.viewportStartMs - (deltaX / Math.max(1, rect.width)) * domainDurationMs,
          model.startMs,
        ),
        modelEndMs - domainDurationMs,
      );
      viewport = { startMs: nextStart, endMs: nextStart + domainDurationMs };
      return;
    }

    draftSelection = orderedTimingOverviewRange(
      gesture.anchorTimeMs,
      timeAt(event.clientX),
    );
  }

  function finishPointer(event: PointerEvent): void {
    if (!gesture || gesture.pointerId !== event.pointerId || !model) return;
    const completed = gesture;
    gesture = null;

    if (completed.button === 2) {
      if (!completed.moved) clearSelection();
      return;
    }

    const range = orderedTimingOverviewRange(
      completed.anchorTimeMs,
      timeAt(event.clientX),
    );
    draftSelection = null;
    if (completed.moved) {
      focusRange(range);
      return;
    }
    const nearest = nearestTimingOverviewSpan(model, range.startMs);
    selection = { startMs: nearest.startMs, endMs: nearest.endMs };
    onNavigate(nearest.ordinal);
  }

  function cancelPointer(): void {
    gesture = null;
    draftSelection = null;
  }

  function zoom(event: WheelEvent): void {
    if (!model) return;
    event.preventDefault();
    const anchor = fractionAt(event.clientX);
    const minimumDuration = Math.min(
      fullDurationMs,
      Math.max(10, fullDurationMs / Math.max(model.spans.length, 8)),
    );
    const nextDuration = Math.min(
      fullDurationMs,
      Math.max(minimumDuration, domainDurationMs * Math.exp(event.deltaY * 0.0015)),
    );
    if (nextDuration >= fullDurationMs * 0.999) {
      viewport = null;
      return;
    }
    const anchorTime = domainStartMs + anchor * domainDurationMs;
    const nextStart = Math.min(
      Math.max(anchorTime - anchor * nextDuration, model.startMs),
      modelEndMs - nextDuration,
    );
    viewport = { startMs: nextStart, endMs: nextStart + nextDuration };
  }

  function handleKeydown(event: KeyboardEvent): void {
    if (event.key === "Escape") {
      if (selection) {
        event.preventDefault();
        clearSelection();
      } else if (viewport) {
        event.preventDefault();
        resetView();
      }
      return;
    }
    if (event.key === "0" && viewport) {
      event.preventDefault();
      resetView();
    }
  }

  function spanIsDimmed(span: TimingOverviewSpan): boolean {
    if (activeSelection && !timingOverviewSpanIntersects(span, activeSelection)) {
      return true;
    }
    return categoryFilter !== null &&
      span.lane === "tools" &&
      span.category !== categoryFilter;
  }
</script>

<section class="overview" aria-label={m.session_vitals_timeline()}>
  <header class="overview-header">
    <span>{m.session_vitals_timeline()}</span>
    <span class="overview-actions">
      {#if selection}
        <span class="selection-count">
          {m.sidebar_selected_count({ countLabel: String(selectedOrdinals.length) })}
        </span>
        <Button
          size="sm"
          surface="soft"
          label={m.sidebar_clear_selection()}
          onclick={clearSelection}
        />
      {:else if viewport}
        <Button
          size="sm"
          surface="soft"
          label={m.session_vitals_timeline_reset_view()}
          onclick={resetView}
        />
      {:else}
        <span class="overview-hint">
          {m.session_vitals_timeline_hint()}
        </span>
      {/if}
    </span>
  </header>

  {#if model}
    <div class="overview-plot">
      <div class="lane-labels" aria-hidden="true">
        {#each lanes as lane (lane.id)}
          <span>{lane.label}</span>
        {/each}
      </div>
      <!-- The plot is one compound pointer/keyboard widget; its child spans remain independently clickable. -->
      <!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
      <!-- svelte-ignore a11y_no_noninteractive_tabindex -->
      <div
        class="overview-track"
        class:panning={gesture?.button === 2 && gesture.moved}
        bind:this={trackEl}
        role="application"
        tabindex="0"
        aria-label={`${m.session_vitals_timeline()}; ${m.session_vitals_timeline_hint()}`}
        onpointerdown={beginPointer}
        onpointermove={movePointer}
        onpointerup={finishPointer}
        onpointercancel={cancelPointer}
        onpointerleave={() => {
          if (!gesture) hoverFraction = null;
        }}
        onwheel={zoom}
        onkeydown={handleKeydown}
        ondblclick={() => {
          clearSelection();
          resetView();
        }}
        oncontextmenu={(event) => event.preventDefault()}
      >
        <div class="grid-lines" aria-hidden="true"></div>
        {#if hoverFraction !== null && !gesture}
          <span
            class="hover-line"
            style={`left: ${hoverFraction * 100}%;`}
            aria-hidden="true"
          ></span>
        {/if}
        {#if activeSelection}
          {@const range = rangeGeometry(activeSelection)}
          <span
            class="selection"
            style={`left: ${range.leftPct}%; width: ${range.widthPct}%;`}
            aria-hidden="true"
          ></span>
        {/if}
        {#if hasEarlierMessages && domainStartMs <= model.startMs}
          <button
            type="button"
            class="earlier"
            data-overview-control
            disabled={loadingEarlierMessages || !onLoadEarlier}
            title={loadingEarlierMessages
              ? m.sidebar_loading()
              : m.session_vitals_timeline_load_earlier()}
            aria-label={loadingEarlierMessages
              ? m.sidebar_loading()
              : m.session_vitals_timeline_load_earlier()}
            onclick={() => {
              void onLoadEarlier?.();
            }}
          >…</button>
        {/if}
        {#each laneOrder as lane, laneIndex (lane)}
          {#each visibleSpans.filter((span) => span.lane === lane) as span (span.key)}
            {@const geometry = spanGeometry(span)}
            <button
              type="button"
              class="span"
              class:point={span.endMs <= span.startMs}
              class:approximate={span.approximate && span.endMs > span.startMs}
              class:running={span.running}
              class:dimmed={spanIsDimmed(span)}
              class:selected={activeSelection !== null &&
                timingOverviewSpanIntersects(span, activeSelection)}
              data-overview-control
              data-overview-span={span.key}
              data-lane={span.lane}
              style={`--span-left: ${geometry.leftPct}%; --span-width: ${geometry.widthPct}%; --span-lane: ${laneIndex}; --span-color: ${spanColor(span)}; --span-min-width: ${MINIMUM_VISIBLE_SPAN_PX}px;`}
              title={spanTitle(span)}
              aria-label={spanTitle(span)}
              onclick={() => {
                selection = null;
                onNavigate(span.ordinal);
              }}
            ></button>
          {/each}
        {/each}
      </div>
    </div>
    <div class="overview-axis" aria-hidden="true">
      <span>{formatDuration(domainStartMs - model.startMs)}</span>
      <span>{formatDuration((domainStartMs + domainEndMs) / 2 - model.startMs)}</span>
      <span>{formatDuration(domainEndMs - model.startMs)}</span>
    </div>
  {/if}
</section>

<style>
  .overview {
    min-width: 0;
  }

  .overview-header {
    min-height: 22px;
    margin-bottom: 6px;
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 8px;
    color: var(--text-muted);
    font-size: 9px;
    font-weight: 500;
    letter-spacing: 0.6px;
    text-transform: uppercase;
  }

  .overview-actions {
    min-width: 0;
    display: inline-flex;
    align-items: center;
    justify-content: flex-end;
    gap: 6px;
  }

  .overview-actions :global(.kit-button) {
    min-height: 20px;
    padding-block: 1px;
    font-size: 9px;
    letter-spacing: 0;
    text-transform: none;
  }

  .overview-hint,
  .selection-count {
    overflow: hidden;
    color: var(--text-muted);
    font-family: var(--font-mono);
    font-size: 9px;
    font-weight: 400;
    letter-spacing: 0;
    text-overflow: ellipsis;
    text-transform: none;
    white-space: nowrap;
  }

  .overview-plot {
    display: grid;
    grid-template-columns: 44px minmax(0, 1fr);
    height: 64px;
    overflow: hidden;
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-sm);
    background: var(--bg-inset);
  }

  .lane-labels {
    display: grid;
    grid-template-rows: repeat(3, 1fr);
    border-right: 1px solid var(--border-muted);
    color: var(--text-muted);
    font-family: var(--font-mono);
    font-size: 9px;
  }

  .lane-labels span {
    display: flex;
    align-items: center;
    justify-content: flex-end;
    padding-right: 5px;
  }

  .overview-track {
    position: relative;
    min-width: 0;
    overflow: hidden;
    cursor: crosshair;
    touch-action: none;
  }

  .overview-track.panning {
    cursor: grabbing;
  }

  .overview-track:focus-visible {
    outline: 2px solid var(--accent-blue);
    outline-offset: -2px;
  }

  .grid-lines {
    position: absolute;
    inset: 0;
    background-image: linear-gradient(
      to right,
      transparent calc(25% - 0.5px),
      var(--border-muted) calc(25% - 0.5px),
      var(--border-muted) calc(25% + 0.5px),
      transparent calc(25% + 0.5px)
    );
    background-size: 25% 100%;
    opacity: 0.65;
    pointer-events: none;
  }

  .span {
    position: absolute;
    z-index: 2;
    top: calc(7px + var(--span-lane) * 20px);
    left: var(--span-left);
    width: max(var(--span-min-width), var(--span-width));
    height: 8px;
    padding: 0;
    border: 0;
    border-radius: 1px;
    background: var(--span-color);
    opacity: 0.84;
    cursor: pointer;
    transition: opacity 0.12s, filter 0.12s, box-shadow 0.12s;
  }

  .span.point {
    width: 3px;
    min-width: 3px;
  }

  .span.approximate {
    background-image: repeating-linear-gradient(
      135deg,
      color-mix(in srgb, var(--span-color) 82%, transparent) 0 4px,
      color-mix(in srgb, var(--span-color) 42%, transparent) 4px 7px
    );
  }

  .span.running {
    background: var(--running-fg);
    animation: duration-pulse 1.6s ease-in-out infinite;
  }

  .span:hover,
  .span:focus-visible,
  .span.selected {
    z-index: 4;
    opacity: 1;
    filter: brightness(1.12);
    box-shadow:
      0 0 0 1px var(--bg-inset),
      0 0 0 2px var(--text-primary);
    outline: none;
  }

  .span.dimmed {
    opacity: 0.18;
  }

  .selection {
    position: absolute;
    z-index: 1;
    inset-block: 0;
    min-width: 1px;
    border-inline: 1px solid color-mix(in srgb, var(--accent-blue) 72%, transparent);
    background: color-mix(in srgb, var(--accent-blue) 12%, transparent);
    box-shadow:
      -100vw 0 0 100vw color-mix(in srgb, var(--bg-surface) 52%, transparent),
      100vw 0 0 100vw color-mix(in srgb, var(--bg-surface) 52%, transparent);
    pointer-events: none;
  }

  .hover-line {
    position: absolute;
    z-index: 3;
    inset-block: 0;
    width: 1px;
    background: color-mix(in srgb, var(--accent-blue) 76%, transparent);
    pointer-events: none;
  }

  .earlier {
    position: absolute;
    z-index: 5;
    inset-block: 0;
    left: 0;
    width: 24px;
    padding: 0 0 0 3px;
    border: 0;
    background: linear-gradient(
      to right,
      var(--bg-inset) 0 42%,
      transparent 100%
    );
    color: var(--text-muted);
    font-family: var(--font-mono);
    text-align: left;
    cursor: pointer;
  }

  .earlier:hover:not(:disabled),
  .earlier:focus-visible {
    color: var(--text-primary);
    outline: none;
  }

  .earlier:disabled {
    cursor: default;
    opacity: 0.5;
  }

  .overview-axis {
    display: flex;
    justify-content: space-between;
    padding: 4px 1px 0 45px;
    color: var(--text-muted);
    font-family: var(--font-mono);
    font-size: 8px;
    font-variant-numeric: tabular-nums;
  }

  @media (prefers-reduced-motion: reduce) {
    .span {
      transition: none;
    }

    .span.running {
      animation: none;
    }
  }
</style>
