<script lang="ts">
  import { Button, Modal } from "@kenn-io/kit-ui";
  import { m } from "../../i18n/index.js";
  import { tick } from "svelte";
  import { ui } from "../../stores/ui.svelte.js";
  import { sessions } from "../../stores/sessions.svelte.js";
  import { truncate } from "../../utils/format.js";
  import { normalizeMessagePreview } from "../../utils/messages.js";
  let deleting = $state(false);
  let actionsEl = $state<HTMLElement>();

  let sessionName = $derived.by(() => {
    const s = sessions.activeSession;
    if (!s) return m.confirm_delete_this_session();
    // normalizeMessagePreview can return "" for empty/null input, so use ||
    // (not ??) to fall through to the project/default fallback.
    const raw =
      s.display_name
      ?? (normalizeMessagePreview(s.first_message) || s.project || m.confirm_delete_this_session());
    return truncate(raw, 60);
  });

  function close() {
    ui.activeModal = null;
  }

  function focusDeleteButton() {
    actionsEl
      ?.querySelector<HTMLButtonElement>(".confirm-delete-action")
      ?.focus();
  }

  // Focus the primary (delete) action once the modal has mounted, after
  // Modal's built-in focus trap has taken its initial focus.
  $effect(() => {
    void tick().then(focusDeleteButton);
  });

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
      focusDeleteButton();
    }
  }
</script>

{#snippet actions()}
  <span class="confirm-actions" bind:this={actionsEl}>
    <Button
      label={m.confirm_delete_cancel()}
      tone="neutral"
      surface="outline"
      onclick={close}
    />
    <Button
      class="confirm-delete-action"
      label={deleting ? m.confirm_delete_deleting() : m.confirm_delete_move_to_trash()}
      tone="danger"
      surface="solid"
      disabled={deleting}
      onclick={confirmDelete}
    />
  </span>
{/snippet}

<Modal
  title={m.confirm_delete_title()}
  tone="danger"
  width="380px"
  onclose={close}
  footer={actions}
>
  <p class="confirm-message">
    {m.confirm_delete_message({ name: sessionName })}
  </p>
  <p class="confirm-hint">
    {m.confirm_delete_hint()}
  </p>
</Modal>

<style>
  .confirm-actions {
    display: contents;
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
</style>
