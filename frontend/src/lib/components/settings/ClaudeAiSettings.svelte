<script lang="ts">
  import { Button } from "@kenn-io/kit-ui";
  import { onDestroy, onMount } from "svelte";
  import { completeClaudeAICloud, failClaudeAICloud, importClaudeAICloud, planClaudeAICloud } from "../../api/client.js";

  type ClaudeAuthStatus = {
    connected: boolean;
    message: string;
  };
  type ClaudeScheduleStatus = {
    enabled: boolean;
    intervalMinutes: number;
    lastStartedAt?: string;
    lastCompletedAt?: string;
    lastError?: string;
    running: boolean;
    lastScanned: number;
    lastChanged: number;
    lastFetched: number;
    lastImported: number;
    lastSkipped: number;
    lastFailed: number;
  };

  type TauriInvoke = <T>(command: string, args?: Record<string, unknown>) => Promise<T>;

  type ClaudeBrowserSummaryBatch = {
    summaries: unknown[];
    has_more: boolean;
  };
  type ClaudeBrowserDetailBatch = {
    conversations: unknown[];
    skipped: number;
  };

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
  let schedule = $state<ClaudeScheduleStatus>({ enabled: false, intervalMinutes: 360, running: false, lastScanned: 0, lastChanged: 0, lastFetched: 0, lastImported: 0, lastSkipped: 0, lastFailed: 0 });
  let busy = $state(false);
  let cancelRequested = $state(false);
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

  async function refreshSchedule() {
    const invoke = desktopInvoke();
    if (!invoke) return;
    try { schedule = await invoke<ClaudeScheduleStatus>("claude_schedule_status"); } catch { /* sidecar may be starting */ }
  }

  async function configureSchedule(enabled: boolean) {
    const invoke = desktopInvoke();
    if (!invoke) return;
    schedule = await invoke<ClaudeScheduleStatus>("claude_schedule_configure", {
      enabled,
      intervalMinutes: schedule.intervalMinutes,
    });
  }

  async function configureScheduleInterval(intervalMinutes: number) {
    const invoke = desktopInvoke();
    if (!invoke) return;
    schedule = await invoke<ClaudeScheduleStatus>("claude_schedule_configure", { enabled: schedule.enabled, intervalMinutes });
  }

  async function importConversations(repair = false) {
    const invoke = desktopInvoke();
    if (!invoke) return;
    busy = true;
    cancelRequested = false;
    error = "";
    let offset = 0;
    let imported = 0;
    let updated = 0;
    let unchanged = 0;
    let fetched = 0;
    let skipped = 0;
    let failed = 0;
    try {
      for (;;) {
        status = { ...status, message: `Fetching Claude conversations (${offset} processed)…` };
        const batch = await invoke<ClaudeBrowserSummaryBatch>("claude_auth_fetch_import_batch", { offset, limit: 50 });
        if (batch.summaries.length === 0) break;
        const plan = await planClaudeAICloud(batch.summaries, repair);
        unchanged += plan.unchanged;
        const changedIDs = Array.isArray(plan.changed_ids) ? plan.changed_ids : [];
        const changed = batch.summaries.filter((summary) => changedIDs.includes((summary as { uuid?: string }).uuid ?? ""));
        skipped += plan.unchanged;
        status = { ...status, message: `Scanned ${plan.state.scanned}; changed ${plan.state.changed}, unchanged ${plan.state.skipped}, fetched ${fetched}, imported ${imported + updated}, failed ${failed}.` };
        if (changed.length > 0) {
          const details = await invoke<ClaudeBrowserDetailBatch>("claude_auth_fetch_import_details", { summaries: changed });
          skipped += details.skipped;
          fetched += details.conversations.length;
          if (details.conversations.length > 0) {
            status = { ...status, message: `Fetched ${fetched}; importing ${details.conversations.length} changed Claude conversations…` };
            const result = await importClaudeAICloud(details.conversations, undefined, repair);
            imported += result.imported;
            updated += result.updated;
          }
        }
        offset += batch.summaries.length;
        if (!batch.has_more) break;
      }
      await completeClaudeAICloud();
      status = { ...status, message: `Import complete: scanned ${offset}, changed ${fetched}, imported ${imported}, updated ${updated}, skipped ${skipped}, failed ${failed}.` };
    } catch (reason) {
      failed++;
      await failClaudeAICloud().catch(() => undefined);
      status = { ...status, message: `Import stopped: scanned ${offset}, fetched ${fetched}, imported ${imported + updated}, skipped ${skipped}, failed ${failed}.` };
      error = reason instanceof Error ? reason.message : String(reason);
    } finally {
      busy = false;
      cancelRequested = false;
    }
  }

  async function cancelImport() {
    const invoke = desktopInvoke();
    if (!invoke) return;
    cancelRequested = true;
    status = { ...status, message: "Cancelling Claude import after the current browser request…" };
    await invoke("claude_auth_cancel_import");
  }

  async function run(command: string) {
    const invoke = desktopInvoke();
    if (!invoke) return;
    busy = true;
    error = "";
    if (command === "claude_auth_browser_list_test") {
      status = { ...status, message: "Testing Claude browser transport…" };
    }
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
        <Button size="sm" disabled={busy} onclick={() => void run("claude_auth_browser_list_test")}>
          Test
        </Button>
        <Button size="sm" disabled={busy} onclick={() => void importConversations()}>
          Import
        </Button>
        <Button size="sm" disabled={busy} onclick={() => void importConversations(true)}>
          Repair import
        </Button>
        {#if busy}
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
      <input type="checkbox" checked={schedule.enabled} disabled={!status.connected || busy} onchange={(event) => void configureSchedule((event.currentTarget as HTMLInputElement).checked)} />
      Sync automatically while AgentsView is running
    </label>
    <label class="schedule-control">
      Every
      <select value={schedule.intervalMinutes} disabled={!status.connected || busy} onchange={(event) => void configureScheduleInterval(Number((event.currentTarget as HTMLSelectElement).value))}>
        <option value="60">1 hour</option>
        <option value="360">6 hours</option>
        <option value="720">12 hours</option>
        <option value="1440">24 hours</option>
      </select>
    </label>
    {#if schedule.running || schedule.lastCompletedAt || schedule.lastError}
      <p class="connection-note" role="status">
        {#if schedule.running}Scheduled Claude sync is running.
        {:else if schedule.lastError}Last scheduled sync failed: {schedule.lastError}
        {:else}Last scheduled sync completed: {schedule.lastCompletedAt}. Scanned {schedule.lastScanned}, changed {schedule.lastChanged}, fetched {schedule.lastFetched}, imported {schedule.lastImported}, skipped {schedule.lastSkipped}, failed {schedule.lastFailed}.{/if}
      </p>
    {/if}
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
    font-size: 12px;
  }

  .connection-actions {
    display: flex;
    flex: 0 0 auto;
    flex-wrap: wrap;
    justify-content: flex-start;
    gap: 8px;
  }


</style>
