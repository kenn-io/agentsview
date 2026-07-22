<script lang="ts">
  import { EmptyState } from "@kenn-io/kit-ui";
  import { m } from "../../i18n/index.js";
  import { TrashIcon } from "../../icons.js";
  import { onDestroy, onMount } from "svelte";
  import type { Session } from "../../api/types.js";
  import { SessionsService } from "../../api/generated/index";
  import {
    callGenerated,
    configureGeneratedClient,
    isAbortError,
  } from "../../api/runtime.js";
  import { sessions } from "../../stores/sessions.svelte.js";
  import { formatRelativeTime, truncate } from "../../utils/format.js";
  import { normalizeMessagePreview } from "../../utils/messages.js";
  import { LatestRead } from "../../utils/latest-read.js";
  let trashedSessions: Session[] = $state([]);
  let loading = $state(true);
  let emptying = $state(false);
  const trashRead = new LatestRead();
  let confirmingDeleteId = $state<string | null>(null);
  let deletingId = $state<string | null>(null);

  interface TrashResponse {
    sessions: Session[];
  }

  onMount(() => {
    loadTrash();
  });

  async function loadTrash() {
    const signal = trashRead.begin();
    loading = true;
    try {
      configureGeneratedClient();
      const res = await callGenerated(
        () => SessionsService.getApiV1Trash(),
        signal,
      ) as unknown as TrashResponse;
      if (!trashRead.isCurrent(signal)) return;
      trashedSessions = res.sessions ?? [];
    } catch (e) {
      if (isAbortError(e) || !trashRead.isCurrent(signal)) return;
      // Silently ignore — page will show empty state.
    } finally {
      if (trashRead.finish(signal)) loading = false;
    }
  }

  onDestroy(() => trashRead.cancel());

  async function restoreSession(id: string) {
    try {
      configureGeneratedClient();
      await SessionsService.postApiV1SessionsIdRestore({ id });
      trashedSessions = trashedSessions.filter((s) => s.id !== id);
      if (confirmingDeleteId === id) confirmingDeleteId = null;
      sessions.clearRecentlyDeleted(id);
      sessions.invalidateFilterCaches();
      sessions.load();
    } catch {
      // silently fail
    }
  }

  function requestPermanentDelete(id: string) {
    confirmingDeleteId = id;
  }

  function cancelPermanentDelete(id: string) {
    if (confirmingDeleteId === id) confirmingDeleteId = null;
  }

  async function permanentDelete(id: string) {
    deletingId = id;
    try {
      configureGeneratedClient();
      await SessionsService.deleteApiV1SessionsIdPermanent({ id });
      trashedSessions = trashedSessions.filter((s) => s.id !== id);
      if (confirmingDeleteId === id) confirmingDeleteId = null;
      sessions.clearRecentlyDeleted(id);
      sessions.invalidateFilterCaches();
    } catch {
      // silently fail
    } finally {
      if (deletingId === id) deletingId = null;
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
    <h2>{m.trash_title()}</h2>
    {#if trashedSessions.length > 0}
      <span class="trash-count">{trashedSessions.length}</span>
      <button
        class="empty-all-btn"
        onclick={emptyAll}
        disabled={emptying}
        title={m.trash_empty_local_title()}
      >
        {emptying ? m.trash_emptying() : m.trash_empty_local()}
      </button>
    {/if}
  </div>

  {#if loading}
    <div class="loading-state">{m.trash_loading()}</div>
  {:else if trashedSessions.length === 0}
    <EmptyState title={m.trash_empty()} description={m.trash_empty_desc()}>
      {#snippet icon()}
        <TrashIcon size="40" strokeWidth="1.6" aria-hidden="true" />
      {/snippet}
    </EmptyState>
  {:else}
    <div class="trash-list">
      {#each trashedSessions as session (session.id)}
        <div class="trash-card">
          <div class="trash-card-info">
            <div class="trash-card-name">{displayName(session)}</div>
            <div class="trash-card-meta">
              <span class="trash-agent">{session.agent}</span>
              <span class="trash-project">{session.project}</span>
              <span class="trash-msgs">{m.trash_messages({
                count: session.user_message_count,
                countLabel: session.user_message_count.toLocaleString(),
              })}</span>
              {#if session.deleted_at}
                <span class="trash-deleted">{m.trash_deleted({ time: formatRelativeTime(session.deleted_at) })}</span>
              {/if}
            </div>
          </div>
          <div class="trash-card-actions">
            <button
              class="restore-btn"
              onclick={() => restoreSession(session.id)}
              title={m.trash_restore_session()}
            >
              {m.trash_restore()}
            </button>
            {#if confirmingDeleteId === session.id}
              <span class="delete-confirm-label">{m.trash_delete_everywhere_prompt()}</span>
              <button
                class="perm-delete-btn perm-delete-btn--confirm"
                onclick={() => permanentDelete(session.id)}
                title={m.trash_confirm_delete_everywhere()}
                disabled={deletingId === session.id}
              >
                {deletingId === session.id ? m.trash_deleting() : m.trash_confirm()}
              </button>
              <button
                class="cancel-delete-btn"
                onclick={() => cancelPermanentDelete(session.id)}
                disabled={deletingId === session.id}
              >
                {m.trash_cancel()}
              </button>
            {:else}
              <button
                class="perm-delete-btn"
                onclick={() => requestPermanentDelete(session.id)}
                title={m.trash_delete_everywhere_title()}
              >
                {m.trash_delete_everywhere()}
              </button>
            {/if}
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
    gap: var(--space-4);
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
    align-items: center;
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

  .perm-delete-btn--confirm {
    border-color: var(--accent-red, #e55);
  }

  .delete-confirm-label {
    color: var(--text-muted);
    font-size: 11px;
    font-weight: 500;
    white-space: nowrap;
  }

  .cancel-delete-btn {
    font-size: 11px;
    font-weight: 500;
    color: var(--text-muted);
    background: none;
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-sm);
    padding: 4px 10px;
    cursor: pointer;
    transition: background 0.12s, color 0.12s;
  }

  .cancel-delete-btn:hover:not(:disabled) {
    color: var(--text-secondary);
    background: var(--bg-surface-hover);
  }

  .perm-delete-btn:disabled,
  .cancel-delete-btn:disabled {
    cursor: default;
    opacity: 0.6;
  }
</style>
