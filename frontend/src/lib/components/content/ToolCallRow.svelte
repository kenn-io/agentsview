<script lang="ts">
  import type { Snippet } from "svelte";
  import { getLocale, m } from "../../i18n/index.js";
  import { ChevronRightIcon, MessageSquareIcon } from "../../icons.js";
  import { router } from "../../stores/router.svelte.js";
  import { ui } from "../../stores/ui.svelte.js";
  import { formatDuration } from "../../utils/duration.js";
  import { OUTCOME_TONES, outcomeLabel, type ToolOutcome } from "../../utils/tool-outcomes.js";
  import ToolOutcomeDot from "./ToolOutcomeDot.svelte";

  interface Props {
    sessionId: string;
    ordinal: number;
    /** Names the call within its message when the message holds several. */
    callIndex?: number | undefined;
    tool: string;
    input: string;
    /** Full input text for the hover title. */
    inputTitle?: string | undefined;
    /** Undefined when the call's outcome isn't known here, which hides the dot and result. */
    outcome?: ToolOutcome | undefined;
    /** Draws a cited message that holds no tool call. */
    message?: boolean;
    tag?: { label: string; title?: string | undefined } | null;
    resultBytes?: number | null | undefined;
    /** Appended to the result, such as how much of it the model saw. */
    cut?: string | undefined;
    /** Null means not measured; undefined leaves the column blank. */
    durationMs?: number | null | undefined;
    highlighted?: boolean;
    /** False when the ordinal may name a different message than the transcript on screen. */
    linked?: boolean;
    /** The transcript revision the ordinal belongs to, so a jump stops if the transcript moves on. */
    revision?: string | undefined;
    /** With ontoggle, the row opens to show children. */
    expanded?: boolean;
    detailId?: string | undefined;
    ontoggle?: (() => void) | undefined;
    children?: Snippet | undefined;
  }

  let {
    sessionId,
    ordinal,
    callIndex = undefined,
    tool,
    input,
    inputTitle = undefined,
    outcome = undefined,
    message = false,
    tag = null,
    resultBytes = undefined,
    cut = undefined,
    durationMs = undefined,
    highlighted = false,
    linked = true,
    revision = undefined,
    expanded = false,
    detailId = undefined,
    ontoggle = undefined,
    children = undefined,
  }: Props = $props();

  const where = $derived(
    callIndex === undefined ? m.tool_sequences_message({ ordinal }) : m.tool_sequences_message_call({ ordinal, callIndex }),
  );
  const jumpLabel = $derived(
    callIndex === undefined
      ? m.tool_sequences_jump_label({ ordinal, tool })
      : m.tool_sequences_jump_call_label({ ordinal, callIndex, tool }),
  );
  const size = $derived(
    resultBytes === null || resultBytes === undefined
      ? null
      : m.tool_sequences_byte_count({ count: resultBytes, countLabel: resultBytes.toLocaleString(getLocale()) }),
  );

  function jump(event: MouseEvent) {
    if (event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
    event.preventDefault();
    ui.scrollToOrdinal(ordinal, sessionId, revision);
    // Same-session jumps leave ?msg alone like the app's other in-session jumps, since replaceParams reloads the sidebar.
    if (router.sessionId !== sessionId) {
      router.navigateToSession(sessionId, { msg: String(ordinal), ...(revision ? { rev: revision } : {}) });
    }
  }
</script>

{#snippet cells()}
  <span class="tool">
    {#if message}
      <MessageSquareIcon size={12} aria-hidden="true" />
    {:else if outcome}
      <ToolOutcomeDot {outcome} />
    {/if}
    <span class="name" title={tool}>{tool}</span>{#if !message}<span class="kit-sr-only">, {where}</span>{/if}
  </span>
  <span class="input" title={inputTitle}>
    {input}
    {#if tag}<span class="tag" title={tag.title}>{tag.label}</span>{/if}
  </span>
  <span class="res">{#if outcome}<b>{outcomeLabel(outcome)}</b>{#if size}{` · ${size}`}{/if}{/if}{#if cut}{outcome ? " · " : ""}<span class="cut">{cut}</span>{/if}</span>
  {#if durationMs === null}
    <span class="dur" title={m.tool_sequences_not_measured()}>
      <span aria-hidden="true">—</span><span class="kit-sr-only">{m.tool_sequences_not_measured()}</span>
    </span>
  {:else}
    <span class="dur">{durationMs === undefined ? "" : formatDuration(durationMs)}</span>
  {/if}
{/snippet}

<div
  class="call"
  class:open={expanded}
  class:highlighted
  class:message
  data-kit-tone={outcome ? OUTCOME_TONES[outcome] : undefined}
>
  <div class="call-line">
    {#if ontoggle}
      <button
        type="button"
        class="call-row"
        aria-expanded={expanded}
        aria-controls={expanded ? detailId : undefined}
        onclick={ontoggle}
      >
        <ChevronRightIcon class="chev" size={12} aria-hidden="true" />
        {@render cells()}
      </button>
    {:else}
      <div class="call-row static">{@render cells()}</div>
    {/if}
    {#if linked}
      <a
        class="jump"
        href={router.buildSessionHref(sessionId, { msg: String(ordinal), ...(revision ? { rev: revision } : {}) })}
        aria-label={message || !tool ? undefined : jumpLabel}
        onclick={jump}
      >{where}<span aria-hidden="true">{" ↗"}</span></a>
    {:else}
      <span class="jump unlinked">{where}</span>
    {/if}
  </div>
  {#if expanded && children}
    {#if durationMs !== undefined}
      <!-- A narrow row drops its duration column, so the opened call carries it there. -->
      <p class="dur-detail">
        <span>{m.tool_sequences_duration()}</span>
        {durationMs === null ? m.tool_sequences_not_measured() : formatDuration(durationMs)}
      </p>
    {/if}
    {@render children()}
  {/if}
</div>

<style>
  /* Rows follow their own width, which differs between the panel and the report's citations. */
  .call {
    container-type: inline-size;
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-sm);
    background: var(--bg-primary);
    transition: border-color 0.15s, background 0.15s;
  }

  .call:has(> .call-line > button):hover {
    border-color: var(--border-default);
    background: var(--bg-surface-hover);
  }

  .call.open {
    border-color: var(--border-default);
    background: var(--bg-surface);
  }

  .call.highlighted {
    border-color: color-mix(in srgb, var(--accent-amber) 45%, var(--border-default));
    background: var(--tool-bg);
  }

  .call-line {
    display: grid;
    grid-template-columns: minmax(0, 1fr) auto;
    align-items: center;
    gap: var(--space-4);
    padding-right: var(--space-4);
  }

  .call-row {
    display: grid;
    grid-template-columns: 12px 6.5rem minmax(0, 1fr) auto 3.5em;
    align-items: center;
    gap: var(--space-4);
    min-width: 0;
    /* Right padding keeps the inset focus ring off the last column. */
    padding: var(--space-2) var(--space-3) var(--space-2) var(--space-4);
    border: 0;
    background: none;
    color: inherit;
    font: inherit;
    font-size: var(--font-size-xs);
    text-align: left;
  }

  button.call-row {
    cursor: pointer;
  }

  .call-row.static {
    grid-template-columns: 6.5rem minmax(0, 1fr) auto 3.5em;
  }

  .call-row:focus-visible,
  .jump:focus-visible {
    outline: var(--focus-ring);
    outline-offset: -2px;
    border-radius: var(--radius-sm);
  }

  .call-row :global(.chev) {
    flex-shrink: 0;
    color: var(--text-muted);
    transition: transform 0.18s ease;
  }

  .call.open .call-row :global(.chev) {
    transform: rotate(90deg);
  }

  .tool {
    display: flex;
    align-items: center;
    gap: var(--space-3);
    min-width: 0;
    color: color-mix(in srgb, var(--accent-amber) 72%, var(--text-primary));
    font-family: var(--font-mono);
    font-weight: 500;
  }

  .name {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .input {
    overflow: hidden;
    color: var(--text-secondary);
    font-family: var(--font-mono);
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .message .tool,
  .message .input {
    color: var(--text-secondary);
    font-family: var(--font-sans);
  }

  .tag {
    display: inline-block;
    margin-left: var(--space-3);
    padding: 0 5px;
    border-radius: 3px;
    background: var(--bg-inset);
    color: var(--text-muted);
    font-family: var(--font-sans);
    font-size: var(--font-size-2xs);
  }

  .res {
    color: var(--text-muted);
    white-space: nowrap;
    font-variant-numeric: tabular-nums;
  }

  .res b {
    color: var(--kit-tone-ink, var(--text-secondary));
    font-weight: 600;
  }

  .cut {
    color: color-mix(in srgb, var(--accent-amber) 72%, var(--text-primary));
  }

  .dur {
    min-width: 3.5em;
    color: var(--text-muted);
    font-family: var(--font-mono);
    font-size: var(--font-size-2xs);
    text-align: right;
  }

  .dur-detail {
    display: none;
    margin: 0;
    padding: var(--space-1) var(--space-4) 0 28px;
    color: var(--text-primary);
    font-size: var(--font-size-xs);
    font-weight: 400;
  }

  .dur-detail span {
    display: inline-block;
    min-width: calc(3.5rem + var(--space-4));
    color: var(--text-muted);
  }

  .jump {
    color: var(--accent-blue);
    font-size: var(--font-size-xs);
    text-decoration: none;
    white-space: nowrap;
  }

  a.jump:hover {
    text-decoration: underline;
  }

  .jump.unlinked {
    color: var(--text-muted);
  }

  @media (prefers-reduced-motion: reduce) {
    .call-row :global(.chev) {
      transition: none;
    }
  }

  @container (max-width: 760px) {
    .call-row {
      grid-template-columns: 12px 5.5rem minmax(0, 1fr) auto;
    }

    .call-row.static {
      grid-template-columns: 5.5rem minmax(0, 1fr) auto;
    }

    .dur {
      display: none;
    }

    .dur-detail {
      display: block;
    }
  }

  @container (max-width: 460px) {
    .call-line {
      align-items: start;
    }

    /* fit-content keeps a long MCP tool name from squeezing out the input. */
    .call-row {
      grid-template-columns: 12px fit-content(40%) minmax(0, 1fr);
      row-gap: var(--space-1);
    }

    .call-row.static {
      grid-template-columns: fit-content(40%) minmax(0, 1fr);
    }

    .res {
      grid-column: 2 / 4;
      white-space: normal;
    }

    .call-row.static .res {
      grid-column: 1 / 3;
    }

    .res:empty {
      display: none;
    }

    .jump {
      padding-top: var(--space-2);
    }
  }

  /* Too narrow for the link beside the row, so it drops below. */
  @container (max-width: 300px) {
    .call-line {
      grid-template-columns: minmax(0, 1fr);
      gap: 0;
    }

    .jump {
      padding: 0 0 var(--space-2) var(--space-4);
    }
  }
</style>
