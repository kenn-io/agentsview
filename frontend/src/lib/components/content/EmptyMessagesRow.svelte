<script lang="ts">
  import { Disclosure } from "@kenn-io/kit-ui";
  import type { DbMessage as Message } from "../../api/generated/index.js";
  import { m } from "../../i18n/index.js";
  import MessageContent from "./MessageContent.svelte";

  interface Props {
    messages: Message[];
    open: boolean;
    onToggle: (open: boolean) => void;
  }

  let { messages, open, onToggle }: Props = $props();
</script>

<Disclosure
  variant="plain"
  class="empty-run"
  {open}
  ontoggle={(o) => {
    if (o !== open) onToggle(o);
  }}
>
  {#snippet summary()}
    <span class="empty-summary">
      <span class="empty-line"></span>
      <span class="empty-label" title={m.transcript_empty_messages_title()}>
        {m.transcript_empty_messages({ count: messages.length })}
      </span>
      <span class="empty-line"></span>
    </span>
  {/snippet}
  {#if open}
    <div class="empty-messages">
      {#each messages as message (message.id)}
        <div data-message-ordinal={message.ordinal}>
          <MessageContent {message} searchOrdinal={message.ordinal} />
        </div>
      {/each}
    </div>
  {/if}
</Disclosure>

<style>
  /* Keep the row's divider inset, and render the hidden cards exactly like
     the transcript's other cards rather than as indented secondary text. */
  :global(.empty-run.kit-disclosure--plain > .kit-disclosure__summary) {
    padding: 2px 12px;
  }
  :global(.empty-run.kit-disclosure--plain > .kit-disclosure__content) {
    padding: 0;
    color: inherit;
    font-size: inherit;
    line-height: inherit;
  }
  .empty-summary {
    display: flex;
    align-items: center;
    gap: 8px;
    color: var(--text-muted);
    font-size: 11px;
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
    white-space: nowrap;
  }
  .empty-messages {
    display: flex;
    flex-direction: column;
    gap: var(--space-5);
    padding-top: var(--space-5);
  }
</style>
