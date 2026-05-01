<script lang="ts">
  import type { Message } from "../../api/types.js";
  import { formatTimestamp } from "../../utils/format.js";

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

<div class="boundary" title="Context window compacted at this point">
  <span class="boundary-line"></span>
  <span class="boundary-label">
    <span class="boundary-icon" aria-hidden="true">↻</span>
    Context compacted
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
    <details class="boundary-details">
      <summary class="boundary-preview">
        <span>{preview}</span>
        <span class="boundary-expand-hint">Show full summary</span>
      </summary>
      <pre class="boundary-full">{content}</pre>
    </details>
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
    font-size: 13px;
    line-height: 1;
  }
  .boundary-time {
    color: var(--text-muted);
    font-weight: 500;
    text-transform: none;
    letter-spacing: 0;
  }
  .boundary-details {
    margin: 4px 12px 6px;
  }
  .boundary-preview {
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
  .boundary-details > .boundary-preview {
    margin: 0;
    cursor: pointer;
    list-style: none;
    display: flex;
    align-items: flex-start;
    justify-content: space-between;
    gap: 12px;
  }
  .boundary-details > .boundary-preview::-webkit-details-marker {
    display: none;
  }
  .boundary-expand-hint {
    flex: none;
    font-size: 11px;
    color: var(--text-muted);
    white-space: nowrap;
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
