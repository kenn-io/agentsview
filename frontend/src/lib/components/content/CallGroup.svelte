<!-- ABOUTME: Visual grouping for parallel calls in the Calls section — left rail + header chip + member CallRows. -->
<script lang="ts">
  import type { CallTiming } from "../../api/types/timing.js";
  import { formatDuration } from "../../utils/duration.js";
  import CallRow from "./CallRow.svelte";

  interface Props {
    calls: CallTiming[];
    groupDurationMs: number | null;
    barScalePct: (call: CallTiming) => number;
    headerBarPct: number;
    onCallClick: (call: CallTiming) => void;
    onSubagentExpand: (call: CallTiming) => void;
    expandedSubagentIds: Set<string>;
    isLive?: boolean;
    /** Elapsed ms for the running tail call when `isLive` is
     *  true. Forwarded to the last CallRow so the duration cell
     *  ticks instead of rendering "running 0ms+". */
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
    groupDurationMs,
    barScalePct,
    headerBarPct,
    onCallClick,
    onSubagentExpand,
    expandedSubagentIds,
    isLive = false,
    liveDurationMs,
    isSlow,
    expandable = true,
    dimmed = false,
  }: Props = $props();

  let sharedLabel = $derived(
    groupDurationMs != null
      ? `≤${formatDuration(groupDurationMs)}`
      : isLive && liveDurationMs != null
        ? `≤${formatDuration(liveDurationMs)}+`
        : null,
  );
</script>

<div class="cgroup" class:dimmed>
  <div class="cg-rail"></div>
  <div class="cg-members">
    <div class="cg-header">
      <span class="cg-h-label">parallel · {calls.length} calls</span>
      <span class="cg-h-bar-wrap">
        <span class="cg-h-bar" style="width: {headerBarPct}%"></span>
      </span>
      <span class="cg-h-dur">
        {groupDurationMs != null ? formatDuration(groupDurationMs) : "—"}
      </span>
    </div>
    {#each calls as call, i (call.tool_use_id)}
      {@const isLastLive = isLive && i === calls.length - 1}
      <CallRow
        {call}
        barWidthPct={barScalePct(call)}
        isShared={call.subagent_session_id == null && !isLastLive}
        isLive={isLastLive}
        liveDurationMs={isLastLive ? liveDurationMs : undefined}
        isSlow={isSlow ? isSlow(call) : false}
        {expandable}
        isSubagentExpanded={call.subagent_session_id != null &&
          expandedSubagentIds.has(call.subagent_session_id)}
        sharedDurationLabel={sharedLabel}
        onClick={() => onCallClick(call)}
        onChevronClick={() => onSubagentExpand(call)}
      />
    {/each}
  </div>
</div>

<style>
  /* Copied verbatim from
     docs/superpowers/specs/2026-04-26-session-duration-ux-mockup.html
     (.cgroup rules, lines 608–668). */
  .cgroup {
    display: grid;
    grid-template-columns: 14px 1fr;
    margin: 3px 0;
    background: rgba(255, 255, 255, 0.025);
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
    background: #4a4a4a;
    border-radius: 1px;
  }
  .cgroup .cg-members {
    display: flex;
    flex-direction: column;
    gap: 1px;
  }
  .cgroup .cg-header {
    display: grid;
    grid-template-columns: 1fr 56px 56px;
    gap: 5px;
    align-items: center;
    padding: 0 5px 4px;
    margin-bottom: 2px;
  }
  .cgroup .cg-h-label {
    font-family: ui-monospace, monospace;
    font-size: 9px;
    color: #888;
    text-transform: uppercase;
    letter-spacing: 0.5px;
  }
  .cgroup .cg-h-bar-wrap {
    height: 7px;
    background: #1c1c1c;
    border-radius: 1px;
    position: relative;
    overflow: hidden;
  }
  .cgroup .cg-h-bar {
    position: absolute;
    inset: 0 auto 0 0;
    background: linear-gradient(
      90deg,
      rgba(124, 124, 124, 0.6),
      rgba(124, 124, 124, 0.3)
    );
    border-radius: 1px;
  }
  .cgroup .cg-h-dur {
    font-family: ui-monospace, monospace;
    font-size: 10px;
    color: #aaa;
    text-align: right;
  }
  .cgroup.dimmed {
    opacity: 0.3;
    transition: opacity 0.18s;
  }
</style>
