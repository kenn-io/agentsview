<script lang="ts">
  import { Button, Modal, TextInput } from "@kenn-io/kit-ui";
  import { onDestroy } from "svelte";
  import { m } from "../../i18n/index.js";
  import { SessionsService } from "../../api/generated/index";
  import { callGenerated, isAbortError } from "../../api/runtime.js";
  import { sessions } from "../../stores/sessions.svelte.js";
  import { router } from "../../stores/router.svelte.js";
  import { ui } from "../../stores/ui.svelte.js";
  import {
    ignoreShortcut,
    isComposingKey,
    protectFindInput,
    type FindCompositionState,
  } from "../../search/find-input.js";
  import { LatestRead } from "../../utils/latest-read.js";
  import {
    resolveSessionId,
    sessionLookupPartial,
    type SessionIdResolution,
  } from "../../utils/go-to-session.js";

  const SESSION_ID_LIMIT = 1000;
  type ErrorKind = Exclude<SessionIdResolution["kind"], "resolved"> | "failed";

  let inputValue = $state("");
  let inputEl = $state<HTMLInputElement>();
  let errorKind = $state<ErrorKind | null>(null);
  let closed = false;
  let composition = $state<FindCompositionState>({ composing: false });
  const lookup = new LatestRead();

  function isOpen(): boolean {
    return !closed && ui.activeModal === "goToSession";
  }

  function closeModal(): void {
    if (closed) return;
    closed = true;
    lookup.cancel();
    ui.activeModal = null;
  }

  function messageFor(kind: ErrorKind): string {
    switch (kind) {
      case "empty":
        return m.go_to_session_empty();
      case "unknown":
        return m.go_to_session_not_found();
      case "ambiguous":
        return m.go_to_session_ambiguous();
      case "capped":
        return m.go_to_session_capped();
      case "failed":
        return m.go_to_session_failed();
    }
  }

  function setError(kind: ErrorKind): void {
    if (isOpen()) errorKind = kind;
  }

  async function submit(): Promise<void> {
    if (!isOpen()) return;
    lookup.cancel();
    const value = inputValue.trim();
    if (!value) {
      setError("empty");
      return;
    }

    errorKind = null;
    const signal = lookup.begin();
    try {
      const response = await callGenerated(
        (options) =>
          SessionsService.getApiV1SessionIdsResolve(
            { partial: sessionLookupPartial(value), limit: SESSION_ID_LIMIT },
            options,
          ),
        signal,
      );
      if (!isOpen() || !lookup.isCurrent(signal)) return;

      const resolution = resolveSessionId(
        value,
        response.ids,
        response.ids.length >= SESSION_ID_LIMIT,
      );
      if (resolution.kind === "resolved") {
        void sessions.navigateToSession(resolution.id);
        router.navigateToSession(resolution.id);
        closeModal();
      } else {
        errorKind = resolution.kind;
      }
    } catch (error: unknown) {
      if (!isOpen() || !lookup.isCurrent(signal) || isAbortError(error)) return;
      errorKind = "failed";
    } finally {
      lookup.finish(signal);
    }
  }

  function handleKeydown(event: KeyboardEvent): void {
    if (composition.composing || isComposingKey(event) || ignoreShortcut(event)) {
      event.stopPropagation();
      return;
    }
    if (event.key === "Enter") {
      event.preventDefault();
      void submit();
    } else if (event.key === "Escape") {
      event.preventDefault();
      event.stopPropagation();
      closeModal();
    }
  }

  $effect(() => {
    if (isOpen() && inputEl) inputEl.focus();
  });

  onDestroy(() => {
    closed = true;
    lookup.cancel();
  });
</script>

{#snippet actions()}
  <Button
    label={m.go_to_session_submit()}
    tone="info"
    surface="solid"
    onclick={() => void submit()}
  />
{/snippet}

<Modal
  title={m.go_to_session_title()}
  closeLabel={m.go_to_session_close()}
  width="440px"
  onclose={closeModal}
  footer={actions}
>
  <div class="go-to-session-form">
    <div class="go-to-session-input" use:protectFindInput={composition}>
      <TextInput
        class="go-to-session-control"
        id="go-to-session-input"
        bind:value={inputValue}
        bind:inputEl
        ariaLabel={m.go_to_session_input_label()}
        placeholder={m.go_to_session_placeholder()}
        ariaDescribedby={errorKind ? "go-to-session-error" : undefined}
        invalid={errorKind !== null}
        autofocus
        block
        autocomplete="off"
        onkeydown={handleKeydown}
      />
    </div>
    {#if errorKind}
      <p id="go-to-session-error" class="go-to-session-error" role="alert">
        {messageFor(errorKind)}
      </p>
    {/if}
  </div>
</Modal>

<style>
  .go-to-session-form {
    display: flex;
    flex-direction: column;
    gap: var(--space-4);
  }

  :global(.go-to-session-control.kit-text-input:focus-within),
  :global(.go-to-session-control.kit-text-input:has(.kit-text-input__control:focus-visible)) {
    outline: none;
  }

  :global(.go-to-session-control .kit-text-input__control:focus-visible) {
    outline: none;
  }

  .go-to-session-error {
    margin: 0;
    color: var(--accent-red);
    font-size: 12px;
  }
</style>
