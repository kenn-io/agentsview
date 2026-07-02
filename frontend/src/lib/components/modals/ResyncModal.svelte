<script lang="ts">
  import { Button, Modal, Spinner } from "@kenn-io/kit-ui";
  import { m } from "../../i18n/index.js";
  import { ui } from "../../stores/ui.svelte.js";
  import { sync } from "../../stores/sync.svelte.js";

  type View = "confirm" | "progress" | "done" | "error";

  let view: View = $state("confirm");
  let errorMessage: string = $state("");

  function startResync() {
    if (sync.readOnly) {
      errorMessage = m.resync_error_read_only();
      view = "error";
      return;
    }
    const started = sync.triggerResync(
      () => {
        view = "done";
      },
      (err) => {
        errorMessage = err.message;
        view = "error";
      },
    );
    if (started) {
      view = "progress";
    } else if (!errorMessage) {
      errorMessage = m.resync_error_in_progress();
      view = "error";
    }
  }

  function close() {
    ui.activeModal = null;
  }

  function handleClose() {
    if (view !== "progress") close();
  }

  const progressPct = $derived(
    sync.progress
      ? sync.progress.sessions_total > 0
        ? (sync.progress.sessions_done /
            sync.progress.sessions_total) *
          100
        : 0
      : 0,
  );
</script>

{#snippet actions()}
  {#if view === "confirm"}
    <Button
      label={m.resync_cancel()}
      tone="neutral"
      surface="outline"
      onclick={close}
    />
    <Button
      label={m.resync_start()}
      tone="info"
      surface="solid"
      onclick={startResync}
    />
  {:else if view === "done"}
    <Button
      label={m.resync_close_btn()}
      tone="info"
      surface="solid"
      onclick={close}
    />
  {:else if view === "error"}
    <Button
      label={m.resync_retry()}
      tone="info"
      surface="solid"
      onclick={startResync}
    />
    <Button
      label={m.resync_close_btn()}
      tone="neutral"
      surface="outline"
      onclick={close}
    />
  {/if}
{/snippet}

<Modal
  title={m.resync_title()}
  width="400px"
  closable={view !== "progress"}
  closeOnOverlayClick={view !== "progress"}
  onclose={handleClose}
  footer={view === "progress" ? undefined : actions}
>
  {#if view === "confirm"}
    <p class="confirm-text">
      {m.resync_confirm_text()}
    </p>

  {:else if view === "progress"}
    <div class="progress-view">
      <Spinner />
      <p class="progress-label">
        {#if sync.progress}
          {m.resync_syncing_progress({ done: sync.progress.sessions_done, total: sync.progress.sessions_total })}
        {:else}
          {m.resync_preparing()}
        {/if}
      </p>
      <div class="progress-bar-track">
        <div
          class="progress-bar-fill"
          style="width: {progressPct}%"
        ></div>
      </div>
    </div>

  {:else if view === "done"}
    <div class="done-view">
      {#if sync.lastSyncStats}
        <p class="done-summary">
          {m.resync_sessions_synced({ count: sync.lastSyncStats.synced })}
        </p>
        {#if sync.lastSyncStats.failed > 0}
          <p class="done-warning">
            {m.resync_failed({ count: sync.lastSyncStats.failed })}
          </p>
        {/if}
      {/if}
    </div>

  {:else if view === "error"}
    <p class="modal-error-text">{errorMessage}</p>
  {/if}
</Modal>

<style>
  .confirm-text {
    font-size: 12px;
    color: var(--text-secondary);
    line-height: 1.5;
  }

  .progress-view {
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: 12px;
    padding: 16px 0;
  }

  .progress-label {
    font-size: 12px;
    color: var(--text-secondary);
    font-variant-numeric: tabular-nums;
  }

  .progress-bar-track {
    width: 100%;
    height: 4px;
    background: var(--bg-inset);
    border-radius: 2px;
    overflow: hidden;
  }

  .progress-bar-fill {
    height: 100%;
    background: var(--accent-blue);
    border-radius: 2px;
    transition: width 0.3s;
  }

  .done-view {
    display: flex;
    flex-direction: column;
    gap: 8px;
  }

  .done-summary {
    font-size: 12px;
    color: var(--text-primary);
    font-variant-numeric: tabular-nums;
  }

  .done-warning {
    font-size: 12px;
    color: var(--accent-orange, #e09040);
    font-variant-numeric: tabular-nums;
  }

  .modal-error-text {
    font-size: var(--font-size-sm);
    color: var(--accent-red, #f85149);
    background: var(--bg-inset);
    padding: 8px 12px;
    border-radius: var(--radius-sm);
    border: 1px solid var(--accent-red, #f85149);
    word-break: break-word;
  }
</style>
