<script lang="ts">
  import { Button, TextInput } from "@kenn-io/kit-ui";
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
      });
      await settings.load();
    } catch (e) {
      error = e instanceof Error ? e.message : m.settings_github_save_failed();
    } finally {
      saving = false;
    }
  }
</script>

<div class="github-settings">
  <div class="setting-row">
    <span class="setting-label">{m.settings_github_status()}</span>
    <span class="status-value" class:configured={settings.githubConfigured}>
      {settings.githubConfigured ? m.settings_github_configured() : m.settings_github_not_configured()}
    </span>
  </div>

  <div class="token-row control-offset">
    <TextInput
      class="setting-input"
      size="md"
      type="password"
      placeholder="ghp_..."
      bind:value={tokenInput}
    />
    <Button
      tone="info"
      surface="solid"
      disabled={saving || settings.saving || !tokenInput.trim()}
      onclick={handleSave}
    >
      {saving ? m.settings_github_saving() : m.settings_github_save_token()}
    </Button>
  </div>

  {#if error}
    <p class="msg error control-offset">{error}</p>
  {/if}
  {#if success}
    <p class="msg success control-offset">{success}</p>
  {/if}
</div>

<style>
  .github-settings {
    display: flex;
    flex-direction: column;
    gap: var(--space-5);
  }

  /* The status row uses the shared settings grid from SettingsPage; the
     token field and its action start on the same control column. */
  .setting-label {
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

  .control-offset {
    padding-left: calc(var(--setting-label-width, 148px) + 12px);
  }

  .token-row {
    display: flex;
    gap: 8px;
  }

  /* Scoped to this row: a global flex: 1 collapsed the stacked inputs
     in Remote access to 18px. */
  .token-row :global(.setting-input.kit-text-input) {
    flex: 1;
    font-family: var(--font-mono, monospace);
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

  @media (max-width: 640px) {
    .control-offset {
      padding-left: 0;
    }
  }
</style>
