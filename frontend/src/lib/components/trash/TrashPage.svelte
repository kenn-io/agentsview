<script lang="ts">
  import { TrashIcon } from "../../icons.js";
  import { onMount } from "svelte";
  import type { Session } from "../../api/types.js";
  import { SessionsService } from "../../api/generated/index";
  import { configureGeneratedClient } from "../../api/runtime.js";
  import { sessions } from "../../stores/sessions.svelte.js";
  import { formatRelativeTime, truncate } from "../../utils/format.js";
  import { normalizeMessagePreview } from "../../utils/messages.js";
  let trashedSessions: Session[] = $state([]);
  let loading = $state(true);
  let emptying = $state(false);

  interface TrashResponse {
    sessions: Session[];
  }

  onMount(() => {
    loadTrash();
  });

  async function loadTrash() {
    loading = true;
    try {
      configureGeneratedClient();
      const res =
        await SessionsService.getApiV1Trash() as unknown as TrashResponse;
      trashedSessions = res.sessions ?? [];
    } catch {
      // Silently ignore — page will show empty state.
    } finally {
      loading = false;
    }
  }

  async function restoreSession(id: string) {
    try {
      configureGeneratedClient();
      await SessionsService.postApiV1SessionsIdRestore({ id });
      trashedSessions = trashedSessions.filter((s) => s.id !== id);
      sessions.clearRecentlyDeleted(id);
      sessions.invalidateFilterCaches();
      sessions.load();
    } catch {
      // silently fail
    }
  }

  async function permanentDelete(id: string) {
    try {
      configureGeneratedClient();
      await SessionsService.deleteApiV1SessionsIdPermanent({ id });
      trashedSessions = trashedSessions.filter((s) => s.id !== id);
      sessions.clearRecentlyDeleted(id);
      sessions.invalidateFilterCaches();
    } catch {
      // silently fail
    }
  }

  async function emptyAll() {
    emptying = true;
    try {
      configureGeneratedClient();
      await SessionsService.deleteApiV1Trash();
      trashedSessions = [];
      sessions.clearRecentlyDeleted();
      sessions.invalidateFilterCaches();
    } catch {
      // Silently ignore — button resets to allow retry.
    } finally {
      emptying = false;
    }
  }

  function displayName(s: Session): string {
    const raw = s.display_name ?? normalizeMessagePreview(s.first_message);
    return raw ? truncate(raw, 70) : s.project;
  }
</script>

<div class="trash-page">
  <div class="trash-header">
    <TrashIcon size="18" strokeWidth="2" class="trash-icon" aria-hidden="true" />
    <h2>Trash</h2>
    {#if trashedSessions.length > 0}
      <span class="trash-count">{trashedSessions.length}</span>
      <button
        class="empty-all-btn"
        onclick={emptyAll}
        disabled={emptying}
      >
        {emptying ? "Emptying..." : "Empty Trash"}
      </button>
    {/if}
  </div>

  <p class="trash-desc">
    Deleted sessions are kept until you permanently delete them or empty the trash.
  </p>

  {#if loading}
    <div class="loading-state">Loading trash...</div>
  {:else if trashedSessions.length === 0}
    <div class="empty-state">
      <TrashIcon size="40" strokeWidth="1.6" class="empty-icon" aria-hidden="true" />
      <p class="empty-title">Trash is empty</p>
      <p class="empty-desc-text">Deleted sessions will appear here.</p>
    </div>
  {:else}
    <div class="trash-list">
      {#each trashedSessions as session (session.id)}
        <div class="trash-card">
          <div class="trash-card-info">
            <div class="trash-card-name">{displayName(session)}</div>
            <div class="trash-card-meta">
              <span class="trash-agent">{session.agent}</span>
              <span class="trash-project">{session.project}</span>
              <span class="trash-msgs">{session.user_message_count} msgs</span>
              {#if session.deleted_at}
                <span class="trash-deleted">deleted {formatRelativeTime(session.deleted_at)}</span>
              {/if}
            </div>
          </div>
          <div class="trash-card-actions">
            <button
              class="restore-btn"
              onclick={() => restoreSession(session.id)}
              title="Restore session"
            >
              Restore
            </button>
            <button
              class="perm-delete-btn"
              onclick={() => permanentDelete(session.id)}
              title="Permanently delete"
            >
              Delete Forever
            </button>
          </div>
        </div>
      {/each}
    </div>
  {/if}
</div>

<style>
  .trash-page {
    max-width: 800px;
    margin: 0 auto;
    padding: 40px 24px;
  }

  .trash-header {
    display: flex;
    align-items: center;
    gap: 10px;
    margin-bottom: 8px;
  }

  :global(.trash-icon) {
    color: var(--text-muted);
  }

  .trash-header h2 {
    font-size: 20px;
    font-weight: 600;
    color: var(--text-primary);
    margin: 0;
  }

  .trash-count {
    background: var(--text-muted);
    color: white;
    font-size: 11px;
    font-weight: 600;
    padding: 1px 7px;
    border-radius: 10px;
  }

  .trash-desc {
    font-size: 12px;
    color: var(--text-muted);
    margin-bottom: 24px;
  }

  .empty-all-btn {
    margin-left: auto;
    font-size: 11px;
    font-weight: 500;
    color: var(--accent-red, #e55);
    background: none;
    border: 1px solid var(--accent-red, #e55);
    border-radius: var(--radius-sm);
    padding: 4px 12px;
    cursor: pointer;
    transition: background 0.12s;
  }

  .empty-all-btn:hover:not(:disabled) {
    background: color-mix(in srgb, var(--accent-red, #e55) 8%, transparent);
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

  .empty-desc-text {
    font-size: 13px;
    margin: 0;
  }

  .trash-list {
    display: flex;
    flex-direction: column;
    gap: 8px;
  }

  .trash-card {
    display: flex;
    align-items: center;
    background: var(--bg-surface);
    border: 1px solid var(--border-muted);
    border-radius: 8px;
    padding: 12px 14px;
    gap: 12px;
    transition: border-color 0.15s;
  }

  .trash-card:hover {
    border-color: var(--border-default);
  }

  .trash-card-info {
    flex: 1;
    min-width: 0;
  }

  .trash-card-name {
    font-size: 13px;
    font-weight: 500;
    color: var(--text-primary);
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
    margin-bottom: 3px;
  }

  .trash-card-meta {
    display: flex;
    align-items: center;
    gap: 8px;
    font-size: 10px;
    color: var(--text-muted);
  }

  .trash-agent {
    font-weight: 600;
    text-transform: capitalize;
  }

  .trash-project {
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
    max-width: 150px;
  }

  .trash-msgs {
    white-space: nowrap;
  }

  .trash-deleted {
    white-space: nowrap;
    color: var(--accent-red, #e55);
    font-style: italic;
  }

  .trash-card-actions {
    display: flex;
    gap: 6px;
    flex-shrink: 0;
  }

  .restore-btn {
    font-size: 11px;
    font-weight: 500;
    color: var(--accent-green);
    background: none;
    border: 1px solid var(--accent-green);
    border-radius: var(--radius-sm);
    padding: 4px 10px;
    cursor: pointer;
    transition: background 0.12s;
  }

  .restore-btn:hover {
    background: color-mix(in srgb, var(--accent-green) 8%, transparent);
  }

  .perm-delete-btn {
    font-size: 11px;
    font-weight: 500;
    color: var(--accent-red, #e55);
    background: none;
    border: 1px solid transparent;
    border-radius: var(--radius-sm);
    padding: 4px 10px;
    cursor: pointer;
    transition: background 0.12s, color 0.12s;
  }

  .perm-delete-btn:hover {
    background: color-mix(in srgb, var(--accent-red, #e55) 8%, transparent);
  }
</style>
