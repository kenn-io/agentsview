<script lang="ts">
  import type { Message } from "../../api/types.js";
  import type { CallTiming, TurnTiming } from "../../api/types/timing.js";
  import { formatTimestamp } from "../../utils/format.js";
  import { formatDuration } from "../../utils/duration.js";
  import { copyToClipboard } from "../../utils/clipboard.js";
  import { formatMessageForCopy } from "../../utils/copy-message.js";
  import {
    parseContent,
    enrichSegments,
  } from "../../utils/content-parser.js";
  import { sessionTiming } from "../../stores/sessionTiming.svelte.js";
  import { liveTick } from "../../stores/liveTick.svelte.js";
  import ToolBlock from "./ToolBlock.svelte";
  import ParallelGroup from "./ParallelGroup.svelte";
  import { displayToolName } from "../../utils/toolDisplay.js";

  interface Props {
    messages: Message[];
    timestamp: string;
    highlightQuery?: string;
    isCurrentHighlight?: boolean;
  }

  let {
    messages,
    timestamp,
    highlightQuery = "",
    isCurrentHighlight = false,
  }: Props = $props();

  let copied = $state(false);

  /** Effective tool-call count for one message: structured
   *  `tool_calls` when present, falling back to parsed tool
   *  segments so legacy transcripts (e.g. `[Bash]...` markers
   *  with no structured calls) still match the rendering path. */
  function messageToolCount(m: Message): number {
    const structured = m.tool_calls?.length ?? 0;
    if (structured > 0) return structured;
    return enrichSegments(
      parseContent(m.content, m.has_tool_use, m.id, m.content_length),
      m.tool_calls,
    ).filter((s) => s.type === "tool").length;
  }

  let totalCalls = $derived(
    messages.reduce((n, m) => n + messageToolCount(m), 0),
  );

  let label = $derived(
    totalCalls === 1 ? "1 tool call" : `${totalCalls} tool calls`,
  );

  /** Index turn timings by message id for O(1) lookup. */
  let turnByMessage = $derived.by(() => {
    const m = new Map<number, TurnTiming>();
    for (const t of sessionTiming.timing?.turns ?? []) {
      m.set(t.message_id, t);
    }
    return m;
  });

  /** Index call timings by tool_use_id for O(1) lookup. */
  let callByToolUseID = $derived.by(() => {
    const m = new Map<string, CallTiming>();
    for (const t of sessionTiming.timing?.turns ?? []) {
      for (const c of t.calls) m.set(c.tool_use_id, c);
    }
    return m;
  });

  /** Resolve the duration badge for a solo (non-grouped) tool call.
   *  Sub-agent calls show their exact duration; non-sub-agent solo
   *  calls inherit the turn's wall-clock duration. Running turns
   *  synthesize a live `running …+` label from the turn's
   *  `started_at`, ticked once per second by `liveTick`. */
  function soloDurationLabel(
    ct: CallTiming | undefined,
    turn: TurnTiming | undefined,
    msg: Message,
  ): string | undefined {
    if (ct?.subagent_session_id && ct.duration_ms != null) {
      return formatDuration(ct.duration_ms);
    }
    if (turn?.duration_ms != null) {
      return formatDuration(turn.duration_ms);
    }
    if (sessionTiming.timing?.running && turn != null) {
      const startSrc = turn.started_at ?? msg.timestamp;
      const startMs = new Date(startSrc).getTime();
      const elapsed = Number.isNaN(startMs)
        ? 0
        : Math.max(0, liveTick.now - startMs);
      return `running ${formatDuration(elapsed)}+`;
    }
    return undefined;
  }

  /** A turn is running iff the session is active AND its
   *  duration isn't yet known. */
  function isRunningTurn(msg: Message): boolean {
    if (!sessionTiming.timing?.running) return false;
    const turn = turnByMessage.get(msg.id);
    return turn != null && turn.duration_ms == null;
  }

  let copyTimer: ReturnType<typeof setTimeout>;

  async function handleCopy() {
    const combined = messages.map((m) => formatMessageForCopy(m)).join("\n\n");
    const ok = await copyToClipboard(combined);
    if (ok) {
      clearTimeout(copyTimer);
      copied = true;
      copyTimer = setTimeout(() => { copied = false; }, 1500);
    }
  }
</script>

<div class="tool-group">
  <div class="tool-group-header">
    <span class="gear-icon">
      <svg width="12" height="12" viewBox="0 0 16 16"
        fill="var(--accent-amber)">
        <path d="M8 4.754a3.246 3.246 0 100 6.492
          3.246 3.246 0 000-6.492zM5.754 8a2.246
          2.246 0 114.492 0 2.246 2.246 0
          01-4.492 0z"/>
        <path d="M9.796 1.343c-.527-1.79-3.065-1.79-3.592
          0l-.094.319a.873.873 0
          01-1.255.52l-.292-.16c-1.64-.892-3.433.902-2.54
          2.541l.159.292a.873.873 0
          01-.52 1.255l-.319.094c-1.79.527-1.79 3.065
          0 3.592l.319.094a.873.873 0
          01.52 1.255l-.16.292c-.892 1.64.901 3.434
          2.541 2.54l.292-.159a.873.873 0
          011.255.52l.094.319c.527 1.79 3.065 1.79
          3.592 0l.094-.319a.873.873 0
          011.255-.52l.292.16c1.64.893 3.434-.902
          2.54-2.541l-.159-.292a.873.873 0
          01.52-1.255l.319-.094c1.79-.527 1.79-3.065
          0-3.592l-.319-.094a.873.873 0
          01-.52-1.255l.16-.292c.893-1.64-.902-3.433-2.541-2.54l-.292.159a.873.873
          0 01-1.255-.52l-.094-.319zm-2.633.283a.909.909
          0 011.674 0l.094.319a1.873 1.873 0
          002.693 1.115l.291-.16a.909.909 0
          011.18 1.18l-.159.292a1.873 1.873 0
          001.116 2.692l.318.094a.909.909 0
          010 1.674l-.319.094a1.873 1.873 0
          00-1.115 2.693l.16.291a.909.909 0
          01-1.18 1.18l-.292-.159a1.873 1.873 0
          00-2.692 1.116l-.094.318a.909.909 0
          01-1.674 0l-.094-.319a1.873 1.873 0
          00-2.693-1.115l-.291.16a.909.909 0
          01-1.18-1.18l.159-.292a1.873 1.873 0
          00-1.116-2.692l-.318-.094a.909.909 0
          010-1.674l.319-.094a1.873 1.873 0
          001.115-2.693l-.16-.291a.909.909 0
          011.18-1.18l.292.159a1.873 1.873 0
          002.692-1.116l.094-.318z"/>
      </svg>
    </span>
    <span class="group-label">{label}</span>
    <button
      type="button"
      class="copy-btn"
      title={copied ? "Copied!" : "Copy tool calls"}
      onclick={handleCopy}
    >
      {#if copied}
        <svg width="14" height="14" viewBox="0 0 16 16" fill="currentColor">
          <path d="M13.78 4.22a.75.75 0 010 1.06l-7.25 7.25a.75.75 0 01-1.06 0L2.22 9.28a.75.75 0 011.06-1.06L6 10.94l6.72-6.72a.75.75 0 011.06 0z"/>
        </svg>
      {:else}
        <svg width="14" height="14" viewBox="0 0 16 16" fill="currentColor">
          <path d="M0 6.75C0 5.784.784 5 1.75 5h1.5a.75.75 0 010 1.5h-1.5a.25.25 0 00-.25.25v7.5c0 .138.112.25.25.25h7.5a.25.25 0 00.25-.25v-1.5a.75.75 0 011.5 0v1.5A1.75 1.75 0 019.25 16h-7.5A1.75 1.75 0 010 14.25v-7.5z"/>
          <path d="M5 1.75C5 .784 5.784 0 6.75 0h7.5C15.216 0 16 .784 16 1.75v7.5A1.75 1.75 0 0114.25 11h-7.5A1.75 1.75 0 015 9.25v-7.5zm1.75-.25a.25.25 0 00-.25.25v7.5c0 .138.112.25.25.25h7.5a.25.25 0 00.25-.25v-7.5a.25.25 0 00-.25-.25h-7.5z"/>
        </svg>
      {/if}
    </button>
    <span class="group-timestamp">
      {formatTimestamp(timestamp)}
    </span>
  </div>

  <div class="tool-group-body">
    {#each messages as message (message.id)}
      {@const calls = message.tool_calls ?? []}
      {@const turn = turnByMessage.get(message.id)}
      {#if calls.length === 1}
        {@const soloCall = calls[0]!}
        <ToolBlock
          toolCall={soloCall}
          content=""
          label={displayToolName(soloCall)}
          durationLabel={soloDurationLabel(
            callByToolUseID.get(soloCall.tool_use_id ?? ""),
            turn,
            message,
          )}
          isRunning={isRunningTurn(message)}
          {highlightQuery}
          {isCurrentHighlight}
        />
      {:else if calls.length >= 2}
        <ParallelGroup
          toolCalls={calls}
          callTimingByID={callByToolUseID}
          turnDurationMs={turn?.duration_ms ?? null}
          isRunning={isRunningTurn(message)}
          {highlightQuery}
          {isCurrentHighlight}
        />
      {:else}
        <!-- Fallback for messages with `has_tool_use` but no
             structured tool_calls — parse the content for tool
             markers (legacy/synthetic transcripts). -->
        {#each enrichSegments(parseContent(message.content, message.has_tool_use, message.id, message.content_length), message.tool_calls).filter((s) => s.type === "tool") as seg, segIdx (`${message.id}-${segIdx}`)}
          <ToolBlock
            content={seg.content}
            label={seg.label}
            toolCall={seg.toolCall}
            {highlightQuery}
            {isCurrentHighlight}
          />
        {/each}
      {/if}
    {/each}
  </div>
</div>

<style>
  .tool-group {
    border-left: 3px solid var(--accent-amber);
    background: var(--tool-bg);
    border-radius: 0 var(--radius-md) var(--radius-md) 0;
    padding: 8px 12px;
  }

  .tool-group-header {
    display: flex;
    align-items: center;
    gap: 8px;
    margin-bottom: 6px;
  }

  .gear-icon {
    display: flex;
    align-items: center;
    flex-shrink: 0;
  }

  .group-label {
    font-size: 12px;
    font-weight: 600;
    color: var(--accent-amber);
  }

  .group-timestamp {
    font-size: 12px;
    color: var(--text-muted);
    margin-left: auto;
  }

  .copy-btn {
    display: flex;
    align-items: center;
    justify-content: center;
    width: 26px;
    height: 26px;
    border: none;
    border-radius: var(--radius-sm, 4px);
    background: transparent;
    color: var(--text-muted);
    cursor: pointer;
    opacity: 0;
    transition: opacity 0.15s, background 0.15s, color 0.15s;
    flex-shrink: 0;
  }

  .tool-group:hover .copy-btn,
  .copy-btn:focus-visible {
    opacity: 1;
  }

  @media (hover: none) {
    .copy-btn {
      opacity: 1;
    }
  }

  .copy-btn:hover {
    background: var(--bg-surface-hover);
    color: var(--text-secondary);
  }

  .copy-btn:active {
    transform: scale(0.92);
  }

  .tool-group-body {
    display: flex;
    flex-direction: column;
    gap: 2px;
  }

  .tool-group-body :global(.tool-block) {
    margin: 0;
    border-left: none;
    border-radius: 0;
  }
</style>
