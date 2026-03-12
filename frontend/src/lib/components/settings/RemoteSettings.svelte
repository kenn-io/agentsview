<script lang="ts">
  import SettingsSection from "./SettingsSection.svelte";
  import { settings } from "../../stores/settings.svelte.js";
  import {
    getServerUrl,
    setServerUrl,
    getAuthToken,
    setAuthToken,
    isRemoteConnection,
  } from "../../api/client.js";

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
          message: `Connected (v${data.version || "unknown"})`,
        };
      } else {
        testResult = { ok: false, message: `Server returned ${res.status}` };
      }
    } catch (e) {
      testResult = {
        ok: false,
        message: e instanceof Error ? e.message : "Connection failed",
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
    saveMsg = "Connected. Reloading...";
    setTimeout(() => window.location.reload(), 500);
  }

  function handleDisconnect() {
    // Clear the remote token before clearing the URL, so the
    // scoped key resolves to the remote server's token.
    setAuthToken("");
    setServerUrl("");
    saveMsg = "Disconnected. Reloading...";
    setTimeout(() => window.location.reload(), 500);
  }

  async function handleToggleRemote() {
    remoteToggling = true;
    try {
      await settings.save({ remote_access: !settings.remoteAccess });
    } finally {
      remoteToggling = false;
    }
  }

  function handleCopyToken() {
    if (!settings.authToken) return;
    navigator.clipboard.writeText(settings.authToken);
    copied = true;
    setTimeout(() => (copied = false), 2000);
  }
</script>

<SettingsSection
  title="Remote Access"
  description="Connect to a remote agentsview server or enable remote access for this instance."
>
  {#if !isRemote}
    <div class="subsection">
      <div class="toggle-row">
        <span class="toggle-label">Allow remote connections</span>
        <button
          class="toggle-btn"
          class:active={settings.remoteAccess}
          disabled={remoteToggling}
          onclick={handleToggleRemote}
        >
          {settings.remoteAccess ? "Enabled" : "Disabled"}
        </button>
      </div>

      <p class="restart-note">
        Note: Toggling remote access requires a server restart to take effect.
      </p>

      {#if settings.remoteAccess && settings.authToken}
        <div class="security-warning">
          Warning: Remote connections use unencrypted HTTP. Use a secure
          tunnel (Tailscale, SSH tunnel, or a reverse proxy with TLS) to
          protect your data in transit.
        </div>

        <div class="token-display">
          <span class="field-label">Auth Token</span>
          <div class="token-row">
            <code class="token-value">{settings.authToken}</code>
            <button class="copy-btn" onclick={handleCopyToken}>
              {copied ? "Copied" : "Copy"}
            </button>
          </div>
        </div>

        <div class="server-info">
          <span class="field-label">Server</span>
          {#if settings.host === "0.0.0.0" || settings.host === "::"}
            <span class="info-value">
              Listening on all interfaces (port {settings.port}).
              Connect using your machine's IP address or hostname.
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
      {isRemote ? "Remote Connection" : "Connect to Remote Server"}
    </span>

    {#if isRemote}
      <div class="connected-info">
        <span class="field-label">Connected to</span>
        <code class="info-value">{getServerUrl()}</code>
      </div>
      <button class="disconnect-btn" onclick={handleDisconnect}>
        Disconnect
      </button>
    {:else}
      <div class="field">
        <label class="field-label" for="remote-url">Server URL</label>
        <input
          id="remote-url"
          class="setting-input"
          type="url"
          placeholder="http://192.168.1.100:8080"
          bind:value={serverUrl}
        />
      </div>

      <div class="field">
        <label class="field-label" for="remote-token">Auth Token</label>
        <input
          id="remote-token"
          class="setting-input"
          type="password"
          placeholder="Paste auth token from server"
          bind:value={tokenInput}
        />
      </div>

      <div class="actions">
        <button
          class="test-btn"
          disabled={testing || !serverUrl.trim()}
          onclick={handleTestConnection}
        >
          {testing ? "Testing..." : "Test Connection"}
        </button>
        <button
          class="connect-btn"
          disabled={saving || !serverUrl.trim()}
          onclick={handleConnect}
        >
          Connect
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
    color: white;
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
    color: white;
    background: var(--accent-blue);
  }

  .disconnect-btn {
    color: white;
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
