<script lang="ts">
  import { onMount } from "svelte";
  import { sync } from "../../stores/sync.svelte.js";
  import { ui } from "../../stores/ui.svelte.js";
  import {
    formatNumber,
    formatRelativeTime,
    formatTimestamp,
  } from "../../utils/format.js";

  const RELATIVE_TIME_REFRESH_MS = 10_000;
  const isMac = navigator.platform.toUpperCase().includes("MAC");
  const mod = isMac ? "Cmd" : "Ctrl";
  let relativeTimeTick = $state(0);

  let progressText = $derived.by(() => {
    if (!sync.syncing || !sync.progress) return null;
    const p = sync.progress;
    if (p.phase === "scan") {
      return `Scanning ${p.current_project || ""}...`;
    }
    if (p.phase === "parse") {
      const pct = p.sessions_total > 0
        ? Math.round((p.sessions_done / p.sessions_total) * 100)
        : 0;
      return `Syncing ${pct}% (${p.sessions_done}/${p.sessions_total})`;
    }
    return "Syncing...";
  });

  let lastSyncText = $derived.by(() => {
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

<footer class="status-bar">
  <div class="status-left">
    {#if sync.stats}
      <span>{formatNumber(sync.stats.session_count)} sessions</span>
      <span class="sep">&middot;</span>
      <span>{formatNumber(sync.stats.message_count)} messages</span>
      <span class="sep">&middot;</span>
      <span>{formatNumber(sync.stats.project_count)} projects</span>
    {/if}
  </div>

  <div class="status-right">
    {#if sync.isDesktop}
      <div class="zoom-controls">
        <button
          class="zoom-btn"
          onclick={() => ui.zoomOut()}
          disabled={ui.zoomLevel <= 67}
          title="Zoom out ({mod}+-)"
        >
          &minus;
        </button>
        <button
          class="zoom-level"
          onclick={() => ui.resetZoom()}
          title="Reset zoom ({mod}+0)"
        >
          {ui.zoomLevel}%
        </button>
        <button
          class="zoom-btn"
          onclick={() => ui.zoomIn()}
          disabled={ui.zoomLevel >= 200}
          title="Zoom in ({mod}++)"
        >
          +
        </button>
      </div>
      <span class="sep">&middot;</span>
    {/if}
    {#if sync.updateAvailable && !sync.isDesktop}
      <button
        class="update-available"
        onclick={() => (ui.activeModal = "update")}
        title="A new version is available: {sync.latestVersion}"
      >
        update available
      </button>
      <span class="sep">&middot;</span>
    {/if}
    {#if sync.versionMismatch}
      <button
        class="version-warn"
        onclick={() => window.location.reload()}
        title="Frontend and backend versions differ. Click to reload."
      >
        version mismatch - reload
      </button>
    {/if}
    {#if progressText}
      {#if sync.versionMismatch}<span class="sep">&middot;</span>{/if}
      <span class="sync-progress">{progressText}</span>
    {:else if lastSyncText}
      {#if sync.versionMismatch}<span class="sep">&middot;</span>{/if}
      <span title={lastSyncTimestamp ?? undefined}>
        synced {lastSyncText}
      </span>
    {/if}
    {#if sync.serverVersion}
      {#if sync.versionMismatch || progressText || sync.lastSync}
        <span class="sep">&middot;</span>
      {/if}
      <button
        class="version"
        title="Build: {sync.serverVersion.commit}"
        onclick={() => {
          if (ui.activeModal === "resync" && sync.syncing) return;
          ui.activeModal = "about";
        }}
      >
        {sync.serverVersion.version}
      </button>
    {/if}
  </div>
</footer>

<style>
  .status-bar {
    height: var(--status-bar-height, 24px);
    display: flex;
    align-items: center;
    justify-content: space-between;
    padding: 0 14px;
    background: var(--bg-surface);
    border-top: 1px solid var(--border-default);
    font-size: 10px;
    color: var(--text-muted);
    flex-shrink: 0;
    letter-spacing: 0.01em;
  }

  .status-left,
  .status-right {
    display: flex;
    align-items: center;
    gap: 4px;
  }

  .sep {
    color: var(--border-default);
  }

  .sync-progress {
    color: var(--accent-green);
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

  @media (max-width: 767px) {
    .status-left {
      display: none;
    }
  }
</style>
