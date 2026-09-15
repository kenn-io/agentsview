<script lang="ts">
  import type { Message } from "../../api/types.js";
  import type { CallTiming, TurnTiming } from "../../api/types/timing.js";
  import { formatTimestamp } from "../../utils/format.js";
  import { formatDuration } from "../../utils/duration.js";
  import { copyToClipboard } from "../../utils/clipboard.js";
  import { formatMessageForCopy } from "../../utils/copy-message.js";
  import { parseContent, enrichSegments } from "../../utils/content-parser.js";
  import { sessionTiming } from "../../stores/sessionTiming.svelte.js";
  import { liveTick } from "../../stores/liveTick.svelte.js";
  import ToolBlock from "./ToolBlock.svelte";
  import ThinkingBlock from "./ThinkingBlock.svelte";
  import { collectSearchBlocks } from "../../search/block-text.js";
  import { ui } from "../../stores/ui.svelte.js";
  import ParallelGroup from "./ParallelGroup.svelte";
  import { CopyButton } from "@kenn-io/kit-ui";
  import { displayToolName } from "../../utils/toolDisplay.js";
  import { SettingsIcon } from "../../icons.js";
  import { m } from "../../i18n/index.js";

  interface Props {
    messages: Message[];
    timestamp: string;
    searchable?: boolean;
    sortNewestFirst?: boolean;
    divider?: { ordinal: number; label: string };
  }

  let { messages, timestamp, searchable = false, sortNewestFirst = false, divider }: Props = $props();
  let copied = $state(false);

  function messageToolCount(message: Message): number {
    const structured = message.tool_calls?.length ?? 0;
    if (structured > 0) return structured;
    return enrichSegments(
      parseContent(message.content, message.has_tool_use, message.id, message.content_length),
      message.tool_calls,
    ).filter((segment) => segment.type === "tool").length;
  }

  let totalCalls = $derived(messages.reduce((n, message) => n + messageToolCount(message), 0));
  let label = $derived(m.tool_call_group_call_count({ count: totalCalls }));
  let displayMessages = $derived(sortNewestFirst ? [...messages].reverse() : messages);
  let turnByMessage = $derived.by(() => {
    const map = new Map<number, TurnTiming>();
    for (const turn of sessionTiming.timing?.turns ?? []) map.set(turn.message_id, turn);
    return map;
  });
  let callByToolUseID = $derived.by(() => {
    const map = new Map<string, CallTiming>();
    for (const turn of sessionTiming.timing?.turns ?? []) {
      for (const call of turn.calls) map.set(call.tool_use_id, call);
    }
    return map;
  });

  function soloDurationLabel(ct: CallTiming | undefined, turn: TurnTiming | undefined, msg: Message): string | undefined {
    if (ct?.subagent_session_id && ct.duration_ms != null) return formatDuration(ct.duration_ms);
    if (turn?.duration_ms != null) return formatDuration(turn.duration_ms);
    if (sessionTiming.timing?.running && turn != null) {
      const startMs = new Date(turn.started_at ?? msg.timestamp).getTime();
      const elapsed = Number.isNaN(startMs) ? 0 : Math.max(0, liveTick.now - startMs);
      return m.tool_call_group_running_duration({ duration: formatDuration(elapsed) });
    }
    return undefined;
  }

  function isRunningTurn(msg: Message): boolean {
    if (!sessionTiming.timing?.running) return false;
    const turn = turnByMessage.get(msg.id);
    return turn != null && turn.duration_ms == null;
  }

  let copyTimer: ReturnType<typeof setTimeout>;
  async function handleCopy() {
    const combined = messages.map((message) => formatMessageForCopy(message)).join("\n\n");
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
    <span class="gear-icon"><SettingsIcon size="12" strokeWidth="2" aria-hidden="true" /></span>
    <span class="group-label">{label}</span>
    <CopyButton
      revealOnHover
      {copied}
      ariaLabel={m.tool_call_group_copy_tool_calls()}
      copiedAriaLabel={m.tool_call_group_copied_tool_calls()}
      title={m.tool_call_group_copy_tool_calls()}
      copiedTitle={m.tool_call_group_copied()}
      onclick={handleCopy}
    />
    <span class="group-timestamp">{formatTimestamp(timestamp)}</span>
  </div>
  <div class="tool-group-body">
    {#each displayMessages as message (message.ordinal)}
      {#if divider?.ordinal === message.ordinal}
        <div class="read-progress-divider" role="separator" aria-label={m.read_progress_boundary()}>{divider.label}</div>
      {/if}
      {@const calls = message.tool_calls ?? []}
      {@const turn = turnByMessage.get(message.id)}
      <div data-message-ordinal={message.ordinal}>
        {#if ui.isBlockVisible("thinking")}
          {#each collectSearchBlocks(message).filter((block) => block.kind === "thinking") as block (block.key)}
            <ThinkingBlock content={block.text} searchKey={searchable ? block.key : undefined} />
          {/each}
        {/if}
        {#if calls.length === 1}
          {@const soloCall = calls[0]!}
          <ToolBlock
            toolCall={soloCall}
            content=""
            label={displayToolName(soloCall)}
            durationLabel={soloDurationLabel(callByToolUseID.get(soloCall.tool_use_id ?? ""), turn, message)}
            isRunning={isRunningTurn(message)}
            searchScope={searchable ? { ordinal: message.ordinal, callIdx: 0 } : undefined}
          />
        {:else if calls.length >= 2}
          <ParallelGroup
            toolCalls={calls}
            callTimingByID={callByToolUseID}
            turnDurationMs={turn?.duration_ms ?? null}
            isRunning={isRunningTurn(message)}
            searchOrdinal={searchable ? message.ordinal : undefined}
          />
        {:else}
          {#each enrichSegments(parseContent(message.content, message.has_tool_use, message.id, message.content_length), message.tool_calls).filter((s) => s.type === "tool") as seg, segIdx (`${message.id}-${segIdx}`)}
            <ToolBlock
              content={seg.content}
              label={seg.label}
              toolCall={seg.toolCall}
              searchScope={searchable ? { ordinal: message.ordinal, callIdx: `seg${segIdx}` } : undefined}
            />
          {/each}
        {/if}
      </div>
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
  .tool-group-header { display: flex; align-items: center; gap: 8px; margin-bottom: 6px; }
  .gear-icon { display: flex; align-items: center; flex-shrink: 0; color: var(--accent-amber); }
  .group-label { font-size: 12px; font-weight: 600; color: var(--accent-amber); }
  .group-timestamp { font-size: 12px; color: var(--text-muted); margin-left: auto; }
  .tool-group:hover :global(.kit-copy-btn) { opacity: 1; }
  .tool-group-body { display: flex; flex-direction: column; gap: 2px; }
  .read-progress-divider {
    display: flex;
    align-items: center;
    gap: 8px;
    margin: 4px 0 6px;
    color: var(--accent-blue);
    font-size: 11px;
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: 0.06em;
  }
  .read-progress-divider::before,
  .read-progress-divider::after {
    content: "";
    height: 1px;
    flex: 1;
    background: color-mix(in srgb, var(--accent-blue) 35%, transparent);
  }
  .tool-group-body :global(.tool-block) { margin: 0; border-left: none; border-radius: 0; }
</style>
