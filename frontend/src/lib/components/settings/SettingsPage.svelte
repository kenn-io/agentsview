<script lang="ts">
  import { Button, TextInput } from "@kenn-io/kit-ui";
  import { onMount } from "svelte";
  import { settings } from "../../stores/settings.svelte.js";
  import { sync } from "../../stores/sync.svelte.js";
  import { ui } from "../../stores/ui.svelte.js";
  import { router } from "../../stores/router.svelte.js";
  import { setAuthToken, getAuthToken, setServerUrl, isRemoteConnection } from "../../api/runtime.js";
  import { m } from "../../i18n/index.js";
  import AppearanceSettings from "./AppearanceSettings.svelte";
  import AgentDirSettings from "./AgentDirSettings.svelte";
  import DateRangeSettings from "./DateRangeSettings.svelte";
  import TerminalSettings from "./TerminalSettings.svelte";
  import EmbeddingsSettings from "./EmbeddingsSettings.svelte";
  import GithubSettings from "./GithubSettings.svelte";
  import LanguageSettings from "./LanguageSettings.svelte";
  import RemoteSettings from "./RemoteSettings.svelte";
  import SettingsSection from "./SettingsSection.svelte";

  let authTokenInput: string = $state("");

  onMount(() => {
    authTokenInput = getAuthToken();
    settings.load();
  });

  function handleAuthSubmit() {
    const token = authTokenInput.trim();
    if (!token) return;
    setAuthToken(token);
    window.location.reload();
  }
</script>

<div class="settings-page">
  <div class="settings-header">
    <h2 class="settings-title">{m.settings_title()}</h2>
  </div>

  {#if settings.loading || !settings.loaded}
    <div class="settings-loading">{m.settings_loading()}</div>
  {:else if settings.needsAuth}
    <div class="auth-prompt">
      <h3 class="auth-title">{m.app_auth_title()}</h3>
      <p class="auth-description">
        {m.app_auth_description()}
      </p>
      <div class="auth-field">
        <TextInput
          class="auth-input"
          size="md"
          type="password"
          placeholder={m.app_auth_placeholder()}
          bind:value={authTokenInput}
          onkeydown={(e) => { if (e.key === "Enter") handleAuthSubmit(); }}
        />
        <button
          class="auth-btn"
          disabled={!authTokenInput.trim()}
          onclick={handleAuthSubmit}
        >
          {m.app_auth_authenticate()}
        </button>
      </div>
      <button
        class="auth-disconnect"
        onclick={() => {
          setAuthToken("");
          setServerUrl("");
          settings.needsAuth = false;
          settings.load();
        }}
      >
        {m.app_auth_disconnect_reset()}
      </button>
    </div>
  {:else if settings.error}
    <div class="settings-error">
      <p>{settings.error}</p>
      {#if isRemoteConnection()}
        <button
          class="auth-disconnect"
          onclick={() => {
            setAuthToken("");
            setServerUrl("");
            window.location.reload();
        }}
      >
          {m.app_auth_disconnect_reset()}
        </button>
      {/if}
    </div>
  {:else}
    <div class="settings-sections">
      <LanguageSettings />
      <AppearanceSettings />
      <DateRangeSettings />
      <AgentDirSettings />
      <TerminalSettings />
      <SettingsSection
        title={m.worktree_title()}
        description={m.settings_worktree_moved()}
      >
        <Button
          label={m.settings_worktree_moved_link()}
          onclick={() => router.navigate("data", { view: "rules" })}
        />
      </SettingsSection>
      <EmbeddingsSettings />
      <GithubSettings />
      <RemoteSettings />

      <div class="settings-actions">
        <Button
          onclick={() => {
            if (!sync.readOnly) ui.activeModal = "resync";
          }}
          disabled={sync.readOnly}
          title={sync.readOnly
            ? m.settings_resync_title_unavailable()
            : m.resync_title()}
        >
          {m.resync_title()}
        </Button>
        <span class="settings-actions-hint">
          {sync.readOnly
            ? m.settings_resync_unavailable_hint()
            : m.settings_resync_hint()}
        </span>
      </div>
    </div>
  {/if}
</div>

<style>
  .settings-page {
    max-width: 640px;
    margin: 0 auto;
    padding: 24px 20px 48px;
  }

  .settings-header {
    margin-bottom: 20px;
  }

  .settings-title {
    font-size: 18px;
    font-weight: 600;
    color: var(--text-primary);
    margin: 0;
  }

  .settings-sections {
    display: flex;
    flex-direction: column;
    gap: 16px;
  }

  .settings-loading,
  .settings-error {
    font-size: 13px;
    color: var(--text-muted);
    padding: 40px 0;
    text-align: center;
  }

  .settings-error {
    color: var(--accent-red, #ef4444);
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: 8px;
  }

  .settings-error p {
    margin: 0;
  }

  .settings-actions {
    display: flex;
    align-items: center;
    gap: 12px;
    padding: 16px 0 0;
    border-top: 1px solid var(--border-muted);
  }

  .settings-actions-hint {
    font-size: 11px;
    color: var(--text-muted);
  }

  .auth-prompt {
    text-align: center;
    padding: 40px 20px;
  }

  .auth-title {
    font-size: 16px;
    font-weight: 600;
    color: var(--text-primary);
    margin: 0 0 8px;
  }

  .auth-description {
    font-size: 13px;
    color: var(--text-muted);
    margin: 0 0 20px;
    max-width: 400px;
    margin-left: auto;
    margin-right: auto;
  }

  .auth-field {
    display: flex;
    gap: 8px;
    justify-content: center;
    max-width: 400px;
    margin: 0 auto;
  }

  :global(.auth-input.kit-text-input) {
    flex: 1;
    height: 34px;
    font-family: var(--font-mono, monospace);
  }

  .auth-btn {
    height: 34px;
    padding: 0 16px;
    border-radius: var(--radius-sm);
    font-size: 13px;
    font-weight: 500;
    color: var(--accent-blue-foreground);
    background: var(--accent-blue);
    border: none;
    cursor: pointer;
    white-space: nowrap;
  }

  .auth-btn:disabled {
    opacity: 0.6;
    cursor: default;
  }

  .auth-btn:hover:not(:disabled) {
    opacity: 0.9;
  }

  .auth-disconnect {
    margin-top: 12px;
    background: none;
    border: none;
    color: var(--text-muted);
    font-size: 12px;
    cursor: pointer;
    text-decoration: underline;
  }

  .auth-disconnect:hover {
    color: var(--text-secondary);
  }
</style>
