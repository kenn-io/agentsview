<script lang="ts">
  import { ui } from "../../stores/ui.svelte.js";
  import { sessions } from "../../stores/sessions.svelte.js";
  import {
    ConfigService,
    SessionsService,
  } from "../../api/generated/index";
  import { configureGeneratedClient } from "../../api/runtime.js";
  import type { PublishResponse } from "../../api/types.js";

  type View = "setup" | "progress" | "success" | "error";

  let view: View = $state("progress");
  let tokenInput: string = $state("");
  let errorMessage: string = $state("");
  let result: PublishResponse | null = $state(null);

  async function init() {
    try {
      configureGeneratedClient();
      const config = await ConfigService.getApiV1ConfigGithub();
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
      await doPublish();
    } catch (err) {
      errorMessage =
        err instanceof Error ? err.message : "Failed to save token";
      view = "error";
    }
  }

  async function doPublish() {
    const id = sessions.activeSessionId;
    if (!id) {
      errorMessage = "No session selected";
      view = "error";
      return;
    }

    view = "progress";
    try {
      configureGeneratedClient();
      result =
        await SessionsService.postApiV1SessionsIdPublish({
          id,
          secret: ui.publishSecret,
        }) as unknown as PublishResponse;
      view = "success";
    } catch (err) {
      errorMessage =
        err instanceof Error ? err.message : "Publish failed";
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
      ui.activeModal = null;
    }
  }

  init();
</script>

<!-- svelte-ignore a11y_no_static_element_interactions -->
<div
  class="modal-overlay"
  onclick={handleOverlayClick}
  onkeydown={(e) => {
    if (e.key === "Escape") ui.activeModal = null;
  }}
>
  <div class="modal-panel publish-panel">
    <div class="modal-header">
      <h3 class="modal-title">
        Publish to {ui.publishSecret ? "secret" : "public"} GitHub Gist
      </h3>
      <button
        class="modal-close"
        onclick={() => ui.activeModal = null}
        title="Close publish dialog"
        aria-label="Close publish dialog"
      >
        &times;
      </button>
    </div>

    <div class="modal-body">
      {#if view === "setup"}
        <p class="setup-text">
          Enter a GitHub personal access token with the
          <code>gist</code> scope.
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
            Create token on GitHub
          </a>
          <button
            class="modal-btn modal-btn-primary"
            onclick={handleSaveToken}
            disabled={!tokenInput.trim()}
          >
            Save & Publish
          </button>
        </div>

      {:else if view === "progress"}
        <div class="progress-view">
          <div class="modal-spinner"></div>
          <p>
            Creating {ui.publishSecret ? "secret" : "public"} GitHub Gist...
          </p>
        </div>

      {:else if view === "success" && result}
        <div class="success-view">
          <div class="url-field">
            <label class="url-label" for="publish-view-url">
              View URL
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
                Copy
              </button>
            </div>
          </div>
          <div class="url-field">
            <label class="url-label" for="publish-gist-url">
              Gist URL
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
                Copy
              </button>
            </div>
          </div>
          <div class="success-actions">
            <button
              class="modal-btn modal-btn-primary"
              onclick={() => window.open(result!.view_url, "_blank")}
            >
              Open in Browser
            </button>
            <button
              class="modal-btn"
              onclick={() => ui.activeModal = null}
            >
              Close
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
              Retry
            </button>
            <button
              class="modal-btn"
              onclick={() => ui.activeModal = null}
            >
              Close
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

  .setup-text code {
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
