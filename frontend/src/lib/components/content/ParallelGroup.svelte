<script lang="ts">
  import type { DbToolCall as ToolCall } from "../../api/generated/index.js";
  import type { DbCallTiming as CallTiming } from "../../api/generated/index.js";
  import ToolBlock from "./ToolBlock.svelte";
  import { formatDuration } from "../../utils/duration.js";
  import { displayToolName } from "../../utils/toolDisplay.js";
  import { m } from "../../i18n/index.js";

  interface Props {
    toolCalls: ToolCall[];
    callTimingByID?: Map<string, CallTiming>;
    isRunning?: boolean;
    searchOrdinal?: number;
  }

  let {
    toolCalls,
    callTimingByID,
    isRunning = false,
    searchOrdinal,
  }: Props = $props();

</script>

<div class="parallel-group">
  <div class="pg-header">
    <span class="pg-label">{m.parallel_group_label()}</span>
    <span class="pg-count">{m.parallel_group_call_count({ count: toolCalls.length })}</span>
    <span class="pg-spacer"></span>
    {#if isRunning}
      <span class="pg-running">{m.parallel_group_running()}</span>
    {/if}
  </div>
  <div class="pg-members">
    {#each toolCalls as toolCall, i (toolCall.tool_use_id || `idx:${i}`)}
      {@const ct = callTimingByID?.get(toolCall.tool_use_id ?? "")}
      {@const dur = ct === undefined ? undefined : ct.duration_ms != null ? formatDuration(ct.duration_ms) : m.shared_unknown()}
      <ToolBlock
        {toolCall}
        content=""
        label={displayToolName(toolCall)}
        durationLabel={dur}
        inGroup={true}
        searchScope={searchOrdinal === undefined ? undefined : { ordinal: searchOrdinal, callIdx: i }}
      />
    {/each}
  </div>
</div>

<style>
  .parallel-group {
    /* The members drop their own bars, so the rail carries the tool marker. */
    border-left: var(--role-tool-bar-width) var(--role-tool-bar-style) var(--role-tool-group-rail);
    background: transparent;
    border-radius: 0 var(--radius-sm) var(--radius-sm) 0;
    margin: 4px 0;
    padding: 0;
    overflow: hidden;
  }
  /* Same header voice as ToolCallGroup's "3 tool calls": a 12px semibold
     sans label with the count in muted ink beside it. */
  .pg-header {
    display: flex;
    align-items: baseline;
    gap: 6px;
    padding: 3px 10px 3px;
    font-family: var(--font-sans);
    font-size: 12px;
    color: var(--text-muted);
  }
  .pg-label { color: var(--text-secondary); font-weight: 600; }
  .pg-count {
    color: var(--text-muted);
    font-variant-numeric: tabular-nums;
  }
  .pg-spacer { flex: 1; }
  .pg-running {
    color: var(--running-fg);
    font-size: 11px;
    animation: duration-pulse 1.6s ease-in-out infinite;
  }
  .pg-members :global(.tool-block) { margin: 0; border-radius: 0; }
  .pg-members :global(.tool-block + .tool-block) {
    border-top: 1px solid color-mix(in srgb, var(--text-primary) 4%, transparent);
  }
  .pg-members :global(.tool-block:last-child) { border-bottom-right-radius: var(--radius-sm); }
</style>
