<script lang="ts">
  import { RefreshControl as KitRefreshControl } from "@kenn-io/kit-ui";
  import type { ComponentProps } from "svelte";
  import { getLocale } from "../../i18n/index.js";
  import { formatRefreshAge } from "../../utils/refresh.js";

  // Thin wrapper over kit-ui's RefreshControl: injects the app's localized
  // age formatter (m.shared_refresh_* via formatRefreshAge) and the current
  // app locale for the timestamp tooltip in one place, so pages pass only
  // data props — mirroring shared/RangePicker.svelte.

  type Props = Omit<
    ComponentProps<typeof KitRefreshControl>,
    "formatAge" | "locale"
  > & {
    /** Replaces the relative age while a parent operation reports progress. */
    status?: string;
  };

  let { status = undefined, lastUpdatedAt, ...rest }: Props = $props();
</script>

<KitRefreshControl
  {...rest}
  lastUpdatedAt={status === undefined ? lastUpdatedAt : null}
  formatAge={status === undefined ? formatRefreshAge : () => status ?? ""}
  locale={getLocale()}
/>
