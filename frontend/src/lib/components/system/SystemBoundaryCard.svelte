<script lang="ts">
  import { _ } from "svelte-i18n";
  import { formatTimestamp } from "../../utils/format.js";

  interface Props {
    subtype: string;
    content: string;
    timestamp: string;
  }
  let { subtype, content, timestamp }: Props = $props();

  const LABELS: Record<string, string> = $derived({
    continuation: $_("systemBoundary.continuation"),
    resume: $_("systemBoundary.resume"),
    interrupted: $_("systemBoundary.interrupted"),
    task_notification: $_("systemBoundary.taskNotification"),
    stop_hook: $_("systemBoundary.stopHook"),
  });

  let label = $derived(LABELS[subtype] ?? subtype);
  let preview = $derived.by(() => {
    const text = (content ?? "").trim();
    if (!text) return "";
    const firstLine = text.split("\n")[0] ?? "";
    return firstLine.length > 140
      ? firstLine.slice(0, 140) + "…"
      : firstLine;
  });
</script>

<div
  class="system-boundary"
  title={$_("systemBoundary.title", { values: { subtype } })}
>
  <span class="label">{label}</span>
  {#if timestamp}
    <span class="timestamp">
      &middot; {formatTimestamp(timestamp)}
    </span>
  {/if}
  {#if preview}
    <details class="details">
      <summary>{$_("systemBoundary.showContent")}</summary>
      <pre>{content}</pre>
    </details>
  {/if}
</div>

<style>
  .system-boundary {
    padding: 6px 12px;
    margin: 4px 12px;
    background: color-mix(
      in srgb, var(--text-muted) 6%, transparent
    );
    border-left: 3px solid
      color-mix(in srgb, var(--text-muted) 40%, transparent);
    border-radius: 0 var(--radius-sm, 4px)
      var(--radius-sm, 4px) 0;
    font-size: 12px;
    color: var(--text-secondary);
    display: flex;
    flex-direction: column;
    gap: 4px;
  }
  .label {
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: 0.04em;
    font-size: 11px;
  }
  .timestamp {
    font-size: 11px;
    color: var(--text-muted);
  }
  .details {
    margin-top: 2px;
  }
  .details summary {
    cursor: pointer;
    font-size: 11px;
    color: var(--text-muted);
  }
  .details pre {
    white-space: pre-wrap;
    margin: 6px 0 0;
    padding: 8px 10px;
    font-size: 12px;
    background: var(--bg-inset);
    border-radius: var(--radius-sm, 4px);
    overflow-x: auto;
  }
</style>
