<script lang="ts">
  import { Disclosure } from "@kenn-io/kit-ui";
  import type { DbMessage as Message } from "../../api/generated/index.js";
  import { formatTimestamp } from "../../utils/format.js";
  import { RefreshCwIcon } from "../../icons.js";
  import { m } from "../../i18n/index.js";

  interface Props {
    message: Message;
  }

  let { message }: Props = $props();

  let content = $derived((message.content ?? "").trim());
  let preview = $derived.by(() => {
    if (content.length === 0) return "";
    const firstLine = content.split("\n")[0]!;
    return firstLine.length > 140
      ? firstLine.slice(0, 140) + "…"
      : firstLine;
  });
  let hasMore = $derived(content.length > 0 && content !== preview);
</script>

<div class="boundary" title={m.compact_boundary_title()}>
  <span class="boundary-line"></span>
  <span class="boundary-label">
    <span class="boundary-icon">
      <RefreshCwIcon size="12" strokeWidth="2.2" aria-hidden="true" />
    </span>
    {m.compact_boundary_label()}
    {#if message.timestamp}
      <span class="boundary-time">
        &middot; {formatTimestamp(message.timestamp)}
      </span>
    {/if}
  </span>
  <span class="boundary-line"></span>
</div>
{#if preview}
  {#if hasMore}
    <Disclosure variant="plain" class="boundary-details">
      {#snippet summary()}
        <span class="boundary-summary">{preview}</span>
      {/snippet}
      {#snippet meta()}{m.compact_boundary_show_full_summary()}{/snippet}
      <pre class="boundary-full">{content}</pre>
    </Disclosure>
  {:else}
    <div class="boundary-preview">{preview}</div>
  {/if}
{/if}

<style>
  .boundary {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 6px 12px 0;
    color: var(--accent-amber);
    font-size: 11px;
    text-transform: uppercase;
    letter-spacing: 0.06em;
    font-weight: 600;
  }
  .boundary-line {
    flex: 1;
    height: 1px;
    background: color-mix(
      in srgb, var(--accent-amber) 35%, transparent
    );
  }
  .boundary-label {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    white-space: nowrap;
  }
  .boundary-icon {
    display: inline-flex;
    align-items: center;
  }
  .boundary-time {
    color: var(--text-muted);
    font-weight: 500;
    text-transform: none;
    letter-spacing: 0;
  }
  .boundary-preview,
  :global(.boundary-details) {
    margin: 4px 12px 6px;
    padding: 6px 10px;
    background: color-mix(
      in srgb, var(--accent-amber) 8%, transparent
    );
    border-left: 2px solid
      color-mix(in srgb, var(--accent-amber) 50%, transparent);
    color: var(--text-secondary);
    font-size: 12px;
    line-height: 1.5;
    border-radius: 0 var(--radius-sm, 4px)
      var(--radius-sm, 4px) 0;
  }
  /* The shared summary row adds 4px above and below, so the card keeps
     the short preview's height. */
  :global(.boundary-details) {
    padding-block: 2px;
  }
  .boundary-summary {
    color: var(--text-secondary);
    font-weight: 400;
  }
  .boundary-full {
    white-space: pre-wrap;
    margin: 6px 0 0;
    padding: 8px 10px;
    font-size: 12px;
    line-height: 1.55;
    color: var(--text-primary);
    background: var(--bg-inset);
    border-radius: var(--radius-sm, 4px);
    overflow-x: auto;
  }
</style>
