<script lang="ts">
  import { getServerUrl } from "../../api/runtime.js";
  import { CheckIcon, LinkIcon } from "../../icons.js";
  import { m } from "../../i18n/index.js";
  import { getBasePath, router } from "../../stores/router.svelte.js";
  import { sync } from "../../stores/sync.svelte.js";
  import { copyToClipboard } from "../../utils/clipboard.js";

  let { sessionId, ordinal }: { sessionId: string; ordinal: number } = $props();
  let copied = $state(false);
  let copyTimer: ReturnType<typeof setTimeout>;

  async function copyLink() {
    const server = getServerUrl();
    const href = router.buildMessageHref(sessionId, ordinal);
    const url = server
      ? server.replace(/\/+$/, "") + href.slice(getBasePath().length)
      : sync.isDesktop
        ? `agentsview://sessions/${encodeURIComponent(sessionId)}?msg=${ordinal}`
        : window.location.origin + href;
    if (await copyToClipboard(url)) {
      clearTimeout(copyTimer);
      copied = true;
      copyTimer = setTimeout(() => { copied = false; }, 1500);
    }
  }
</script>

<button type="button" onclick={copyLink}
  aria-label={copied ? m.message_content_copied() : m.message_content_copy_link()}
  title={copied ? m.message_content_copied() : m.message_content_copy_link()}>
  {#if copied}<CheckIcon size="14" strokeWidth="2.4" aria-hidden="true" />
  {:else}<LinkIcon size="14" strokeWidth="1.8" aria-hidden="true" />{/if}
</button>

<style>
  button {
    display: flex; align-items: center; justify-content: center; width: 26px; height: 26px;
    border: none; border-radius: var(--radius-sm, 4px); background: transparent;
    color: var(--text-muted); cursor: pointer; opacity: 0;
    transition: opacity 0.15s, background 0.15s, color 0.15s; flex-shrink: 0;
  }
  :global(.message:hover) button, button:focus-visible { opacity: 1; }
  @media (hover: none) { button { opacity: 1; } }
  button:hover { background: var(--bg-surface-hover); color: var(--text-secondary); }
  button:active { transform: var(--press-transform); }
</style>
