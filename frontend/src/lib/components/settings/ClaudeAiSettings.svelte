<script lang="ts">
  import { Button } from "@kenn-io/kit-ui";
  import { onDestroy, onMount } from "svelte";
  import { cancelClaudeAISync, configureClaudeAISchedule, getClaudeAISchedule, getClaudeAISyncStatus, startClaudeAISync, type ClaudeScheduleConfig, type ClaudeSyncStatus } from "../../api/client.js";

  type ClaudeAuthStatus = {
    connected: boolean;
    message: string;
  };
  type TauriInvoke = <T>(command: string, args?: Record<string, unknown>) => Promise<T>;

  let sync = $state<ClaudeSyncStatus>({ id: "", status: "idle", mode: "incremental", scanned: 0, changed: 0, fetched: 0, imported: 0, updated: 0, skipped: 0, failed: 0 });
  let schedule = $state<ClaudeScheduleConfig>({ enabled: false, interval_minutes: 360 });

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
  let cancelRequested = $state(false);
  let error = $state("");
  const syncActive = $derived(sync.status === "running" || sync.status === "cancelling");
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

  async function refreshSchedule() {
    try { schedule = await getClaudeAISchedule(); } catch (reason) { error = reason instanceof Error ? reason.message : String(reason); }
  }

  async function refreshSync() {
    try {
      sync = await getClaudeAISyncStatus();
    } catch (reason) {
      error = reason instanceof Error ? reason.message : String(reason);
    }
  }

  async function waitForSync() {
    while (sync.status === "running" || sync.status === "cancelling") {
      await new Promise((resolve) => setTimeout(resolve, 500));
      await refreshSync();
    }
    status = { ...status, message: `Claude sync ${sync.status}: scanned ${sync.scanned}, changed ${sync.changed}, imported ${sync.imported + sync.updated}, skipped ${sync.skipped}, failed ${sync.failed}.` };
    if (sync.error) error = sync.error;
  }

  async function configureSchedule(enabled: boolean) {
    try { schedule = await configureClaudeAISchedule({ ...schedule, enabled }); } catch (reason) { error = reason instanceof Error ? reason.message : String(reason); }
  }

  async function importConversations(repair = false) {
    busy = true;
    cancelRequested = false;
    error = "";
    try {
      sync = await startClaudeAISync(repair ? "repair" : "incremental");
      await waitForSync();
    } catch (reason) {
      // The scheduler or another browser tab can start a job between the last
      // status read and this click. Attach to that job instead of presenting a
      // misleading failed import.
      await refreshSync();
      if (syncActive) {
        error = "";
        await waitForSync();
      } else {
        error = reason instanceof Error ? reason.message : String(reason);
      }
    } finally {
      busy = false;
      cancelRequested = false;
    }
  }

  async function cancelImport() {
    cancelRequested = true;
    try { sync = await cancelClaudeAISync(); } catch (reason) { error = reason instanceof Error ? reason.message : String(reason); }
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
    if (desktopInvoke()) {
      void refreshStatus();
      void refreshSchedule();
      void refreshSync();
    }
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
        Connect
      </Button>
      {#if status.connected}
        <Button size="sm" disabled={busy || syncActive} onclick={() => void importConversations()}>
          Import
        </Button>
        <Button size="sm" disabled={busy || syncActive} onclick={() => void importConversations(true)}>
          Repair import
        </Button>
        {#if busy || syncActive}
          <Button size="sm" disabled={cancelRequested} onclick={() => void cancelImport()}>
            {cancelRequested ? "Cancelling…" : "Cancel"}
          </Button>
        {:else if error}
          <Button size="sm" onclick={() => void importConversations()}>
            Retry failed
          </Button>
        {/if}
        <Button size="sm" disabled={busy} onclick={() => void run("claude_auth_disconnect")}>
          Disconnect
        </Button>
      {/if}
    </div>
    <label class="schedule-control">
      <input type="checkbox" checked={schedule.enabled} disabled={busy} onchange={(event) => void configureSchedule((event.currentTarget as HTMLInputElement).checked)} />
      Sync automatically while AgentsView is running
    </label>
  {/if}
</div>

<style>
  .claude-ai-settings {
    display: flex;
    flex-direction: column;
    align-items: stretch;
    gap: var(--space-4);
  }

  .connection-copy {
    min-width: 0;
    max-width: 44rem;
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



  .schedule-control {
    color: var(--text-secondary);
    font-size: 13px;
  }

  .connection-actions {
    display: flex;
    flex: 0 0 auto;
    flex-wrap: wrap;
    justify-content: flex-start;
    gap: 8px;
  }


</style>
