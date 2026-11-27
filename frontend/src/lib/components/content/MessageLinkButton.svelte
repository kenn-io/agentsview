<script lang="ts">
  import { CopyButton } from "@kenn-io/kit-ui";
  import { getServerUrl } from "../../api/runtime.js";
  import { router } from "../../stores/router.svelte.js";
  import { sync } from "../../stores/sync.svelte.js";
  import type { DbMessage } from "../../api/generated/index.js";
  import { messageIdentity } from "../../utils/message-identity.js";
  import { m } from "../../i18n/index.js";

  interface Props { message: DbMessage; }
  let { message }: Props = $props();
  let url = $derived.by(() => {
    const server = getServerUrl();
    const { session_id: sessionId, ordinal } = message;
    const identity = messageIdentity(message);
    return server
      ? router.buildMessageHref(sessionId, ordinal, identity, server.replace(/\/+$/, ""))
      : sync.isDesktop
        ? `agentsview://sessions/${encodeURIComponent(sessionId)}?msg=${ordinal}&mid=${identity}`
        : window.location.origin + router.buildMessageHref(sessionId, ordinal, identity);
  });
</script>

<CopyButton text={url} revealOnHover class="message-link-btn"
  ariaLabel={m.message_content_copy_link()} copiedAriaLabel={m.message_content_copied()}
  title={m.message_content_copy_link()} copiedTitle={m.message_content_copied()} />
