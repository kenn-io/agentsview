<script lang="ts">
  import { onDestroy } from "svelte";
  import { m } from "../../i18n/index.js";
  import { ui } from "../../stores/ui.svelte.js";
  import { sessions } from "../../stores/sessions.svelte.js";
  import {
    ConfigService,
    InsightsService,
    SessionsService,
  } from "../../api/generated/index";
  import { configureGeneratedClient } from "../../api/runtime.js";
  import type { PublishResponse } from "../../api/types.js";
  import { XIcon } from "../../icons.js";

  type View = "setup" | "progress" | "success" | "error";

  let view: View = $state("progress");
  let tokenInput: string = $state("");
  let errorMessage: string = $state("");
  let result: PublishResponse | null = $state(null);
  let closed = false;

  const target = ui.publishTarget ??
    (sessions.activeSessionId
      ? { kind: "session" as const, id: sessions.activeSessionId }
      : null);
  const publishSecret = ui.publishSecret;

  function isClosed() {
    return closed || ui.activeModal !== "publish";
  }

  function closeModal() {
    closed = true;
    ui.activeModal = null;
  }

  async function init() {
    try {
      configureGeneratedClient();
      const config = await ConfigService.getApiV1ConfigGithub();
      if (isClosed()) return;
      if (config.configured) {
        await doPublish();
      } else {
        view = "setup";
      }
    } catch {
      view = "setup";
    }
  }

  async function handleSaveToken() {
    const token = tokenInput.trim();
    if (!token) return;

    view = "progress";
    try {
      configureGeneratedClient();
      await ConfigService.postApiV1ConfigGithub({
        requestBody: { token },
      });
      if (isClosed()) return;
      await doPublish();
    } catch (err) {
      if (isClosed()) return;
      errorMessage =
        err instanceof Error ? err.message : m.publish_save_token_failed();
      view = "error";
    }
  }

  async function doPublish() {
    if (isClosed()) return;
    if (!target) {
      errorMessage = m.publish_no_session_selected();
      view = "error";
      return;
    }

    if (target.kind === "insight") {
      view = "progress";
      try {
        configureGeneratedClient();
        result =
          await InsightsService.postApiV1InsightsIdPublish({
            id: target.id,
            secret: publishSecret,
          }) as unknown as PublishResponse;
        if (isClosed()) return;
        view = "success";
      } catch (err) {
        if (isClosed()) return;
        errorMessage =
          err instanceof Error ? err.message : m.publish_failed();
        view = "error";
      }
      return;
    }

    view = "progress";
    try {
      configureGeneratedClient();
      result =
        await SessionsService.postApiV1SessionsIdPublish({
          id: target.id,
          secret: publishSecret,
        }) as unknown as PublishResponse;
      if (isClosed()) return;
      view = "success";
    } catch (err) {
      if (isClosed()) return;
      errorMessage =
        err instanceof Error ? err.message : m.publish_failed();
      view = "error";
    }
  }

  function copyToClipboard(text: string) {
    navigator.clipboard.writeText(text);
  }

  function handleOverlayClick(e: MouseEvent) {
    if (
      (e.target as HTMLElement).classList.contains(
        "modal-overlay",
      )
    ) {
      closeModal();
    }
  }

  onDestroy(() => {
    closed = true;
  });

  init();
</script>

<!-- svelte-ignore a11y_no_static_element_interactions -->
<div
  class="modal-overlay"
  onclick={handleOverlayClick}
  onkeydown={(e) => {
    if (e.key === "Escape") closeModal();
  }}
>
  <div class="modal-panel publish-panel">
    <div class="modal-header">
      <h3 class="modal-title">
        {publishSecret ? m.publish_title_secret() : m.publish_title_public()}
      </h3>
      <button
        class="modal-close"
        onclick={closeModal}
        title={m.publish_close()}
        aria-label={m.publish_close()}
      >
        <XIcon size="14" strokeWidth="2.2" aria-hidden="true" />
      </button>
    </div>

    <div class="modal-body">
      {#if view === "setup"}
        <p class="setup-text">
          {m.publish_setup_text({ scope: "gist" })}
        </p>
        <input
          class="token-input"
          type="password"
          placeholder="ghp_..."
          bind:value={tokenInput}
          onkeydown={(e) => {
            if (e.key === "Enter") handleSaveToken();
          }}
        />
        <div class="setup-actions">
          <a
            class="token-link"
            href="https://github.com/settings/tokens/new?scopes=gist"
            target="_blank"
            rel="noopener noreferrer"
          >
            {m.publish_create_token()}
          </a>
          <button
            class="modal-btn modal-btn-primary"
            onclick={handleSaveToken}
            disabled={!tokenInput.trim()}
          >
            {m.publish_save_and_publish()}
          </button>
        </div>

      {:else if view === "progress"}
        <div class="progress-view">
          <div class="modal-spinner"></div>
          <p>
            {publishSecret ? m.publish_creating_secret() : m.publish_creating_public()}
          </p>
        </div>

      {:else if view === "success" && result}
        <div class="success-view">
          <div class="url-field">
            <label class="url-label" for="publish-view-url">
              {m.publish_view_url()}
            </label>
            <div class="url-row">
              <input
                id="publish-view-url"
                class="url-input"
                type="text"
                readonly
                value={result.view_url}
              />
              <button
                class="modal-btn btn-copy"
                onclick={() => copyToClipboard(result!.view_url)}
              >
                {m.publish_copy()}
              </button>
            </div>
          </div>
          <div class="url-field">
            <label class="url-label" for="publish-gist-url">
              {m.publish_gist_url()}
            </label>
            <div class="url-row">
              <input
                id="publish-gist-url"
                class="url-input"
                type="text"
                readonly
                value={result.gist_url}
              />
              <button
                class="modal-btn btn-copy"
                onclick={() => copyToClipboard(result!.gist_url)}
              >
                {m.publish_copy()}
              </button>
            </div>
          </div>
          <div class="success-actions">
            <button
              class="modal-btn modal-btn-primary"
              onclick={() => window.open(result!.view_url, "_blank")}
            >
              {m.publish_open_in_browser()}
            </button>
            <button
              class="modal-btn"
              onclick={closeModal}
            >
              {m.publish_close_btn()}
            </button>
          </div>
        </div>

      {:else if view === "error"}
        <div class="error-view">
          <p class="modal-error">{errorMessage}</p>
          <div class="error-actions">
            <button
              class="modal-btn modal-btn-primary"
              onclick={doPublish}
            >
              {m.publish_retry()}
            </button>
            <button
              class="modal-btn"
              onclick={closeModal}
            >
              {m.publish_close_btn()}
            </button>
          </div>
        </div>
      {/if}
    </div>
  </div>
</div>

<style>
  .publish-panel {
    width: 440px;
  }

  .setup-text {
    font-size: 12px;
    color: var(--text-secondary);
    margin-bottom: 12px;
  }

  .setup-text :global(code) {
    font-family: var(--font-mono);
    background: var(--bg-inset);
    padding: 1px 4px;
    border-radius: var(--radius-sm);
  }

  .token-input {
    width: 100%;
    height: 32px;
    padding: 0 8px;
    background: var(--bg-inset);
    border: 1px solid var(--border-default);
    border-radius: var(--radius-sm);
    font-size: 12px;
    font-family: var(--font-mono);
    color: var(--text-primary);
    margin-bottom: 12px;
  }

  .token-input:focus {
    outline: none;
    border-color: var(--accent-blue);
  }

  .setup-actions {
    display: flex;
    align-items: center;
    justify-content: space-between;
  }

  .token-link {
    font-size: 11px;
    color: var(--accent-blue);
    text-decoration: none;
  }

  .token-link:hover {
    text-decoration: underline;
  }

  .progress-view {
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: 12px;
    padding: 24px 0;
    color: var(--text-secondary);
    font-size: 12px;
  }

  .success-view {
    display: flex;
    flex-direction: column;
    gap: 12px;
  }

  .url-field {
    display: flex;
    flex-direction: column;
    gap: 4px;
  }

  .url-label {
    font-size: 11px;
    font-weight: 600;
    color: var(--text-muted);
    text-transform: uppercase;
    letter-spacing: 0.5px;
  }

  .url-row {
    display: flex;
    gap: 4px;
  }

  .url-input {
    flex: 1;
    height: 28px;
    padding: 0 8px;
    background: var(--bg-inset);
    border: 1px solid var(--border-default);
    border-radius: var(--radius-sm);
    font-size: 11px;
    font-family: var(--font-mono);
    color: var(--text-secondary);
    min-width: 0;
  }

  .btn-copy {
    flex-shrink: 0;
  }

  .success-actions {
    display: flex;
    gap: 8px;
    justify-content: flex-end;
    margin-top: 4px;
  }

  .error-view {
    display: flex;
    flex-direction: column;
    gap: 12px;
  }

  .error-actions {
    display: flex;
    gap: 8px;
    justify-content: flex-end;
  }
</style>
