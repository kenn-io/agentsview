<!-- ABOUTME: One row inside the Calls section — call name, args preview, timing bar, duration label. -->
<script lang="ts">
  import { m } from "../../i18n/index.js";
  import type { CallTiming } from "../../api/types/timing.js";
  import { formatDuration } from "../../utils/duration.js";
  import { categoryToken } from "../../utils/categoryToken.js";
  import { displayToolName } from "../../utils/toolDisplay.js";

  interface Props {
    call: CallTiming;
    barWidthPct: number;
    isSlow?: boolean;
    isShared?: boolean;
    isLive?: boolean;
    /** Elapsed ms for the running call, supplied by the parent
     *  from a 1Hz ticker. Used only when `isLive` is true; falls
     *  back to `call.duration_ms` (or 0) when omitted. */
    liveDurationMs?: number;
    isSubagentExpanded?: boolean;
    expandable?: boolean;
    dimmed?: boolean;
    sharedDurationLabel?: string | null;
    onClick?: () => void;
    onChevronClick?: () => void;
  }

  let {
    call,
    barWidthPct,
    isSlow = false,
    isShared = false,
    isLive = false,
    liveDurationMs,
    isSubagentExpanded = false,
    expandable = true,
    dimmed = false,
    sharedDurationLabel,
    onClick,
    onChevronClick,
  }: Props = $props();

  let isSubagent = $derived(call.subagent_session_id != null);

  let durationLabel = $derived.by(() => {
    if (isLive) {
      const elapsed = liveDurationMs ?? call.duration_ms ?? 0;
      return m.call_row_running_duration({
        duration: formatDuration(elapsed),
      });
    }
    if (call.duration_ms == null) {
      return sharedDurationLabel ?? "—";
    }
    return formatDuration(call.duration_ms);
  });

  function handleChevronClick(e: MouseEvent) {
    e.stopPropagation();
    onChevronClick?.();
  }
</script>

<!-- svelte-ignore a11y_no_static_element_interactions -->
<!-- svelte-ignore a11y_no_noninteractive_tabindex -->
<div
  class="call"
  class:slow={isSlow}
  class:expanded={isSubagentExpanded}
  class:dimmed
  class:interactive={!!onClick}
  role={onClick ? "button" : undefined}
  tabindex={onClick ? 0 : undefined}
  onclick={onClick}
  onkeydown={onClick
    ? (e) => {
        if (e.target !== e.currentTarget) return;
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          onClick();
        }
      }
    : undefined}
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
  <span class="cn" style="color: {categoryToken(call.category)}">{displayToolName(call)}</span>
  <span class="ca">{call.input_preview}</span>
  <span class="cbar-wrap">
    <span
      class="cbar"
      class:shared={isShared}
      class:live={isLive}
      style={isLive
        ? `width: ${barWidthPct}%`
        : `width: ${barWidthPct}%; background: ${categoryToken(call.category)}`}
    ></span>
  </span>
  <span class="cd" class:slow={isSlow} class:live={isLive} class:muted={!isSlow && !isLive}>
    {durationLabel}
  </span>
</div>

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
  .call .cbar.shared {
    opacity: 0.55;
    background-image: repeating-linear-gradient(
      45deg,
      color-mix(in srgb, var(--text-primary) 18%, transparent) 0 3px,
      transparent 3px 6px
    );
  }
  .call .cbar.live {
    background: linear-gradient(
      90deg,
      var(--running-fg),
      color-mix(in srgb, var(--running-fg) 70%, black)
    );
    animation:
      duration-pulse 1.6s ease-in-out infinite,
      live-grow-fallback 1s linear infinite;
    transform-origin: left center;
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
  @keyframes live-grow-fallback {
    from {
      transform: scaleX(0.985);
    }
    to {
      transform: scaleX(1);
    }
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
