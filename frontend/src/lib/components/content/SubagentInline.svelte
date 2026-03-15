<!-- ABOUTME: Expandable inline view of a subagent's conversation.
     ABOUTME: Lazily loads and renders subagent messages within a parent ToolBlock. -->
<script lang="ts">
  import type { Message, Session } from "../../api/types.js";
  import { getMessages, getSession } from "../../api/client.js";
  import { formatTokenCount } from "../../utils/format.js";
  import { sessions } from "../../stores/sessions.svelte.js";
  import { router } from "../../stores/router.svelte.js";
  import MessageContent from "./MessageContent.svelte";

  interface Props {
    sessionId: string;
  }

  let { sessionId }: Props = $props();
  let expanded = $state(false);
  let messages = $state<Message[] | null>(null);
  let sessionMeta = $state<Session | null>(null);
  let loading = $state(false);
  let error = $state<string | null>(null);

  let subagentSession = $derived(sessions.childSessions.get(sessionId) ?? null);

  async function toggleExpand() {
    expanded = !expanded;
    if (expanded && !messages) {
      loading = true;
      error = null;
      try {
        const [resp, meta] = await Promise.all([
          getMessages(sessionId, { limit: 1000 }),
          getSession(sessionId).catch(() => null),
        ]);
        messages = resp.messages;
        sessionMeta = meta;
      } catch (e) {
        error = e instanceof Error ? e.message : "Failed to load";
      } finally {
        loading = false;
      }
    }
  }

  async function openAsSession(e: MouseEvent) {
    e.preventDefault();
    e.stopPropagation();
    if (router.route === "sessions") {
      await sessions.navigateToSession(sessionId);
    } else {
      sessions.pendingNavTarget = sessionId;
      router.navigate("sessions");
    }
  }

  let agentLabel = $derived(sessionMeta?.agent ?? null);
  let messageCountLabel = $derived(
    sessionMeta ? `${sessionMeta.message_count} messages` : null,
  );
</script>

<div class="subagent-inline">
  <div class="subagent-header">
    <button class="subagent-toggle" onclick={toggleExpand}>
      <span class="toggle-chevron" class:open={expanded}>&#9656;</span>
      <span class="toggle-label">Subagent session</span>
      {#if agentLabel}
        <span class="toggle-meta">{agentLabel}</span>
      {/if}
      {#if messageCountLabel}
        <span class="toggle-meta">{messageCountLabel}</span>
      {/if}
      <span class="toggle-session-id">{sessionId.slice(0, 12)}</span>
      {#if subagentSession}
        {@const ctxTokens = subagentSession.peak_context_tokens}
        {#if ctxTokens + subagentSession.total_output_tokens > 0}
          <span class="toggle-tokens">({formatTokenCount(ctxTokens)} ctx / {formatTokenCount(subagentSession.total_output_tokens)} out)</span>
        {/if}
      {/if}
    </button>
    <a
      href="#{sessionId}"
      class="open-session-link"
      onclick={openAsSession}
      title="Open as full session"
    >
      Open session &#8599;
    </a>
  </div>

  {#if expanded}
    <div class="subagent-messages">
      {#if loading}
        <div class="subagent-status">Loading...</div>
      {:else if error}
        <div class="subagent-status subagent-error">{error}</div>
      {:else if messages && messages.length > 0}
        {#each messages as message}
          <MessageContent {message} isSubagentContext={true} />
        {/each}
      {:else if messages}
        <div class="subagent-status">No messages</div>
      {/if}
    </div>
  {/if}
</div>

<style>
  .subagent-inline {
    border-top: 1px solid var(--border-muted);
    margin-top: 2px;
  }

  .subagent-header {
    display: flex;
    align-items: center;
  }

  .subagent-toggle {
    display: flex;
    align-items: center;
    gap: 6px;
    padding: 6px 10px;
    flex: 1;
    min-width: 0;
    text-align: left;
    font-size: 11px;
    color: var(--accent-green);
    border-radius: 0 0 0 var(--radius-sm);
    transition: background 0.1s;
  }

  .subagent-toggle:hover {
    background: var(--bg-surface-hover);
  }

  .toggle-chevron {
    display: inline-block;
    font-size: 10px;
    transition: transform 0.15s;
    flex-shrink: 0;
  }

  .toggle-chevron.open {
    transform: rotate(90deg);
  }

  .toggle-label {
    font-weight: 600;
    white-space: nowrap;
  }

  .toggle-meta {
    font-family: var(--font-mono);
    font-size: 10px;
    color: var(--text-muted);
    background: var(--bg-inset);
    padding: 1px 5px;
    border-radius: var(--radius-sm);
    white-space: nowrap;
  }

  .toggle-session-id {
    font-family: var(--font-mono);
    font-size: 10px;
    color: var(--text-muted);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    min-width: 0;
  }

  .open-session-link {
    font-size: 10px;
    color: var(--text-secondary);
    padding: 6px 10px;
    white-space: nowrap;
    flex-shrink: 0;
    text-decoration: none;
    transition: color 0.1s, background 0.1s;
  }

  .open-session-link:hover {
    color: var(--accent-green);
    background: var(--bg-surface-hover);
  }

  .toggle-tokens {
    font-size: 10px;
    font-variant-numeric: tabular-nums;
    color: color-mix(in srgb, var(--accent-green) 60%, var(--text-muted));
    white-space: nowrap;
    flex-shrink: 0;
  }


  .subagent-messages {
    border-left: 3px solid var(--accent-green);
    margin: 0 0 4px 10px;
    display: flex;
    flex-direction: column;
    gap: 4px;
    padding: 4px 0;
  }

  .subagent-status {
    padding: 8px 14px;
    font-size: 12px;
    color: var(--text-muted);
  }

  .subagent-error {
    color: var(--accent-red);
  }
</style>
