<script lang="ts">
  import type { DbMessage as Message } from "../../api/generated/index.js";
  import { ChevronRightIcon } from "../../icons.js";
  import { m } from "../../i18n/index.js";
  import MessageContent from "./MessageContent.svelte";

  interface Props {
    messages: Message[];
    open: boolean;
    onToggle: (open: boolean) => void;
  }

  let { messages, open, onToggle }: Props = $props();
</script>

<details
  class="empty-run"
  {open}
  ontoggle={(event) => {
    if (event.currentTarget.open !== open) onToggle(event.currentTarget.open);
  }}
>
  <summary class="empty-summary" title={m.transcript_empty_messages_title()}>
    <span class="empty-line"></span>
    <span class="empty-label">
      <span class="empty-chevron">
        <ChevronRightIcon size="12" strokeWidth="2.2" aria-hidden="true" />
      </span>
      {m.transcript_empty_messages({ count: messages.length })}
    </span>
    <span class="empty-line"></span>
  </summary>
  {#if open}
    <div class="empty-messages">
      {#each messages as message (message.id)}
        <div data-message-ordinal={message.ordinal}>
          <MessageContent {message} searchOrdinal={message.ordinal} />
        </div>
      {/each}
    </div>
  {/if}
</details>

<style>
  .empty-summary {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 2px 12px;
    color: var(--text-muted);
    font-size: 11px;
    font-weight: 500;
    cursor: pointer;
    list-style: none;
  }
  .empty-summary::-webkit-details-marker {
    display: none;
  }
  .empty-summary:hover {
    color: var(--text-secondary);
  }
  .empty-line {
    flex: 1;
    height: 1px;
    background: var(--border-muted);
  }
  .empty-label {
    display: inline-flex;
    align-items: center;
    gap: 4px;
    white-space: nowrap;
  }
  .empty-chevron {
    display: inline-flex;
    align-items: center;
    transition: transform 0.15s ease;
  }
  .empty-run[open] .empty-chevron {
    transform: rotate(90deg);
  }
  .empty-messages {
    display: flex;
    flex-direction: column;
    gap: var(--space-5);
    padding-top: var(--space-5);
  }
</style>
