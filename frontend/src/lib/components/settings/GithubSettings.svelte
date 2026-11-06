<script lang="ts">
  import { Button, TextInput } from "@kenn-io/kit-ui";
  import { m } from "../../i18n/index.js";
  import { CircleCheckIcon, InfoIcon } from "../../icons.js";
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

<!-- One setting, so the row is compact: the label fits its text and the
     status, token field, and messages share the control cell beside it. -->
<div class="setting-row compact github-settings">
  <span class="setting-label">{m.settings_github_status()}</span>
  <div class="control-stack">
    <span class="status-value" class:configured={settings.githubConfigured}>
      {#if settings.githubConfigured}
        <CircleCheckIcon size="14" aria-hidden="true" />
      {:else}
        <InfoIcon size="14" aria-hidden="true" />
      {/if}
      {settings.githubConfigured ? m.settings_github_configured() : m.settings_github_not_configured()}
    </span>

    <div class="token-row">
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
      <p class="msg error">{error}</p>
    {/if}
    {#if success}
      <p class="msg success">{success}</p>
    {/if}
  </div>
</div>

<style>
  /* The row uses the shared settings grid from SettingsPage. The label
     sits level with the status line, not centred on the whole stack. */
  .setting-label,
  .control-stack {
    align-self: baseline;
  }

  .control-stack {
    display: flex;
    flex-direction: column;
    gap: var(--space-5);
    width: 100%;
    min-width: 0;
  }

  .setting-label {
    font-size: 12px;
    font-weight: 500;
    color: var(--text-secondary);
  }

  /* A status glyph leads the word: the neutral info circle used for
     unavailable states when nothing is set up, a green check circle once
     a token is saved. */
  .status-value {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    font-size: 12px;
    color: var(--text-secondary);
  }

  .status-value :global(svg) {
    color: var(--text-muted);
  }

  .status-value.configured,
  .status-value.configured :global(svg) {
    color: var(--accent-green);
  }

  /* The button moves under the field when a long translated label
     leaves the field too little room beside it. */
  .token-row {
    display: flex;
    flex-wrap: wrap;
    gap: 8px;
  }

  /* Scoped to this row: a global flex: 1 collapsed the stacked inputs
     in Remote access to 18px. */
  .token-row :global(.setting-input.kit-text-input) {
    flex: 1 1 180px;
    min-width: 0;
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
</style>
