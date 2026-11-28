<script lang="ts">
  import { IconButton, copyToClipboard } from "@kenn-io/kit-ui";
  import { CheckIcon, LinkIcon } from "../../icons.js";
  import { getServerUrl } from "../../api/runtime.js";
  import { router } from "../../stores/router.svelte.js";
  import { sync } from "../../stores/sync.svelte.js";
  import { m } from "../../i18n/index.js";

  interface Props { sessionId: string; ordinal: number; }
  let { sessionId, ordinal }: Props = $props();
  let url = $derived.by(() => {
    const server = getServerUrl();
    return server
      ? router.buildMessageHref(sessionId, ordinal, server.replace(/\/+$/, ""))
      : sync.isDesktop
        ? `agentsview://sessions/${encodeURIComponent(sessionId)}?msg=${ordinal}`
        : window.location.origin + router.buildMessageHref(sessionId, ordinal);
  });
  let copied = $state(false);
  let resetTimer: ReturnType<typeof setTimeout> | undefined;

  async function copyLink() {
    if (!(await copyToClipboard(url))) return;
    copied = true;
    clearTimeout(resetTimer);
    resetTimer = setTimeout(() => (copied = false), 1500);
  }
</script>

<IconButton size="sm" class="message-link-btn" onclick={copyLink}
  ariaLabel={copied ? m.message_content_copied() : m.message_content_copy_link()}>
  {#if copied}
    <CheckIcon size="14" strokeWidth="2.4" aria-hidden="true" />
  {:else}
    <LinkIcon size="14" strokeWidth="2" aria-hidden="true" />
  {/if}
</IconButton>

<style>
  /* Matches the 26px header actions beside it and hides until hover like them. */
  :global(button.message-link-btn.kit-icon-button) { width: 26px; height: 26px; opacity: 0; }
  :global(button.message-link-btn.kit-icon-button:focus-visible) { opacity: 1; }
  @media (hover: none) { :global(button.message-link-btn.kit-icon-button) { opacity: 1; } }
</style>
