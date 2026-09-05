<script lang="ts">
  import { TextInput } from "@kenn-io/kit-ui";
  import { m } from "../../i18n/index.js";
  import { settings } from "../../stores/settings.svelte.js";
  import { ConfigService } from "../../api/generated/index";

  let tokenInput: string = $state("");
  let saving: boolean = $state(false);
  let error: string | null = $state(null);
  let success: string | null = $state(null);

  async function handleSave() {
    if (settings.saving || !tokenInput.trim()) return;
    saving = true;
    error = null;
    success = null;
    try {
      await settings.runMutation(async () => {
        await ConfigService.postApiV1ConfigGithub({ token: tokenInput.trim() });
        tokenInput = "";
        success = m.settings_github_token_saved();
        await settings.load();
      });
    } catch (e) {
      error = e instanceof Error ? e.message : m.settings_github_save_failed();
    } finally {
      saving = false;
    }
  }
</script>

<div class="github-settings">
  <div class="status-row">
    <span class="status-label">{m.settings_github_status()}</span>
    <span class="status-value" class:configured={settings.githubConfigured}>
      {settings.githubConfigured ? m.settings_github_configured() : m.settings_github_not_configured()}
    </span>
  </div>

  <div class="token-row">
    <TextInput
      class="setting-input"
      size="md"
      type="password"
      placeholder="ghp_..."
      bind:value={tokenInput}
    />
    <button
      class="save-btn"
      disabled={saving || settings.saving || !tokenInput.trim()}
      onclick={handleSave}
    >
      {saving ? m.settings_github_saving() : m.settings_github_save_token()}
    </button>
  </div>

  {#if error}
    <p class="msg error">{error}</p>
  {/if}
  {#if success}
    <p class="msg success">{success}</p>
  {/if}
</div>

<style>
  .github-settings {
    display: flex;
    flex-direction: column;
    gap: var(--space-5);
  }

  .status-row {
    display: flex;
    align-items: center;
    gap: 8px;
  }

  .status-label {
    font-size: 12px;
    font-weight: 500;
    color: var(--text-secondary);
  }

  .status-value {
    font-size: 12px;
    color: var(--text-muted);
  }

  .status-value.configured {
    color: var(--accent-green);
  }

  .token-row {
    display: flex;
    gap: 8px;
  }

  :global(.setting-input.kit-text-input) {
    flex: 1;
    font-family: var(--font-mono, monospace);
  }

  .save-btn {
    height: 30px;
    padding: 0 14px;
    border-radius: var(--radius-sm);
    font-size: 12px;
    font-weight: 500;
    color: var(--accent-blue-foreground);
    background: var(--accent-blue);
    border: none;
    cursor: pointer;
    white-space: nowrap;
    transition: opacity 0.12s;
  }

  .save-btn:hover:not(:disabled) {
    opacity: 0.9;
  }

  .save-btn:disabled {
    opacity: 0.6;
    cursor: default;
  }

  .msg {
    font-size: 11px;
    margin: 0;
  }

  .msg.error {
    color: var(--accent-red, #ef4444);
  }

  .msg.success {
    color: var(--accent-green, #22c55e);
  }
</style>
