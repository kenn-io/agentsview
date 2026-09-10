<script lang="ts">
  import { SegmentedControl } from "@kenn-io/kit-ui";
  import { m } from "../../i18n/index.js";
  import {
    settings,
    type ToolResultImagesPolicy,
  } from "../../stores/settings.svelte.js";

  const TOOL_RESULT_IMAGE_OPTIONS: { value: ToolResultImagesPolicy; label: string }[] =
    $derived([
      { value: "keep", label: m.settings_tool_result_images_keep() },
      { value: "drop", label: m.settings_tool_result_images_drop() },
    ]);

  let restartRequired = $state(false);
</script>

<div class="archive-content-settings">
  <div class="setting-row option-row">
    <span class="setting-label">{m.settings_tool_result_images()}</span>
    <SegmentedControl
      options={TOOL_RESULT_IMAGE_OPTIONS}
      value={settings.toolResultImages}
      ariaLabel={m.settings_tool_result_images()}
      disabled={settings.saving || settings.readOnly}
      onchange={async (value) => {
        if (value !== "keep" && value !== "drop") return;
        if (value === settings.toolResultImages) return;
        // A failed write leaves the stored policy alone, so the notice waits
        // for the save the way AgentDirSettings does.
        if (await settings.save({ tool_result_images: value })) {
          restartRequired = true;
        }
      }}
    />
  </div>
  <p class="setting-hint">{m.settings_tool_result_images_hint()}</p>
  {#if restartRequired}
    <p class="setting-hint" role="status" aria-live="polite">
      {m.settings_tool_result_images_restart_notice()}
    </p>
  {/if}
</div>

<style>
  .archive-content-settings {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }

  .setting-row {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 12px;
  }

  .setting-label {
    font-size: 12px;
    font-weight: 500;
    color: var(--text-secondary);
    white-space: nowrap;
  }

  .setting-hint {
    margin: 0;
    color: var(--text-muted);
    font-size: 11px;
    line-height: 1.4;
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
