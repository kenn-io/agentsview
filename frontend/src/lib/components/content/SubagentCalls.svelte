<!-- ABOUTME: Inline-expansion of a sub-agent session's call list inside the parent Calls section. -->
<script lang="ts">
  import type {
    SessionTiming,
    CallTiming,
    TurnTiming,
  } from "../../api/types/timing.js";
  import { formatDuration } from "../../utils/duration.js";
  import { liveTick } from "../../stores/liveTick.svelte.js";
  import CallRow from "./CallRow.svelte";
  import CallGroup from "./CallGroup.svelte";

  interface Props {
    timing: SessionTiming;
    barScalePct: (call: CallTiming) => number;
    categoryFilter?: string | null;
  }

  let {
    timing,
    barScalePct,
    categoryFilter = null,
  }: Props = $props();

  // Nested-nested expansion is intentionally disabled in v1.
  // expandable={false} on the inner CallRow / CallGroup cuts the
  // chevron off at render time, so the empty Set and noop callback
  // here are never observable but still satisfy CallGroup's
  // required props.
  const noSubagentExpansion: Set<string> = new Set();
  function noopExpand(_c: CallTiming): void {
    /* no-op for nested rows */
  }

  function isLastTurn(idx: number): boolean {
    return idx === timing.turns.length - 1;
  }

  function turnHeaderBarPct(turn: {
    duration_ms: number | null;
  }): number {
    if (turn.duration_ms == null || timing.total_duration_ms <= 0) {
      return 0;
    }
    return Math.min(
      100,
      (turn.duration_ms / timing.total_duration_ms) * 100,
    );
  }

  function liveElapsedFor(turn: TurnTiming): number {
    const start = new Date(turn.started_at).getTime();
    if (Number.isNaN(start)) return 0;
    return Math.max(0, liveTick.now - start);
  }
</script>

<div class="sa-expand">
  <div class="sa-eh">
    <span class="sa-eh-label">↳ sub-agent</span>
    <span class="sa-eh-meta"
      >{timing.tool_call_count} call{timing.tool_call_count === 1
        ? ""
        : "s"} ·
      {timing.running ? "running " : ""}{formatDuration(
        timing.total_duration_ms,
      )}{timing.running ? "+" : ""}</span
    >
  </div>
  <div class="calls">
    {#each timing.turns as turn, i (turn.message_id)}
      {@const isLive =
        turn.duration_ms == null && isLastTurn(i) && timing.running}
      {@const liveElapsed = isLive ? liveElapsedFor(turn) : undefined}
      {#if turn.calls.length === 1}
        {@const call = turn.calls[0]!}
        <CallRow
          {call}
          barWidthPct={barScalePct(call)}
          isLive={isLive}
          liveDurationMs={liveElapsed}
          expandable={false}
          dimmed={categoryFilter !== null &&
            call.category !== categoryFilter}
        />
      {:else}
        <CallGroup
          calls={turn.calls}
          groupDurationMs={turn.duration_ms}
          {barScalePct}
          headerBarPct={turnHeaderBarPct(turn)}
          isLive={isLive}
          liveDurationMs={liveElapsed}
          expandable={false}
          dimmed={categoryFilter !== null &&
            turn.primary_category !== categoryFilter}
          onCallClick={() => {}}
          onSubagentExpand={noopExpand}
          expandedSubagentIds={noSubagentExpansion}
        />
      {/if}
    {/each}
  </div>
</div>

<style>
  /* Copied verbatim from
     docs/superpowers/specs/2026-04-26-session-duration-ux-mockup.html
     (.sa-expand rules, lines 671–692). */
  .sa-expand {
    background: rgba(196, 90, 90, 0.04);
    border-left: 2px solid #c45a5a;
    margin: 2px 0 4px 26px;
    padding: 4px 4px 4px 0;
    border-radius: 0 3px 3px 0;
  }
  .sa-expand .sa-eh {
    font-family: ui-monospace, monospace;
    font-size: 9px;
    color: #c47a7a;
    text-transform: uppercase;
    letter-spacing: 0.5px;
    padding: 2px 8px 5px;
    display: flex;
    justify-content: space-between;
  }
  .sa-expand .sa-eh-meta {
    color: #888;
    text-transform: none;
    letter-spacing: 0;
  }
  /* mirrors .calls in SessionVitals; scoped so the wrapping
     section's rule doesn't leak into nested layouts */
  .calls {
    display: flex;
    flex-direction: column;
    gap: 1px;
  }
</style>
