<script lang="ts">
  import { m } from "../../i18n/index.js";
  import SettingsSection from "./SettingsSection.svelte";
  import { copyToClipboard } from "../../utils/clipboard.js";
  import { settings } from "../../stores/settings.svelte.js";
  import {
    getServerUrl,
    setServerUrl,
    getAuthToken,
    setAuthToken,
    isRemoteConnection,
  } from "../../api/runtime.js";

  let serverUrl: string = $state(getServerUrl());
  let tokenInput: string = $state(getAuthToken());
  let testing: boolean = $state(false);
  let testResult: { ok: boolean; message: string } | null = $state(null);
  let saving: boolean = $state(false);
  let saveMsg: string | null = $state(null);
  let remoteToggling: boolean = $state(false);

  let isRemote: boolean = $derived(isRemoteConnection());
  let copied: boolean = $state(false);

  async function handleTestConnection() {
    if (!serverUrl.trim()) return;
    testing = true;
    testResult = null;
    try {
      const base = serverUrl.replace(/\/+$/, "");
      const headers: Record<string, string> = {};
      if (tokenInput.trim()) {
        headers["Authorization"] = `Bearer ${tokenInput.trim()}`;
      }
      const res = await fetch(`${base}/api/v1/version`, { headers });
      if (res.ok) {
        const data = await res.json();
        testResult = {
          ok: true,
          message: m.settings_remote_connected_version({ version: data.version || m.settings_remote_unknown() }),
        };
      } else {
        testResult = { ok: false, message: m.settings_remote_server_returned({ status: res.status }) };
      }
    } catch (e) {
      testResult = {
        ok: false,
        message: e instanceof Error ? e.message : m.settings_remote_connection_failed(),
      };
    } finally {
      testing = false;
    }
  }

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

  async function handleToggleRemote() {
    remoteToggling = true;
    try {
      await settings.save({ require_auth: !settings.requireAuth });
    } finally {
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

<SettingsSection
  title={m.settings_remote_title()}
  description={m.settings_remote_description()}
>
  {#if !isRemote}
    <div class="subsection">
      <div class="toggle-row">
        <span class="toggle-label">{m.settings_remote_require_auth()}</span>
        <button
          class="toggle-btn"
          class:active={settings.requireAuth}
          disabled={remoteToggling}
          onclick={handleToggleRemote}
        >
          {settings.requireAuth ? m.settings_remote_enabled() : m.settings_remote_disabled()}
        </button>
      </div>

      <p class="restart-note">
        {m.settings_remote_restart_note()}
      </p>

      {#if settings.requireAuth && settings.authToken}
        <div class="security-warning">
          {m.settings_remote_security_warning()}
        </div>

        <div class="token-display">
          <span class="field-label">{m.settings_remote_auth_token()}</span>
          <div class="token-row">
            <code class="token-value">{settings.authToken}</code>
            <button class="copy-btn" onclick={handleCopyToken}>
              {copied ? m.settings_remote_copied() : m.settings_remote_copy()}
            </button>
          </div>
        </div>

        <div class="server-info">
          <span class="field-label">{m.settings_remote_server()}</span>
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
      <div class="connected-info">
        <span class="field-label">{m.settings_remote_connected_to()}</span>
        <code class="info-value">{getServerUrl()}</code>
      </div>
      <button class="disconnect-btn" onclick={handleDisconnect}>
        {m.settings_remote_disconnect()}
      </button>
    {:else}
      <div class="field">
        <label class="field-label" for="remote-url">{m.settings_remote_server_url()}</label>
        <input
          id="remote-url"
          class="setting-input"
          type="url"
          placeholder="http://192.168.1.100:8080"
          bind:value={serverUrl}
        />
      </div>

      <div class="field">
        <label class="field-label" for="remote-token">{m.settings_remote_auth_token()}</label>
        <input
          id="remote-token"
          class="setting-input"
          type="password"
          placeholder={m.settings_remote_paste_token_from_server()}
          bind:value={tokenInput}
        />
      </div>

      <div class="actions">
        <button
          class="test-btn"
          disabled={testing || !serverUrl.trim()}
          onclick={handleTestConnection}
        >
          {testing ? m.settings_remote_testing() : m.settings_remote_test_connection()}
        </button>
        <button
          class="connect-btn"
          disabled={saving || !serverUrl.trim()}
          onclick={handleConnect}
        >
          {m.settings_remote_connect()}
        </button>
      </div>

      {#if testResult}
        <p class="msg" class:success={testResult.ok} class:error={!testResult.ok}>
          {testResult.message}
        </p>
      {/if}

      {#if saveMsg}
        <p class="msg success">{saveMsg}</p>
      {/if}
    {/if}
  </div>
</SettingsSection>

<style>
  .subsection {
    display: flex;
    flex-direction: column;
    gap: 10px;
  }

  .subsection-title {
    font-size: 12px;
    font-weight: 600;
    color: var(--text-secondary);
  }

  .divider {
    border-top: 1px solid var(--border-muted);
    margin: 2px 0;
  }

  .toggle-row {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 8px;
  }

  .toggle-label {
    font-size: 12px;
    color: var(--text-primary);
  }

  .toggle-btn {
    height: 26px;
    padding: 0 12px;
    border-radius: var(--radius-sm);
    font-size: 11px;
    font-weight: 500;
    border: 1px solid var(--border-muted);
    cursor: pointer;
    background: var(--bg-inset);
    color: var(--text-secondary);
    transition:
      background 0.12s,
      color 0.12s;
  }

  .toggle-btn.active {
    background: var(--accent-green, #22c55e);
    color: var(--accent-green-foreground);
    border-color: transparent;
  }

  .toggle-btn:disabled {
    opacity: 0.6;
    cursor: default;
  }

  .token-display,
  .server-info,
  .connected-info {
    display: flex;
    flex-direction: column;
    gap: 4px;
  }

  .field-label {
    font-size: 11px;
    font-weight: 500;
    color: var(--text-muted);
  }

  .token-row {
    display: flex;
    align-items: center;
    gap: 8px;
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

  .copy-btn {
    height: 24px;
    padding: 0 10px;
    border-radius: var(--radius-sm);
    font-size: 11px;
    font-weight: 500;
    color: var(--text-secondary);
    background: var(--bg-inset);
    border: 1px solid var(--border-muted);
    cursor: pointer;
    white-space: nowrap;
    transition: opacity 0.12s;
  }

  .copy-btn:hover {
    opacity: 0.8;
  }

  .info-value {
    font-size: 12px;
    font-family: var(--font-mono, monospace);
    color: var(--text-primary);
  }

  .field {
    display: flex;
    flex-direction: column;
    gap: 4px;
  }

  .setting-input {
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

  .actions {
    display: flex;
    gap: 8px;
  }

  .test-btn,
  .connect-btn,
  .disconnect-btn {
    height: 30px;
    padding: 0 14px;
    border-radius: var(--radius-sm);
    font-size: 12px;
    font-weight: 500;
    border: none;
    cursor: pointer;
    white-space: nowrap;
    transition: opacity 0.12s;
  }

  .test-btn {
    color: var(--text-primary);
    background: var(--bg-inset);
    border: 1px solid var(--border-muted);
  }

  .connect-btn {
    color: var(--accent-blue-foreground);
    background: var(--accent-blue);
  }

  .disconnect-btn {
    color: var(--accent-red-foreground);
    background: var(--accent-red, #ef4444);
  }

  .test-btn:hover:not(:disabled),
  .connect-btn:hover:not(:disabled),
  .disconnect-btn:hover:not(:disabled) {
    opacity: 0.9;
  }

  .test-btn:disabled,
  .connect-btn:disabled {
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

  .restart-note {
    font-size: 11px;
    color: var(--text-muted);
    margin: 0;
    font-style: italic;
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
</style>
