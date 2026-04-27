<script lang="ts">
  import { tick } from "svelte";
  import { ui } from "../../stores/ui.svelte.js";
  import { sessions } from "../../stores/sessions.svelte.js";
  import { truncate } from "../../utils/format.js";

  let deleting = $state(false);
  let deleteBtn = $state<HTMLButtonElement>();

  let sessionName = $derived.by(() => {
    const s = sessions.activeSession;
    if (!s) return "this session";
    return truncate(
      s.display_name ?? s.first_message ?? s.project ?? "this session",
      60,
    );
  });

  function close() {
    ui.activeModal = null;
  }

  async function confirmDelete() {
    const id = sessions.activeSessionId;
    if (!id || deleting) return;
    deleting = true;
    try {
      await sessions.deleteSession(id);
      close();
    } catch {
      // silently fail — toast will show undo option
    } finally {
      deleting = false;
      await tick();
      deleteBtn?.focus();
    }
  }

  function handleOverlayClick(e: MouseEvent) {
    if (
      (e.target as HTMLElement).classList.contains(
        "confirm-overlay",
      )
    ) {
      close();
    }
  }
</script>

<svelte:window
  onkeydown={(e) => {
    if (e.key === "Escape") close();
  }}
/>

<!--
  Overlay is closed via Escape (svelte:window above) and via the
  Cancel/× buttons inside the modal, so a separate keydown handler
  here would be redundant.
-->
<!-- svelte-ignore a11y_no_static_element_interactions -->
<!-- svelte-ignore a11y_click_events_have_key_events -->
<div class="confirm-overlay" onclick={handleOverlayClick}>
  <div class="confirm-modal">
    <div class="confirm-header">
      <h3 class="confirm-title">Delete Session</h3>
      <button class="close-btn" onclick={close}>&times;</button>
    </div>

    <div class="confirm-body">
      <p class="confirm-message">
        Move <strong>{sessionName}</strong> to trash?
      </p>
      <p class="confirm-hint">
        You can restore it later from the Trash page.
      </p>
    </div>

    <div class="confirm-actions">
      <button class="cancel-btn" onclick={close}>Cancel</button>
      <!-- svelte-ignore a11y_autofocus -->
      <button
        class="delete-btn"
        bind:this={deleteBtn}
        onclick={confirmDelete}
        disabled={deleting}
        autofocus
      >
        {deleting ? "Deleting..." : "Move to Trash"}
      </button>
    </div>
  </div>
</div>

<style>
  .confirm-overlay {
    position: fixed;
    inset: 0;
    background: var(--overlay-bg);
    display: flex;
    align-items: center;
    justify-content: center;
    z-index: 100;
  }

  .confirm-modal {
    width: 380px;
    background: var(--bg-surface);
    border: 1px solid var(--border-default);
    border-radius: var(--radius-lg);
    box-shadow: var(--shadow-md);
    overflow: hidden;
  }

  .confirm-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    padding: 12px 16px;
    border-bottom: 1px solid var(--border-default);
  }

  .confirm-title {
    font-size: 13px;
    font-weight: 600;
    color: var(--text-primary);
  }

  .close-btn {
    width: 24px;
    height: 24px;
    display: flex;
    align-items: center;
    justify-content: center;
    font-size: 16px;
    color: var(--text-muted);
    border-radius: var(--radius-sm);
  }

  .close-btn:hover {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }

  .confirm-body {
    padding: 16px;
  }

  .confirm-message {
    font-size: 13px;
    color: var(--text-primary);
    margin: 0 0 6px;
  }

  .confirm-hint {
    font-size: 12px;
    color: var(--text-muted);
    margin: 0;
  }

  .confirm-actions {
    display: flex;
    justify-content: flex-end;
    gap: 8px;
    padding: 12px 16px;
    border-top: 1px solid var(--border-default);
  }

  .cancel-btn {
    height: 30px;
    padding: 0 14px;
    border-radius: var(--radius-sm);
    font-size: 12px;
    font-weight: 500;
    color: var(--text-secondary);
    background: var(--bg-inset);
    border: 1px solid var(--border-default);
    cursor: pointer;
  }

  .cancel-btn:hover {
    background: var(--bg-surface-hover);
  }

  .delete-btn {
    height: 30px;
    padding: 0 14px;
    border-radius: var(--radius-sm);
    font-size: 12px;
    font-weight: 500;
    color: white;
    background: var(--accent-red, #d32f2f);
    border: none;
    cursor: pointer;
  }

  .delete-btn:hover:not(:disabled) {
    opacity: 0.9;
  }

  .delete-btn:disabled {
    opacity: 0.6;
    cursor: default;
  }
</style>
