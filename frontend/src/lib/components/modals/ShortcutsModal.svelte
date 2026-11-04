<script lang="ts">
  import { Modal } from "@kenn-io/kit-ui";
  import { m } from "../../i18n/index.js";
  import { ui } from "../../stores/ui.svelte.js";
  import { sync } from "../../stores/sync.svelte.js";

  const isMac = navigator.platform.toUpperCase().includes("MAC");
  const mod = isMac ? "⌘" : "Ctrl";
  const escapeKey = isMac ? "⎋" : "Esc";
  const deleteKey = isMac ? "⌫" : "Del";

  const baseShortcuts = $derived([
    { key: `${mod} K`, action: m.shortcuts_open_command_palette() },
    { key: `${mod} F / /`, action: m.shortcuts_find_in_session() },
    { key: `${mod} G`, action: m.shortcuts_go_to_session() },
    { key: escapeKey, action: m.shortcuts_close_palette() },
    { key: "j / \u2193", action: m.shortcuts_next_message() },
    { key: "k / \u2191", action: m.shortcuts_prev_message() },
    { key: "Shift J", action: m.shortcuts_next_user_prompt() },
    { key: "Shift K", action: m.shortcuts_prev_user_prompt() },
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

  // Rows that end a family (palette and find, message navigation, session
  // navigation, session actions) get a little space below them.
  const GROUP_ENDS = $derived(new Set([3, 7, 9, baseShortcuts.length - 1]));

  const shortcuts = $derived(sync.isDesktop
    ? [...baseShortcuts, ...zoomShortcuts]
    : baseShortcuts);
</script>

<Modal
  title={m.shortcuts_title()}
  closeLabel={m.shortcuts_close()}
  width="360px"
  onclose={() => (ui.activeModal = null)}
>
  <div class="shortcuts-list">
    {#each shortcuts as shortcut, index}
      <div class="shortcut-row" class:group-end={GROUP_ENDS.has(index) && index < shortcuts.length - 1}>
        <!-- kit-ui-check-ignore: reference list enumerates compound alternatives ("j / ↓") and must stay visible on touch, where KbdBadge hides itself -->
        <kbd class="shortcut-key">{shortcut.key}</kbd>
        <span class="shortcut-action">{shortcut.action}</span>
      </div>
    {/each}
  </div>
</Modal>

<style>
  .shortcuts-list {
    padding: 0;
  }

  /* Keys in a fixed column, descriptions left-aligned beside them. */
  .shortcut-row {
    display: grid;
    grid-template-columns: 96px minmax(0, 1fr);
    align-items: center;
    gap: 12px;
    padding: 3px 0;
  }

  .shortcut-row.group-end {
    margin-bottom: 6px;
    padding-bottom: 9px;
    border-bottom: 1px solid var(--border-muted);
  }

  .shortcut-key {
    font-family: var(--font-mono);
    font-size: 11px;
    padding: 1px 6px;
    border: 1px solid var(--border-default);
    border-radius: var(--radius-sm);
    background: var(--bg-inset);
    color: var(--text-secondary);
    justify-self: start;
    min-width: 28px;
    text-align: center;
  }

  .shortcut-action {
    font-size: 12.5px;
    color: var(--text-primary);
  }
</style>
