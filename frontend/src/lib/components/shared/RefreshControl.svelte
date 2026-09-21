<script lang="ts">
  import { RefreshControl as KitRefreshControl } from "@kenn-io/kit-ui";
  import type { ComponentProps } from "svelte";
  import { formatDateTime, getLocale } from "../../i18n/index.js";
  import {
    formatQueryDuration,
    formatQueryStepLabel,
    formatRefreshStatus,
    refreshStatusWidthSamples,
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
    {#if lastUpdatedAt != null}
      <div class="query-steps__at">
        {formatDateTime(lastUpdatedAt, { dateStyle: "medium", timeStyle: "medium" })}
      </div>
    {/if}
    <dl class="query-steps__list">
      {#each querySteps as step (step.name)}
        <dt>{formatQueryStepLabel(step.name)}</dt>
        <dd>{formatQueryDuration(step.durationMs)}</dd>
      {/each}
    </dl>
  </div>
{/snippet}

<style>
  .query-steps {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    font-size: var(--font-size-xs);
  }

  .query-steps__at {
    color: var(--text-muted);
  }

  .query-steps__list {
    display: grid;
    grid-template-columns: auto max-content;
    column-gap: var(--space-5);
    row-gap: var(--space-1);
    margin: 0;
  }

  .query-steps__list dt {
    color: var(--text-secondary);
  }

  .query-steps__list dd {
    margin: 0;
    text-align: end;
    font-variant-numeric: tabular-nums;
    color: var(--text-primary);
  }
</style>
