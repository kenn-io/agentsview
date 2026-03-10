<script lang="ts">
  import { onMount } from "svelte";
  import { settings } from "../../stores/settings.svelte.js";
  import AppearanceSettings from "./AppearanceSettings.svelte";
  import AgentDirSettings from "./AgentDirSettings.svelte";
  import TerminalSettings from "./TerminalSettings.svelte";
  import GithubSettings from "./GithubSettings.svelte";
  import RemoteSettings from "./RemoteSettings.svelte";

  onMount(() => {
    settings.load();
  });
</script>

<div class="settings-page">
  <div class="settings-header">
    <h2 class="settings-title">Settings</h2>
  </div>

  {#if settings.loading}
    <div class="settings-loading">Loading settings...</div>
  {:else if settings.error}
    <div class="settings-error">{settings.error}</div>
  {:else}
    <div class="settings-sections">
      <AppearanceSettings />
      <AgentDirSettings />
      <TerminalSettings />
      <GithubSettings />
      <RemoteSettings />
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
  }
</style>
