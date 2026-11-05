<script lang="ts">
  import { Modal } from "@kenn-io/kit-ui";
  import { m } from "../../i18n/index.js";
  import { ui } from "../../stores/ui.svelte.js";
  import { sync } from "../../stores/sync.svelte.js";

  // Each shortcut lists its alternatives; each alternative is the keys
  // pressed together. Every key renders as its own keycap, so a literal
  // "/" key never reads as the separator between alternatives.
  const isMac = navigator.platform.toUpperCase().includes("MAC");
  const mod = isMac ? "⌘" : "Ctrl";
  const escapeKey = isMac ? "⎋" : "Esc";
  const deleteKey = isMac ? "⌫" : "Del";

  const baseShortcuts = $derived([
    { keys: [[mod, "K"]], action: m.shortcuts_open_command_palette() },
    { keys: [[mod, "F"], ["/"]], action: m.shortcuts_find_in_session() },
    { keys: [[mod, "G"]], action: m.shortcuts_go_to_session() },
    { keys: [[escapeKey]], action: m.shortcuts_close_palette() },
    { keys: [["j"], ["\u2193"]], action: m.shortcuts_next_message() },
    { keys: [["k"], ["\u2191"]], action: m.shortcuts_prev_message() },
    { keys: [["Shift", "J"]], action: m.shortcuts_next_user_prompt() },
    { keys: [["Shift", "K"]], action: m.shortcuts_prev_user_prompt() },
    { keys: [["]"]], action: m.shortcuts_next_session() },
    { keys: [["["]], action: m.shortcuts_prev_session() },
    { keys: [["o"]], action: m.shortcuts_toggle_sort() },
    { keys: [["l"]], action: m.shortcuts_cycle_layout() },
    { keys: [["r"]], action: m.shortcuts_trigger_sync() },
    { keys: [["s"]], action: m.shortcuts_star_session() },
    { keys: [["e"]], action: m.shortcuts_export_session() },
    { keys: [["p"]], action: m.shortcuts_publish_gist() },
    { keys: [["c"]], action: m.shortcuts_copy_resume() },
    { keys: [[deleteKey]], action: m.shortcuts_delete_session() },
    { keys: [["?"]], action: m.shortcuts_show_modal() },
  ]);

  const zoomShortcuts = $derived([
    { keys: [[mod, "+"]], action: m.shortcuts_zoom_in() },
    { keys: [[mod, "-"]], action: m.shortcuts_zoom_out() },
    { keys: [[mod, "0"]], action: m.shortcuts_reset_zoom() },
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
        <span class="shortcut-keys">
          {#each shortcut.keys as combo, alt}
            {#if alt > 0}<span class="shortcut-or" aria-hidden="true">/</span>{/if}
            <span class="shortcut-combo">
              {#each combo as key}<kbd class="shortcut-key">{key}</kbd>{/each}
            </span>
          {/each}
        </span>
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
    grid-template-columns: 108px minmax(0, 1fr);
    align-items: center;
    gap: 12px;
    padding: 3px 0;
  }

  .shortcut-row.group-end {
    margin-bottom: 6px;
    padding-bottom: 9px;
    border-bottom: 1px solid var(--border-muted);
  }

  .shortcut-keys {
    display: inline-flex;
    align-items: center;
    gap: 5px;
    justify-self: start;
    white-space: nowrap;
  }

  .shortcut-combo {
    display: inline-flex;
    gap: 3px;
  }

  .shortcut-or {
    font-size: 11px;
    color: var(--text-muted);
  }

  .shortcut-key {
    font-family: var(--font-mono);
    font-size: 11px;
    padding: 1px 6px;
    border: 1px solid var(--border-default);
    border-radius: var(--radius-sm);
    background: var(--bg-inset);
    color: var(--text-secondary);
    min-width: 20px;
    text-align: center;
  }

  .shortcut-action {
    font-size: 12.5px;
    color: var(--text-primary);
  }
</style>
