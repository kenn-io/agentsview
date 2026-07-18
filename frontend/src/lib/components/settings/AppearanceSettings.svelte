<script lang="ts">
  import { Button, Checkbox, SegmentedControl } from "@kenn-io/kit-ui";
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

  const FONT_SCALE_OPTIONS = $derived(
    FONT_SCALE_STEPS.map((step) => ({
      value: String(step),
      label: `${step}%`,
    })),
  );
</script>

<SettingsSection
  title={m.appearance_title()}
  description={m.appearance_description()}
>
  <div class="setting-row">
    <span class="setting-label">{m.appearance_theme()}</span>
    <Button size="sm" onclick={() => ui.toggleTheme()}>
      {ui.theme === "light" ? m.appearance_light() : m.appearance_dark()}
    </Button>
  </div>

  <div class="setting-row">
    <span class="setting-label">{m.appearance_high_contrast()}</span>
    <Button size="sm" onclick={() => ui.toggleHighContrast()}>
      {ui.highContrast ? m.appearance_on() : m.appearance_off()}
    </Button>
  </div>

  <div class="setting-row option-row">
    <span class="setting-label">{m.appearance_message_layout()}</span>
    <SegmentedControl
      options={LAYOUT_OPTIONS}
      value={ui.messageLayout}
      ariaLabel={m.appearance_message_layout()}
      onchange={(value) => ui.setLayout(value as MessageLayout)}
    />
  </div>

  <div class="setting-row option-row">
    <span class="setting-label">{m.appearance_text_size()}</span>
    <SegmentedControl
      options={FONT_SCALE_OPTIONS}
      value={String(ui.fontScale)}
      ariaLabel={m.appearance_text_size()}
      onchange={(value) => ui.setFontScale(Number(value))}
    />
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

  .block-toggles {
    display: flex;
    flex-wrap: wrap;
    gap: 8px;
  }

  @media (max-width: 640px) {
    .setting-row.option-row {
      flex-direction: column;
      align-items: stretch;
    }

    .option-row .setting-label {
      align-self: flex-start;
    }

    .option-row :global(.kit-segmented) {
      width: 100%;
    }

    .option-row :global(.kit-segmented__btn) {
      flex: 1;
      min-width: 0;
      padding-inline: 6px;
    }
  }
</style>
