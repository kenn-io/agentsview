<script lang="ts">
  import { Checkbox } from "@kenn-io/kit-ui";
  import { m } from "../../i18n/index.js";
  import SettingsSection from "./SettingsSection.svelte";
  import {
    ui,
    ALL_BLOCK_TYPES,
    FONT_SCALE_STEPS,
    type BlockType,
    type MessageLayout,
  } from "../../stores/ui.svelte.js";

  const LAYOUT_OPTIONS: { value: MessageLayout; label: string }[] = $derived([
    { value: "default", label: m.appearance_layout_default() },
    { value: "compact", label: m.appearance_layout_compact() },
    { value: "stream", label: m.appearance_layout_stream() },
    { value: "skim", label: m.appearance_layout_skim() },
  ]);

  const BLOCK_LABELS: Record<BlockType, string> = $derived({
    user: m.header_transcript_blocks_user(),
    assistant: m.header_transcript_blocks_assistant(),
    thinking: m.header_transcript_blocks_thinking(),
    tool: m.header_transcript_blocks_tool(),
    code: m.header_transcript_blocks_code(),
  });
</script>

<SettingsSection
  title={m.appearance_title()}
  description={m.appearance_description()}
>
  <div class="setting-row">
    <span class="setting-label">{m.appearance_theme()}</span>
    <button class="setting-toggle" onclick={() => ui.toggleTheme()}>
      {ui.theme === "light" ? m.appearance_light() : m.appearance_dark()}
    </button>
  </div>

  <div class="setting-row">
    <span class="setting-label">{m.appearance_high_contrast()}</span>
    <button class="setting-toggle" onclick={() => ui.toggleHighContrast()}>
      {ui.highContrast ? m.appearance_on() : m.appearance_off()}
    </button>
  </div>

  <div class="setting-row">
    <span class="setting-label">{m.appearance_message_layout()}</span>
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

  <div class="setting-row">
    <span class="setting-label">{m.appearance_text_size()}</span>
    <div class="setting-options">
      {#each FONT_SCALE_STEPS as step}
        <button
          class="option-btn"
          class:active={ui.fontScale === step}
          onclick={() => ui.setFontScale(step)}
        >
          {step}%
        </button>
      {/each}
    </div>
  </div>

  <div class="setting-row column">
    <span class="setting-label">{m.appearance_block_visibility()}</span>
    <div class="block-toggles">
      {#each ALL_BLOCK_TYPES as bt}
        <Checkbox
          checked={ui.isBlockVisible(bt)}
          onchange={() => ui.toggleBlock(bt)}
          label={BLOCK_LABELS[bt]}
        />
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
</style>
