<script lang="ts">
  import { SegmentedControl } from "@kenn-io/kit-ui";
  import { m } from "../../i18n/index.js";
  import InlineCode from "../shared/InlineCode.svelte";
  import {
    settings,
    type ToolResultImagesPolicy,
  } from "../../stores/settings.svelte.js";

  const TOOL_RESULT_IMAGE_OPTIONS: { value: ToolResultImagesPolicy; label: string }[] =
    $derived([
      { value: "keep", label: m.settings_tool_result_images_keep() },
      { value: "drop", label: m.settings_tool_result_images_drop() },
      { value: "offload", label: m.settings_tool_result_images_offload() },
    ]);

  let restartRequired = $state(false);

  // Every locale keeps the command verbatim inside the hint; render it as
  // code by splitting the translated sentence around it. A locale that
  // drops or rewrites it falls back to plain text.
  const MIGRATE_IMAGES_COMMAND = "agentsview db migrate --images";
  const imagesHint = $derived.by(() => {
    const text = m.settings_tool_result_images_hint();
    const at = text.indexOf(MIGRATE_IMAGES_COMMAND);
    if (at < 0) return { before: text, after: "", hasCommand: false };
    return {
      before: text.slice(0, at),
      after: text.slice(at + MIGRATE_IMAGES_COMMAND.length),
      hasCommand: true,
    };
  });
</script>

<!-- One setting, so the row is compact: the label fits its text and the
     notes sit under the control in the same cell. -->
<div class="archive-content-settings">
  <div class="setting-row option-row compact">
    <span class="setting-label">{m.settings_tool_result_images()}</span>
    <div class="control-stack">
      <SegmentedControl
        options={TOOL_RESULT_IMAGE_OPTIONS}
        value={settings.toolResultImages}
        ariaLabel={m.settings_tool_result_images()}
        disabled={settings.saving || settings.readOnly}
        onchange={async (value) => {
          if (value !== "keep" && value !== "drop" && value !== "offload") return;
          if (value === settings.toolResultImages) return;
          // A failed write leaves the stored policy alone, so the notice waits
          // for the save the way AgentDirSettings does.
          if (await settings.save({ tool_result_images: value })) {
            restartRequired = true;
          }
        }}
      />
      <p class="setting-hint">
        {imagesHint.before}{#if imagesHint.hasCommand}<InlineCode
            >{MIGRATE_IMAGES_COMMAND}</InlineCode
          >{/if}{imagesHint.after}
      </p>
      {#if restartRequired}
        <p class="setting-hint" role="status" aria-live="polite">
          {m.settings_tool_result_images_restart_notice()}
        </p>
      {/if}
    </div>
  </div>
</div>

<style>
  /* The row uses the shared settings grid from SettingsPage. The label
     sits level with the segmented control, not centred on the notes. */
  .setting-label,
  .control-stack {
    align-self: baseline;
  }

  .setting-label {
    font-size: 12px;
    font-weight: 500;
    color: var(--text-secondary);
  }

  .control-stack {
    display: flex;
    flex-direction: column;
    align-items: flex-start;
    gap: var(--space-2);
    width: 100%;
    min-width: 0;
  }

  .setting-hint {
    margin: 0;
    color: var(--text-muted);
    font-size: 11px;
    line-height: 1.4;
  }

  @media (max-width: 640px) {
    .control-stack {
      align-items: stretch;
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
