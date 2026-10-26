<script lang="ts">
  import {
    Button,
    Checkbox,
    ColorInput,
    SegmentedControl,
    Typeahead,
    type ColorInputSwatch,
  } from "@kenn-io/kit-ui";
  import { getLocale, m } from "../../i18n/index.js";
  import { TriangleAlertIcon } from "../../icons.js";
  import { messageColors } from "../../stores/messageColors.svelte.js";
  import { settings } from "../../stores/settings.svelte.js";
  import {
    ui,
    ALL_BLOCK_TYPES,
    ZOOM_STEPS,
    type BlockType,
    type MessageLayout,
  } from "../../stores/ui.svelte.js";
  import {
    isChartPalette,
    type ChartPalette,
  } from "../../utils/chartPalette.js";
  import {
    colorIssues,
    COLORBLIND_COLORS,
    defaultRoleColor,
    MESSAGE_ROLES,
    type ColorIssue,
    type ContrastMode,
    type MessageColorPreset,
    type MessageRole,
  } from "../../utils/messageColors.js";
  import MessageColorPreview from "./MessageColorPreview.svelte";

  const CHART_PALETTE_OPTIONS: { value: ChartPalette; label: string }[] =
    $derived([
      {
        value: "agentsview",
        label: m.appearance_chart_palette_agentsview(),
      },
      {
        value: "matplotlib",
        label: m.appearance_chart_palette_matplotlib(),
      },
    ]);

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
    system: m.header_transcript_blocks_system(),
  });

  const ROLE_LABELS: Record<MessageRole, string> = $derived({
    user: m.header_transcript_blocks_user(),
    assistant: m.header_transcript_blocks_assistant(),
    thinking: m.header_transcript_blocks_thinking(),
    tool: m.header_transcript_blocks_tool(),
    code: m.header_transcript_blocks_code(),
  });

  const MODE_LABELS: Record<ContrastMode, string> = $derived({
    light: m.appearance_message_colors_mode_light(),
    dark: m.appearance_message_colors_mode_dark(),
    "high-contrast-light": m.appearance_message_colors_mode_high_contrast_light(),
    "high-contrast-dark": m.appearance_message_colors_mode_high_contrast_dark(),
  });

  const COLOR_SWATCHES: ColorInputSwatch[] = $derived([
    { value: COLORBLIND_COLORS.user, label: m.appearance_message_colors_swatch_blue() },
    { value: COLORBLIND_COLORS.assistant, label: m.appearance_message_colors_swatch_vermillion() },
    { value: COLORBLIND_COLORS.tool, label: m.appearance_message_colors_swatch_bluish_green() },
    { value: COLORBLIND_COLORS.thinking, label: m.appearance_message_colors_swatch_reddish_purple() },
    { value: COLORBLIND_COLORS.code, label: m.appearance_message_colors_swatch_dark_yellow() },
    { value: "#7f7f7f", label: m.appearance_message_colors_swatch_gray() },
  ]);

  const PRESET_OPTIONS = $derived([
    { value: "default", label: m.appearance_message_colors_preset_default() },
    { value: "colorblind", label: m.appearance_message_colors_preset_colorblind() },
    ...(messageColors.preset === "custom"
      ? [{ value: "custom", label: m.appearance_message_colors_preset_custom() }]
      : []),
  ]);

  const currentMode: ContrastMode = $derived(
    ui.highContrast
      ? ui.theme === "dark" ? "high-contrast-dark" : "high-contrast-light"
      : ui.theme,
  );

  function roleColor(role: MessageRole): string {
    return messageColors.colors[role] ?? defaultRoleColor(role, currentMode);
  }

  function issueText(issue: ColorIssue): string {
    const params = {
      modes: new Intl.ListFormat(getLocale(), { type: "conjunction" }).format(
        issue.modes.map((mode) => MODE_LABELS[mode]),
      ),
      ratio: new Intl.NumberFormat(getLocale(), { maximumFractionDigits: 1 }).format(issue.ratio),
      required: new Intl.NumberFormat(getLocale(), { maximumFractionDigits: 1 }).format(
        issue.required,
      ),
    };
    return issue.kind === "text"
      ? m.appearance_message_colors_warning_text(params)
      : m.appearance_message_colors_warning_marker(params);
  }

  const ZOOM_OPTIONS = $derived(
    ZOOM_STEPS.map((step) => ({
      name: String(step),
      label: `${step}%`,
    })),
  );
</script>

<div class="appearance-settings">
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
    <span class="setting-label">{m.appearance_chart_palette()}</span>
    <SegmentedControl
      options={CHART_PALETTE_OPTIONS}
      value={settings.chartPalette}
      ariaLabel={m.appearance_chart_palette()}
      disabled={settings.saving}
      onchange={(value) => {
        if (!isChartPalette(value)) return;
        void settings.save({ chart_palette: value });
      }}
    />
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

  <div class="setting-row">
    <span class="setting-label">{m.appearance_zoom()}</span>
    <Typeahead
      options={ZOOM_OPTIONS}
      value={String(ui.zoomLevel)}
      fallbackLabel="100%"
      triggerPrefix={m.appearance_zoom()}
      placeholder={m.appearance_zoom()}
      title={m.appearance_zoom()}
      emptyLabel={m.filter_dropdown_no_matches()}
      onselect={(value) => ui.setZoomLevel(Number(value))}
    />
  </div>

  <div class="setting-row">
    <Checkbox
      checked={ui.renderUnknownXmlBlocksAsPreformatted}
      onchange={() => ui.toggleUnknownXmlBlocksAsPreformatted()}
      ariaLabel={m.appearance_render_unknown_xml_blocks()}
      label={m.appearance_render_unknown_xml_blocks()}
    />
  </div>

  <div class="setting-row column message-colors">
    <span class="setting-label">{m.appearance_message_colors()}</span>
    <p class="setting-hint">{m.appearance_message_colors_description()}</p>
    <div class="message-color-actions">
      <SegmentedControl
        options={PRESET_OPTIONS}
        value={messageColors.preset}
        ariaLabel={m.appearance_message_colors_preset()}
        onchange={(value) => {
          if (value === "default" || value === "colorblind") {
            messageColors.applyPreset(value as MessageColorPreset);
          }
        }}
      />
      <Button
        size="sm"
        disabled={messageColors.isDefault}
        onclick={() => messageColors.applyPreset("default")}
      >
        {m.appearance_message_colors_reset_all()}
      </Button>
    </div>
    <Checkbox
      checked={messageColors.patterns}
      onchange={() => messageColors.setPatterns(!messageColors.patterns)}
      label={m.appearance_message_colors_patterns()}
    />
    <div class="role-colors">
      {#each MESSAGE_ROLES as role (role)}
        {@const custom = messageColors.colors[role]}
        {@const issues = custom ? colorIssues(role, custom) : []}
        {@const warningId = `message-color-warning-${role}`}
        <div class="role-color" data-role={role}>
          <span class="role-color-name">{ROLE_LABELS[role]}</span>
          <div class="role-color-controls">
          <ColorInput
            size="sm"
            value={roleColor(role)}
            swatches={COLOR_SWATCHES}
            ariaLabel={m.appearance_message_colors_role_label({ role: ROLE_LABELS[role] })}
            pickerLabel={m.appearance_message_colors_picker()}
            hexLabel={m.appearance_message_colors_hex()}
            invalid={issues.length > 0}
            ariaDescribedby={issues.length > 0 ? warningId : undefined}
            onchange={(value) => messageColors.setColor(role, value)}
          />
          {#if custom}
            <Button
              size="sm"
              ariaLabel={m.appearance_message_colors_reset_role_label({ role: ROLE_LABELS[role] })}
              onclick={() => messageColors.resetColor(role)}
            >
              {m.appearance_message_colors_reset_role()}
            </Button>
          {/if}
          </div>
          {#if issues.length > 0}
            <div id={warningId} class="color-warning">
              {#each issues as issue (issue.kind)}
                <p>
                  <TriangleAlertIcon size="12" strokeWidth="2" aria-hidden="true" />
                  {issueText(issue)}
                </p>
              {/each}
            </div>
          {/if}
        </div>
      {/each}
    </div>
    <MessageColorPreview />
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
</div>

<style>
  .appearance-settings {
    display: flex;
    flex-direction: column;
    gap: var(--space-5);
  }

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

  .setting-hint {
    font-size: 12px;
    color: var(--text-muted);
    max-width: 60ch;
  }

  .message-colors {
    gap: var(--space-5);
  }

  .message-color-actions {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--space-4);
  }

  .role-colors {
    display: grid;
    grid-template-columns: max-content minmax(0, 1fr);
    align-items: center;
    gap: var(--space-4) var(--space-5);
    width: 100%;
  }

  .role-color {
    display: contents;
  }

  .role-color-name {
    font-size: 12px;
    color: var(--text-primary);
  }

  .role-color-controls {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--space-4);
  }

  .color-warning {
    grid-column: 2;
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    font-size: 12px;
    color: var(--text-primary);
  }

  .color-warning p {
    display: flex;
    align-items: flex-start;
    gap: var(--space-3);
  }

  .color-warning :global(svg) {
    flex-shrink: 0;
    margin-top: 2px;
    color: var(--accent-amber);
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
