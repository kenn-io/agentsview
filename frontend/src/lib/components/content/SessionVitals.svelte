<!-- ABOUTME: Session Vital Signs panel — replaces ActivityMinimap on the right column. -->
<script lang="ts">
  import { onDestroy } from "svelte";
  import { CopyButton } from "@kenn-io/kit-ui";
  import { sessionTiming } from "../../stores/sessionTiming.svelte.js";
  import { liveTick } from "../../stores/liveTick.svelte.js";
  import { fetchSessionTiming } from "../../api/timing.js";
  import { isAbortError } from "../../api/runtime.js";
  import { formatDuration } from "../../utils/duration.js";
  import { categoryToken } from "../../utils/categoryToken.js";
  import { displayToolName } from "../../utils/toolDisplay.js";
  import { ui } from "../../stores/ui.svelte.js";
  import { m } from "../../i18n/index.js";
  import { formatNumber } from "../../utils/format.js";
  import type {
    CallTiming,
    SessionTiming,
    TurnTiming,
  } from "../../api/types/timing.js";
  import ActivityLane from "./ActivityLane.svelte";
  import CallRow from "./CallRow.svelte";
  import CallGroup from "./CallGroup.svelte";
  import SubagentCalls from "./SubagentCalls.svelte";
  import { XIcon } from "../../icons.js";
  import { LatestRead } from "../../utils/latest-read.js";
  import type { Session } from "../../api/types/core.js";

  interface Props {
    sessionId: string;
    session: Session | undefined;
  }

  let { sessionId, session }: Props = $props();

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
  const subagentTimingReads = new Map<string, LatestRead>();
  let subagentReadSessionId: string | null = null;

  function clearPendingSubagent(sid: string) {
    const next = new Set(pendingSubagentIds);
    next.delete(sid);
    pendingSubagentIds = next;
  }

  function cancelSubagentRead(sid: string) {
    subagentTimingReads.get(sid)?.cancel();
    subagentTimingReads.delete(sid);
    clearPendingSubagent(sid);
  }

  function cancelAllSubagentReads() {
    for (const read of subagentTimingReads.values()) read.cancel();
    subagentTimingReads.clear();
  }

  $effect(() => {
    if (subagentReadSessionId === null) {
      subagentReadSessionId = sessionId;
      return;
    }
    if (sessionId === subagentReadSessionId) return;
    cancelAllSubagentReads();
    subagentReadSessionId = sessionId;
    expandedSubagentIds = new Set();
    subagentTimings = new Map();
    pendingSubagentIds = new Set();
  });

  onDestroy(cancelAllSubagentReads);

  async function toggleSubagent(call: CallTiming) {
    if (!call.subagent_session_id) return;
    const sid = call.subagent_session_id;
    if (pendingSubagentIds.has(sid)) {
      cancelSubagentRead(sid);
      return;
    }
    if (expandedSubagentIds.has(sid)) {
      const next = new Set(expandedSubagentIds);
      next.delete(sid);
      expandedSubagentIds = next;
      return;
    }
    if (!subagentTimings.has(sid)) {
      const ownerSessionId = sessionId;
      const read = new LatestRead();
      subagentTimingReads.set(sid, read);
      const signal = read.begin();
      const nextPending = new Set(pendingSubagentIds);
      nextPending.add(sid);
      pendingSubagentIds = nextPending;
      try {
        const t = await fetchSessionTiming(sid, signal);
        if (
          !t ||
          ownerSessionId !== sessionId ||
          subagentTimingReads.get(sid) !== read ||
          !read.isCurrent(signal)
        ) return;
        const m = new Map(subagentTimings);
        m.set(sid, t);
        subagentTimings = m;
      } catch (err) {
        if (signal.aborted || isAbortError(err)) return;
        console.error("failed to load sub-agent timing", err);
        return;
      } finally {
        if (
          subagentTimingReads.get(sid) === read &&
          read.finish(signal)
        ) {
          subagentTimingReads.delete(sid);
          clearPendingSubagent(sid);
        }
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

  // Bar width for one call, scaled against the longest call duration
  // in the supplied session's scope. The slowest call fills the bar;
  // everything else is relative to it, so call-vs-call comparisons are
  // legible even in long sessions where any single call is a tiny
  // fraction of total wall-clock. Parallel siblings (duration_ms ==
  // null) use the parent turn's duration both when computing the max
  // and when scaling each row, so a turn whose only signal lives at
  // the group level still contributes meaningfully.
  //
  // The max is memoized per SessionTiming reference: callBarPct runs
  // once per rendered row, and recomputing the max each time would
  // be O(n²) across the call list.
  const maxCallMsCache = new WeakMap<SessionTiming, number>();

  function maxCallMs(t: SessionTiming): number {
    const cached = maxCallMsCache.get(t);
    if (cached !== undefined) return cached;
    let max = 0;
    for (const turn of t.turns) {
      const turnFallback = turn.duration_ms ?? 0;
      for (const call of turn.calls) {
        const d = call.duration_ms ?? turnFallback;
        if (d > max) max = d;
      }
    }
    maxCallMsCache.set(t, max);
    return max;
  }

  function callBarPct(c: CallTiming, t: SessionTiming): number {
    const maxMs = maxCallMs(t);
    if (maxMs <= 0) return 0;
    let dur = c.duration_ms;
    if (dur == null) {
      const turn = t.turns.find((tt) => tt.calls.includes(c));
      dur = turn?.duration_ms ?? 0;
    }
    if (dur <= 0) return 0;
    const pct = (dur / maxMs) * 100;
    return Math.min(100, Math.max(pct, 4));
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
        : m.session_vitals_running();
    return `${turn.primary_category} · ${dur}`;
  }

  function scrollToTurn(turn: TurnTiming) {
    ui.scrollToOrdinal(turn.ordinal);
  }
</script>

<div class="vital">
  <header class="vital-titlebar">
    <div>
      <div class="vital-title">{m.session_vitals_title()}</div>
      <div class="vital-subtitle">{m.session_vitals_subtitle()}</div>
    </div>
    <button
      type="button"
      class="vital-close"
      title={m.session_vitals_close()}
      aria-label={m.session_vitals_close()}
      onclick={() => ui.closeVitals()}
    >
      <XIcon size="12" strokeWidth="2.4" aria-hidden="true" />
    </button>
  </header>

  {#if timing || session}
    <section class="v-section">
      <header class="v-h">
        <span>{m.session_vitals_session()}</span>
        {#if timing}
          <span class="v-meta" class:live={timing.running}>
            {#if timing.running}
              {m.session_vitals_running_duration({ duration: formatDuration(timing.total_duration_ms) })}
            {:else}
              {formatDuration(timing.total_duration_ms)}
            {/if}
          </span>
        {/if}
      </header>
      {#if session}
        <div class="session-context">
          <div class="context-row">
            <div class="context-text">
              <div class="context-label">
                {m.session_vitals_repository()}
              </div>
              <div class="context-value" title={session.project}>
                {session.project}
              </div>
            </div>
            <CopyButton
              text={session.project}
              revealOnHover
              ariaLabel={m.session_vitals_copy_repository()}
              copiedAriaLabel={m.session_vitals_repository_copied()}
              title={m.session_vitals_copy_repository()}
              copiedTitle={m.session_vitals_repository_copied()}
            />
          </div>
          <div class="context-row">
            <div class="context-text">
              <div class="context-label">
                {m.session_vitals_worktree()}
              </div>
              <div class="context-value" title={session.cwd || undefined}>
                {session.cwd || "—"}
              </div>
            </div>
            {#if session.cwd}
              <CopyButton
                text={session.cwd}
                revealOnHover
                ariaLabel={m.session_vitals_copy_worktree()}
                copiedAriaLabel={m.session_vitals_worktree_copied()}
                title={m.session_vitals_copy_worktree()}
                copiedTitle={m.session_vitals_worktree_copied()}
              />
            {/if}
          </div>
        </div>
      {/if}
      {#if timing}
        <div class="stat-grid">
          <div>
            <div class="lbl">{m.session_vitals_tool_calls()}</div>
            <div class="val">{timing.tool_call_count}</div>
          </div>
          <div>
            <div class="lbl">{m.session_vitals_tool_time()}</div>
            <div class="val" class:live={timing.running}>
              {formatDuration(timing.tool_duration_ms)}{timing.running ? "+" : ""}
            </div>
          </div>
          <div>
            <div class="lbl">{m.session_vitals_slowest_call()}</div>
            {#if timing.slowest_call}
              {@const slowest = timing.slowest_call}
              <button
                type="button"
                class="val slow val-link"
                title={m.session_vitals_jump_to_call()}
                onclick={() => scrollToCall(slowest)}
              >
                {displayToolName(slowest)} · {formatDuration(slowest.duration_ms ?? 0)}
              </button>
            {:else}
              <div class="val slow">—</div>
            {/if}
          </div>
          <div>
            <div class="lbl">{m.session_vitals_turns()}</div>
            <div class="val">{timing.turn_count}</div>
          </div>
          <div>
            <div class="lbl">{m.session_vitals_subagents()}</div>
            <div class="val">{timing.subagent_count}</div>
          </div>
        </div>
      {/if}
    </section>
  {/if}

  {#if timing}
    {#if timing.by_category.length > 0}
      <section class="v-section">
        <header class="v-h">
          <span>{m.session_vitals_time_spent()}</span>
          {#if categoryFilter}
            <button
              class="filter-chip"
              style="color: {categoryToken(categoryFilter)}; border-color: {categoryToken(categoryFilter)};"
              onclick={() => (categoryFilter = null)}
              aria-label={m.session_vitals_clear_category_filter()}
            >
              {categoryFilter}<span class="x">
                <XIcon size="10" strokeWidth="2.4" aria-hidden="true" />
              </span>
            </button>
          {:else}
            <span class="v-meta">{m.session_vitals_completed_turns_hint()}</span>
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
          <span>{m.session_vitals_timeline()}</span>
          <span class="v-meta">{m.session_vitals_click_marks_to_scroll()}</span>
        </header>

        <div class="lane-row">
          <span class="lane-label">{m.session_vitals_turns()}</span>
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
                aria-label={m.session_vitals_jump_to_turn({
                  category: t.primary_category,
                  time: t.started_at,
                })}
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
                  aria-label={m.session_vitals_jump_to_turn({
                    category: cat.category,
                    time: t.started_at,
                  })}
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
          <span>{m.session_vitals_calls()}</span>
          <span class="v-meta">
            {m.session_vitals_calls_summary({
              count: timing.tool_call_count,
              countLabel: formatNumber(timing.tool_call_count),
              runningCount: timing.running ? 1 : 0,
            })}
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
              ? m.session_vitals_now()
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

  .vital-titlebar {
    position: sticky;
    top: 0;
    z-index: 2;
    min-height: 42px;
    padding: 7px 10px 7px 14px;
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 12px;
    background: var(--bg-surface);
    border-bottom: 1px solid var(--border-default);
  }

  .vital-title {
    color: var(--text-primary);
    font-size: 12px;
    font-weight: 650;
    line-height: 1.2;
  }

  .vital-subtitle {
    color: var(--text-muted);
    font-size: 10px;
    line-height: 1.2;
    margin-top: 1px;
  }

  .vital-close {
    width: 26px;
    height: 26px;
    display: inline-flex;
    align-items: center;
    justify-content: center;
    flex-shrink: 0;
    border-radius: var(--radius-sm);
    color: var(--text-muted);
    transition: background 0.12s, color 0.12s;
  }

  .vital-close:hover {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }

  .vital-close:focus-visible {
    outline: 2px solid var(--accent-blue);
    outline-offset: 2px;
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

  .session-context {
    display: grid;
    gap: var(--space-2);
    margin-bottom: 12px;
    padding-bottom: 11px;
    border-bottom: 1px solid var(--border-muted);
    font-family: var(--font-mono);
  }

  .context-row {
    display: grid;
    grid-template-columns: minmax(0, 1fr) 26px;
    align-items: center;
    gap: 4px;
    min-width: 0;
  }

  .context-text {
    min-width: 0;
  }

  .context-row:hover :global(.kit-copy-btn--reveal),
  .context-row:focus-within :global(.kit-copy-btn--reveal) {
    opacity: 1;
  }

  .context-label {
    color: var(--text-muted);
    font-size: 9px;
    margin-bottom: 2px;
    text-transform: uppercase;
    letter-spacing: 0.4px;
  }

  .context-value {
    overflow: hidden;
    color: var(--text-primary);
    font-size: 10px;
    line-height: 1.35;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  /* Stat grid */
  .stat-grid {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: var(--space-4);
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
    background: color-mix(in srgb, var(--text-primary) 3%, transparent);
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
    background: color-mix(in srgb, var(--text-primary) 4%, transparent);
    border: 1px solid color-mix(in srgb, var(--text-primary) 12%, transparent);
    padding: 2px 6px;
    border-radius: var(--radius-sm);
    font-family: var(--font-mono);
    font-size: 9px;
    cursor: pointer;
    color: var(--text-primary);
  }
  .filter-chip:hover {
    background: color-mix(in srgb, var(--text-primary) 8%, transparent);
  }
  .filter-chip .x {
    display: inline-flex;
    align-items: center;
    margin-left: 2px;
    flex-shrink: 0;
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
  /* Adapted from the session-duration UX mockup, with the raw colors mapped to
     theme tokens. */
  .scale-axis {
    display: flex;
    justify-content: space-between;
    font-family: ui-monospace, monospace;
    font-size: 9px;
    color: var(--text-muted);
    padding: 0 4px 5px;
    border-bottom: 1px solid var(--border-muted);
    margin-bottom: 8px;
  }
  .scale-axis .now {
    color: var(--running-fg);
    font-weight: 500;
  }
  .calls {
    display: flex;
    flex-direction: column;
    gap: 1px;
  }
</style>
