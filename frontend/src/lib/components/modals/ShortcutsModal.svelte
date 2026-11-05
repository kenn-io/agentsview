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

  const uid = $props.id();

  // Four labelled families. On desktop the zoom shortcuts join Actions.
  const groups = $derived([
    {
      label: m.shortcuts_group_navigation(),
      items: [
        { keys: [[mod, "K"]], action: m.shortcuts_open_command_palette() },
        { keys: [[mod, "F"], ["/"]], action: m.shortcuts_find_in_session() },
        { keys: [[mod, "G"]], action: m.shortcuts_go_to_session() },
        { keys: [[escapeKey]], action: m.shortcuts_close_palette() },
      ],
    },
    {
      label: m.shortcuts_group_messages(),
      items: [
        { keys: [["j"], ["\u2193"]], action: m.shortcuts_next_message() },
        { keys: [["k"], ["\u2191"]], action: m.shortcuts_prev_message() },
        { keys: [["Shift", "J"]], action: m.shortcuts_next_user_prompt() },
        { keys: [["Shift", "K"]], action: m.shortcuts_prev_user_prompt() },
      ],
    },
    {
      label: m.shortcuts_group_sessions(),
      items: [
        { keys: [["]"]], action: m.shortcuts_next_session() },
        { keys: [["["]], action: m.shortcuts_prev_session() },
      ],
    },
    {
      label: m.shortcuts_group_actions(),
      items: [
        { keys: [["o"]], action: m.shortcuts_toggle_sort() },
        { keys: [["l"]], action: m.shortcuts_cycle_layout() },
        { keys: [["r"]], action: m.shortcuts_trigger_sync() },
        { keys: [["s"]], action: m.shortcuts_star_session() },
        { keys: [["e"]], action: m.shortcuts_export_session() },
        { keys: [["p"]], action: m.shortcuts_publish_gist() },
        { keys: [["c"]], action: m.shortcuts_copy_resume() },
        { keys: [[deleteKey]], action: m.shortcuts_delete_session() },
        { keys: [["?"]], action: m.shortcuts_show_modal() },
        ...(sync.isDesktop
          ? [
              { keys: [[mod, "+"]], action: m.shortcuts_zoom_in() },
              { keys: [[mod, "-"]], action: m.shortcuts_zoom_out() },
              { keys: [[mod, "0"]], action: m.shortcuts_reset_zoom() },
            ]
          : []),
      ],
    },
  ]);
</script>

<Modal
  title={m.shortcuts_title()}
  closeLabel={m.shortcuts_close()}
  width="360px"
  onclose={() => (ui.activeModal = null)}
>
  <div class="shortcuts-list">
    {#each groups as group, g}
      <div class="shortcut-group" role="group" aria-labelledby="{uid}-group-{g}">
        <div class="shortcut-group-label" id="{uid}-group-{g}">{group.label}</div>
        {#each group.items as shortcut}
          <div class="shortcut-row">
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

  .shortcut-group + .shortcut-group {
    margin-top: 6px;
    padding-top: 6px;
    border-top: 1px solid var(--border-muted);
  }

  /* Sentence-case family label, same muted ink as other section labels. */
  .shortcut-group-label {
    font-size: 11px;
    font-weight: 500;
    color: var(--text-muted);
    padding: 1px 0 2px;
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

  /* Light theme keycaps read pale on the white dialog: one step darker
     ink, and a border one step past --border-default (the ladder has no
     stronger border token). Dark theme keeps the values above. */
  :global(:root:not(.dark)) .shortcut-key {
    border-color: color-mix(in srgb, var(--border-default), var(--text-muted) 30%);
    color: var(--text-primary);
  }

  .shortcut-action {
    font-size: 12.5px;
    color: var(--text-primary);
  }
</style>
