<script lang="ts">
  import { RefreshControl as KitRefreshControl } from "@kenn-io/kit-ui";
  import type { ComponentProps } from "svelte";
  import { getLocale } from "../../i18n/index.js";
  import {
    formatQueryDuration,
    formatRefreshAge,
    queryDurationWidthSamples,
    refreshAgeWidthSamples,
  } from "../../utils/refresh.js";

  // Thin wrapper over kit-ui's RefreshControl: injects the app's localized
  // age formatter (m.shared_refresh_* via formatRefreshAge), the last-query
  // duration readout, the current app locale for the timestamp tooltip, and
  // the localized width samples that keep both text boxes a constant width,
  // so pages pass only data props — mirroring shared/RangePicker.svelte.

  type Props = Omit<
    ComponentProps<typeof KitRefreshControl>,
    "formatAge" | "locale" | "detail" | "ageWidthSamples" | "detailWidthSamples"
  > & {
    /** Replaces the relative age while a parent operation reports progress. */
    status?: string;
    /** Wall-clock time of the page's most recent data query, request start
     * to data applied. Rendered right of the age label; null before the
     * first query completes. */
    queryDurationMs?: number | null;
  };

  let { status = undefined, queryDurationMs = null, lastUpdatedAt, ...rest }: Props = $props();

  // Locale is fixed for the life of a page load (a language change reloads),
  // so the samples are computed once per mount.
  const ageWidthSamples = refreshAgeWidthSamples();
  const detailWidthSamples = queryDurationWidthSamples();
</script>

<KitRefreshControl
  {...rest}
  lastUpdatedAt={status === undefined ? lastUpdatedAt : null}
  formatAge={status === undefined ? formatRefreshAge : () => status ?? ""}
  locale={getLocale()}
  detail={formatQueryDuration(queryDurationMs)}
  {ageWidthSamples}
  {detailWidthSamples}
/>
