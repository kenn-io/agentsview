<script lang="ts">
  import { Button, Modal, Spinner } from "@kenn-io/kit-ui";
  import { m } from "../../i18n/index.js";
  import { untrack } from "svelte";
  import {
    importClaudeAI,
    importChatGPT,
    type ImportStats,
  } from "../../api/client.js";
  import {
    FileCheckIcon,
    FileIcon,
    FileXIcon,
    TriangleAlertIcon,
    UploadIcon,
  } from "../../icons.js";

  interface Props {
    open: boolean;
    onclose: () => void;
    onimported: () => void;
  }

  let {
    open = $bindable(),
    onclose,
    onimported,
  }: Props = $props();

  type ImportResult = {
    imported: number;
    updated: number;
    skipped: number;
    errors: number;
  };

  let fileInput: HTMLInputElement | undefined = $state();
  let selectedFile = $state<File | null>(null);
  let provider: "claude-ai" | "chatgpt" =
    $state("claude-ai");
  let importing = $state(false);
  let dragOver = $state(false);
  let dragCount = $state(0);
  let result = $state<ImportResult | null>(null);
  let error = $state<string | null>(null);
  let phase = $state<"importing" | "indexing">(
    "importing",
  );
  let progressStats = $state<ImportStats | null>(null);

  const fileSize = $derived(
    selectedFile
      ? selectedFile.size < 1024 * 1024
        ? `${(selectedFile.size / 1024).toFixed(1)} KB`
        : `${(selectedFile.size / (1024 * 1024)).toFixed(1)} MB`
      : "",
  );

  const accepted = $derived(
    provider === "claude-ai" ? ".json,.zip" : ".zip",
  );

  const totalProcessed = $derived(
    result
      ? result.imported +
          result.updated +
          result.skipped
      : 0,
  );

  // Reset file when provider changes. The fileInput access
  // must be untracked: when the result view replaces the
  // upload view, bind:this sets fileInput to undefined,
  // which would re-trigger this effect and wipe the results.
  $effect(() => {
    // eslint-disable-next-line @typescript-eslint/no-unused-expressions
    provider;
    selectedFile = null;
    result = null;
    error = null;
    untrack(() => {
      if (fileInput) fileInput.value = "";
    });
  });

  function selectProvider(p: typeof provider) {
    if (importing) return;
    provider = p;
  }

  function handleFileChange(e: Event) {
    const input = e.target as HTMLInputElement;
    selectedFile = input.files?.[0] ?? null;
    result = null;
    error = null;
  }

  function handleDragEnter(e: DragEvent) {
    e.preventDefault();
    dragCount++;
    if (!importing) dragOver = true;
  }

  function handleDragOver(e: DragEvent) {
    e.preventDefault();
    if (e.dataTransfer) e.dataTransfer.dropEffect = "copy";
  }

  function handleDragLeave() {
    dragCount--;
    if (dragCount <= 0) {
      dragCount = 0;
      dragOver = false;
    }
  }

  function handleDrop(e: DragEvent) {
    e.preventDefault();
    dragCount = 0;
    dragOver = false;
    if (importing) return;

    const file = e.dataTransfer?.files[0];
    if (!file) return;

    const ext =
      file.name.toLowerCase().split(".").pop() ?? "";
    const allowed =
      provider === "claude-ai"
        ? ["json", "zip"]
        : ["zip"];
    if (!allowed.includes(ext)) {
      error =
        `Expected ${allowed.map((s) => "." + s).join(" or ")} file`;
      return;
    }
    selectedFile = file;
    result = null;
    error = null;
  }

  function clearFile() {
    selectedFile = null;
    error = null;
    if (fileInput) fileInput.value = "";
  }

  async function handleImport() {
    if (importing || !selectedFile) return;
    importing = true;
    error = null;
    result = null;
    phase = "importing";
    progressStats = null;

    const cb = {
      onProgress: (stats: ImportStats) => {
        progressStats = stats;
      },
      onIndexing: () => {
        phase = "indexing";
      },
    };

    try {
      if (provider === "chatgpt") {
        result = await importChatGPT(selectedFile, cb);
      } else {
        result = await importClaudeAI(selectedFile, cb);
      }
      onimported();
    } catch (e) {
      error =
        e instanceof Error
          ? e.message
          : m.import_failed();
    } finally {
      importing = false;
    }
  }

  function handleClose() {
    if (importing) return;
    selectedFile = null;
    result = null;
    error = null;
    dragOver = false;
    dragCount = 0;
    open = false;
    onclose();
  }

  function handleReset() {
    selectedFile = null;
    result = null;
    error = null;
    if (fileInput) fileInput.value = "";
  }
</script>

{#snippet actions()}
  {#if result}
    <Button
      label={m.import_import_more()}
      tone="neutral"
      surface="outline"
      onclick={handleReset}
    />
    <Button
      label={m.import_done()}
      tone="info"
      surface="solid"
      onclick={handleClose}
    />
  {:else}
    <Button
      label={m.import_cancel()}
      tone="neutral"
      surface="outline"
      disabled={importing}
      onclick={handleClose}
    />
    <Button
      label={m.import_import()}
      tone="info"
      surface="solid"
      disabled={!selectedFile || importing}
      onclick={handleImport}
    />
  {/if}
{/snippet}

{#if open}
  <Modal
    title={m.import_title()}
    width="460px"
    maxWidth="min(460px, 92vw)"
    closable={!importing}
    closeOnOverlayClick={!importing}
    onclose={handleClose}
    footer={actions}
  >
    {#if result}
      <!-- ── Results ── -->
      <div class="result-view">
        <div class="result-check">
          <FileCheckIcon size="32" strokeWidth="1.5" aria-hidden="true" />
        </div>

        <p class="result-heading">
          {m.import_processed({ count: totalProcessed })}
        </p>

        <div class="result-stats">
          <div class="stat">
            <span class="stat-num new">
              {result.imported}
            </span>
            <span class="stat-lbl">{m.import_new()}</span>
          </div>
          <div class="stat">
            <span class="stat-num updated">
              {result.updated}
            </span>
            <span class="stat-lbl">{m.import_updated()}</span>
          </div>
          {#if result.skipped > 0}
            <div class="stat">
              <span class="stat-num skipped">
                {result.skipped}
              </span>
              <span class="stat-lbl">{m.import_unchanged()}</span>
            </div>
          {/if}
          {#if result.errors > 0}
            <div class="stat">
              <span class="stat-num errors">
                {result.errors}
              </span>
              <span class="stat-lbl">{m.import_errors()}</span>
            </div>
          {/if}
        </div>
      </div>
    {:else}
      <!-- ── Provider selector ── -->
      <div class="provider-strip">
        <button
          class="provider-pill"
          class:selected={provider === "claude-ai"}
          onclick={() => selectProvider("claude-ai")}
          disabled={importing}
        >
          <span class="pdot claude"></span>
          <span>Claude.ai</span>
        </button>
        <button
          class="provider-pill"
          class:selected={provider === "chatgpt"}
          onclick={() => selectProvider("chatgpt")}
          disabled={importing}
        >
          <span class="pdot chatgpt"></span>
          <span>ChatGPT</span>
        </button>
      </div>

      <p class="hint">
        {#if provider === "claude-ai"}
          {m.import_hint_claude({ json: "conversations.json", zip: ".zip" })}
        {:else}
          {m.import_hint_chatgpt({ zip: ".zip" })}
        {/if}
      </p>

      <!-- ── Drop zone ── -->
      {#if importing}
        <div class="zone zone-importing">
          <Spinner />
          {#if phase === "indexing"}
            <span class="importing-label">
              {m.import_rebuilding_index()}
            </span>
          {:else if progressStats}
            {@const n =
              progressStats.imported +
              progressStats.updated +
              progressStats.skipped +
              progressStats.errors}
            <span class="importing-label">
              {m.import_processing_progress({ count: n })}
            </span>
          {:else}
            <span class="importing-label">
              {m.import_importing()}
            </span>
          {/if}
        </div>
      {:else if selectedFile}
        <div class="zone zone-file">
          <div class="file-row">
            <FileIcon class="file-icon" size="20" strokeWidth="1.6" aria-hidden="true" />
            <div class="file-meta">
              <span class="file-name">
                {selectedFile.name}
              </span>
              <span class="file-size">
                {fileSize}
              </span>
            </div>
            <button
              class="file-clear"
              onclick={clearFile}
              title={m.import_remove_file()}
              aria-label={m.import_remove_file()}
            >
              <FileXIcon size="14" strokeWidth="1.8" aria-hidden="true" />
            </button>
          </div>
        </div>
      {:else}
        <div
          class="zone zone-empty"
          class:drag-over={dragOver}
          role="button"
          tabindex="0"
          ondragenter={handleDragEnter}
          ondragover={handleDragOver}
          ondragleave={handleDragLeave}
          ondrop={handleDrop}
          onclick={() => fileInput?.click()}
          onkeydown={(e) => {
            if (
              e.key === "Enter" ||
              e.key === " "
            ) {
              e.preventDefault();
              fileInput?.click();
            }
          }}
        >
          <UploadIcon class="upload-icon" size="28" strokeWidth="1.5" aria-hidden="true" />
          <span class="drop-label">
            {m.import_drop_here()}
          </span>
          <span class="drop-sub">
            {m.import_or_browse()}
          </span>
        </div>
      {/if}

      <input
        bind:this={fileInput}
        type="file"
        accept={accepted}
        onchange={handleFileChange}
        class="sr-only"
      />

      {#if error}
        <div class="import-error">
          <TriangleAlertIcon size="14" strokeWidth="1.8" aria-hidden="true" />
          <span>{error}</span>
        </div>
      {/if}
    {/if}
  </Modal>
{/if}

<style>
  /* ── Provider strip ── */
  .provider-strip {
    display: flex;
    gap: 1px;
    background: var(--border-default);
    border-radius: var(--radius-md);
    overflow: hidden;
    margin-bottom: 12px;
  }

  .provider-pill {
    flex: 1;
    display: flex;
    align-items: center;
    justify-content: center;
    gap: 7px;
    height: 34px;
    font-size: 12px;
    font-weight: 500;
    color: var(--text-muted);
    background: var(--bg-surface);
    transition: color 0.12s, background 0.12s;
  }

  .provider-pill:hover:not(:disabled) {
    background: var(--bg-surface-hover);
    color: var(--text-secondary);
  }

  .provider-pill.selected {
    color: var(--text-primary);
    font-weight: 600;
  }

  .pdot {
    width: 8px;
    height: 8px;
    border-radius: 50%;
    opacity: 0.35;
    transition: opacity 0.15s;
  }

  .provider-pill.selected .pdot {
    opacity: 1;
  }

  .pdot.claude {
    background: var(--accent-coral);
  }

  .pdot.chatgpt {
    background: var(--accent-green);
  }

  /* ── Hint ── */
  .hint {
    font-size: 12px;
    color: var(--text-muted);
    margin-bottom: 12px;
    line-height: 1.5;
  }

  .hint :global(code) {
    font-family: var(--font-mono);
    font-size: 11px;
    background: var(--bg-inset);
    padding: 1px 5px;
    border-radius: var(--radius-sm);
    color: var(--text-secondary);
  }

  /* ── Drop zone (shared) ── */
  .zone {
    display: flex;
    flex-direction: column;
    align-items: center;
    justify-content: center;
    gap: 6px;
    border-radius: var(--radius-lg);
    transition:
      border-color 0.15s,
      background 0.15s,
      box-shadow 0.15s;
  }

  /* Empty: dashed border, invites interaction */
  .zone-empty {
    min-height: 140px;
    padding: 24px;
    border: 2px dashed var(--border-default);
    cursor: pointer;
    box-shadow: inset 0 1px 3px rgba(0, 0, 0, 0.03);
  }

  .zone-empty:hover {
    border-color: var(--text-muted);
    background: var(--bg-surface-hover);
  }

  .zone-empty:focus-visible {
    outline: 2px solid var(--accent-blue);
    outline-offset: 2px;
  }

  .zone-empty.drag-over {
    border-color: var(--accent-blue);
    border-style: solid;
    background: color-mix(
      in srgb,
      var(--accent-blue) 6%,
      transparent
    );
    box-shadow: 0 0 0 3px
      color-mix(
        in srgb,
        var(--accent-blue) 12%,
        transparent
      );
  }

  .zone-empty.drag-over :global(.upload-icon) {
    color: var(--accent-blue);
    animation: icon-lift 0.35s ease-out;
  }

  @keyframes icon-lift {
    0% {
      transform: translateY(0);
    }
    50% {
      transform: translateY(-3px);
    }
    100% {
      transform: translateY(0);
    }
  }

  :global(.upload-icon) {
    color: var(--text-muted);
    margin-bottom: 2px;
    transition: color 0.15s;
  }

  .drop-label {
    font-size: 13px;
    font-weight: 500;
    color: var(--text-secondary);
  }

  .drop-sub {
    font-size: 11px;
    color: var(--text-muted);
  }

  /* File selected */
  .zone-file {
    border: 1px solid var(--border-default);
    background: var(--bg-inset);
    padding: 12px 16px;
  }

  .file-row {
    display: flex;
    align-items: center;
    gap: 10px;
    width: 100%;
  }

  :global(.file-icon) {
    color: var(--text-muted);
    flex-shrink: 0;
  }

  .file-meta {
    flex: 1;
    min-width: 0;
    display: flex;
    flex-direction: column;
    gap: 1px;
  }

  .file-name {
    font-size: 12px;
    font-weight: 500;
    color: var(--text-primary);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .file-size {
    font-size: 11px;
    color: var(--text-muted);
  }

  .file-clear {
    width: 24px;
    height: 24px;
    display: flex;
    align-items: center;
    justify-content: center;
    border-radius: var(--radius-sm);
    color: var(--text-muted);
    flex-shrink: 0;
    transition: background 0.08s, color 0.08s;
  }

  .file-clear:hover {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }

  /* Importing state */
  .zone-importing {
    min-height: 140px;
    padding: 24px;
    border: 1px solid var(--border-muted);
    background: var(--bg-inset);
  }

  .importing-label {
    font-size: 12px;
    color: var(--text-muted);
    margin-top: 4px;
  }

  /* ── Error ── */
  .import-error {
    display: flex;
    align-items: flex-start;
    gap: 8px;
    padding: 8px 12px;
    border: 1px solid var(--accent-red);
    border-radius: var(--radius-sm);
    font-size: 12px;
    color: var(--accent-red);
    margin-top: 12px;
    word-break: break-word;
  }

  .import-error :global(svg) {
    flex-shrink: 0;
    margin-top: 1px;
  }

  /* ── Result view ── */
  .result-view {
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: 8px;
    padding: 8px 0 4px;
  }

  .result-check {
    color: var(--accent-green);
    margin-bottom: 2px;
    animation: check-pop 0.35s ease-out;
  }

  @keyframes check-pop {
    0% {
      opacity: 0;
      transform: scale(0.6);
    }
    60% {
      transform: scale(1.08);
    }
    100% {
      opacity: 1;
      transform: scale(1);
    }
  }

  .result-heading {
    font-size: 14px;
    font-weight: 600;
    color: var(--text-primary);
  }

  .result-stats {
    display: flex;
    gap: 12px;
    margin: 8px 0 4px;
  }

  .stat {
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: 2px;
    min-width: 56px;
    padding: 8px 14px;
    background: var(--bg-inset);
    border-radius: var(--radius-md);
    animation: stat-up 0.3s ease-out backwards;
  }

  .stat:nth-child(1) {
    animation-delay: 0.05s;
  }

  .stat:nth-child(2) {
    animation-delay: 0.12s;
  }

  .stat:nth-child(3) {
    animation-delay: 0.19s;
  }

  .stat:nth-child(4) {
    animation-delay: 0.26s;
  }

  @keyframes stat-up {
    from {
      opacity: 0;
      transform: translateY(6px);
    }
  }

  .stat-num {
    font-size: 18px;
    font-weight: 700;
    font-variant-numeric: tabular-nums;
  }

  .stat-num.new {
    color: var(--accent-green);
  }

  .stat-num.updated {
    color: var(--accent-blue);
  }

  .stat-num.skipped {
    color: var(--accent-amber);
  }

  .stat-num.errors {
    color: var(--accent-red);
  }

  .stat-lbl {
    font-size: 10px;
    font-weight: 500;
    color: var(--text-muted);
    text-transform: uppercase;
    letter-spacing: 0.04em;
  }
</style>
