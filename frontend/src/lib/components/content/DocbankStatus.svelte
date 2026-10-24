<script lang="ts">
  import { Button } from "@kenn-io/kit-ui";
  import { fileReferences } from "../../stores/file-references.svelte.js";
  import { m } from "../../i18n/index.js";
</script>

{#if fileReferences.enabled && fileReferences.result?.integration !== "disabled"}
  {#if fileReferences.status === "failed" || fileReferences.result?.lookup_failed}
    <div class="docbank-status">{fileReferences.status === "failed" ? m.docbank_read_unavailable() : m.docbank_unavailable()} <Button size="sm" surface="soft" label={m.shared_retry()} onclick={() => fileReferences.refresh()} /></div>
  {:else if fileReferences.status === "loading" && !fileReferences.result}
    <div class="docbank-status">{m.docbank_checking()}</div>
  {/if}
{/if}

<style>
  .docbank-status { padding: 4px 12px; font-size: 12px; color: var(--text-muted); }
</style>
