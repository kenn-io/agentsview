<script lang="ts">
  import { onDestroy } from "svelte";
  import { getServerUrl } from "../../api/runtime.js";
  import { router } from "../../stores/router.svelte.js";
  import { sync } from "../../stores/sync.svelte.js";
  import { copyToClipboard } from "../../utils/clipboard.js";
  import { CheckIcon, LinkIcon } from "../../icons.js";
  import { m } from "../../i18n/index.js";

  interface Props {
    sessionId: string;
    ordinal: number;
  }
  let { sessionId, ordinal }: Props = $props();
  let copied = $state(false);
  let timer: ReturnType<typeof setTimeout>;
  onDestroy(() => clearTimeout(timer));

  async function handleClick() {
    const server = getServerUrl();
    const url = server
      ? router.buildMessageHref(sessionId, ordinal, server.replace(/\/+$/, ""))
      : sync.isDesktop
        ? `agentsview://sessions/${encodeURIComponent(sessionId)}?msg=${ordinal}`
        : window.location.origin + router.buildMessageHref(sessionId, ordinal);
    if (!await copyToClipboard(url)) return;
    clearTimeout(timer);
    copied = true;
    timer = setTimeout(() => { copied = false; }, 1500);
  }
</script>

<button type="button" class="message-link-btn" onclick={handleClick}
  aria-label={copied ? m.message_content_copied() : m.message_content_copy_link()}
  title={copied ? m.message_content_copied() : m.message_content_copy_link()}>
  {#if copied}<CheckIcon size="14" strokeWidth="2.4" aria-hidden="true" />
  {:else}<LinkIcon size="14" strokeWidth="1.8" aria-hidden="true" />{/if}
</button>

<style>
  /* Hidden until the owning message row is hovered; the parent reveals it. */
  .message-link-btn {
    display: flex; align-items: center; justify-content: center; width: 26px; height: 26px;
    padding: 0; border: none; border-radius: var(--radius-sm, 4px); background: transparent;
    color: var(--text-muted); cursor: pointer; opacity: 0;
    transition: opacity 0.15s, background 0.15s, color 0.15s; flex-shrink: 0;
  }
  .message-link-btn:focus-visible { opacity: 1; }
  @media (hover: none) { .message-link-btn { opacity: 1; } }
  .message-link-btn:hover { background: var(--bg-surface-hover); color: var(--text-secondary); }
  .message-link-btn:active { transform: var(--press-transform); }
</style>
