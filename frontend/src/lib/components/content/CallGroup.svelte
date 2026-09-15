<!-- ABOUTME: Visual grouping for parallel calls in the Calls section — left rail + header chip + member CallRows. -->
<script lang="ts">
  import { m } from "../../i18n/index.js";
  import type { CallTiming } from "../../api/types/timing.js";
  import { formatNumber } from "../../utils/format.js";
  import CallRow from "./CallRow.svelte";

  interface Props {
    calls: CallTiming[];
    barScalePct: (call: CallTiming) => number;
    onCallClick: (call: CallTiming) => void;
    onSubagentExpand: (call: CallTiming) => void;
    expandedSubagentIds: Set<string>;
    isLive?: boolean;
    liveDurationMs?: number;
    /** Per-call slow predicate. When provided, rows whose call
     *  matches get the slow tint. The parent computes the
     *  threshold once for the whole timing snapshot and passes
     *  the predicate so we don't recompute per row. */
    isSlow?: (call: CallTiming) => boolean;
    expandable?: boolean;
    dimmed?: boolean;
  }

  let {
    calls,
    barScalePct,
    onCallClick,
    onSubagentExpand,
    expandedSubagentIds,
    isLive = false,
    liveDurationMs,
    isSlow,
    expandable = true,
    dimmed = false,
  }: Props = $props();
</script>

<div class="cgroup" class:dimmed>
  <div class="cg-rail"></div>
  <div class="cg-members">
    <div class="cg-header">
      <span class="cg-h-label">
        {m.call_group_parallel_label()} · {m.call_group_parallel_call_count({
          count: calls.length,
          countLabel: formatNumber(calls.length),
        })}
      </span>
    </div>
    {#each calls as call, i (call.tool_use_id)}
      {@const isLastLive = isLive && i === calls.length - 1}
      <CallRow
        {call}
        barWidthPct={barScalePct(call)}
        isLive={isLastLive}
        liveDurationMs={isLastLive ? liveDurationMs : undefined}
        isSlow={isSlow ? isSlow(call) : false}
        {expandable}
        isSubagentExpanded={call.subagent_session_id != null &&
          expandedSubagentIds.has(call.subagent_session_id)}
        onClick={() => onCallClick(call)}
        onChevronClick={() => onSubagentExpand(call)}
      />
    {/each}
  </div>
</div>

<style>
  /* Adapted from the session-duration UX mockup, with the raw dark-theme
     colors mapped to theme tokens. */
  .cgroup {
    display: grid;
    grid-template-columns: 14px 1fr;
    margin: 3px 0;
    background: color-mix(in srgb, var(--text-primary) 3%, transparent);
    border-radius: 3px;
    padding: 3px 0;
  }
  .cgroup .cg-rail {
    position: relative;
    margin: 4px 0 4px 5px;
  }
  .cgroup .cg-rail::before {
    content: "";
    position: absolute;
    left: 4px;
    top: 0;
    bottom: 0;
    width: 2px;
    background: var(--cat-mixed);
    border-radius: 1px;
  }
  .cgroup .cg-members {
    display: flex;
    flex-direction: column;
    gap: 1px;
  }
  .cgroup .cg-header {
    padding: 0 5px 4px;
    margin-bottom: 2px;
  }
  .cgroup .cg-h-label {
    font-family: ui-monospace, monospace;
    font-size: 9px;
    color: var(--text-muted);
    text-transform: uppercase;
    letter-spacing: 0.5px;
  }
  .cgroup.dimmed {
    opacity: 0.3;
    transition: opacity 0.18s;
  }
  :global(.high-contrast) .cgroup .cg-h-label {
    color: var(--text-secondary);
  }
  :global(.high-contrast) .cgroup .cg-rail::before {
    background: var(--border-default);
  }
</style>
