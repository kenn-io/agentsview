<script lang="ts">
  import type { Message } from "../../api/types.js";
  import type { CallTiming, TurnTiming } from "../../api/types/timing.js";
  import {
    parseContent,
    enrichSegments,
  } from "../../utils/content-parser.js";
  import {
    formatTimestamp,
    formatTokenUsage,
  } from "../../utils/format.js";
  import { formatDuration } from "../../utils/duration.js";
  import { copyToClipboard } from "../../utils/clipboard.js";
  import { formatMessageForCopy } from "../../utils/copy-message.js";
  import { messages as messagesStore } from "../../stores/messages.svelte.js";
  import { sessionTiming } from "../../stores/sessionTiming.svelte.js";
  import { liveTick } from "../../stores/liveTick.svelte.js";
  import ThinkingBlock from "./ThinkingBlock.svelte";
  import ToolBlock from "./ToolBlock.svelte";
  import ParallelGroup from "./ParallelGroup.svelte";
  import CodeBlock from "./CodeBlock.svelte";
  import SkillBlock from "./SkillBlock.svelte";
  import { ui } from "../../stores/ui.svelte.js";
  import { pins } from "../../stores/pins.svelte.js";
  import { sessions } from "../../stores/sessions.svelte.js";
  import { applyHighlight } from "../../utils/highlight.js";
  import { renderMarkdown } from "../../utils/markdown.js";
  import { displayToolName } from "../../utils/toolDisplay.js";
  import type { Session } from "../../api/types.js";

  interface Props {
    message: Message;
    isSubagentContext?: boolean;
    highlightQuery?: string;
    isCurrentHighlight?: boolean;
  }

  let { message, isSubagentContext = false, highlightQuery = "", isCurrentHighlight = false }: Props = $props();

  let copied = $state(false);

  let segments = $derived(
    enrichSegments(
      parseContent(
        message.content,
        message.has_tool_use,
        message.id,
        message.content_length,
      ),
      message.tool_calls,
    ),
  );

  let isUser = $derived(message.role === "user");

  let mainModel = $derived(
    !isSubagentContext &&
    messagesStore.sessionId === message.session_id
      ? messagesStore.mainModel
      : "",
  );

  let offMainModel = $derived.by((): string => {
    if (isUser || !message.model || !mainModel) return "";
    return message.model !== mainModel ? message.model : "";
  });

  let hasContextTokens = $derived(
    message.has_context_tokens ?? message.context_tokens > 0,
  );

  let hasOutputTokens = $derived(
    message.has_output_tokens ?? message.output_tokens > 0,
  );

  let tokenSummary = $derived(
    formatTokenUsage(
      message.context_tokens,
      hasContextTokens,
      message.output_tokens,
      hasOutputTokens,
    ),
  );

  /** Resolve the session that owns this message, falling back to activeSession. */
  let owningSession = $derived(
    sessions.sessions.find((s) => s.id === message.session_id) ??
      sessions.activeSession,
  );

  /** Walk the parent chain to check if any ancestor has the teammate tag. */
  function isTeammateAncestry(s: Session, all: Session[]): boolean {
    if ((s.first_message ?? "").includes("<teammate-message")) return true;
    if (!s.parent_session_id) return false;
    const visited = new Set<string>();
    let cur: Session | undefined = s;
    while (cur?.parent_session_id && !visited.has(cur.id)) {
      visited.add(cur.id);
      const parent = all.find((p) => p.id === cur!.parent_session_id);
      if (!parent) break;
      if ((parent.first_message ?? "").includes("<teammate-message")) return true;
      cur = parent;
    }
    return false;
  }

  /** Walk the parent chain to check if any ancestor is a subagent. */
  function isSubagentAncestry(s: Session, all: Session[]): boolean {
    if (s.relationship_type === "subagent") return true;
    if (!s.parent_session_id) return false;
    const visited = new Set<string>();
    let cur: Session | undefined = s;
    while (cur?.parent_session_id && !visited.has(cur.id)) {
      visited.add(cur.id);
      const parent = all.find((p) => p.id === cur!.parent_session_id);
      if (!parent) break;
      if (parent.relationship_type === "subagent") return true;
      cur = parent;
    }
    return false;
  }

  /** Classify the session kind, walking the parent chain. */
  let sessionKind = $derived.by((): "teammate" | "subagent" | "user" => {
    const s = owningSession;
    if (!s) return "user";
    const all = sessions.sessions;
    if (isSubagentAncestry(s, all)) return "subagent";
    if (isTeammateAncestry(s, all)) return "teammate";
    return "user";
  });

  /** Context-aware role labels based on session type. */
  let roleLabel = $derived.by(() => {
    if (!isUser) return "Assistant";
    if (isSubagentContext) return "Agent";
    if (sessionKind === "teammate") return "Teammate";
    if (sessionKind === "subagent") return "Agent";
    return "User";
  });

  let roleIcon = $derived.by(() => {
    if (!isUser) return "A";
    if (isSubagentContext) return "S";
    if (sessionKind === "teammate") return "T";
    if (sessionKind === "subagent") return "S";
    return "U";
  });

  let hasSearchQuery = $derived(highlightQuery.trim() !== "");

  /** Whether the text (prose) segments for this role should render. */
  let showText = $derived(
    hasSearchQuery || ui.isBlockVisible(isUser ? "user" : "assistant"),
  );

  let accentColor = $derived(
    isUser ? "var(--accent-blue)" : "var(--accent-purple)",
  );

  let roleBg = $derived(
    isUser ? "var(--user-bg)" : "var(--assistant-bg)",
  );

  let pinned = $derived(pins.isPinned(message.id));
  let pinFeedback = $state("");

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
   *  calls inherit the turn's wall-clock duration since per-call
   *  timing isn't available without tool_result deltas. Running
   *  turns synthesize a live `running …+` label from the turn's
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

  /** Build the chip payload for an assistant turn. Returns null
   *  when the message has no tool calls or timing isn't loaded. */
  let turnSummary = $derived.by(() => {
    if (isUser || !message.has_tool_use) return null;
    const calls = message.tool_calls?.length ?? 0;
    const turn = turnByMessage.get(message.id);
    if (turn?.duration_ms != null) {
      return {
        text: `turn ${formatDuration(turn.duration_ms)} · ${calls} call${calls === 1 ? "" : "s"}`,
        slow: false,
        running: false,
      };
    }
    if (sessionTiming.timing?.running && turn != null) {
      const startSrc = turn.started_at ?? message.timestamp;
      const startMs = new Date(startSrc).getTime();
      const elapsed = Number.isNaN(startMs)
        ? 0
        : Math.max(0, liveTick.now - startMs);
      return {
        text: `running ${formatDuration(elapsed)}+ · ${calls} call${calls === 1 ? "" : "s"}`,
        slow: false,
        running: true,
      };
    }
    return null;
  });

  let copyTimer: ReturnType<typeof setTimeout>;
  let pinTimer: ReturnType<typeof setTimeout>;

  async function handleCopy() {
    const ok = await copyToClipboard(formatMessageForCopy(message));
    if (ok) {
      clearTimeout(copyTimer);
      copied = true;
      copyTimer = setTimeout(() => { copied = false; }, 1500);
    }
  }

  async function handleTogglePin() {
    const wasPinned = pinned;
    try {
      await pins.togglePin(
        message.session_id,
        message.id,
        message.ordinal,
      );
      clearTimeout(pinTimer);
      pinFeedback = wasPinned ? "Unpinned" : "Pinned";
      pinTimer = setTimeout(() => { pinFeedback = ""; }, 1500);
    } catch {
      // silently fail
    }
  }
</script>

<div
  class="message"
  class:is-user={isUser}
  style:border-left-color={accentColor}
  style:background={roleBg}
>
  <div class="message-header">
    <span
      class="role-icon"
      style:background={accentColor}
    >
      {roleIcon}
    </span>
    <span
      class="role-label"
      style:color={accentColor}
    >
      {roleLabel}
    </span>
    <button
      type="button"
      class="copy-btn"
      title={copied ? "Copied!" : "Copy message"}
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
    <button
      type="button"
      class="pin-btn"
      class:pinned
      title={pinned ? "Unpin message" : "Pin message"}
      onclick={handleTogglePin}
    >
      <svg width="14" height="14" viewBox="0 0 16 16" fill="currentColor">
        <path d="M4.146.146A.5.5 0 014.5 0h7a.5.5 0 01.5.5c0 .68-.342 1.174-.646 1.479-.126.125-.25.224-.354.298v4.431l.078.048c.203.127.476.314.751.555C12.36 7.775 13 8.527 13 9.5a.5.5 0 01-.5.5H8.5v5.5a.5.5 0 01-1 0V10H3.5a.5.5 0 01-.5-.5c0-.973.64-1.725 1.17-2.189A6 6 0 015 6.708V2.277a3 3 0 01-.354-.298C4.342 1.674 4 1.179 4 .5a.5.5 0 01.146-.354z"/>
      </svg>
    </button>
    {#if pinFeedback}
      <span class="pin-feedback">{pinFeedback}</span>
    {/if}
    <div class="header-meta">
      {#if tokenSummary}
        <span class="message-tokens">
          {tokenSummary}
        </span>
      {/if}
      {#if turnSummary}
        <span
          class="turn-summary"
          class:slow={turnSummary.slow}
          class:running={turnSummary.running}
        >
          {turnSummary.text}
        </span>
      {/if}
      <span class="timestamp">
        {formatTimestamp(message.timestamp)}
      </span>
      {#if offMainModel}
        <span class="message-model" title={offMainModel}>
          {offMainModel}
        </span>
      {/if}
    </div>
  </div>

  <div class="message-body">
    {#each segments as segment}
      {#if segment.type === "thinking"}
        {#if hasSearchQuery || ui.isBlockVisible("thinking")}
          <ThinkingBlock
            content={segment.content}
            highlightQuery={highlightQuery}
            isCurrentHighlight={isCurrentHighlight}
          />
        {/if}
      {:else if segment.type === "tool"}
        <!-- Tool segments are rendered after the loop so contiguous
             tool_calls can be grouped into a single ParallelGroup
             (v1 simplification: text first, then all tools). -->
      {:else if segment.type === "code"}
        {#if hasSearchQuery || ui.isBlockVisible("code")}
          <CodeBlock
            content={segment.content}
            language={segment.label}
            highlightQuery={highlightQuery}
            isCurrentHighlight={isCurrentHighlight}
          />
        {/if}
      {:else if segment.type === "skill"}
        {#if showText}
          <SkillBlock content={segment.content} name={segment.label} />
        {/if}
      {:else}
        {#if showText}
          <div
            class="text-content markdown"
            use:applyHighlight={{
              q: highlightQuery,
              current: isCurrentHighlight,
              content: segment.content,
            }}
          >
            {@html renderMarkdown(segment.content)}
          </div>
        {/if}
      {/if}
    {/each}

    {#if (hasSearchQuery || ui.isBlockVisible("tool"))}
      {@const turn = turnByMessage.get(message.id)}
      {@const structuredCalls = message.tool_calls ?? []}
      {#if structuredCalls.length === 1}
        {@const soloCall = structuredCalls[0]!}
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
          highlightQuery={highlightQuery}
          isCurrentHighlight={isCurrentHighlight}
        />
      {:else if structuredCalls.length >= 2}
        <ParallelGroup
          toolCalls={structuredCalls}
          callTimingByID={callByToolUseID}
          turnDurationMs={turn?.duration_ms ?? null}
          isRunning={isRunningTurn(message)}
          highlightQuery={highlightQuery}
          isCurrentHighlight={isCurrentHighlight}
        />
      {:else}
        <!-- Fallback for messages with `has_tool_use` but no
             structured tool_calls — render parsed tool segments
             so legacy/synthetic transcripts (e.g. `[Bash]...`
             markers) keep their tool blocks. Mirrors
             ToolCallGroup.svelte's fallback path. -->
        {#each segments.filter((s) => s.type === "tool") as seg, segIdx (`${message.id}-${segIdx}`)}
          <ToolBlock
            content={seg.content}
            label={seg.label}
            toolCall={seg.toolCall}
            highlightQuery={highlightQuery}
            isCurrentHighlight={isCurrentHighlight}
          />
        {/each}
      {/if}
    {/if}
  </div>
</div>

<style>
  .message {
    border-left: 4px solid;
    padding: 14px 20px;
    border-radius: 0 var(--radius-md) var(--radius-md) 0;
  }

  .message-header {
    display: flex;
    align-items: center;
    gap: 8px;
    margin-bottom: 10px;
  }

  .role-icon {
    width: 22px;
    height: 22px;
    border-radius: 50%;
    display: flex;
    align-items: center;
    justify-content: center;
    font-size: 11px;
    font-weight: 700;
    color: white;
    flex-shrink: 0;
    line-height: 1;
  }

  .role-label {
    font-size: 13px;
    font-weight: 600;
    letter-spacing: 0.01em;
  }

  .timestamp {
    font-size: 12px;
    color: var(--text-muted);
  }

  .header-meta {
    margin-left: auto;
    display: flex;
    align-items: center;
    gap: 8px;
    min-width: 0;
  }

  .message-tokens {
    font-size: 10px;
    color: var(--text-muted);
    font-family: var(--font-mono);
    white-space: nowrap;
  }

  .message-model {
    font-size: 10px;
    color: var(--text-muted);
    padding: 1px 4px;
    border-radius: 3px;
    background: var(--bg-tertiary);
    white-space: nowrap;
    flex-shrink: 0;
    opacity: 0.8;
  }

  .turn-summary {
    font-family: var(--font-mono);
    font-size: 10px;
    color: var(--text-muted);
    background: rgba(255, 255, 255, 0.04);
    padding: 2px 8px;
    border-radius: var(--radius-sm);
    border: 1px solid rgba(255, 255, 255, 0.04);
    white-space: nowrap;
    flex-shrink: 0;
  }

  .turn-summary.slow {
    color: var(--slow-fg);
    background: var(--slow-bg);
    border-color: var(--slow-ring);
  }

  .turn-summary.running {
    color: var(--running-fg);
    background: var(--running-bg);
    border-color: var(--running-ring);
    animation: duration-pulse 1.6s ease-in-out infinite;
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

  .message:hover .copy-btn,
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

  .pin-btn {
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

  .message:hover .pin-btn,
  .pin-btn:focus-visible,
  .pin-btn.pinned {
    opacity: 1;
  }

  @media (hover: none) {
    .pin-btn {
      opacity: 1;
    }
  }

  .pin-btn:hover {
    background: var(--bg-surface-hover);
    color: var(--text-secondary);
  }

  .pin-btn.pinned {
    color: var(--accent-blue);
  }

  .pin-btn:active {
    transform: scale(0.92);
  }

  .pin-feedback {
    font-size: 11px;
    color: var(--text-muted);
    animation: fade-in-out 1.5s ease-in-out;
  }

  @keyframes fade-in-out {
    0% { opacity: 0; }
    15% { opacity: 1; }
    75% { opacity: 1; }
    100% { opacity: 0; }
  }

  .text-content {
    font-size: 14px;
    line-height: 1.7;
    color: var(--text-primary);
    word-wrap: break-word;
  }

  .message-body {
    display: flex;
    flex-direction: column;
    gap: 8px;
  }

  /* Markdown prose styles */
  .markdown :global(p) {
    margin: 0.5em 0;
  }

  .markdown :global(p:first-child) {
    margin-top: 0;
  }

  .markdown :global(p:last-child) {
    margin-bottom: 0;
  }

  .markdown :global(h1),
  .markdown :global(h2),
  .markdown :global(h3),
  .markdown :global(h4),
  .markdown :global(h5),
  .markdown :global(h6) {
    margin: 0.8em 0 0.4em;
    line-height: 1.3;
    font-weight: 600;
  }

  .markdown :global(h1) { font-size: 1.35em; }
  .markdown :global(h2) { font-size: 1.2em; }
  .markdown :global(h3) { font-size: 1.1em; }
  .markdown :global(h4),
  .markdown :global(h5),
  .markdown :global(h6) { font-size: 1em; }

  .markdown :global(a) {
    color: var(--accent-blue);
    text-decoration: none;
  }

  .markdown :global(a:hover) {
    text-decoration: underline;
  }

  .markdown :global(code) {
    font-family: var(--font-mono);
    font-size: 0.85em;
    background: var(--bg-inset);
    border: 1px solid var(--border-muted);
    border-radius: 4px;
    padding: 0.15em 0.4em;
  }

  .markdown :global(pre) {
    background: var(--code-bg);
    color: var(--code-text);
    border-radius: var(--radius-md);
    padding: 12px 16px;
    overflow-x: auto;
    margin: 0.5em 0;
  }

  .markdown :global(pre code) {
    background: none;
    border: none;
    padding: 0;
    font-size: 13px;
    color: inherit;
  }

  .markdown :global(blockquote) {
    border-left: 3px solid var(--border-default);
    margin: 0.5em 0;
    padding: 0.3em 1em;
    color: var(--text-secondary);
  }

  .markdown :global(ul),
  .markdown :global(ol) {
    padding-left: 1.6em;
    margin: 0.5em 0;
  }

  .markdown :global(li) {
    margin: 0.2em 0;
    line-height: 1.65;
  }

  .markdown :global(hr) {
    border: none;
    border-top: 1px solid var(--border-muted);
    margin: 0.8em 0;
  }

  .markdown :global(table) {
    border-collapse: collapse;
    margin: 0.5em 0;
    width: auto;
    font-size: 13px;
  }

  .markdown :global(th),
  .markdown :global(td) {
    border: 1px solid var(--border-muted);
    padding: 6px 10px;
    text-align: left;
  }

  .markdown :global(th) {
    background: var(--bg-inset);
    font-weight: 600;
  }

  .markdown :global(img) {
    max-width: 100%;
    border-radius: var(--radius-sm);
  }

  .markdown :global(strong) {
    font-weight: 600;
  }
</style>
