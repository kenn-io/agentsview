<script lang="ts">
  import { RefreshControl as KitRefreshControl } from "@kenn-io/kit-ui";
  import type { ComponentProps } from "svelte";
  import { formatDateTime, getLocale } from "../../i18n/index.js";
  import {
    formatQueryDuration,
    formatQueryPhaseLabel,
    formatQueryStepLabel,
    formatQueryTick,
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

  type Props = Omit<
    ComponentProps<typeof KitRefreshControl>,
    "formatAge" | "locale" | "ageWidthSamples" | "ageTooltip"
  > & {
    /** Replaces the relative age while a parent operation reports progress. */
    status?: string;
    /** Wall-clock time of the page's most recent data query, request start
     * to data applied. Shown after the age label; null before the first
     * query completes. */
    queryDurationMs?: number | null;
    /** Per-step timings behind `queryDurationMs`, in execution order. Shown
     * as a list when the label is hovered or focused. */
    querySteps?: readonly QueryStep[];
  };

  let {
    status = undefined,
    queryDurationMs = null,
    querySteps = [],
    lastUpdatedAt,
    ...rest
  }: Props = $props();

  // Locale is fixed for the life of a page load (a language change reloads),
  // so the samples are computed once per mount.
  const ageWidthSamples = refreshStatusWidthSamples();
  const showSteps = $derived(status === undefined && querySteps.length > 0);

  // Shared time axis for the timeline: the whole query, or the last step's
  // end if a step outran the recorded total.
  const axisMs = $derived(
    Math.max(
      queryDurationMs ?? 0,
      ...querySteps.map((step) => step.startMs + step.durationMs),
      1,
    ),
  );
  const ticks = $derived(queryAxisTicks(axisMs));
  const hasSegments = $derived(querySteps.some((step) => step.segments !== undefined));
  const PHASES: QueryPhase[] = ["wait", "download", "apply"];

  function percent(ms: number): string {
    return ((100 * ms) / axisMs).toFixed(2);
  }

  // Tick labels centre on their line; one that would spill past the right
  // edge of the track hangs to the left of its line instead.
  function tickStyle(tick: number): string {
    const pct = (100 * tick) / axisMs;
    const shift = pct > 90 ? "-100%" : "-50%";
    return `left: ${pct.toFixed(2)}%; transform: translateX(${shift})`;
  }

  function barStyle(startMs: number, durationMs: number): string {
    return `left: ${percent(startMs)}%; width: ${percent(durationMs)}%`;
  }
</script>

<KitRefreshControl
  {...rest}
  lastUpdatedAt={status === undefined ? lastUpdatedAt : null}
  formatAge={status === undefined
    ? (at, now) => formatRefreshStatus(at, queryDurationMs, now)
    : () => status ?? ""}
  locale={getLocale()}
  {ageWidthSamples}
  ageTooltip={showSteps ? querySteps_tooltip : undefined}
/>

{#snippet querySteps_tooltip()}
  <div class="query-steps">
    <div class="query-steps__head">
      {#if lastUpdatedAt != null}
        <span class="query-steps__at">
          {formatDateTime(lastUpdatedAt, { dateStyle: "medium", timeStyle: "medium" })}
        </span>
      {/if}
      <span class="query-steps__total">{formatQueryDuration(queryDurationMs)}</span>
    </div>
    <div class="query-steps__list" role="table">
      <div class="query-steps__row" role="row">
        <span role="columnheader"></span>
        <span class="query-steps__axis" role="columnheader" aria-hidden="true">
          {#each ticks as tick (tick)}
            <span class="query-steps__tick" style={tickStyle(tick)}>
              {formatQueryTick(tick)}
            </span>
          {/each}
        </span>
        <span role="columnheader"></span>
      </div>
      {#each querySteps as step (step.name)}
        <div class="query-steps__row" role="row">
          <span class="query-steps__name" role="cell">{formatQueryStepLabel(step.name)}</span>
          <span class="query-steps__track" role="cell" aria-hidden="true">
            {#each ticks as tick (tick)}
              <span class="query-steps__grid" style={`left: ${percent(tick)}%`}></span>
            {/each}
            {#if step.segments}
              {#each step.segments as segment (segment.phase)}
                <span
                  class={`query-steps__bar query-steps__bar--${segment.phase}`}
                  style={barStyle(segment.startMs, segment.durationMs)}
                ></span>
              {/each}
            {:else}
              <span
                class="query-steps__bar query-steps__bar--wait"
                style={barStyle(step.startMs, step.durationMs)}
              ></span>
            {/if}
          </span>
          <span class="query-steps__duration" role="cell">
            {formatQueryDuration(step.durationMs)}
          </span>
        </div>
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
    grid-template-columns: max-content 200px max-content;
    column-gap: var(--space-4);
    align-items: center;
  }

  .query-steps__name,
  .query-steps__duration {
    padding: 2px 0;
  }

  .query-steps__row {
    display: contents;
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

  .query-steps__bar--wait {
    background: var(--accent-blue);
    border-radius: 2px 0 0 2px;
  }

  .query-steps__bar--download {
    background: color-mix(in srgb, var(--accent-blue) 45%, transparent);
  }

  .query-steps__bar--apply {
    background: color-mix(in srgb, var(--text-muted) 60%, transparent);
    border-radius: 0 2px 2px 0;
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
