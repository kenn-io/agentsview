<script lang="ts">
  import { onMount } from "svelte";
  import { StatusBar } from "@kenn-io/kit-ui";
  import { m } from "../../i18n/index.js";
  import { sync } from "../../stores/sync.svelte.js";
  import { perf } from "../../stores/perf.svelte.js";
  import { ui } from "../../stores/ui.svelte.js";
  import { router } from "../../stores/router.svelte.js";
  import { ActivityIcon } from "../../icons.js";
  import {
    formatNumber,
    formatRelativeTime,
    formatTimestamp,
  } from "../../utils/format.js";

  const RELATIVE_TIME_REFRESH_MS = 10_000;
  const isMac = navigator.platform.toUpperCase().includes("MAC");
  const mod = isMac ? "⌘" : "Ctrl";
  let relativeTimeTick = $state(0);

  let progressText = $derived.by(() => {
    if (!sync.syncing || !sync.progress) return null;
    const p = sync.progress;
    if (p.detail) {
      if (p.sessions_total > 0) {
        const pct = Math.round(
          (p.sessions_done / p.sessions_total) * 100,
        );
        return `${p.detail}: ${pct}% (${p.sessions_done}/${p.sessions_total})`;
      }
      return p.detail;
    }
    if (p.phase === "discovering" || p.phase === "scan") {
      return m.status_bar_scanning({
        project: p.current_project || "",
      });
    }
    if (p.phase === "syncing" || p.phase === "parse") {
      const pct = p.sessions_total > 0
        ? Math.round((p.sessions_done / p.sessions_total) * 100)
        : 0;
      return m.status_bar_syncing_percent({
        percent: pct,
        done: p.sessions_done,
        total: p.sessions_total,
      });
    }
    return m.status_bar_syncing();
  });

  let progressTitle = $derived.by(() => {
    if (!sync.syncing || !sync.progress) return null;
    return sync.progress.hint || sync.progress.detail || null;
  });

  let lastSyncText = $derived.by(() => {
    // eslint-disable-next-line @typescript-eslint/no-unused-expressions -- reactive dependency: recompute relative time when the tick advances
    relativeTimeTick;
    return sync.lastSync
      ? formatRelativeTime(sync.lastSync)
      : null;
  });

  let lastSyncTimestamp = $derived(
    sync.lastSync ? formatTimestamp(sync.lastSync) : null,
  );

  onMount(() => {
    const interval = window.setInterval(() => {
      relativeTimeTick = Date.now();
    }, RELATIVE_TIME_REFRESH_MS);
    return () => window.clearInterval(interval);
  });
</script>

<StatusBar>
  {#snippet left()}
    {#if sync.stats}
      <span class="counts">
        <span>{m.status_bar_sessions({
          count: sync.stats.session_count,
          countLabel: formatNumber(sync.stats.session_count),
        })}</span>
        <span class="sep">&middot;</span>
        <span>{m.status_bar_messages({
          count: sync.stats.message_count,
          countLabel: formatNumber(sync.stats.message_count),
        })}</span>
        <span class="sep">&middot;</span>
        <span>{m.status_bar_projects({
          count: sync.stats.project_count,
          countLabel: formatNumber(sync.stats.project_count),
        })}</span>
      </span>
    {/if}
  {/snippet}

  {#snippet right()}
    <button
      class="perf-toggle"
      class:active={perf.panelOpen}
      onclick={() => perf.togglePanel()}
      title={m.status_bar_open_performance_debug()}
      aria-label={m.status_bar_open_performance_debug()}
    >
      <ActivityIcon size="12" strokeWidth="2" aria-hidden="true" />
      <span>{m.status_bar_perf()}</span>
    </button>
    <span class="sep">&middot;</span>
    {#if sync.remoteUnreachable}
      <button
        class="remote-warn"
        onclick={() => router.navigate("settings")}
        title={m.status_bar_remote_unreachable_title()}
      >
        {m.status_bar_remote_unreachable()}
      </button>
      <span class="sep">&middot;</span>
    {/if}
    {#if sync.backendDegraded}
      <button
        class="backend-warn"
        onclick={() => sync.loadStats()}
        title={sync.backendDegradedMessage ?? m.status_bar_sync_not_ready()}
      >
        {m.status_bar_sync_not_ready()}
      </button>
      <span class="sep">&middot;</span>
    {/if}
    {#if sync.isDesktop}
      <div class="zoom-controls">
        <button
          class="zoom-btn"
          onclick={() => ui.zoomOut()}
          disabled={ui.zoomLevel <= 67}
          title={m.status_bar_zoom_out({ shortcut: mod })}
        >
          &minus;
        </button>
        <button
          class="zoom-level"
          onclick={() => ui.resetZoom()}
          title={m.status_bar_reset_zoom({ shortcut: mod })}
        >
          {ui.zoomLevel}%
        </button>
        <button
          class="zoom-btn"
          onclick={() => ui.zoomIn()}
          disabled={ui.zoomLevel >= 200}
          title={m.status_bar_zoom_in({ shortcut: mod })}
        >
          +
        </button>
      </div>
      <span class="sep">&middot;</span>
    {/if}
    {#if sync.updateAvailable && !sync.isDesktop}
      {@const latestVersion = sync.latestVersion ?? ""}
      <button
        class="update-available"
        onclick={() => (ui.activeModal = "update")}
        title={m.status_bar_update_available_title({ version: latestVersion })}
      >
        {m.status_bar_update_available()}
      </button>
      <span class="sep">&middot;</span>
    {/if}
    {#if sync.versionMismatch}
      <button
        class="version-warn"
        onclick={() => window.location.reload()}
        title={m.status_bar_version_mismatch_title()}
      >
        {m.status_bar_version_mismatch()}
      </button>
    {/if}
    {#if progressText}
      {#if sync.versionMismatch}<span class="sep">&middot;</span>{/if}
      <span class="sync-progress" title={progressTitle ?? undefined}>
        {progressText}
      </span>
    {:else if lastSyncText}
      {#if sync.versionMismatch}<span class="sep">&middot;</span>{/if}
      <span title={lastSyncTimestamp ?? undefined}>
        {m.status_bar_synced_ago({ time: lastSyncText })}
      </span>
    {/if}
    {#if sync.serverVersion}
      {#if sync.versionMismatch || progressText || sync.lastSync}
        <span class="sep">&middot;</span>
      {/if}
      <button
        class="version"
        title={m.status_bar_build({ commit: sync.serverVersion.commit })}
        onclick={() => {
          if (ui.activeModal === "resync" && sync.syncing) return;
          ui.activeModal = "about";
        }}
      >
        {sync.serverVersion.version}
      </button>
    {/if}
  {/snippet}
</StatusBar>

<style>
  .counts {
    display: flex;
    align-items: center;
    gap: 4px;
    min-width: 0;
    overflow: hidden;
    white-space: nowrap;
  }

  .sep {
    color: var(--border-default);
  }

  .sync-progress {
    color: var(--accent-green);
  }

  .perf-toggle {
    height: 18px;
    display: flex;
    align-items: center;
    gap: 3px;
    padding: 0 5px;
    border-radius: var(--radius-sm);
    color: var(--text-muted);
    font-size: 10px;
    cursor: pointer;
  }

  .perf-toggle:hover,
  .perf-toggle.active {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }

  .update-available {
    color: var(--accent-blue);
    font-size: 10px;
    cursor: pointer;
    font-weight: 500;
  }

  .update-available:hover {
    text-decoration: underline;
  }

  .version-warn {
    color: var(--accent-red);
    font-size: 10px;
    cursor: pointer;
    font-weight: 500;
  }

  .version-warn:hover {
    text-decoration: underline;
  }

  .remote-warn {
    color: var(--accent-red);
    font-size: 10px;
    cursor: pointer;
    font-weight: 500;
  }

  .backend-warn {
    color: var(--accent-red);
    font-size: 10px;
    cursor: pointer;
    font-weight: 500;
  }

  .remote-warn:hover,
  .backend-warn:hover {
    text-decoration: underline;
  }

  .version {
    font-family: var(--font-mono);
    font-size: 10px;
    color: var(--text-muted);
    cursor: pointer;
  }

  .version:hover {
    color: var(--text-secondary);
  }

  .zoom-controls {
    display: flex;
    align-items: center;
    gap: 1px;
  }

  .zoom-btn {
    width: 18px;
    height: 16px;
    display: flex;
    align-items: center;
    justify-content: center;
    font-size: 11px;
    font-weight: 500;
    color: var(--text-muted);
    border-radius: var(--radius-sm);
    line-height: 1;
  }

  .zoom-btn:hover:not(:disabled) {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }

  .zoom-level {
    font-family: var(--font-mono);
    font-size: 10px;
    color: var(--text-muted);
    padding: 0 2px;
    min-width: 32px;
    text-align: center;
    border-radius: var(--radius-sm);
  }

  .zoom-level:hover {
    background: var(--bg-surface-hover);
    color: var(--text-secondary);
  }

  @media (max-width: 760px) {
    .counts {
      display: none;
    }
  }
</style>
