<script lang="ts">
  import { Button, Modal } from "@kenn-io/kit-ui";
  import { m } from "../../i18n/index.js";
  import { ui } from "../../stores/ui.svelte.js";
  import { sync } from "../../stores/sync.svelte.js";

  function close() {
    ui.activeModal = null;
  }
</script>

<Modal title={m.update_title()} width="400px" onclose={close}>
  {#if sync.updateAvailable && sync.latestVersion}
    <p class="update-text">
      {m.update_available({ version: sync.latestVersion })}
    </p>
    <p class="update-current">
      {m.update_current({ version: sync.serverVersion?.version ?? m.update_unknown() })}
    </p>
    <p class="update-instructions">
      {m.update_instructions({ cmd: "agentsview update" })}
    </p>
  {:else}
    <p class="update-text">
      {m.update_latest({ version: sync.serverVersion?.version ?? m.update_unknown() })}
    </p>
  {/if}
  {#snippet footer()}
    <Button
      label={m.update_close_btn()}
      tone="info"
      surface="solid"
      onclick={close}
    />
  {/snippet}
</Modal>

<style>
  .update-text {
    font-size: 12px;
    color: var(--text-primary);
    line-height: 1.5;
  }

  .update-current {
    font-size: 12px;
    color: var(--text-secondary);
    line-height: 1.5;
    margin-top: 4px;
  }

  .update-instructions {
    font-size: 12px;
    color: var(--text-secondary);
    line-height: 1.5;
    margin-top: 8px;
  }

  .update-instructions :global(code) {
    font-family: var(--font-mono);
    background: var(--bg-inset);
    padding: 1px 4px;
    border-radius: 3px;
    font-size: 11px;
  }
</style>
