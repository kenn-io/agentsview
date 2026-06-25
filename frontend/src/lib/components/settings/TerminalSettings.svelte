<script lang="ts">
  import { m } from "../../i18n/index.js";
  import SettingsSection from "./SettingsSection.svelte";
  import { settings } from "../../stores/settings.svelte.js";
  import {
    ConfigService,
    TerminalConfigBody,
  } from "../../api/generated/index";
  import { configureGeneratedClient } from "../../api/runtime.js";

  const MODES = $derived([
    { value: "auto", label: m.settings_terminal_mode_auto() },
    { value: "custom", label: m.settings_terminal_mode_custom() },
    { value: "clipboard", label: m.settings_terminal_mode_clipboard() },
  ] as const);

  let localMode: string = $state(settings.terminal.mode || "auto");
  let localBin: string = $state(settings.terminal.custom_bin ?? "");
  let localArgs: string = $state(settings.terminal.custom_args ?? "");

  $effect(() => {
    localMode = settings.terminal.mode || "auto";
    localBin = settings.terminal.custom_bin ?? "";
    localArgs = settings.terminal.custom_args ?? "";
  });

  async function saveTerminal() {
    configureGeneratedClient();
    await ConfigService.postApiV1ConfigTerminal({
      requestBody: {
        mode: localMode as TerminalConfigBody.mode,
        custom_bin: localBin || undefined,
        custom_args: localArgs || undefined,
      },
    });
    // Reload settings to pick up the saved values
    await settings.load();
  }

  let dirty = $derived(
    localMode !== (settings.terminal.mode || "auto") ||
      localBin !== (settings.terminal.custom_bin ?? "") ||
      localArgs !== (settings.terminal.custom_args ?? ""),
  );
</script>

<SettingsSection
  title={m.settings_terminal_title()}
  description={m.settings_terminal_description()}
>
  <div class="setting-row">
    <span class="setting-label">{m.settings_terminal_launch_mode()}</span>
    <div class="setting-options">
      {#each MODES as opt}
        <button
          class="option-btn"
          class:active={localMode === opt.value}
          onclick={() => (localMode = opt.value)}
        >
          {opt.label}
        </button>
      {/each}
    </div>
  </div>

  {#if localMode === "custom"}
    <div class="setting-row column">
      <label class="setting-label" for="terminal-bin">{m.settings_terminal_terminal_binary()}</label>
      <input
        id="terminal-bin"
        class="setting-input"
        type="text"
        placeholder="/usr/bin/kitty"
        bind:value={localBin}
      />
    </div>

    <div class="setting-row column">
      <label class="setting-label" for="terminal-args">
        {m.settings_terminal_arguments()} <span class="hint">{m.settings_terminal_args_hint()}</span>
      </label>
      <input
        id="terminal-args"
        class="setting-input"
        type="text"
        placeholder="-- bash -c {"{cmd}"}"
        bind:value={localArgs}
      />
    </div>
  {/if}

  {#if dirty}
    <div class="save-row">
      <button
        class="save-btn"
        disabled={settings.saving}
        onclick={saveTerminal}
      >
        {settings.saving ? m.settings_terminal_saving() : m.settings_terminal_save()}
      </button>
    </div>
  {/if}
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

  .hint {
    font-weight: 400;
    color: var(--text-muted);
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

  .setting-input {
    width: 100%;
    height: 30px;
    padding: 0 10px;
    border-radius: var(--radius-sm);
    font-size: 12px;
    font-family: var(--font-mono, monospace);
    color: var(--text-primary);
    background: var(--bg-inset);
    border: 1px solid var(--border-muted);
    transition: border-color 0.15s;
  }

  .setting-input:focus {
    outline: none;
    border-color: var(--accent-blue);
  }

  .save-row {
    display: flex;
    justify-content: flex-end;
  }

  .save-btn {
    height: 28px;
    padding: 0 16px;
    border-radius: var(--radius-sm);
    font-size: 12px;
    font-weight: 500;
    color: var(--accent-blue-foreground);
    background: var(--accent-blue);
    border: none;
    cursor: pointer;
    transition: opacity 0.12s;
  }

  .save-btn:hover:not(:disabled) {
    opacity: 0.9;
  }

  .save-btn:disabled {
    opacity: 0.6;
    cursor: default;
  }
</style>
