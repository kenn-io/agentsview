<!-- ABOUTME: Expandable inline view of a subagent's conversation.
     ABOUTME: Lazily loads and renders subagent messages within a parent ToolBlock. -->
<script lang="ts">
  import type {
    Message,
    MessagesResponse,
    Session,
  } from "../../api/types.js";
  import { SessionsService } from "../../api/generated/index";
  import { configureGeneratedClient } from "../../api/runtime.js";
  import { formatTokenUsage } from "../../utils/format.js";
  import { computeMainModel } from "../../utils/model.js";
  import { sessions } from "../../stores/sessions.svelte.js";
  import { router } from "../../stores/router.svelte.js";
  import MessageContent from "./MessageContent.svelte";
  import {
    ChevronRightIcon,
    ExternalLinkIcon,
  } from "../../icons.js";
  import { m } from "../../i18n/index.js";

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
  let tokenSourceSession = $derived(sessionMeta ?? subagentSession);

  async function toggleExpand() {
    expanded = !expanded;
    if (expanded && !messages) {
      loading = true;
      error = null;
      try {
        configureGeneratedClient();
        const [resp, meta] = await Promise.all([
          SessionsService.getApiV1SessionsIdMessages({
            id: sessionId,
            limit: 1000,
          }) as unknown as Promise<MessagesResponse>,
          (SessionsService.getApiV1SessionsId({
            id: sessionId,
          }) as unknown as Promise<Session>).catch(() => null),
        ]);
        messages = resp.messages;
        sessionMeta = meta;
      } catch (e) {
        error = e instanceof Error
          ? e.message
          : m.subagent_inline_failed_to_load();
      } finally {
        loading = false;
      }
    }
  }

  async function openAsSession(e: MouseEvent) {
    e.preventDefault();
    e.stopPropagation();
    router.navigateToSession(sessionId);
  }

  let agentLabel = $derived(sessionMeta?.agent ?? null);
  let messageCountLabel = $derived(
    tokenSourceSession
      ? m.subagent_inline_message_count({
          count: tokenSourceSession.message_count,
        })
      : null,
  );
  let subagentModel = $derived(
    messages && sessionMeta &&
    messages.length >= sessionMeta.message_count
      ? computeMainModel(messages)
      : "",
  );
  let subagentHasContextTokens = $derived(
    tokenSourceSession
      ? (tokenSourceSession.has_peak_context_tokens ??
        tokenSourceSession.peak_context_tokens > 0)
      : false,
  );
  let subagentHasOutputTokens = $derived(
    tokenSourceSession
      ? (tokenSourceSession.has_total_output_tokens ??
        tokenSourceSession.total_output_tokens > 0)
      : false,
  );
  let subagentTokenSummary = $derived(
    tokenSourceSession
      ? formatTokenUsage(
          tokenSourceSession.peak_context_tokens,
          subagentHasContextTokens,
          tokenSourceSession.total_output_tokens,
          subagentHasOutputTokens,
        )
      : null,
  );
</script>

<div class="subagent-inline">
  <div class="subagent-header">
    <button class="subagent-toggle" onclick={toggleExpand}>
      <span class="toggle-chevron" class:open={expanded}>
        <ChevronRightIcon size="10" strokeWidth="2.4" aria-hidden="true" />
      </span>
      <span class="toggle-label">{m.subagent_inline_label()}</span>
      {#if agentLabel}
        <span class="toggle-meta">{agentLabel}</span>
      {/if}
      {#if messageCountLabel}
        <span class="toggle-meta">{messageCountLabel}</span>
      {/if}
      <span class="toggle-session-id">{sessionId.slice(0, 12)}</span>
      {#if subagentTokenSummary}
        <span class="toggle-tokens">({subagentTokenSummary})</span>
      {/if}
      {#if subagentSession}
        {#if subagentModel}
          <span class="toggle-model" title={subagentModel}>{subagentModel}</span>
        {/if}
      {/if}
    </button>
    <a
      href={router.buildSessionHref(sessionId)}
      class="open-session-link"
      onclick={openAsSession}
      title={m.subagent_inline_open_as_full_session()}
    >
      {m.subagent_inline_open_session()}
      <ExternalLinkIcon size="10" strokeWidth="2.2" aria-hidden="true" />
    </a>
  </div>

  {#if expanded}
    <div class="subagent-messages">
      {#if loading}
        <div class="subagent-status">{m.subagent_inline_loading()}</div>
      {:else if error}
        <div class="subagent-status subagent-error">{error}</div>
      {:else if messages && messages.length > 0}
        {#each messages as message}
          <MessageContent
            {message}
            session={tokenSourceSession}
            isSubagentContext={true}
          />
        {/each}
      {:else if messages}
        <div class="subagent-status">{m.subagent_inline_no_messages()}</div>
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
    display: inline-flex;
    align-items: center;
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
    display: inline-flex;
    align-items: center;
    gap: 4px;
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

  .toggle-model {
    font-size: 10px;
    color: var(--text-muted);
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

  /* Inner messages already get their role identity from the avatar
     and name; the green rail of .subagent-messages already groups
     them. The per-message left rail is redundant and reads as
     toothy in this context. */
  .subagent-messages :global(.message) {
    border-left: none;
    border-radius: var(--radius-md);
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
