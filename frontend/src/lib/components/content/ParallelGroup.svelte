<!-- ABOUTME: Wraps a contiguous run of parallel tool_use calls. -->
<script lang="ts">
  import type { ToolCall } from "../../api/types.js";
  import type { CallTiming } from "../../api/types/timing.js";
  import ToolBlock from "./ToolBlock.svelte";
  import { formatDuration } from "../../utils/duration.js";
  import { displayToolName } from "../../utils/toolDisplay.js";
  import { m } from "../../i18n/index.js";

  interface Props {
    toolCalls: ToolCall[];
    turnDurationMs: number | null;
    callTimingByID?: Map<string, CallTiming>;
    isRunning?: boolean;
    highlightQuery?: string;
    isCurrentHighlight?: boolean;
  }

  let {
    toolCalls,
    turnDurationMs,
    callTimingByID,
    isRunning = false,
    highlightQuery = "",
    isCurrentHighlight = false,
  }: Props = $props();

  let upperBoundLabel = $derived.by(() => {
    if (isRunning) return null;
    if (turnDurationMs == null) return null;
    return m.parallel_group_each_duration({
      duration: formatDuration(turnDurationMs),
    });
  });
</script>

<div class="parallel-group">
  <div class="pg-header">
    <span class="pg-label">{m.parallel_group_label()}</span>
    <span class="pg-count">{m.parallel_group_call_count({
      count: toolCalls.length,
    })}</span>
    <span class="pg-spacer"></span>
    {#if isRunning}
      <span class="pg-running">{m.parallel_group_running()}</span>
    {:else if upperBoundLabel}
      <span class="pg-upper">{upperBoundLabel}</span>
    {/if}
  </div>
  <div class="pg-members">
    {#each toolCalls as toolCall, i (toolCall.tool_use_id || `idx:${i}`)}
      {@const ct = callTimingByID?.get(toolCall.tool_use_id ?? "")}
      {@const dur =
        ct?.subagent_session_id && ct.duration_ms != null
          ? formatDuration(ct.duration_ms)
          : undefined}
      <ToolBlock
        {toolCall}
        content=""
        label={displayToolName(toolCall)}
        durationLabel={dur}
        inGroup={true}
        {highlightQuery}
        {isCurrentHighlight}
      />
    {/each}
  </div>
</div>

<style>
  .parallel-group {
    border-left: 2px solid var(--cat-mixed);
    background: color-mix(in srgb, var(--text-primary) 3%, transparent);
    border-radius: 0 var(--radius-sm) var(--radius-sm) 0;
    margin: 6px 0;
    padding: 4px 0;
    overflow: hidden;
  }
  .pg-header {
    display: flex;
    align-items: center;
    gap: var(--space-4);
    padding: 5px 12px 7px;
    font-family: var(--font-mono);
    font-size: 10px;
    color: var(--text-muted);
  }
  .pg-label {
    color: var(--text-secondary);
    font-weight: 500;
  }
  .pg-count {
    background: color-mix(in srgb, var(--text-primary) 6%, transparent);
    padding: 1px 7px;
    border-radius: 999px;
    font-size: 9px;
    color: var(--text-primary);
  }
  .pg-spacer {
    flex: 1;
  }
  .pg-upper {
    color: var(--text-muted);
    font-size: 10px;
  }
  .pg-running {
    color: var(--running-fg);
    font-size: 10px;
    animation: duration-pulse 1.6s ease-in-out infinite;
  }
  /* members render flush; ToolBlock honors inGroup={true} */
  .pg-members :global(.tool-block) {
    margin: 0;
    border-radius: 0;
  }
  .pg-members :global(.tool-block + .tool-block) {
    border-top: 1px solid color-mix(in srgb, var(--text-primary) 4%, transparent);
  }
  .pg-members :global(.tool-block:last-child) {
    border-bottom-right-radius: var(--radius-sm);
  }
</style>
