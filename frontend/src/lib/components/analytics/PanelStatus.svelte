<script lang="ts">
  import { Button, Spinner } from "@kenn-io/kit-ui";
  import { m } from "../../i18n/index.js";

  interface Props {
    hasData: boolean;
    pending: boolean;
    error: string | null;
    onRetry: () => void;
    loadingLabel?: string;
  }

  let { hasData, pending, error, onRetry, loadingLabel = m.usage_loading() }: Props = $props();
</script>

{#if error}
  <div class="error">
    <span>{error}</span>
    <!-- The spinner sits beside the label; Button's busy overlay would
         cover the short "Retry" text. A click while pending is ignored. -->
    <Button
      size="sm"
      surface="soft"
      label={m.shared_retry()}
      onclick={() => {
        if (!pending) onRetry();
      }}
    >
      {#if pending}
        <span class="retry-spinner" aria-hidden="true"><Spinner size={10} /></span>
      {/if}
    </Button>
  </div>
{:else if !hasData}
  <div class="loading" role="status">{loadingLabel}</div>
{/if}

<style>
  .error {
    color: var(--accent-red);
    font-size: 12px;
    /* Block padding only, so the message shares the panel title's left edge. */
    padding: 12px 0;
    display: flex;
    align-items: center;
    gap: 8px;
  }

  .retry-spinner {
    display: flex;
    align-items: center;
  }

  .loading {
    color: var(--text-muted);
    font-size: 12px;
    padding: 24px;
    text-align: center;
  }
</style>
