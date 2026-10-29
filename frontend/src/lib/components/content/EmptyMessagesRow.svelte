<script lang="ts">
  import type { DbMessage as Message } from "../../api/generated/index.js";
  import { formatTimestamp } from "../../utils/format.js";
  import { ChevronRightIcon } from "../../icons.js";
  import { m } from "../../i18n/index.js";

  interface Props {
    messages: Message[];
  }

  let { messages }: Props = $props();
</script>

<details class="empty-run">
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
  <ul class="empty-list">
    {#each messages as message (message.id)}
      <li class="empty-item" data-message-ordinal={message.ordinal}>
        {message.role === "user"
          ? m.message_content_role_user()
          : m.message_content_role_assistant()}
        {#if message.timestamp}
          <span class="empty-time">&middot; {formatTimestamp(message.timestamp)}</span>
        {/if}
      </li>
    {/each}
  </ul>
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
  .empty-list {
    margin: 4px 0 0;
    padding: 0 0 8px;
    list-style: none;
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: 2px;
    color: var(--text-secondary);
    font-size: 12px;
  }
  .empty-time {
    color: var(--text-muted);
  }
</style>
