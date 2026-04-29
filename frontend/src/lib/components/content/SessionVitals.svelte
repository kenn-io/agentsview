<!-- ABOUTME: Session Vital Signs panel — replaces ActivityMinimap on the right column. -->
<script lang="ts">
  import { sessionTiming } from "../../stores/sessionTiming.svelte.js";
  import { liveTick } from "../../stores/liveTick.svelte.js";
  import { fetchSessionTiming } from "../../api/timing.js";
  import { formatDuration } from "../../utils/duration.js";
  import { categoryToken } from "../../utils/categoryToken.js";
  import { displayToolName } from "../../utils/toolDisplay.js";
  import { ui } from "../../stores/ui.svelte.js";
  import type {
    CallTiming,
    SessionTiming,
    TurnTiming,
  } from "../../api/types/timing.js";
  import ActivityLane from "./ActivityLane.svelte";
  import CallRow from "./CallRow.svelte";
  import CallGroup from "./CallGroup.svelte";
  import SubagentCalls from "./SubagentCalls.svelte";

  interface Props {
    sessionId: string;
  }

  let { sessionId }: Props = $props();

  $effect(() => {
    void sessionTiming.load(sessionId);
  });

  let timing = $derived(sessionTiming.timing);

  let categoryFilter = $state<string | null>(null);

  function toggleCategory(cat: string) {
    categoryFilter = categoryFilter === cat ? null : cat;
  }

  // Sub-agent inline expansion. Each entry maps a child session ID
  // to the timing snapshot we fetched for it. Distinct from the
  // singleton sessionTiming store, which is reserved for the parent
  // session this panel is mounted for.
  let expandedSubagentIds = $state(new Set<string>());
  let subagentTimings = $state(new Map<string, SessionTiming>());
  let pendingSubagentIds = $state(new Set<string>());

  async function toggleSubagent(call: CallTiming) {
    if (!call.subagent_session_id) return;
    const sid = call.subagent_session_id;
    if (pendingSubagentIds.has(sid)) return;
    if (expandedSubagentIds.has(sid)) {
      const next = new Set(expandedSubagentIds);
      next.delete(sid);
      expandedSubagentIds = next;
      return;
    }
    if (!subagentTimings.has(sid)) {
      const nextPending = new Set(pendingSubagentIds);
      nextPending.add(sid);
      pendingSubagentIds = nextPending;
      try {
        const t = await fetchSessionTiming(sid);
        if (!t) return;
        const m = new Map(subagentTimings);
        m.set(sid, t);
        subagentTimings = m;
      } catch (err) {
        console.error("failed to load sub-agent timing", err);
        return;
      } finally {
        const cleanup = new Set(pendingSubagentIds);
        cleanup.delete(sid);
        pendingSubagentIds = cleanup;
      }
    }
    const next = new Set(expandedSubagentIds);
    next.add(sid);
    expandedSubagentIds = next;
  }

  // Slow threshold: top 10% of measurable call durations. With
  // fewer than 10 measurable calls, mark only the longest. Spec
  // section: "Slow threshold".
  let slowCallThresholdMs = $derived.by(() => {
    if (!timing) return Infinity;
    const durations = timing.turns
      .flatMap((t) => t.calls)
      .map((c) => c.duration_ms)
      .filter((d): d is number => d != null);
    if (durations.length === 0) return Infinity;
    if (durations.length < 10) return Math.max(...durations);
    durations.sort((a, b) => b - a);
    const idx = Math.max(0, Math.ceil(durations.length * 0.1) - 1);
    return durations[idx] ?? Infinity;
  });

  function isSlowCall(c: CallTiming): boolean {
    return (
      c.duration_ms != null && c.duration_ms >= slowCallThresholdMs
    );
  }

  function isLastTurn(turn: TurnTiming): boolean {
    if (!timing || timing.turns.length === 0) return false;
    return (
      turn.message_id ===
      timing.turns[timing.turns.length - 1]!.message_id
    );
  }

  /** Wall-clock elapsed for the running tail turn, recomputed on
   *  each `liveTick.now`. Returns 0 when no turn is running. */
  function liveElapsedFor(turn: TurnTiming): number {
    const start = new Date(turn.started_at).getTime();
    if (Number.isNaN(start)) return 0;
    return Math.max(0, liveTick.now - start);
  }

  function turnForCall(call: CallTiming): TurnTiming | undefined {
    if (!timing) return undefined;
    return timing.turns.find((t) =>
      t.calls.some((c) => c.tool_use_id === call.tool_use_id),
    );
  }

  function scrollToCall(call: CallTiming) {
    const turn = turnForCall(call);
    if (turn) ui.scrollToOrdinal(turn.ordinal);
  }

  // Bar width for one call, scaled against the supplied session's
  // total wall-clock duration. Solo calls render at their own
  // duration; parallel siblings (duration_ms == null) fall back to
  // the parent turn's duration so they fill the same fraction as
  // the group header.
  function callBarPct(c: CallTiming, t: SessionTiming): number {
    if (t.total_duration_ms <= 0) return 0;
    let dur = c.duration_ms;
    if (dur == null) {
      const turn = t.turns.find((tt) => tt.calls.includes(c));
      dur = turn?.duration_ms ?? 0;
    }
    return Math.min(100, (dur / t.total_duration_ms) * 100);
  }

  function turnHeaderBarPct(
    turn: TurnTiming,
    t: SessionTiming,
  ): number {
    if (turn.duration_ms == null || t.total_duration_ms <= 0) {
      return 0;
    }
    return Math.min(
      100,
      (turn.duration_ms / t.total_duration_ms) * 100,
    );
  }

  // Timeline-lane geometry. Both endpoints are in epoch-ms; the duration
  // window includes any in-flight running time so live marks reach the
  // right edge of the track.
  let sessionStartMs = $derived.by(() => {
    if (!timing || timing.turns.length === 0) return 0;
    return new Date(timing.turns[0]!.started_at).getTime();
  });

  let sessionEndMs = $derived.by(() => {
    if (!timing) return sessionStartMs;
    return sessionStartMs + Math.max(timing.total_duration_ms, 1);
  });

  function turnLeftPct(turn: TurnTiming): number {
    const span = Math.max(sessionEndMs - sessionStartMs, 1);
    const t = new Date(turn.started_at).getTime();
    return ((t - sessionStartMs) / span) * 100;
  }

  function turnWidthPct(turn: TurnTiming): number {
    const span = Math.max(sessionEndMs - sessionStartMs, 1);
    if (turn.duration_ms == null) {
      // Running turn: stretch to the right edge so it reads as in-flight.
      const t = new Date(turn.started_at).getTime();
      return Math.max(0.5, ((sessionEndMs - t) / span) * 100);
    }
    return Math.max(0.3, (turn.duration_ms / span) * 100);
  }

  function turnTitle(turn: TurnTiming): string {
    const dur =
      turn.duration_ms != null
        ? formatDuration(turn.duration_ms)
        : "running";
    return `${turn.primary_category} · ${dur}`;
  }

  function scrollToTurn(turn: TurnTiming) {
    ui.scrollToOrdinal(turn.ordinal);
  }
</script>

<div class="vital">
  {#if timing}
    <section class="v-section">
      <header class="v-h">
        <span>Session</span>
        <span class="v-meta" class:live={timing.running}>
          {#if timing.running}
            running {formatDuration(timing.total_duration_ms)}+
          {:else}
            {formatDuration(timing.total_duration_ms)}
          {/if}
        </span>
      </header>
      <div class="stat-grid">
        <div>
          <div class="lbl">tool calls</div>
          <div class="val">{timing.tool_call_count}</div>
        </div>
        <div>
          <div class="lbl">tool time</div>
          <div class="val" class:live={timing.running}>
            {formatDuration(timing.tool_duration_ms)}{timing.running ? "+" : ""}
          </div>
        </div>
        <div>
          <div class="lbl">slowest call</div>
          {#if timing.slowest_call}
            {@const slowest = timing.slowest_call}
            <button
              type="button"
              class="val slow val-link"
              title="Jump to call"
              onclick={() => scrollToCall(slowest)}
            >
              {displayToolName(slowest)} · {formatDuration(slowest.duration_ms ?? 0)}
            </button>
          {:else}
            <div class="val slow">—</div>
          {/if}
        </div>
        <div>
          <div class="lbl">turns</div>
          <div class="val">{timing.turn_count}</div>
        </div>
        <div>
          <div class="lbl">sub-agents</div>
          <div class="val">{timing.subagent_count}</div>
        </div>
      </div>
    </section>

    {#if timing.by_category.length > 0}
      <section class="v-section">
        <header class="v-h">
          <span>Time spent</span>
          {#if categoryFilter}
            <button
              class="filter-chip"
              style="color: {categoryToken(categoryFilter)}; border-color: {categoryToken(categoryFilter)};"
              onclick={() => (categoryFilter = null)}
              aria-label="clear category filter"
            >
              {categoryFilter}<span class="x">×</span>
            </button>
          {:else}
            <span class="v-meta">completed turns · click to highlight</span>
          {/if}
        </header>
        {#each timing.by_category as cat (cat.category)}
          {@const isActive = categoryFilter === cat.category}
          {@const isDimmed = categoryFilter !== null && !isActive}
          <button
            class="agg-row"
            class:active={isActive}
            class:dimmed={isDimmed}
            style={isActive ? `--ring: ${categoryToken(cat.category)};` : ""}
            onclick={() => toggleCategory(cat.category)}
            type="button"
          >
            <span class="agg-name">{cat.category}</span>
            <span class="agg-bar">
              <span
                class="agg-fill"
                style="width: {(cat.duration_ms / Math.max(timing.tool_duration_ms, 1)) * 100}%; background: {categoryToken(cat.category)};"
              ></span>
            </span>
            <span class="agg-val">{formatDuration(cat.duration_ms)}</span>
          </button>
        {/each}
      </section>
    {/if}

    {#if timing.turns.length > 0}
      <section class="v-section">
        <header class="v-h">
          <span>Timeline</span>
          <span class="v-meta">click marks to scroll</span>
        </header>

        <div class="lane-row">
          <span class="lane-label">turns</span>
          <span class="lane-track">
            {#each timing.turns as t (t.message_id)}
              {@const isLive = t.duration_ms == null}
              <button
                class="lane-mark"
                class:live={isLive}
                class:dimmed={categoryFilter !== null && t.primary_category !== categoryFilter}
                style="left: {turnLeftPct(t)}%; width: {turnWidthPct(t)}%; {isLive
                  ? ''
                  : `background: ${categoryToken(t.primary_category)};`}"
                title={turnTitle(t)}
                onclick={() => scrollToTurn(t)}
                type="button"
                aria-label="Jump to {t.primary_category} turn at {t.started_at}"
              ></button>
            {/each}
          </span>
        </div>

        <div class="lane-spacer"></div>

        {#each timing.by_category as cat (cat.category)}
          <div
            class="lane-row"
            class:dimmed={categoryFilter !== null && cat.category !== categoryFilter}
          >
            <span class="lane-label">{cat.category}</span>
            <span class="lane-track">
              {#each timing.turns.filter((tt) => tt.primary_category === cat.category) as t (t.message_id)}
                {@const isLive = t.duration_ms == null}
                <button
                  class="lane-mark"
                  class:live={isLive}
                  style="left: {turnLeftPct(t)}%; width: {turnWidthPct(t)}%; {isLive
                    ? ''
                    : `background: ${categoryToken(cat.category)};`}"
                  title={turnTitle(t)}
                  onclick={() => scrollToTurn(t)}
                  type="button"
                  aria-label="Jump to {cat.category} turn at {t.started_at}"
                ></button>
              {/each}
            </span>
          </div>
        {/each}

        <div class="lane-spacer"></div>

        <ActivityLane {sessionId} />

        <div class="legend">
          {#each timing.by_category as cat (cat.category)}
            <span>
              <span
                class="legend-dot"
                style="background: {categoryToken(cat.category)};"
              ></span>
              {cat.category}
            </span>
          {/each}
        </div>
      </section>
    {/if}

    {#if timing.turns.length > 0}
      <section class="v-section">
        <header class="v-h">
          <span>Calls</span>
          <span class="v-meta">
            {timing.tool_call_count} call{timing.tool_call_count === 1
              ? ""
              : "s"}{timing.running ? " · 1 running" : ""}
          </span>
        </header>
        <div class="scale-axis">
          <span>0</span>
          <span>{formatDuration(timing.total_duration_ms / 4)}</span>
          <span>{formatDuration(timing.total_duration_ms / 2)}</span>
          <span
            >{formatDuration(
              (3 * timing.total_duration_ms) / 4,
            )}</span
          >
          <span class:now={timing.running}
            >{timing.running
              ? "now"
              : formatDuration(timing.total_duration_ms)}</span
          >
        </div>
        <div class="calls">
          {#each timing.turns as turn (turn.message_id)}
            {@const isLive =
              turn.duration_ms == null &&
              isLastTurn(turn) &&
              !!timing.running}
            {@const liveElapsed = isLive ? liveElapsedFor(turn) : undefined}
            {#if turn.calls.length === 1}
              {@const call = turn.calls[0]!}
              <CallRow
                {call}
                barWidthPct={callBarPct(call, timing)}
                isSlow={isSlowCall(call)}
                {isLive}
                liveDurationMs={liveElapsed}
                dimmed={categoryFilter !== null &&
                  call.category !== categoryFilter}
                isSubagentExpanded={!!call.subagent_session_id &&
                  expandedSubagentIds.has(call.subagent_session_id)}
                onClick={() => ui.scrollToOrdinal(turn.ordinal)}
                onChevronClick={() => {
                  void toggleSubagent(call);
                }}
              />
              {#if call.subagent_session_id && expandedSubagentIds.has(call.subagent_session_id)}
                {@const subT = subagentTimings.get(
                  call.subagent_session_id,
                )}
                {#if subT}
                  <SubagentCalls
                    timing={subT}
                    barScalePct={(c) => callBarPct(c, subT)}
                    {categoryFilter}
                  />
                {/if}
              {/if}
            {:else}
              <CallGroup
                calls={turn.calls}
                groupDurationMs={turn.duration_ms}
                barScalePct={(c) => callBarPct(c, timing)}
                headerBarPct={turnHeaderBarPct(turn, timing)}
                {isLive}
                liveDurationMs={liveElapsed}
                isSlow={isSlowCall}
                dimmed={categoryFilter !== null &&
                  turn.primary_category !== categoryFilter}
                onCallClick={() => ui.scrollToOrdinal(turn.ordinal)}
                onSubagentExpand={(c) => {
                  void toggleSubagent(c);
                }}
                {expandedSubagentIds}
              />
              {#each turn.calls.filter((c) => !!c.subagent_session_id && expandedSubagentIds.has(c.subagent_session_id)) as expandedCall (expandedCall.tool_use_id)}
                {@const subT = subagentTimings.get(
                  expandedCall.subagent_session_id!,
                )}
                {#if subT}
                  <SubagentCalls
                    timing={subT}
                    barScalePct={(c) => callBarPct(c, subT)}
                    {categoryFilter}
                  />
                {/if}
              {/each}
            {/if}
          {/each}
        </div>
      </section>
    {/if}
  {:else if sessionTiming.error}
    <p class="v-error">{sessionTiming.error}</p>
  {/if}
</div>

<style>
  /* Outer panel */
  .vital {
    flex: 1;
    overflow-y: auto;
    min-height: 0;
  }

  .v-section {
    padding: 12px 14px 14px;
    border-bottom: 1px solid var(--border-muted);
  }
  .v-section:last-child { border-bottom: 0; }

  .v-h {
    color: var(--text-muted);
    font-size: 9px;
    text-transform: uppercase;
    letter-spacing: 0.6px;
    margin-bottom: 9px;
    font-weight: 500;
    display: flex;
    justify-content: space-between;
    align-items: center;
  }

  .v-meta {
    color: var(--text-muted);
    font-size: 9px;
    font-family: var(--font-mono);
    text-transform: none;
    letter-spacing: 0;
  }
  .v-meta.live {
    color: var(--running-fg);
    animation: duration-pulse 1.6s ease-in-out infinite;
  }

  /* Stat grid */
  .stat-grid {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: 10px;
    font-family: var(--font-mono);
    font-size: 11px;
  }
  .stat-grid .lbl {
    color: var(--text-muted);
    font-size: 9px;
    margin-bottom: 2px;
    text-transform: uppercase;
    letter-spacing: 0.4px;
  }
  .stat-grid .val { color: var(--text-primary); }
  .stat-grid .val.slow { color: var(--slow-fg); }
  .stat-grid .val.live {
    color: var(--running-fg);
    animation: duration-pulse 1.6s ease-in-out infinite;
  }
  .stat-grid .val-link {
    background: transparent;
    border: 0;
    padding: 0;
    font: inherit;
    text-align: left;
    cursor: pointer;
  }
  .stat-grid .val-link:hover {
    text-decoration: underline;
  }
  .stat-grid .val-link:focus-visible {
    outline: 1px solid currentColor;
    outline-offset: 2px;
    border-radius: 2px;
  }

  .v-error {
    color: var(--slow-fg);
    font-size: 11px;
    padding: 12px 14px;
  }

  /* Time spent — aggregate rows */
  .agg-row {
    display: grid;
    grid-template-columns: 48px 1fr 56px;
    align-items: center;
    gap: 8px;
    font-size: 10px;
    margin-bottom: 5px;
    cursor: pointer;
    padding: 2px 4px;
    border-radius: var(--radius-sm);
    background: transparent;
    border: 1px solid transparent;
    width: 100%;
    text-align: left;
    font-family: var(--font-mono);
    color: inherit;
    transition: background 0.12s, opacity 0.18s, border-color 0.12s;
  }
  .agg-row:hover {
    background: rgba(255, 255, 255, 0.03);
  }
  .agg-row.active {
    background: color-mix(in srgb, var(--ring, transparent) 10%, transparent);
    border-color: color-mix(in srgb, var(--ring, transparent) 30%, transparent);
  }
  .agg-row.dimmed {
    opacity: 0.40;
  }

  .agg-name {
    font-family: var(--font-mono);
    color: var(--text-primary);
    font-size: 9px;
  }
  .agg-bar {
    height: 7px;
    background: var(--bg-inset, rgba(255, 255, 255, 0.04));
    border-radius: 1px;
    position: relative;
    overflow: hidden;
  }
  .agg-fill {
    display: block;
    height: 100%;
    border-radius: 1px;
  }
  .agg-val {
    font-family: var(--font-mono);
    font-size: 10px;
    color: var(--text-muted);
    text-align: right;
  }

  .filter-chip {
    display: inline-flex;
    align-items: center;
    gap: 4px;
    background: rgba(255, 255, 255, 0.04);
    border: 1px solid rgba(255, 255, 255, 0.12);
    padding: 2px 6px;
    border-radius: var(--radius-sm);
    font-family: var(--font-mono);
    font-size: 9px;
    cursor: pointer;
    color: var(--text-primary);
  }
  .filter-chip:hover {
    background: rgba(255, 255, 255, 0.08);
  }
  .filter-chip .x {
    margin-left: 2px;
    font-size: 11px;
    line-height: 1;
  }

  /* Timeline lanes ----------------------------------------------------- */
  .lane-row {
    display: grid;
    grid-template-columns: 48px 1fr;
    align-items: center;
    gap: 8px;
    margin-bottom: 4px;
    transition: opacity 0.18s;
  }
  .lane-row.dimmed {
    opacity: 0.40;
  }
  .lane-label {
    font-family: var(--font-mono);
    font-size: 10px;
    color: var(--text-muted);
  }
  .lane-track {
    height: 12px;
    background: var(--bg-inset, rgba(255, 255, 255, 0.04));
    border-radius: 2px;
    position: relative;
  }
  /* `.lane-track.activity` lives in ActivityLane.svelte (Svelte scopes
     styles per component, so it owns its own rule). */
  .lane-mark {
    position: absolute;
    top: 1px;
    bottom: 1px;
    border-radius: 1px;
    cursor: pointer;
    border: 0;
    padding: 0;
    transition: opacity 0.18s, filter 0.12s;
  }
  .lane-mark:hover {
    filter: brightness(1.3);
  }
  .lane-mark.dimmed {
    opacity: 0.40;
  }
  .lane-mark.live {
    background: linear-gradient(
      90deg,
      var(--running-fg, #6ad0a8),
      color-mix(in srgb, var(--running-fg, #6ad0a8) 65%, #000)
    );
    animation: duration-pulse 1.6s ease-in-out infinite;
  }
  .lane-spacer {
    height: 8px;
  }

  .legend {
    display: flex;
    flex-wrap: wrap;
    gap: 8px 12px;
    margin-top: 10px;
    font-size: 9px;
    color: var(--text-muted);
    font-family: var(--font-mono);
  }
  .legend-dot {
    display: inline-block;
    width: 8px;
    height: 8px;
    border-radius: 1px;
    margin-right: 4px;
    vertical-align: -1px;
  }

  /* Calls section --------------------------------------------------- */
  /* Copied verbatim from
     docs/superpowers/specs/2026-04-26-session-duration-ux-mockup.html
     (.scale-axis and .calls rules, lines 498–516). */
  .scale-axis {
    display: flex;
    justify-content: space-between;
    font-family: ui-monospace, monospace;
    font-size: 9px;
    color: #666;
    padding: 0 4px 5px;
    border-bottom: 1px solid #232323;
    margin-bottom: 8px;
  }
  .scale-axis .now {
    color: #6ad0a8;
    font-weight: 500;
  }
  .calls {
    display: flex;
    flex-direction: column;
    gap: 1px;
  }
</style>
