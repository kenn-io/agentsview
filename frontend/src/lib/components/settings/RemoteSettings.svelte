<script lang="ts">
  import { Button, TextInput, Toggle } from "@kenn-io/kit-ui";
  import { onDestroy } from "svelte";
  import { m } from "../../i18n/index.js";
  import { copyToClipboard } from "../../utils/clipboard.js";
  import { settings } from "../../stores/settings.svelte.js";
  import {
    ApiError,
    getServerUrl,
    setServerUrl,
    getAuthToken,
    setAuthToken,
    isRemoteConnection,
  } from "../../api/runtime.js";
  import { MetadataService } from "../../api/generated/index.js";
  import { LatestRead } from "../../utils/latest-read.js";

  let serverUrl: string = $state(getServerUrl());
  let tokenInput: string = $state(getAuthToken());
  let testing: boolean = $state(false);
  let testResult: { ok: boolean; message: string } | null = $state(null);
  let saving: boolean = $state(false);
  let saveMsg: string | null = $state(null);
  let remoteToggling: boolean = $state(false);
  let pendingRequireAuth: boolean = $state(settings.requireAuth);

  let isRemote: boolean = $derived(isRemoteConnection());
  let copied: boolean = $state(false);
  const versionRead = new LatestRead();

  $effect(() => {
    if (!remoteToggling) pendingRequireAuth = settings.requireAuth;
  });

  async function handleTestConnection() {
    if (!serverUrl.trim()) return;
    testing = true;
    const signal = versionRead.begin();
    testResult = null;
    try {
      const base = serverUrl.replace(/\/+$/, "");
      const headers: Record<string, string> = {};
      if (tokenInput.trim()) {
        headers["Authorization"] = `Bearer ${tokenInput.trim()}`;
      }
      const data = await MetadataService.getApiV1Version({ baseUrl: base, headers, signal });
      if (!versionRead.isCurrent(signal)) return;
      testResult = {
        ok: true,
        message: m.settings_remote_connected_version({ version: data.version || m.settings_remote_unknown() }),
      };
    } catch (e) {
      if (signal.aborted || !versionRead.isCurrent(signal)) return;
      testResult = {
        ok: false,
        message: e instanceof ApiError
          ? m.settings_remote_server_returned({ status: e.status })
          : e instanceof Error ? e.message : m.settings_remote_connection_failed(),
      };
    } finally {
      if (versionRead.finish(signal)) testing = false;
    }
  }

  onDestroy(() => versionRead.cancel());

  function handleConnect() {
    if (!serverUrl.trim()) return;
    const url = serverUrl.replace(/\/+$/, "");
    setServerUrl(url);
    setAuthToken(tokenInput.trim());
    saveMsg = m.settings_remote_connected_reloading();
    setTimeout(() => window.location.reload(), 500);
  }

  function handleDisconnect() {
    // Clear the remote token before clearing the URL, so the
    // scoped key resolves to the remote server's token.
    setAuthToken("");
    setServerUrl("");
    saveMsg = m.settings_remote_disconnected_reloading();
    setTimeout(() => window.location.reload(), 500);
  }

  async function handleToggleRemote(requireAuth: boolean) {
    if (settings.saving || settings.readOnly) return;
    pendingRequireAuth = requireAuth;
    remoteToggling = true;
    try {
      await settings.save({ require_auth: requireAuth });
    } finally {
      pendingRequireAuth = settings.requireAuth;
      remoteToggling = false;
    }
  }

  function handleCopyToken() {
    if (!settings.authToken) return;
    // Fire-and-forget like the previous navigator.clipboard call: the
    // copied indicator flips immediately regardless of the async result.
    void copyToClipboard(settings.authToken);
    copied = true;
    setTimeout(() => (copied = false), 2000);
  }
</script>

<div class="remote-settings">
  {#if !isRemote}
    <div class="subsection">
      <div class="setting-row">
        <span class="setting-label">{m.settings_remote_require_auth()}</span>
        <Toggle
          checked={pendingRequireAuth}
          disabled={remoteToggling || settings.saving || settings.readOnly}
          ariaLabel={m.settings_remote_require_auth()}
          onchange={handleToggleRemote}
        >
          {pendingRequireAuth ? m.settings_remote_enabled() : m.settings_remote_disabled()}
        </Toggle>
      </div>

      <p class="restart-note control-offset">
        {m.settings_remote_restart_note()}
      </p>

      {#if settings.requireAuth && settings.authToken}
        <div class="security-warning">
          {m.settings_remote_security_warning()}
        </div>

        <div class="setting-row">
          <span class="setting-label">{m.settings_remote_auth_token()}</span>
          <div class="token-row">
            <code class="token-value">{settings.authToken}</code>
            <Button onclick={handleCopyToken}>
              {copied ? m.settings_remote_copied() : m.settings_remote_copy()}
            </Button>
          </div>
        </div>

        <div class="setting-row">
          <span class="setting-label">{m.settings_remote_server()}</span>
          {#if settings.host === "0.0.0.0" || settings.host === "::"}
            <span class="info-value">
              {m.settings_remote_listening_all({ port: settings.port })}
            </span>
          {:else}
            <code class="info-value"
              >http://{settings.host}:{settings.port}</code
            >
          {/if}
        </div>
      {/if}
    </div>

    <div class="divider"></div>
  {/if}

  <div class="subsection">
    <span class="subsection-title">
      {isRemote ? m.settings_remote_remote_connection() : m.settings_remote_connect_to_remote()}
    </span>

    {#if isRemote}
      <div class="setting-row">
        <span class="setting-label">{m.settings_remote_connected_to()}</span>
        <code class="info-value">{getServerUrl()}</code>
      </div>
      <div class="actions control-offset">
        <Button tone="danger" surface="solid" onclick={handleDisconnect}>
          {m.settings_remote_disconnect()}
        </Button>
      </div>
    {:else}
      <div class="setting-row">
        <label class="setting-label" for="remote-url">{m.settings_remote_server_url()}</label>
        <TextInput
          id="remote-url"
          class="setting-input"
          size="md"
          block
          type="url"
          placeholder="http://192.168.1.100:8080"
          bind:value={serverUrl}
        />
      </div>

      <div class="setting-row">
        <label class="setting-label" for="remote-token">{m.settings_remote_auth_token()}</label>
        <TextInput
          id="remote-token"
          class="setting-input"
          size="md"
          block
          type="password"
          placeholder={m.settings_remote_paste_token_from_server()}
          bind:value={tokenInput}
        />
      </div>

      <div class="actions control-offset">
        <Button
          disabled={testing || !serverUrl.trim()}
          onclick={handleTestConnection}
        >
          {testing ? m.settings_remote_testing() : m.settings_remote_test_connection()}
        </Button>
        <Button
          tone="info"
          surface="solid"
          disabled={saving || !serverUrl.trim()}
          onclick={handleConnect}
        >
          {m.settings_remote_connect()}
        </Button>
      </div>

      {#if testResult}
        <p class="msg control-offset" class:success={testResult.ok} class:error={!testResult.ok}>
          {testResult.message}
        </p>
      {/if}

      {#if saveMsg}
        <p class="msg success control-offset">{saveMsg}</p>
      {/if}
    {/if}
  </div>
</div>

<style>
  .remote-settings {
    display: flex;
    flex-direction: column;
    gap: var(--space-5);
  }

  .subsection {
    display: flex;
    flex-direction: column;
    gap: var(--space-5);
  }

  /* Same heading style as the subsections in Appearance. */
  .subsection-title {
    font-size: 13px;
    font-weight: 600;
    color: var(--text-primary);
  }

  .divider {
    border-top: 1px solid var(--border-muted);
    margin: 2px 0;
  }

  /* Rows use the shared settings grid from SettingsPage: label column,
     then the control. */
  .setting-label {
    font-size: 12px;
    font-weight: 500;
    color: var(--text-secondary);
  }

  /* Notes, actions, and messages start on the control column's edge. */
  .control-offset {
    padding-left: calc(var(--setting-label-width, 148px) + 12px);
  }

  .token-row {
    display: flex;
    align-items: center;
    gap: 8px;
    width: 100%;
    min-width: 0;
  }

  .token-value {
    font-size: 11px;
    font-family: var(--font-mono, monospace);
    color: var(--text-primary);
    background: var(--bg-inset);
    padding: 4px 8px;
    border-radius: var(--radius-sm);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    flex: 1;
    min-width: 0;
  }

  .info-value {
    font-size: 12px;
    font-family: var(--font-mono, monospace);
    color: var(--text-primary);
    overflow-wrap: anywhere;
  }

  .setting-row :global(.setting-input.kit-text-input) {
    font-family: var(--font-mono, monospace);
  }

  .actions {
    display: flex;
    flex-wrap: wrap;
    gap: 8px;
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

  /* Upright muted text, like the other settings notes. */
  .restart-note {
    font-size: 11px;
    line-height: 1.4;
    color: var(--text-muted);
    margin: 0;
  }

  .security-warning {
    font-size: 11px;
    color: var(--accent-amber, #f59e0b);
    background: color-mix(in srgb, var(--accent-amber, #f59e0b) 8%, transparent);
    border: 1px solid color-mix(in srgb, var(--accent-amber, #f59e0b) 25%, transparent);
    border-radius: var(--radius-sm);
    padding: 8px 10px;
    line-height: 1.5;
  }

  @media (max-width: 640px) {
    .control-offset {
      padding-left: 0;
    }
  }
</style>
