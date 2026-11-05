<script lang="ts">
  import { Button, SegmentedControl, TextInput } from "@kenn-io/kit-ui";
  import { m } from "../../i18n/index.js";
  import { settings } from "../../stores/settings.svelte.js";
  import {
    ConfigService,
    type TerminalConfigBody,
  } from "../../api/generated/index";

  const MODES = $derived([
    { value: "auto", label: m.settings_terminal_mode_auto() },
    { value: "custom", label: m.settings_terminal_mode_custom() },
    { value: "clipboard", label: m.settings_terminal_mode_clipboard() },
  ]);

  let localMode: string = $state(settings.terminal.mode || "auto");
  let localBin: string = $state(settings.terminal.custom_bin ?? "");
  let localArgs: string = $state(settings.terminal.custom_args ?? "");

  $effect(() => {
    localMode = settings.terminal.mode || "auto";
    localBin = settings.terminal.custom_bin ?? "";
    localArgs = settings.terminal.custom_args ?? "";
  });

  async function saveTerminal() {
    if (settings.saving) return;
    await settings.runMutation(async () => {
      await ConfigService.postApiV1ConfigTerminal({
        mode: localMode as TerminalConfigBody["mode"],
        custom_bin: localBin || undefined,
        custom_args: localArgs || undefined,
      });
    });
    // Reload settings after the mutation so hydration sees the saved config.
    await settings.load();
  }

  let dirty = $derived(
    localMode !== (settings.terminal.mode || "auto") ||
      localBin !== (settings.terminal.custom_bin ?? "") ||
      localArgs !== (settings.terminal.custom_args ?? ""),
  );
</script>

<div class="terminal-settings">
  <div class="setting-row">
    <span class="setting-label">{m.settings_terminal_launch_mode()}</span>
    <SegmentedControl
      options={MODES}
      value={localMode}
      ariaLabel={m.settings_terminal_launch_mode()}
      onchange={(value) => (localMode = value)}
    />
  </div>

  {#if localMode === "custom"}
    <div class="setting-row">
      <label class="setting-label" for="terminal-bin">{m.settings_terminal_terminal_binary()}</label>
      <TextInput
        id="terminal-bin"
        class="setting-input"
        size="md"
        block
        type="text"
        placeholder="/usr/bin/kitty"
        bind:value={localBin}
      />
    </div>

    <div class="setting-row">
      <label class="setting-label" for="terminal-args">
        {m.settings_terminal_arguments()} <span class="hint">{m.settings_terminal_args_hint()}</span>
      </label>
      <TextInput
        id="terminal-args"
        class="setting-input"
        size="md"
        block
        type="text"
        placeholder="-- bash -c {"{cmd}"}"
        bind:value={localArgs}
      />
    </div>
  {/if}

  {#if dirty}
    <div class="save-row control-offset">
      <Button
        tone="info"
        surface="solid"
        disabled={settings.saving}
        onclick={saveTerminal}
      >
        {settings.saving ? m.settings_terminal_saving() : m.settings_terminal_save()}
      </Button>
    </div>
  {/if}
</div>

<style>
  .terminal-settings {
    display: flex;
    flex-direction: column;
    gap: var(--space-5);
  }

  /* Rows use the shared settings grid from SettingsPage: label column,
     then the control. */
  .setting-label {
    font-size: 12px;
    font-weight: 500;
    color: var(--text-secondary);
  }

  .hint {
    font-weight: 400;
    color: var(--text-muted);
  }

  :global(.setting-input.kit-text-input) {
    font-family: var(--font-mono, monospace);
  }

  .save-row {
    display: flex;
  }

  .control-offset {
    padding-left: calc(var(--setting-label-width, 148px) + 12px);
  }

  @media (max-width: 640px) {
    .control-offset {
      padding-left: 0;
    }
  }
</style>
