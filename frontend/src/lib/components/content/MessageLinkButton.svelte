<script lang="ts">
  import { CopyButton } from "@kenn-io/kit-ui";
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
</script>

<CopyButton text={url} revealOnHover class="message-link-btn"
  ariaLabel={m.message_content_copy_link()} copiedAriaLabel={m.message_content_copied()}
  title={m.message_content_copy_link()} copiedTitle={m.message_content_copied()} />
