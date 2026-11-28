<script lang="ts">
  import { m } from "../../i18n/index.js";
  import LoadError from "../shared/LoadError.svelte";
  import LoadingState from "../shared/LoadingState.svelte";

  interface Props {
    hasData: boolean;
    pending: boolean;
    error: string | null;
    onRetry: () => void;
    loadingLabel?: string;
  }

  let { hasData, pending, error, onRetry, loadingLabel = m.usage_loading() }: Props = $props();
</script>

<!-- A dashboard panel's own load state, so one failed panel never blanks
     the others. A failed retry keeps the error until a load succeeds. -->
{#if error}
  <LoadError class="panel-status-error" message={error} onRetry={pending ? undefined : onRetry} telemetry="analytics" />
{:else if !hasData}
  <LoadingState align="center" label={loadingLabel} />
{/if}

<style>
  :global(.panel-status-error) {
    margin-block: 12px;
  }
</style>
