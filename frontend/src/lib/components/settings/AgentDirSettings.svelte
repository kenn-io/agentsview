<script lang="ts">
  import { Toggle } from "@kenn-io/kit-ui";
  import { m } from "../../i18n/index.js";
  import { settings } from "../../stores/settings.svelte.js";

  let pendingDisabledAgents: string[] = $state([]);
  let providerSaving = $state(false);
  let saveFailed = $state(false);
  let restartRequired = $state(false);

  $effect(() => {
    if (!providerSaving) {
      pendingDisabledAgents = [...settings.disabledAgents];
    }
  });

  function providerEnabled(id: string): boolean {
    return !pendingDisabledAgents.includes(id);
  }

  async function setProviderEnabled(id: string, enabled: boolean) {
    if (providerSaving || settings.readOnly) return;

    const confirmed = [...settings.disabledAgents];
    const next = enabled
      ? confirmed.filter((agent) => agent !== id)
      : [...confirmed, id];
    pendingDisabledAgents = next;
    providerSaving = true;
    saveFailed = false;

    const saved = await settings.save({ disabled_agents: next });
    if (saved) {
      pendingDisabledAgents = [...settings.disabledAgents];
      restartRequired = true;
    } else {
      pendingDisabledAgents = confirmed;
      saveFailed = true;
    }
    providerSaving = false;
  }
</script>

<div class="provider-list">
  {#each settings.sessionProviders as provider (provider.id)}
    <div class="provider-row">
      <div class="provider-details">
        <span class="provider-name">{provider.display_name}</span>
        <div class="provider-paths">
          {#if provider.dirs.length === 0}
            <span class="provider-not-configured">
              {m.settings_session_providers_not_configured()}
            </span>
          {:else}
            {#each provider.dirs as dir}
              <code class="provider-path">{dir}</code>
            {/each}
          {/if}
        </div>
      </div>
      <Toggle
        checked={providerEnabled(provider.id)}
        disabled={providerSaving || settings.readOnly}
        ariaLabel={m.settings_session_providers_enable_aria({
          provider: provider.display_name,
        })}
        onchange={(enabled) => setProviderEnabled(provider.id, enabled)}
      >
        {providerEnabled(provider.id)
          ? m.settings_session_providers_enabled()
          : m.settings_session_providers_disabled()}
      </Toggle>
    </div>
  {/each}

  {#if restartRequired}
    <p class="provider-status" role="status" aria-live="polite">
      {m.settings_session_providers_restart_notice()}
    </p>
  {/if}
  {#if saveFailed}
    <p class="provider-error" role="alert">
      {m.settings_session_providers_save_failed()}
    </p>
  {/if}
</div>

<style>
  .provider-list {
    display: flex;
    flex-direction: column;
  }

  .provider-row {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-5);
    min-height: 50px;
    padding: 8px 0;
    border-bottom: 1px solid var(--border-muted);
  }

  .provider-row:first-child {
    padding-top: 0;
  }

  .provider-details {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    min-width: 0;
  }

  .provider-name {
    color: var(--text-primary);
    font-size: 12px;
    font-weight: 600;
  }

  .provider-paths {
    display: flex;
    flex-direction: column;
    gap: 2px;
    min-width: 0;
  }

  .provider-path,
  .provider-not-configured {
    color: var(--text-muted);
    font-size: 11px;
    line-height: 1.35;
  }

  .provider-path {
    overflow-wrap: anywhere;
  }

  .provider-not-configured {
    font-style: italic;
  }

  .provider-status,
  .provider-error {
    margin: 10px 0 0;
    font-size: 11px;
    line-height: 1.5;
  }

  .provider-status {
    color: var(--text-muted);
  }

  .provider-error {
    color: var(--accent-red);
  }

  @media (max-width: 640px) {
    .provider-row {
      align-items: flex-start;
    }
  }
</style>
