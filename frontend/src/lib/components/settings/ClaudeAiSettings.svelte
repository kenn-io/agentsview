<script lang="ts">
  import { Button } from "@kenn-io/kit-ui";
  import { onDestroy, onMount } from "svelte";

  type ClaudeAuthStatus = {
    connected: boolean;
    message: string;
  };

  type TauriInvoke = <T>(command: string) => Promise<T>;

  function desktopInvoke(): TauriInvoke | null {
    const tauri = (window as Window & {
      __TAURI__?: { core?: { invoke?: TauriInvoke } };
    }).__TAURI__;
    return tauri?.core?.invoke ?? null;
  }

  let status = $state<ClaudeAuthStatus>({
    connected: false,
    message: "Checking desktop connection…",
  });
  let busy = $state(false);
  let error = $state("");
  let pollTimer: ReturnType<typeof setInterval> | undefined;
  const isDesktop = $derived(desktopInvoke() !== null);

  function stopPolling() {
    if (pollTimer) clearInterval(pollTimer);
    pollTimer = undefined;
  }

  function startPolling() {
    stopPolling();
    pollTimer = setInterval(() => void refreshStatus(), 1000);
  }

  async function refreshStatus() {
    const invoke = desktopInvoke();
    if (!invoke) return;
    try {
      status = await invoke<ClaudeAuthStatus>("claude_auth_status");
      if (status.connected) stopPolling();
    } catch {
      // The sign-in window may be closing while its browser profile flushes.
      // Preserve the visible status and retry on the next poll.
    }
  }

  async function run(command: string) {
    const invoke = desktopInvoke();
    if (!invoke) return;
    busy = true;
    error = "";
    try {
      status = await invoke<ClaudeAuthStatus>(command);
      if (command === "claude_auth_start" && !status.connected) startPolling();
      if (status.connected || command === "claude_auth_disconnect") stopPolling();
    } catch (reason) {
      error = reason instanceof Error ? reason.message : String(reason);
    } finally {
      busy = false;
    }
  }

  onMount(() => {
    if (desktopInvoke()) void refreshStatus();
  });

  onDestroy(stopPolling);
</script>

<div class="claude-ai-settings">
  <div class="connection-copy">
    <div class="connection-heading">
      <span class:connected={status.connected} class="status-dot" aria-hidden="true"></span>
      <span>{status.connected ? "Connected" : "Not connected"}</span>
    </div>
    <p>
      Sign in through an isolated Claude.ai window. Your session is saved in the
      system credential store and is never shown here.
    </p>
    {#if !isDesktop}
      <p class="connection-note">Claude connection is available in the AgentsView desktop app.</p>
    {:else}
      <p class="connection-note" role="status">{status.message}</p>
    {/if}
    {#if error}
      <p class="connection-error" role="alert">{error}</p>
    {/if}
  </div>

  {#if isDesktop}
    <div class="connection-actions">
      <Button size="sm" disabled={busy} onclick={() => void run("claude_auth_start")}>
        {status.connected ? "Open Claude" : "Connect Claude"}
      </Button>
      {#if status.connected}
        <Button size="sm" disabled={busy} onclick={() => void run("claude_auth_test_connection")}>
          Test connection
        </Button>
        <Button size="sm" disabled={busy} onclick={() => void run("claude_auth_disconnect")}>
          Disconnect
        </Button>
      {/if}
    </div>
  {/if}
</div>

<style>
  .claude-ai-settings {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-5);
  }

  .connection-copy {
    min-width: 0;
  }

  .connection-heading {
    display: flex;
    align-items: center;
    gap: 8px;
    color: var(--text-primary);
    font-size: 13px;
    font-weight: 600;
  }

  .status-dot {
    width: 8px;
    height: 8px;
    border-radius: 999px;
    background: var(--text-muted);
  }

  .status-dot.connected {
    background: var(--accent-green, #22c55e);
  }

  p {
    margin: 6px 0 0;
    color: var(--text-secondary);
    font-size: 12px;
    line-height: 1.5;
  }

  .connection-note {
    color: var(--text-muted);
  }

  .connection-error {
    color: var(--accent-red, #ef4444);
  }

  .connection-actions {
    display: flex;
    flex: 0 0 auto;
    flex-wrap: wrap;
    justify-content: flex-end;
    gap: 8px;
  }

  @media (max-width: 640px) {
    .claude-ai-settings {
      align-items: flex-start;
      flex-direction: column;
    }

    .connection-actions {
      justify-content: flex-start;
    }
  }
</style>
