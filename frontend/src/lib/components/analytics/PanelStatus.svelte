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
    {error}
    <Button size="sm" surface="soft" label={m.shared_retry()} busy={pending} onclick={onRetry} />
  </div>
{:else if !hasData}
  <div class="loading" role="status">{loadingLabel}</div>
{/if}

<style>
  .error {
    color: var(--accent-red);
    font-size: 12px;
    padding: 12px;
    display: flex;
    align-items: center;
    gap: 8px;
  }

  .loading {
    color: var(--text-muted);
    font-size: 12px;
    padding: 24px;
    text-align: center;
  }
</style>
