<script lang="ts">
  import { RefreshControl as KitRefreshControl } from "@kenn-io/kit-ui";
  import { untrack, type ComponentProps } from "svelte";
  import { formatDateTime, getLocale, m } from "../../i18n/index.js";
  import type { LiveQuery } from "../../utils/liveQuery.svelte.js";
  import {
    formatQueryDuration,
    formatQueryPhaseLabel,
    formatQueryStepLabel,
    formatQueryTick,
    formatRefreshProgress,
    formatRefreshStatus,
    queryAxisTicks,
    refreshStatusWidthSamples,
    type QueryPhase,
    type QueryStep,
  } from "../../utils/refresh.js";

  // Thin wrapper over kit-ui's RefreshControl: injects the app's localized
  // label (age plus last-query duration via formatRefreshStatus), the current
  // app locale, the localized width samples that keep the label box a
  // constant width, and a hover breakdown of the last query's steps, so
  // pages pass only data props — mirroring shared/RangePicker.svelte.
  // While `liveQuery` has a query running, the duration counts up and the
  // breakdown shows that query's steps as they start and finish.

  type Props = Omit<
    ComponentProps<typeof KitRefreshControl>,
    "formatAge" | "locale" | "ageWidthSamples" | "ageTooltip"
  > & {
    /** Replaces the relative age while a parent operation reports progress. */
    status?: string;
    /** Every text `status` can take, so the label box is sized for them
     * too. Read once at mount. */
    statusWidthSamples?: readonly string[];
    /** Wall-clock time of the page's most recent data query, request start
     * to data applied. Shown after the age label; null before the first
     * query completes. */
    queryDurationMs?: number | null;
    /** Per-step timings behind `queryDurationMs`, in execution order. Shown
     * as a list when the label is hovered or focused. */
    querySteps?: readonly QueryStep[];
    /** The page's in-flight query. While it runs, it replaces
     * `queryDurationMs` and `querySteps`. */
    liveQuery?: LiveQuery;
  };

  let {
    status = undefined,
    statusWidthSamples = [],
    queryDurationMs = null,
    querySteps = [],
    liveQuery = undefined,
    lastUpdatedAt,
    ...rest
  }: Props = $props();

  // Locale is fixed for the life of a page load (a language change reloads),
  // so the samples are computed once per mount.
  const ageWidthSamples = refreshStatusWidthSamples(untrack(() => statusWidthSamples));

  // A running query's clock. Ticks only while a query runs, often enough
  // for a sub-second count to read as live without redrawing needlessly.
  const LIVE_TICK_MS = 100;
  const liveStartedAt = $derived(liveQuery?.startedAt ?? null);
  let now = $state(performance.now());
  $effect(() => {
    if (liveStartedAt === null) return;
    now = performance.now();
    const timer = setInterval(() => {
      now = performance.now();
    }, LIVE_TICK_MS);
    return () => clearInterval(timer);
  });
  const elapsedMs = $derived(liveStartedAt === null ? null : Math.max(0, now - liveStartedAt));
  const durationMs = $derived(elapsedMs ?? queryDurationMs);
  const steps = $derived.by((): readonly QueryStep[] => {
    if (elapsedMs === null || liveQuery === undefined) return querySteps;
    return liveQuery.steps.map((step) =>
      step.running ? { ...step, durationMs: Math.max(0, elapsedMs - step.startMs) } : step,
    );
  });
  // The last query's steps would be stale next to a progress status, so the
  // breakdown only shows beside one while a query is running. A running
  // query keeps the breakdown up before its first step starts, so an open
  // popover stays open across the switch from the last query to the next.
  const showSteps = $derived(
    elapsedMs !== null || (status === undefined && steps.length > 0),
  );

  // Time zero on the axis is the first request going out, not the refresh
  // being asked for: the sub-millisecond setup before the first send would
  // otherwise nudge every bar off the zero line. The axis runs to the last
  // step's end, or to the recorded total if that is later. A running query
  // gets at least a second of axis: in its first milliseconds the axis
  // would otherwise be as short as the bars, drawing them full width and
  // spreading sub-millisecond start gaps across the track.
  const LIVE_MIN_AXIS_MS = 1000;
  const originMs = $derived(
    steps.length === 0 ? 0 : Math.min(...steps.map((step) => step.startMs)),
  );
  const axisMs = $derived(
    Math.max(
      (durationMs ?? 0) - originMs,
      ...steps.map((step) => step.startMs + step.durationMs - originMs),
      elapsedMs === null ? 1 : LIVE_MIN_AXIS_MS,
    ),
  );
  // Requests fired in one dispatch burst leave a millisecond or two apart,
  // which at this scale draws as a false stagger off the zero line. Anything
  // starting within two pixels of the origin is drawn flush with it; later
  // starts keep their real offset.
  const TRACK_PX = 200;
  const snapMs = $derived((2 * axisMs) / TRACK_PX);
  function shiftMs(step: QueryStep): number {
    const startMs = step.startMs - originMs;
    return startMs < snapMs ? step.startMs : originMs;
  }
  const ticks = $derived(queryAxisTicks(axisMs));
  const hasSegments = $derived(steps.some((step) => step.segments !== undefined));
  const PHASES: QueryPhase[] = ["wait", "download", "apply"];

  function percent(ms: number): string {
    return ((100 * ms) / axisMs).toFixed(2);
  }

  function barStyle(startMs: number, durationMs: number, shift: number): string {
    return `left: ${percent(startMs - shift)}%; width: ${percent(durationMs)}%`;
  }

  // Tick labels centre on their line. A centred label may spill into the
  // column gap, but one at the very end of the track would run into the
  // duration column, so it hangs to the left of its line instead.
  function tickStyle(tick: number): string {
    const pct = (100 * tick) / axisMs;
    const shift = pct > 98 ? "-100%" : "-50%";
    return `left: ${pct.toFixed(2)}%; transform: translateX(${shift})`;
  }
</script>

<KitRefreshControl
  {...rest}
  lastUpdatedAt={status === undefined ? lastUpdatedAt : null}
  formatAge={status === undefined
    ? (at, now) => formatRefreshStatus(at, durationMs, now)
    : () => formatRefreshProgress(status ?? "", elapsedMs)}
  locale={getLocale()}
  {ageWidthSamples}
  ageTooltip={showSteps ? querySteps_tooltip : undefined}
/>

{#snippet querySteps_tooltip()}
  <div class="query-steps">
    <div class="query-steps__head">
      {#if elapsedMs !== null}
        <span class="query-steps__at">{m.shared_refresh_in_progress()}</span>
      {:else if lastUpdatedAt != null}
        <span class="query-steps__at">
          {formatDateTime(lastUpdatedAt, { dateStyle: "medium", timeStyle: "medium" })}
        </span>
      {/if}
      <span class="query-steps__total">{formatQueryDuration(durationMs)}</span>
    </div>
    <div class="query-steps__list">
      <span></span>
      <span class="query-steps__axis" aria-hidden="true">
        {#each ticks as tick (tick)}
          <span class="query-steps__tick" style={tickStyle(tick)}>
            {formatQueryTick(tick)}
          </span>
        {/each}
      </span>
      <span></span>
      {#each steps as step (step.name)}
        {@const shift = shiftMs(step)}
        <span class="query-steps__name">{formatQueryStepLabel(step.name)}</span>
        <span class="query-steps__track" aria-hidden="true">
          {#each ticks as tick (tick)}
            <span class="query-steps__grid" style={`left: ${percent(tick)}%`}></span>
          {/each}
          {#if step.segments}
            {#each step.segments as segment (segment.phase)}
              <span
                class={`query-steps__bar query-steps__bar--${segment.phase}`}
                style={barStyle(segment.startMs, segment.durationMs, shift)}
              ></span>
            {/each}
          {:else}
            <span
              class="query-steps__bar query-steps__bar--wait"
              style={barStyle(step.startMs, step.durationMs, shift)}
            ></span>
          {/if}
        </span>
        <span class="query-steps__duration">{formatQueryDuration(step.durationMs)}</span>
      {/each}
    </div>
    {#if hasSegments}
      <div class="query-steps__legend">
        {#each PHASES as phase (phase)}
          <span class="query-steps__legend-item">
            <span class={`query-steps__swatch query-steps__bar--${phase}`}></span>
            {formatQueryPhaseLabel(phase)}
          </span>
        {/each}
      </div>
    {/if}
  </div>
{/snippet}

<style>
  .query-steps {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    font-size: var(--font-size-xs);
  }

  .query-steps__head {
    display: flex;
    justify-content: space-between;
    gap: var(--space-5);
  }

  .query-steps__at {
    color: var(--text-muted);
  }

  .query-steps__total {
    font-variant-numeric: tabular-nums;
  }

  /* Devtools-style timeline: name, a track on the shared time axis, duration. */
  /* Rows touch (no row gap) and each track stretches to the full row height,
   * so the per-row gridlines join into one continuous line per tick. Row
   * rhythm comes from the text cells' padding instead. */
  .query-steps__list {
    display: grid;
    grid-template-columns: max-content 200px max-content; /* track = TRACK_PX */
    column-gap: var(--space-4);
    align-items: center;
  }

  .query-steps__name,
  .query-steps__duration {
    padding: 2px 0;
  }

  .query-steps__name {
    color: var(--text-secondary);
  }

  .query-steps__axis {
    position: relative;
    height: 1.4em;
    color: var(--text-muted);
    font-variant-numeric: tabular-nums;
  }

  .query-steps__tick {
    position: absolute;
    bottom: 0;
    white-space: nowrap;
  }

  .query-steps__track {
    position: relative;
    align-self: stretch;
  }

  /* Centred on the tick position, so a bar starting at zero begins at the
   * middle of the zero line instead of hiding it. */
  .query-steps__grid {
    position: absolute;
    top: 0;
    bottom: 0;
    width: 2px;
    transform: translateX(-50%);
    background: var(--border-muted);
  }

  .query-steps__bar {
    position: absolute;
    top: calc(50% - 4px);
    height: 8px;
    min-width: 1px;
  }

  /* Square ends: a rounded start would sit visibly right of the zero line. */
  .query-steps__bar--wait {
    background: var(--accent-blue);
  }

  .query-steps__bar--download {
    background: color-mix(in srgb, var(--accent-blue) 45%, transparent);
  }

  .query-steps__bar--apply {
    background: color-mix(in srgb, var(--text-muted) 60%, transparent);
  }

  .query-steps__duration {
    text-align: end;
    font-variant-numeric: tabular-nums;
    color: var(--text-primary);
  }

  .query-steps__legend {
    display: flex;
    gap: var(--space-4);
    color: var(--text-muted);
  }

  .query-steps__legend-item {
    display: inline-flex;
    align-items: center;
    gap: var(--space-2);
  }

  .query-steps__swatch {
    width: 10px;
    height: 8px;
    border-radius: 2px;
  }
</style>
