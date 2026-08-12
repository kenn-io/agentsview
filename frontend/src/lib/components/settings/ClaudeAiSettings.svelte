<script lang="ts">
  import { Button, Spinner } from "@kenn-io/kit-ui";
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
  let syncPollTimer: ReturnType<typeof setInterval> | undefined;
  const isDesktop = $derived(desktopInvoke() !== null);

  function stopPolling() {
    if (pollTimer) clearInterval(pollTimer);
    pollTimer = undefined;
  }

  function startPolling() {
    stopPolling();
    pollTimer = setInterval(() => void refreshStatus(), 1000);
  }

  function stopSyncPolling() {
    if (syncPollTimer) clearInterval(syncPollTimer);
    syncPollTimer = undefined;
  }

  function startSyncPolling() {
    if (syncPollTimer) return;
    syncPollTimer = setInterval(() => void refreshSync(), 500);
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
      if (syncActive) startSyncPolling();
      else stopSyncPolling();
    } catch (reason) {
      error = reason instanceof Error ? reason.message : String(reason);
      stopSyncPolling();
    }
  }

  async function waitForSync() {
    startSyncPolling();
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

  onDestroy(() => {
    stopPolling();
    stopSyncPolling();
  });
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
    {#if syncActive}
      <div class="sync-progress" role="status" aria-live="polite">
        <span class="sync-progress-spinner" aria-hidden="true"><Spinner size={14} /></span>
        <div>
          <strong>{sync.status === "cancelling" ? "Cancelling Claude sync…" : "Syncing Claude conversations…"}</strong>
          <span>
            Scanned {sync.scanned} · changed {sync.changed} · fetched {sync.fetched} · imported {sync.imported + sync.updated} · skipped {sync.skipped}
          </span>
          <small>You can leave this page. The sync continues while AgentsView is running.</small>
        </div>
      </div>
    {:else if sync.status === "completed" || sync.status === "cancelled" || sync.status === "failed"}
      <p class:connection-error={sync.status === "failed"} class="sync-result" role="status">
        Claude sync {sync.status}: scanned {sync.scanned}, changed {sync.changed}, imported {sync.imported + sync.updated}, skipped {sync.skipped}, failed {sync.failed}.
      </p>
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

  .sync-progress {
    display: flex;
    gap: 8px;
    align-items: flex-start;
    margin-top: 12px;
    padding: 10px 12px;
    border: 1px solid var(--border-muted);
    border-radius: 6px;
    color: var(--text-secondary);
    font-size: 12px;
    line-height: 1.45;
  }

  .sync-progress-spinner {
    display: flex;
    margin-top: 2px;
    color: var(--accent-blue);
  }

  .sync-progress strong,
  .sync-progress span,
  .sync-progress small {
    display: block;
  }

  .sync-progress strong {
    color: var(--text-primary);
    font-weight: 600;
  }

  .sync-progress small,
  .sync-result {
    color: var(--text-muted);
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
