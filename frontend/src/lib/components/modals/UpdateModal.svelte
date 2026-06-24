<script lang="ts">
  import { _ } from "svelte-i18n";
  import { ui } from "../../stores/ui.svelte.js";
  import { sync } from "../../stores/sync.svelte.js";
  import { XIcon } from "../../icons.js";

  function close() {
    ui.activeModal = null;
  }

  function handleOverlayClick(e: MouseEvent) {
    if (
      (e.target as HTMLElement).classList.contains(
        "modal-overlay",
      )
    ) {
      close();
    }
  }

  function handleKeydown(e: KeyboardEvent) {
    if (e.key === "Escape") {
      close();
    }
  }
</script>

<!-- svelte-ignore a11y_no_static_element_interactions -->
<div
  class="modal-overlay"
  onclick={handleOverlayClick}
  onkeydown={handleKeydown}
>
  <div class="modal-panel update-panel">
    <div class="modal-header">
      <h3 class="modal-title">{$_("update.title")}</h3>
      <button
        class="modal-close"
        onclick={close}
        title={$_("update.close")}
        aria-label={$_("update.close")}
      >
        <XIcon size="14" strokeWidth="2.2" aria-hidden="true" />
      </button>
    </div>

    <div class="modal-body">
      {#if sync.updateAvailable && sync.latestVersion}
        <p class="update-text">
          {@html $_("update.available", { values: { version: `<strong>${sync.latestVersion}</strong>` } })}
        </p>
        <p class="update-current">
          {$_("update.current", { values: { version: sync.serverVersion?.version ?? $_("update.unknown") } })}
        </p>
        <p class="update-instructions">
          {@html $_("update.instructions", { values: { cmd: "<code>agentsview update</code>" } })}
        </p>
      {:else}
        <p class="update-text">
          {$_("update.latest", { values: { version: sync.serverVersion?.version ?? $_("update.unknown") } })}
        </p>
      {/if}
      <div class="update-actions">
        <button
          class="modal-btn modal-btn-primary"
          onclick={close}
        >
          {$_("update.closeBtn")}
        </button>
      </div>
    </div>
  </div>
</div>

<style>
  .update-panel {
    width: 400px;
  }

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

  .update-actions {
    display: flex;
    justify-content: flex-end;
    margin-top: 16px;
  }
</style>
