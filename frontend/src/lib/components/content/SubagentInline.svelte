<!-- ABOUTME: Expandable inline view of a subagent's conversation.
     ABOUTME: Lazily loads and renders subagent messages within a parent ToolBlock. -->
<script lang="ts">
  import type { Message } from "../../api/types.js";
  import { getMessages } from "../../api/client.js";
  import MessageContent from "./MessageContent.svelte";

  interface Props {
    sessionId: string;
  }

  let { sessionId }: Props = $props();
  let expanded = $state(false);
  let messages: Message[] | null = $state(null);
  let loading = $state(false);
  let error: string | null = $state(null);

  async function toggleExpand() {
    expanded = !expanded;
    if (expanded && !messages) {
      loading = true;
      error = null;
      try {
        const resp = await getMessages(sessionId, { limit: 1000 });
        messages = resp.messages;
      } catch (e) {
        error = e instanceof Error ? e.message : "Failed to load";
      } finally {
        loading = false;
      }
    }
  }
</script>

<div class="subagent-inline">
  <button class="subagent-toggle" onclick={toggleExpand}>
    <span class="toggle-chevron" class:open={expanded}>&#9656;</span>
    <span class="toggle-label">Subagent session</span>
    <span class="toggle-session-id">{sessionId}</span>
  </button>

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

  .subagent-toggle {
    display: flex;
    align-items: center;
    gap: 6px;
    padding: 6px 10px;
    width: 100%;
    text-align: left;
    font-size: 11px;
    color: var(--accent-green);
    border-radius: 0 0 var(--radius-sm) 0;
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

  .toggle-session-id {
    font-family: var(--font-mono);
    font-size: 10px;
    color: var(--text-muted);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    min-width: 0;
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
