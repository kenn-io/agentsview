<script lang="ts">
  import type { Message } from "../../api/types.js";
  import {
    parseContent,
    enrichSegments,
  } from "../../utils/content-parser.js";
  import { formatTimestamp } from "../../utils/format.js";
  import { copyToClipboard } from "../../utils/clipboard.js";
  import ThinkingBlock from "./ThinkingBlock.svelte";
  import ToolBlock from "./ToolBlock.svelte";
  import CodeBlock from "./CodeBlock.svelte";
  import { ui } from "../../stores/ui.svelte.js";
  import { pins } from "../../stores/pins.svelte.js";
  import { renderMarkdown } from "../../utils/markdown.js";

  interface Props {
    message: Message;
  }

  let { message }: Props = $props();

  let copied = $state(false);

  let segments = $derived(
    enrichSegments(
      parseContent(message.content, message.has_tool_use),
      message.tool_calls,
    ),
  );

  let isUser = $derived(message.role === "user");

  /** Whether the text (prose) segments for this role should render. */
  let showText = $derived(
    ui.isBlockVisible(isUser ? "user" : "assistant"),
  );

  let accentColor = $derived(
    isUser ? "var(--accent-blue)" : "var(--accent-purple)",
  );

  let roleBg = $derived(
    isUser ? "var(--user-bg)" : "var(--assistant-bg)",
  );

  let pinned = $derived(pins.isPinned(message.id));
  let pinFeedback = $state("");

  let copyTimer: ReturnType<typeof setTimeout>;
  let pinTimer: ReturnType<typeof setTimeout>;

  async function handleCopy() {
    const ok = await copyToClipboard(message.content);
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
      {isUser ? "U" : "A"}
    </span>
    <span
      class="role-label"
      style:color={accentColor}
    >
      {isUser ? "User" : "Assistant"}
    </span>
    <span class="timestamp">
      {formatTimestamp(message.timestamp)}
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
  </div>

  <div class="message-body">
    {#each segments as segment}
      {#if segment.type === "thinking"}
        {#if ui.isBlockVisible("thinking")}
          <ThinkingBlock content={segment.content} />
        {/if}
      {:else if segment.type === "tool"}
        {#if ui.isBlockVisible("tool")}
          <ToolBlock
            content={segment.content}
            label={segment.label}
            toolCall={segment.toolCall}
          />
        {/if}
      {:else if segment.type === "code"}
        {#if ui.isBlockVisible("code")}
          <CodeBlock content={segment.content} language={segment.label} />
        {/if}
      {:else}
        {#if showText}
          <div class="text-content markdown">
            {@html renderMarkdown(segment.content)}
          </div>
        {/if}
      {/if}
    {/each}
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
