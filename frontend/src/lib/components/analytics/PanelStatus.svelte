<script lang="ts">
  import { Button } from "@kenn-io/kit-ui";
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
    <Button size="sm" surface="soft" label={m.shared_retry()} busy={pending} onclick={onRetry} />
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

  /* Button's busy spinner would cover and fade the short "Retry" label;
     set it beside the label instead and keep the label legible. */
  .error :global(.kit-button > .kit-button__busy) {
    position: static;
    order: -1;
  }

  .error :global(.kit-button.kit-button--busy > .kit-button__label) {
    opacity: 1;
  }

  .loading {
    color: var(--text-muted);
    font-size: 12px;
    padding: 24px;
    text-align: center;
  }
</style>
