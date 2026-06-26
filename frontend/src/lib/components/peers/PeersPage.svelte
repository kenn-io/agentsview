<script lang="ts">
  import { onMount } from "svelte";
  import {
    UsersRoundIcon,
    MonitorIcon,
    GlobeIcon,
    RefreshCwIcon,
    TriangleAlertIcon,
  } from "../../icons.js";
  import { ArtifactsService } from "../../api/generated/index";
  import type { ArtifactPeer } from "../../api/generated/index";
  import { configureGeneratedClient } from "../../api/runtime.js";
  import { m } from "../../i18n/index.js";
  import {
    formatRelativeTime,
    formatNumber,
  } from "../../utils/format.js";

  let peers: ArtifactPeer[] = $state([]);
  let localOrigin = $state("");
  let conflictCount = $state(0);
  let loading = $state(true);
  let loaded = $state(false);
  let refreshing = $state(false);

  onMount(() => {
    loadPeers();
  });

  async function loadPeers(refresh = false) {
    if (refresh) refreshing = true;
    else loading = true;
    try {
      configureGeneratedClient();
      const res = await ArtifactsService.getApiV1ArtifactsPeers();
      peers = (res.peers ?? []) as ArtifactPeer[];
      localOrigin = res.local_origin ?? "";
      conflictCount = res.conflict_count ?? 0;
      loaded = true;
    } catch {
      // Artifact store unavailable (e.g. read-only/PG mode) — show empty state.
      peers = [];
      loaded = false;
    } finally {
      loading = false;
      refreshing = false;
    }
  }

  // A peer is "in sync" when every published session has landed locally.
  function syncState(p: ArtifactPeer): "synced" | "behind" | "empty" {
    if (p.published_sessions === 0) return "empty";
    if (p.local_sessions >= p.published_sessions) return "synced";
    return "behind";
  }
</script>

<div class="peers-page">
  <div class="peers-header">
    <UsersRoundIcon size="18" strokeWidth="2" class="peers-icon" aria-hidden="true" />
    <h2>{m.peers_title()}</h2>
    {#if peers.length > 0}
      <span class="peers-count">{peers.length}</span>
    {/if}
    <button
      class="refresh-btn"
      class:spinning={refreshing}
      onclick={() => loadPeers(true)}
      disabled={loading || refreshing}
      title={m.peers_refresh_status()}
      aria-label={m.peers_refresh_status()}
    >
      <RefreshCwIcon size="14" strokeWidth="2" aria-hidden="true" />
    </button>
  </div>

  {#if conflictCount > 0}
    <div class="conflict-banner">
      <TriangleAlertIcon size="14" strokeWidth="2" aria-hidden="true" />
      <span>
        {conflictCount === 1
          ? m.peers_conflict_count_singular()
          : m.peers_conflict_count_plural({ count: formatNumber(conflictCount) })}
      </span>
    </div>
  {/if}

  {#if loading}
    <div class="loading-state">{m.peers_loading()}</div>
  {:else if peers.length === 0}
    <div class="empty-state">
      <UsersRoundIcon size="40" strokeWidth="1.6" class="empty-icon" aria-hidden="true" />
      <p class="empty-title">{loaded ? m.peers_empty() : m.peers_unavailable()}</p>
    </div>
  {:else}
    <div class="peer-list">
      {#each peers as peer (peer.origin)}
        {@const state = syncState(peer)}
        <div class="peer-card" class:is-local={peer.is_local}>
          <div class="peer-card-icon">
            {#if peer.is_local}
              <MonitorIcon size="18" strokeWidth="2" aria-hidden="true" />
            {:else}
              <GlobeIcon size="18" strokeWidth="2" aria-hidden="true" />
            {/if}
          </div>
          <div class="peer-card-info">
            <div class="peer-card-name">
              <span class="peer-origin">{peer.origin}</span>
              {#if peer.is_local}
                <span class="peer-badge peer-badge--local">{m.peers_this_machine()}</span>
              {/if}
              {#if state === "synced"}
                <span class="peer-badge peer-badge--synced">{m.peers_in_sync()}</span>
              {:else if state === "behind"}
                <span class="peer-badge peer-badge--behind">
                  {m.peers_pending({
                    count: formatNumber(peer.published_sessions - peer.local_sessions),
                  })}
                </span>
              {/if}
            </div>
            <div class="peer-card-meta">
              <span title={m.peers_local_sessions_title()}>
                {m.peers_local_sessions({ count: formatNumber(peer.local_sessions) })}
              </span>
              <span class="meta-sep">/</span>
              <span title={m.peers_published_sessions_title()}>
                {m.peers_published_sessions({ count: formatNumber(peer.published_sessions) })}
              </span>
              {#if peer.checkpoint_seq > 0}
                <span class="meta-dot">·</span>
                <span title={m.peers_checkpoint_title()}>
                  {m.peers_checkpoint({ seq: String(peer.checkpoint_seq) })}
                </span>
              {/if}
              {#if peer.last_published}
                <span class="meta-dot">·</span>
                <span title={m.peers_last_published_title()}>
                  {m.peers_updated({ time: formatRelativeTime(peer.last_published) })}
                </span>
              {/if}
            </div>
          </div>
        </div>
      {/each}
    </div>
  {/if}
</div>

<style>
  .peers-page {
    max-width: 800px;
    margin: 0 auto;
    padding: 40px 24px;
  }

  .peers-header {
    display: flex;
    align-items: center;
    gap: 10px;
    margin-bottom: 8px;
  }

  :global(.peers-icon) {
    color: var(--text-muted);
  }

  .peers-header h2 {
    font-size: 20px;
    font-weight: 600;
    color: var(--text-primary);
    margin: 0;
  }

  .peers-count {
    background: var(--text-muted);
    color: white;
    font-size: 11px;
    font-weight: 600;
    padding: 1px 7px;
    border-radius: 10px;
  }

  .refresh-btn {
    margin-left: auto;
    width: 28px;
    height: 28px;
    display: flex;
    align-items: center;
    justify-content: center;
    border-radius: var(--radius-sm);
    color: var(--text-muted);
    transition: background 0.12s, color 0.12s;
  }

  .refresh-btn:hover:not(:disabled) {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }

  .refresh-btn:disabled {
    opacity: 0.55;
    cursor: default;
  }

  .refresh-btn.spinning {
    animation: spin 1s linear infinite;
  }

  @keyframes spin {
    from { transform: rotate(0deg); }
    to { transform: rotate(360deg); }
  }

  .conflict-banner {
    display: flex;
    align-items: flex-start;
    gap: 8px;
    font-size: 12px;
    color: var(--accent-amber);
    background: color-mix(in srgb, var(--accent-amber) 8%, transparent);
    border: 1px solid color-mix(in srgb, var(--accent-amber) 30%, transparent);
    border-radius: 8px;
    padding: 10px 12px;
    margin-bottom: 20px;
    line-height: 1.45;
  }

  .conflict-banner :global(svg) {
    flex-shrink: 0;
    margin-top: 1px;
  }

  .loading-state {
    text-align: center;
    color: var(--text-muted);
    padding: 40px 0;
    font-size: 13px;
  }

  .empty-state {
    text-align: center;
    padding: 60px 20px;
    color: var(--text-muted);
  }

  :global(.empty-icon) {
    opacity: 0.15;
    margin-bottom: 16px;
  }

  .empty-title {
    font-size: 16px;
    font-weight: 500;
    color: var(--text-secondary);
    margin: 0 0 6px;
  }

  .peer-list {
    display: flex;
    flex-direction: column;
    gap: 8px;
  }

  .peer-card {
    display: flex;
    align-items: center;
    background: var(--bg-surface);
    border: 1px solid var(--border-muted);
    border-radius: 8px;
    padding: 12px 14px;
    gap: 12px;
    transition: border-color 0.15s;
  }

  .peer-card:hover {
    border-color: var(--border-default);
  }

  .peer-card.is-local {
    border-color: color-mix(in srgb, var(--accent-blue) 40%, var(--border-muted));
  }

  .peer-card-icon {
    flex-shrink: 0;
    color: var(--text-muted);
    display: flex;
    align-items: center;
  }

  .peer-card.is-local .peer-card-icon {
    color: var(--accent-blue);
  }

  .peer-card-info {
    flex: 1;
    min-width: 0;
  }

  .peer-card-name {
    display: flex;
    align-items: center;
    gap: 8px;
    margin-bottom: 4px;
  }

  .peer-origin {
    font-size: 13px;
    font-weight: 600;
    color: var(--text-primary);
    font-family: var(--font-mono, monospace);
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .peer-badge {
    font-size: 10px;
    font-weight: 600;
    padding: 1px 7px;
    border-radius: 10px;
    white-space: nowrap;
    flex-shrink: 0;
  }

  .peer-badge--local {
    color: var(--accent-blue);
    background: color-mix(in srgb, var(--accent-blue) 12%, transparent);
  }

  .peer-badge--synced {
    color: var(--accent-green);
    background: color-mix(in srgb, var(--accent-green) 12%, transparent);
  }

  .peer-badge--behind {
    color: var(--accent-amber);
    background: color-mix(in srgb, var(--accent-amber) 12%, transparent);
  }

  .peer-card-meta {
    display: flex;
    align-items: center;
    gap: 6px;
    font-size: 11px;
    color: var(--text-muted);
    flex-wrap: wrap;
  }

  .meta-sep {
    color: var(--border-default);
  }

  .meta-dot {
    color: var(--border-default);
  }
</style>
