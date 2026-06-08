<script lang="ts">
  import SettingsSection from "./SettingsSection.svelte";
  import {
    ui,
    ALL_BLOCK_TYPES,
    type BlockType,
    type MessageLayout,
  } from "../../stores/ui.svelte.js";

  const LAYOUT_OPTIONS: { value: MessageLayout; label: string }[] = [
    { value: "default", label: "Default" },
    { value: "compact", label: "Compact" },
    { value: "stream", label: "Stream" },
  ];

  const BLOCK_LABELS: Record<BlockType, string> = {
    user: "User messages",
    assistant: "Assistant text",
    thinking: "Thinking blocks",
    tool: "Tool calls",
    code: "Code blocks",
  };
</script>

<SettingsSection
  title="Appearance"
  description="Theme, layout, and block visibility preferences."
>
  <div class="setting-row">
    <span class="setting-label">Theme</span>
    <button class="setting-toggle" onclick={() => ui.toggleTheme()}>
      {ui.theme === "light" ? "Light" : "Dark"}
    </button>
  </div>

  <div class="setting-row">
    <span class="setting-label">Message layout</span>
    <div class="setting-options">
      {#each LAYOUT_OPTIONS as opt}
        <button
          class="option-btn"
          class:active={ui.messageLayout === opt.value}
          onclick={() => ui.setLayout(opt.value)}
        >
          {opt.label}
        </button>
      {/each}
    </div>
  </div>

  <div class="setting-row column">
    <span class="setting-label">Block visibility</span>
    <div class="block-toggles">
      {#each ALL_BLOCK_TYPES as bt}
        <label class="block-toggle">
          <input
            type="checkbox"
            checked={ui.isBlockVisible(bt)}
            onchange={() => ui.toggleBlock(bt)}
          />
          <span>{BLOCK_LABELS[bt]}</span>
        </label>
      {/each}
    </div>
  </div>
</SettingsSection>

<style>
  .setting-row {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 12px;
  }

  .setting-row.column {
    flex-direction: column;
    align-items: flex-start;
  }

  .setting-label {
    font-size: 12px;
    font-weight: 500;
    color: var(--text-secondary);
    white-space: nowrap;
  }

  .setting-toggle {
    height: 26px;
    padding: 0 12px;
    border-radius: var(--radius-sm);
    font-size: 11px;
    font-weight: 500;
    color: var(--text-secondary);
    background: var(--bg-inset);
    border: 1px solid var(--border-muted);
    cursor: pointer;
    transition: background 0.12s;
  }

  .setting-toggle:hover {
    background: var(--bg-surface-hover);
  }

  .setting-options {
    display: flex;
    gap: 4px;
  }

  .option-btn {
    height: 26px;
    padding: 0 10px;
    border-radius: var(--radius-sm);
    font-size: 11px;
    font-weight: 500;
    color: var(--text-muted);
    background: var(--bg-inset);
    border: 1px solid var(--border-muted);
    cursor: pointer;
    transition: all 0.12s;
  }

  .option-btn:hover {
    color: var(--text-secondary);
    background: var(--bg-surface-hover);
  }

  .option-btn.active {
    color: var(--accent-blue);
    background: color-mix(in srgb, var(--accent-blue) 10%, transparent);
    border-color: var(--accent-blue);
  }

  .block-toggles {
    display: flex;
    flex-wrap: wrap;
    gap: 8px;
  }

  .block-toggle {
    display: flex;
    align-items: center;
    gap: 6px;
    font-size: 12px;
    color: var(--text-secondary);
    cursor: pointer;
  }

  .block-toggle input {
    accent-color: var(--accent-blue);
  }
</style>
