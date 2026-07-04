<!-- ABOUTME: Inline-expansion of a sub-agent session's call list inside the parent Calls section. -->
<script lang="ts">
  import { m } from "../../i18n/index.js";
  import type {
    SessionTiming,
    CallTiming,
    TurnTiming,
  } from "../../api/types/timing.js";
  import { formatDuration } from "../../utils/duration.js";
  import { formatNumber } from "../../utils/format.js";
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
    <span class="sa-eh-label">{m.subagent_calls_label()}</span>
    <span class="sa-eh-meta">
      {m.subagent_calls_summary({
        count: timing.tool_call_count,
        countLabel: formatNumber(timing.tool_call_count),
        duration: formatDuration(timing.total_duration_ms),
        running: timing.running ? "true" : "false",
      })}
    </span>
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
  /* Adapted from the session-duration UX mockup, with the raw colors mapped
     to theme tokens (the mockup's rail red is exactly --cat-task). */
  .sa-expand {
    background: color-mix(in srgb, var(--cat-task) 4%, transparent);
    border-left: 2px solid var(--cat-task);
    margin: 2px 0 4px 26px;
    padding: 4px 4px 4px 0;
    border-radius: 0 3px 3px 0;
  }
  .sa-expand .sa-eh {
    font-family: ui-monospace, monospace;
    font-size: 9px;
    /* Mockup used a lighter tint of the task red; mix --cat-task toward
       the foreground so it stays readable on both themes. */
    color: color-mix(in srgb, var(--cat-task) 80%, var(--text-primary));
    text-transform: uppercase;
    letter-spacing: 0.5px;
    padding: 2px 8px 5px;
    display: flex;
    justify-content: space-between;
  }
  .sa-expand .sa-eh-meta {
    color: var(--text-muted);
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
  :global(.high-contrast) .sa-expand .sa-eh-meta {
    color: var(--text-secondary);
  }
</style>
