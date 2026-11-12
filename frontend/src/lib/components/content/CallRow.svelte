<!-- ABOUTME: One row inside the Calls section — call name, args preview, timing bar, duration label. -->
<script lang="ts">
  import { m } from "../../i18n/index.js";
  import type { DbCallTiming as CallTiming } from "../../api/generated/index.js";
  import { formatDuration } from "../../utils/duration.js";
  import { categoryToken } from "../../utils/categoryToken.js";
  import { displayToolName } from "../../utils/toolDisplay.js";

  interface Props {
    call: CallTiming;
    barWidthPct: number;
    isSlow?: boolean;
    isLive?: boolean;
    liveDurationMs?: number;
    isSubagentExpanded?: boolean;
    expandable?: boolean;
    dimmed?: boolean;
    /** False when no call in the list has timing: the row drops its bar
     *  and duration columns. */
    showTiming?: boolean;
    onClick?: () => void;
    onChevronClick?: () => void;
  }

  let {
    call,
    barWidthPct,
    isSlow = false,
    isLive = false,
    liveDurationMs,
    isSubagentExpanded = false,
    expandable = true,
    dimmed = false,
    showTiming = true,
    onClick,
    onChevronClick,
  }: Props = $props();

  let isSubagent = $derived(call.subagent_session_id != null);
  let isLiveRow = $derived(isLive && call.duration_ms == null);

  let durationLabel = $derived.by(() => {
    if (isLiveRow) {
      return m.call_row_running_duration({
        duration: formatDuration(liveDurationMs ?? call.duration_ms ?? 0),
      });
    }
    // A missing duration shows nothing rather than a placeholder.
    return call.duration_ms != null ? formatDuration(call.duration_ms) : "";
  });

  function handleChevronClick(e: MouseEvent) {
    e.stopPropagation();
    onChevronClick?.();
  }
</script>

<!-- The chevron is a sibling of the clickable body, never inside it, so the
     row has no control nested in another. -->
<div
  class="call"
  class:slow={isSlow}
  class:expanded={isSubagentExpanded}
  class:dimmed
  class:untimed={!showTiming}
  class:interactive={!!onClick}
>
  {#if isSubagent && expandable}
    <button
      type="button"
      class="chev"
      aria-label={m.call_row_toggle_subagent_calls()}
      aria-expanded={isSubagentExpanded}
      onclick={handleChevronClick}
    >▸</button>
  {:else}
    <span class="chev spacer">▸</span>
  {/if}
  {#if onClick}
    <button type="button" class="call-body" onclick={onClick}>
      {@render body()}
    </button>
  {:else}
    <div class="call-body">
      {@render body()}
    </div>
  {/if}
</div>

{#snippet body()}
  <span class="cn" style="color: {categoryToken(call.category)}">{displayToolName(call)}</span>
  <span class="ca">{call.input_preview}</span>
  {#if showTiming}
    <span class="cbar-wrap">
      <span
        class="cbar"
        style="width: {call.duration_ms == null || call.duration_ms <= 0 ? 0 : barWidthPct}%; background: {categoryToken(call.category)}"
      ></span>
    </span>
    <span
      class="cd"
      class:slow={isSlow}
      class:live={isLiveRow}
      class:muted={!isSlow && !isLiveRow}
    >
      {durationLabel}
    </span>
  {/if}
{/snippet}

<style>
  /* Adapted from the session-duration UX mockup, with the raw dark-theme colors
     mapped to theme tokens. The .cn color is set via inline style
     from categoryToken() rather than via .cn.read/.bash/etc class
     modifiers — that's the only structural deviation. */
  .call {
    display: grid;
    grid-template-columns: 14px 38px 1fr 56px 56px;
    gap: var(--space-2);
    align-items: center;
    padding: 4px 5px;
    font-size: 10px;
    border-radius: 2px;
    position: relative;
  }
  .call.untimed {
    grid-template-columns: 14px 38px 1fr;
  }
  /* The name, preview, bar, and duration share the row's columns. */
  .call .call-body {
    grid-column: 2 / -1;
    display: grid;
    grid-template-columns: subgrid;
    align-items: center;
    min-width: 0;
    margin: 0;
    padding: 0;
    text-align: left;
  }
  /* A click anywhere on the row, padding included, opens the turn; the
     chevron sits above this layer. */
  .call button.call-body::before {
    content: "";
    position: absolute;
    inset: 0;
  }
  .call button.chev {
    position: relative;
    z-index: 1;
  }
  .call.interactive {
    cursor: pointer;
  }
  .call.interactive:hover {
    background: color-mix(in srgb, var(--text-primary) 4%, transparent);
  }
  .call .chev {
    background: transparent;
    border: 0;
    padding: 0;
    cursor: pointer;
    color: var(--text-muted);
    font: inherit;
    font-size: 10px;
    transition: transform 0.15s;
  }
  .call.expanded .chev {
    transform: rotate(90deg);
    color: var(--text-secondary);
  }
  .call .chev.spacer {
    visibility: hidden;
  }
  .call .cn {
    font-family: ui-monospace, monospace;
    font-size: 10px;
    font-weight: 600;
  }
  .call .ca {
    font-family: ui-monospace, monospace;
    font-size: 10px;
    color: var(--text-muted);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .call .cbar-wrap {
    height: 9px;
    background: transparent;
    border-radius: 1px;
    position: relative;
    overflow: hidden;
  }
  .call .cbar {
    position: absolute;
    left: 0;
    top: 0;
    bottom: 0;
    border-radius: 1px;
  }
  .call.slow .cbar-wrap {
    background: var(--slow-bg);
  }
  .call .cd {
    font-family: ui-monospace, monospace;
    font-size: 10px;
    color: var(--text-secondary);
    text-align: right;
  }
  .call .cd.slow {
    color: var(--slow-fg);
    display: flex;
    align-items: center;
    justify-content: flex-end;
    gap: 4px;
  }
  .call .cd.slow::before {
    content: "";
    display: inline-block;
    width: 4px;
    height: 4px;
    border-radius: 50%;
    background: var(--slow-fg);
  }
  .call .cd.live {
    color: var(--running-fg);
    animation: duration-pulse 1.6s ease-in-out infinite;
  }
  .call .cd.muted {
    color: var(--text-muted);
  }
  .call.dimmed {
    opacity: 0.3;
    transition: opacity 0.18s;
  }
  :global(.high-contrast) .call .ca,
  :global(.high-contrast) .call .cd.muted,
  :global(.high-contrast) .call .chev {
    color: var(--text-secondary);
  }
  :global(.high-contrast) .call.expanded .chev {
    color: var(--text-primary);
  }
</style>
