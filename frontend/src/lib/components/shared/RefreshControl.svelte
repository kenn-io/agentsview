<script lang="ts">
  import { RefreshControl as KitRefreshControl } from "@kenn-io/kit-ui";
  import type { ComponentProps } from "svelte";
  import { getLocale } from "../../i18n/index.js";
  import { formatRefreshStatus, refreshStatusWidthSamples } from "../../utils/refresh.js";

  // Thin wrapper over kit-ui's RefreshControl: injects the app's localized
  // label (age plus last-query duration via formatRefreshStatus), the current
  // app locale for the timestamp tooltip, and the localized width samples that
  // keep the label box a constant width, so pages pass only data props —
  // mirroring shared/RangePicker.svelte.

  type Props = Omit<
    ComponentProps<typeof KitRefreshControl>,
    "formatAge" | "locale" | "ageWidthSamples"
  > & {
    /** Replaces the relative age while a parent operation reports progress. */
    status?: string;
    /** Wall-clock time of the page's most recent data query, request start
     * to data applied. Shown after the age label; null before the first
     * query completes. */
    queryDurationMs?: number | null;
  };

  let { status = undefined, queryDurationMs = null, lastUpdatedAt, ...rest }: Props = $props();

  // Locale is fixed for the life of a page load (a language change reloads),
  // so the samples are computed once per mount.
  const ageWidthSamples = refreshStatusWidthSamples();
</script>

<KitRefreshControl
  {...rest}
  lastUpdatedAt={status === undefined ? lastUpdatedAt : null}
  formatAge={status === undefined
    ? (at, now) => formatRefreshStatus(at, queryDurationMs, now)
    : () => status ?? ""}
  locale={getLocale()}
  {ageWidthSamples}
/>
