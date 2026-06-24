<script lang="ts">
  import { _ } from "svelte-i18n";
  import { ui } from "../../stores/ui.svelte.js";
  import { sync } from "../../stores/sync.svelte.js";
  import { XIcon } from "../../icons.js";

  const isMac = navigator.platform.toUpperCase().includes("MAC");
  const mod = isMac ? "⌘" : "Ctrl";
  const escapeKey = isMac ? "⎋" : "Esc";
  const deleteKey = isMac ? "⌫" : "Del";

  const baseShortcuts = $derived([
    { key: `${mod} K`, action: $_("shortcuts.openCommandPalette") },
    { key: `${mod} F / /`, action: $_("shortcuts.findInSession") },
    { key: escapeKey, action: $_("shortcuts.closePalette") },
    { key: "j / \u2193", action: $_("shortcuts.nextMessage") },
    { key: "k / \u2191", action: $_("shortcuts.prevMessage") },
    { key: "]", action: $_("shortcuts.nextSession") },
    { key: "[", action: $_("shortcuts.prevSession") },
    { key: "o", action: $_("shortcuts.toggleSort") },
    { key: "l", action: $_("shortcuts.cycleLayout") },
    { key: "r", action: $_("shortcuts.triggerSync") },
    { key: "s", action: $_("shortcuts.starSession") },
    { key: "e", action: $_("shortcuts.exportSession") },
    { key: "p", action: $_("shortcuts.publishGist") },
    { key: "c", action: $_("shortcuts.copyResume") },
    { key: deleteKey, action: $_("shortcuts.deleteSession") },
    { key: "?", action: $_("shortcuts.showModal") },
  ]);

  const zoomShortcuts = $derived([
    { key: `${mod} +`, action: $_("shortcuts.zoomIn") },
    { key: `${mod} -`, action: $_("shortcuts.zoomOut") },
    { key: `${mod} 0`, action: $_("shortcuts.resetZoom") },
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
      <h3 class="shortcuts-title">{$_("shortcuts.title")}</h3>
      <button
        class="close-btn"
        onclick={() => ui.activeModal = null}
        title={$_("shortcuts.close")}
        aria-label={$_("shortcuts.close")}
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
