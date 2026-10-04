<script lang="ts">
  import { onDestroy } from "svelte";
  import { Button, Card, CopyButton } from "@kenn-io/kit-ui";
  import { downloadFrictionDigestMarkdown } from "../../api/client.js";
  import { m } from "../../i18n/index.js";
  import { friction } from "../../stores/friction.svelte.js";
  import { copyToClipboard } from "../../utils/clipboard.js";

  interface Props {
    date: string;
  }

  let { date }: Props = $props();
  let copied = $state(false);
  let downloadError: string | null = $state(null);
  let copyTimer: ReturnType<typeof setTimeout> | undefined;

  async function handleCopy() {
    if (friction.markdown === null) return;
    const ok = await copyToClipboard(friction.markdown);
    if (!ok) return;
    clearTimeout(copyTimer);
    copied = true;
    copyTimer = setTimeout(() => {
      copied = false;
    }, 1500);
  }

  async function handleDownload() {
    downloadError = null;
    try {
      await downloadFrictionDigestMarkdown(date);
    } catch (e) {
      downloadError = e instanceof Error ? e.message : m.friction_markdown_error();
    }
  }

  onDestroy(() => clearTimeout(copyTimer));
</script>

<section class="friction-markdown" aria-labelledby="friction-markdown-title">
  <div class="markdown-head">
    <h2 id="friction-markdown-title">{m.friction_markdown_title()}</h2>
    <div class="markdown-actions">
      {#if friction.markdown === null}
        <Button
          size="sm"
          label={m.friction_markdown_show()}
          disabled={friction.loading.markdown}
          onclick={() => void friction.loadMarkdown()}
        />
      {:else}
        <CopyButton
          {copied}
          ariaLabel={m.friction_markdown_copy()}
          copiedAriaLabel={m.friction_markdown_copied()}
          title={m.friction_markdown_copy()}
          copiedTitle={m.friction_markdown_copied()}
          onclick={handleCopy}
        />
      {/if}
      <Button
        size="sm"
        label={m.friction_markdown_download()}
        onclick={() => void handleDownload()}
      />
    </div>
  </div>

  {#if friction.loading.markdown}
    <p class="placeholder">{m.friction_markdown_loading()}</p>
  {:else if friction.errors.markdown}
    <p class="error" role="alert">{friction.errors.markdown}</p>
  {:else if friction.markdown !== null}
    <Card level="inset" padding="none" class="markdown-card">
      <pre class="markdown-source">{friction.markdown}</pre>
    </Card>
  {/if}
  {#if downloadError}
    <p class="error" role="alert">{downloadError}</p>
  {/if}
</section>

<style>
  .friction-markdown {
    display: flex;
    flex-direction: column;
    gap: 8px;
  }
  .markdown-head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 12px;
  }
  .markdown-head h2 {
    margin: 0;
    font-size: 14px;
  }
  .markdown-actions {
    display: flex;
    align-items: center;
    gap: 8px;
  }
  .markdown-source {
    margin: 0;
    padding: 12px;
    max-height: 480px;
    overflow: auto;
    font-size: 12px;
    white-space: pre-wrap;
    word-break: break-word;
  }
  .placeholder {
    margin: 0;
    color: var(--text-muted);
    font-style: italic;
  }
  .error {
    margin: 0;
    color: var(--accent-red);
  }
</style>
