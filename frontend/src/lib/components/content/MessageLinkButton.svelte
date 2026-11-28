<script lang="ts">
  import { getServerUrl, SERVER_URL_CHANGE_EVENT } from "../../api/runtime.js";
  import { CopyButton } from "@kenn-io/kit-ui";
  import { m } from "../../i18n/index.js";
  import { getBasePath, router } from "../../stores/router.svelte.js";
  import { sync } from "../../stores/sync.svelte.js";

  let { sessionId, ordinal }: { sessionId: string; ordinal: number } = $props();
  let server = $state(getServerUrl());
  $effect(() => {
    const update = () => { server = getServerUrl(); };
    window.addEventListener(SERVER_URL_CHANGE_EVENT, update);
    return () => window.removeEventListener(SERVER_URL_CHANGE_EVENT, update);
  });
  let url = $derived.by(() => {
    const href = router.buildMessageHref(sessionId, ordinal);
    return server
      ? server.replace(/\/+$/, "") + href.slice(getBasePath().length)
      : sync.isDesktop
        ? `agentsview://sessions/${encodeURIComponent(sessionId)}?msg=${ordinal}`
        : window.location.origin + href;
  });
</script>

<CopyButton revealOnHover text={url}
  ariaLabel={m.message_content_copy_link()} copiedAriaLabel={m.message_content_copied()}
  title={m.message_content_copy_link()} copiedTitle={m.message_content_copied()} />
