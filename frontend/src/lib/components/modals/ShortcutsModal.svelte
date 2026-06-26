<script lang="ts">
  import { m } from "../../i18n/index.js";
  import { ui } from "../../stores/ui.svelte.js";
  import { sync } from "../../stores/sync.svelte.js";
  import { XIcon } from "../../icons.js";

  const isMac = navigator.platform.toUpperCase().includes("MAC");
  const mod = isMac ? "⌘" : "Ctrl";
  const escapeKey = isMac ? "⎋" : "Esc";
  const deleteKey = isMac ? "⌫" : "Del";

  const baseShortcuts = $derived([
    { key: `${mod} K`, action: m.shortcuts_open_command_palette() },
    { key: `${mod} F / /`, action: m.shortcuts_find_in_session() },
    { key: escapeKey, action: m.shortcuts_close_palette() },
    { key: "j / \u2193", action: m.shortcuts_next_message() },
    { key: "k / \u2191", action: m.shortcuts_prev_message() },
    { key: "]", action: m.shortcuts_next_session() },
    { key: "[", action: m.shortcuts_prev_session() },
    { key: "o", action: m.shortcuts_toggle_sort() },
    { key: "l", action: m.shortcuts_cycle_layout() },
    { key: "r", action: m.shortcuts_trigger_sync() },
    { key: "s", action: m.shortcuts_star_session() },
    { key: "e", action: m.shortcuts_export_session() },
    { key: "p", action: m.shortcuts_publish_gist() },
    { key: "c", action: m.shortcuts_copy_resume() },
    { key: deleteKey, action: m.shortcuts_delete_session() },
    { key: "?", action: m.shortcuts_show_modal() },
  ]);

  const zoomShortcuts = $derived([
    { key: `${mod} +`, action: m.shortcuts_zoom_in() },
    { key: `${mod} -`, action: m.shortcuts_zoom_out() },
    { key: `${mod} 0`, action: m.shortcuts_reset_zoom() },
  ]);

  const shortcuts = $derived(sync.isDesktop
    ? [...baseShortcuts, ...zoomShortcuts]
    : baseShortcuts);

  function handleOverlayClick(e: MouseEvent) {
    if (
      (e.target as HTMLElement).classList.contains(
        "shortcuts-overlay",
      )
    ) {
      ui.activeModal = null;
    }
  }
</script>

<!-- svelte-ignore a11y_no_static_element_interactions -->
<div
  class="shortcuts-overlay"
  onclick={handleOverlayClick}
  onkeydown={(e) => {
    if (e.key === "Escape") ui.activeModal = null;
  }}
>
  <div class="shortcuts-modal">
    <div class="shortcuts-header">
      <h3 class="shortcuts-title">{m.shortcuts_title()}</h3>
      <button
        class="close-btn"
        onclick={() => ui.activeModal = null}
        title={m.shortcuts_close()}
        aria-label={m.shortcuts_close()}
      >
        <XIcon size="14" strokeWidth="2.2" aria-hidden="true" />
      </button>
    </div>

    <div class="shortcuts-list">
      {#each shortcuts as shortcut}
        <div class="shortcut-row">
          <kbd class="shortcut-key">{shortcut.key}</kbd>
          <span class="shortcut-action">{shortcut.action}</span>
        </div>
      {/each}
    </div>
  </div>
</div>

<style>
  .shortcuts-overlay {
    position: fixed;
    inset: 0;
    background: var(--overlay-bg);
    display: flex;
    align-items: center;
    justify-content: center;
    z-index: 100;
  }

  .shortcuts-modal {
    width: 360px;
    background: var(--bg-surface);
    border: 1px solid var(--border-default);
    border-radius: var(--radius-lg);
    box-shadow: var(--shadow-md);
    overflow: hidden;
  }

  .shortcuts-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    padding: 12px 16px;
    border-bottom: 1px solid var(--border-default);
  }

  .shortcuts-title {
    font-size: 13px;
    font-weight: 600;
    color: var(--text-primary);
  }

  .close-btn {
    width: 24px;
    height: 24px;
    display: flex;
    align-items: center;
    justify-content: center;
    font-size: 16px;
    color: var(--text-muted);
    border-radius: var(--radius-sm);
  }

  .close-btn:hover {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }

  .shortcuts-list {
    padding: 8px 0;
  }

  .shortcut-row {
    display: flex;
    align-items: center;
    justify-content: space-between;
    padding: 5px 16px;
  }

  .shortcut-key {
    font-family: var(--font-mono);
    font-size: 11px;
    padding: 1px 6px;
    border: 1px solid var(--border-default);
    border-radius: var(--radius-sm);
    background: var(--bg-inset);
    color: var(--text-secondary);
    min-width: 60px;
    text-align: center;
  }

  .shortcut-action {
    font-size: 12px;
    color: var(--text-secondary);
  }
</style>
