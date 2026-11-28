<script lang="ts">
  import { getServerUrl } from "../../api/runtime.js";
  import { CopyButton } from "@kenn-io/kit-ui";
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

<CopyButton revealOnHover {copied} onclick={copyLink}
  ariaLabel={m.message_content_copy_link()} copiedAriaLabel={m.message_content_copied()}
  title={m.message_content_copy_link()} copiedTitle={m.message_content_copied()} />
