<script lang="ts">
  import { Notice } from "@kenn-io/kit-ui";
  import { m } from "../../i18n/index.js";
  import { errorTelemetry } from "../../utils/error-telemetry.js";
  import type { ErrorKind } from "../../utils/telemetry.js";

  interface Props {
    /** The failure detail, usually the server's error message. */
    message: string;
    /** Retries the failed read. Omit when nothing can retry it. */
    onRetry?: () => void;
    /** Reports the shown error to telemetry under this kind. */
    telemetry?: ErrorKind;
    class?: string;
  }

  let { message, onRetry = undefined, telemetry = undefined, class: className = "" }: Props =
    $props();

  const label = $derived(m.shared_failed_to_load());
</script>

<!-- The failed-load card for every page and panel: a kit-ui error Notice,
     announced as an alert, with Retry when the read can be repeated. It
     replaces content that never loaded, and sits above content kept from
     an earlier load when a refresh fails. -->
<div class="load-error {className}" {@attach errorTelemetry(telemetry, message)}>
  <Notice
    tone="error"
    toneLabel={label}
    message={message && message !== label ? message : undefined}
    actionLabel={onRetry ? m.shared_retry() : undefined}
    onaction={onRetry}
  />
</div>

<style>
  /* Above content kept from an earlier load, the card keeps one step of
     space before it; alone it needs none. Zero specificity, so a caller's
     class (a flex or grid gap, say) overrides it. */
  :global(:where(.load-error:not(:last-child))) {
    margin-bottom: var(--space-5);
  }
</style>
