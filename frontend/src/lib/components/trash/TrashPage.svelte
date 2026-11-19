<script lang="ts">
  import { EmptyState, showFlash } from "@kenn-io/kit-ui";
  import LibraryPageHeader from "../shared/LibraryPageHeader.svelte";
  import LoadError from "../shared/LoadError.svelte";
  import LoadingState from "../shared/LoadingState.svelte";
  import { failureMessage } from "../../utils/failure.js";
  import { m } from "../../i18n/index.js";
  import { TrashIcon } from "../../icons.js";
  import { onDestroy, onMount } from "svelte";
  import type { Session } from "../../api/types.js";
  import { SessionsService } from "../../api/generated/index";
  import {
    isAbortError,
  } from "../../api/runtime.js";
  import { sessions } from "../../stores/sessions.svelte.js";
  import { formatRelativeTime, truncate, formatCount } from "../../utils/format.js";
  import { normalizeMessagePreview } from "../../utils/messages.js";
  import { LatestRead } from "../../utils/latest-read.js";
  let trashedSessions: Session[] = $state([]);
  let loading = $state(true);
  let loadError: string | null = $state(null);
  let emptying = $state(false);
  const trashRead = new LatestRead();

  onMount(() => {
    loadTrash();
  });

  async function loadTrash() {
    const signal = trashRead.begin();
    loading = true;
    loadError = null;
    try {
      const res = await SessionsService.getApiV1Trash({ signal });
      if (!trashRead.isCurrent(signal)) return;
      trashedSessions = res.sessions ?? [];
    } catch (e) {
      if (isAbortError(e) || !trashRead.isCurrent(signal)) return;
      loadError = failureMessage(e);
    } finally {
      if (trashRead.finish(signal)) loading = false;
    }
  }

  onDestroy(() => trashRead.cancel());

  async function restoreSession(id: string) {
    try {
      await SessionsService.postApiV1SessionsByIdRestore({ id });
      trashedSessions = trashedSessions.filter((s) => s.id !== id);
      sessions.clearRecentlyDeleted(id);
      sessions.invalidateFilterCaches();
      sessions.load();
    } catch (e) {
      showFlash(failureMessage(e), { tone: "danger" });
    }
  }

  async function permanentDelete(id: string) {
    try {
      await SessionsService.deleteApiV1SessionsByIdPermanent({ id });
      trashedSessions = trashedSessions.filter((s) => s.id !== id);
      sessions.clearRecentlyDeleted(id);
      sessions.invalidateFilterCaches();
    } catch (e) {
      showFlash(failureMessage(e), { tone: "danger" });
    }
  }

  async function emptyAll() {
    emptying = true;
    try {
      await SessionsService.deleteApiV1Trash();
      trashedSessions = [];
      sessions.clearRecentlyDeleted();
      sessions.invalidateFilterCaches();
    } catch (e) {
      showFlash(failureMessage(e), { tone: "danger" });
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
  <!-- The title stays visible in every state, like the other library
       pages; the count and Empty trash appear only with deleted sessions. -->
  <LibraryPageHeader title={m.trash_title()}>
    {#snippet trailing()}
      {#if !loading && trashedSessions.length > 0}
        <span class="trash-count">{trashedSessions.length}</span>
      {/if}
    {/snippet}
    {#snippet actions()}
      {#if !loading && trashedSessions.length > 0}
        <button
          class="empty-all-btn"
          onclick={emptyAll}
          disabled={emptying}
        >
          {emptying ? m.trash_emptying() : m.trash_empty_trash()}
        </button>
      {/if}
    {/snippet}
  </LibraryPageHeader>
  {#if loadError && trashedSessions.length === 0}
    <LoadError message={loadError} onRetry={loadTrash} />
  {:else if loading && trashedSessions.length === 0}
    <LoadingState label={m.trash_loading()} />
  {:else if trashedSessions.length === 0}
    <EmptyState align="top" title={m.trash_empty()} description={m.trash_empty_desc()}>
      {#snippet icon()}
        <TrashIcon size="24" strokeWidth="1.75" />
      {/snippet}
    </EmptyState>
  {:else}
    {#if loadError}
      <LoadError message={loadError} onRetry={loadTrash} />
    {/if}
    <div class="trash-list">
      {#each trashedSessions as session (session.id)}
        <div class="trash-card">
          <div class="trash-card-info">
            <div class="trash-card-name">{displayName(session)}</div>
            <div class="trash-card-meta">
              <span class="trash-agent">{session.agent}</span>
              <span class="trash-project">{session.project}</span>
              <span class="trash-msgs">{m.trash_msgs({
                count: session.user_message_count,
                countLabel: formatCount(session.user_message_count),
              })}</span>
              {#if session.deleted_at}
                <span class="trash-deleted">{m.trash_deleted_ago({ time: formatRelativeTime(session.deleted_at) })}</span>
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
            <button
              class="perm-delete-btn"
              onclick={() => permanentDelete(session.id)}
              title={m.trash_permanently_delete()}
            >
              {m.trash_delete_forever()}
            </button>
          </div>
        </div>
      {/each}
    </div>
  {/if}
</div>

<style>
  /* Shared library page frame: a 1040px content column. */
  .trash-page {
    max-width: 1088px;
    margin: 0 auto;
    padding: 28px 24px;
  }

  .trash-count {
    color: var(--text-muted);
    font-size: 13px;
    font-variant-numeric: tabular-nums;
  }

  .empty-all-btn {
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
