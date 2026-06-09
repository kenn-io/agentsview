<script lang="ts">
  import {
    CheckIcon,
    CopyIcon,
    ExternalLinkIcon,
    PinIcon,
    XIcon,
  } from "../../icons.js";
  import { pins } from "../../stores/pins.svelte.js";
  import { sessions } from "../../stores/sessions.svelte.js";
  import { router } from "../../stores/router.svelte.js";
  import { ui } from "../../stores/ui.svelte.js";
  import { formatRelativeTime, truncate } from "../../utils/format.js";
  import { renderMarkdown } from "../../utils/markdown.js";
  import { copyToClipboard } from "../../utils/clipboard.js";
  import { normalizeMessagePreview } from "../../utils/messages.js";
  $effect(() => {
    pins.loadAll(sessions.filters.project || undefined);
  });

  /** Set of expanded pin IDs. */
  let expanded: Set<number> = $state(new Set());

  function toggleExpand(pinId: number) {
    const next = new Set(expanded);
    if (next.has(pinId)) next.delete(pinId);
    else next.add(pinId);
    expanded = next;
  }

  function navigateToPin(sessionId: string, ordinal: number) {
    ui.scrollToOrdinal(ordinal, sessionId);
    router.navigateToSession(sessionId);
  }

  function getSessionInfo(pin: import("../../api/types.js").PinnedMessage) {
    // Use backend-provided session metadata (available for all-pins
    // query). Fall back to the sessions store for older data.
    if (pin.session_project || pin.session_agent) {
      return {
        project: pin.session_project ?? "unknown",
        agent: pin.session_agent ?? "unknown",
        name:
          pin.session_display_name
          ?? (
            normalizeMessagePreview(pin.session_first_message)
            || pin.session_project
            || pin.session_id.slice(0, 12)
          ),
      };
    }
    const s = sessions.sessions.find((s) => s.id === pin.session_id);
    return s
      ? {
          project: s.project,
          agent: s.agent,
          // normalizeMessagePreview can return "" — use || not ?? to fall through.
          name:
            s.display_name
            ?? (normalizeMessagePreview(s.first_message) || s.project),
        }
      : {
          project: "unknown",
          agent: "unknown",
          name: pin.session_id.slice(0, 12) + "...",
        };
  }

  let copiedId: number | null = $state(null);

  async function handleCopy(pinId: number, content: string | null | undefined) {
    if (!content) return;
    const ok = await copyToClipboard(content);
    if (ok) {
      copiedId = pinId;
      setTimeout(() => { if (copiedId === pinId) copiedId = null; }, 1500);
    }
  }

  function previewContent(content: string | null | undefined): string {
    if (!content) return "";
    // Strip thinking tags and tool use markers for preview
    const cleaned = content
      .replace(/<antThinking>[\s\S]*?<\/antThinking>/g, "")
      .replace(/\[tool_use:.*?\]/g, "")
      .trim();
    return truncate(cleaned, 300);
  }
</script>

<div class="pinned-page">
  <div class="pinned-header">
    <PinIcon size="18" strokeWidth="2" class="pin-icon" aria-hidden="true" />
    <h2>Pinned Messages</h2>
    {#if pins.pins.length > 0}
      <span class="pin-count">{pins.pins.length}</span>
    {/if}
  </div>

  {#if pins.loading}
    <div class="loading-state">Loading pins...</div>
  {:else if pins.pins.length === 0 && sessions.filters.project}
    <div class="empty-state">
      <p class="empty-title">No pinned messages for this project</p>
      <p class="empty-desc">
        Try selecting a different project or clear the project filter.
      </p>
    </div>
  {:else if pins.pins.length === 0}
    <div class="empty-state">
      <PinIcon size="40" strokeWidth="1.6" class="empty-icon" aria-hidden="true" />
      <p class="empty-title">No pinned messages</p>
      <p class="empty-desc">
        Pin messages from any session by clicking the pin icon in the message header.
      </p>
    </div>
  {:else}
    <div class="pin-list">
      {#each pins.pins as pin (pin.id)}
        {@const info = getSessionInfo(pin)}
        {@const isExpanded = expanded.has(pin.id)}
        {@const preview = previewContent(pin.content)}
        {@const hasMore = (pin.content?.length ?? 0) > 300}
        <div class="pin-card" class:expanded={isExpanded}>
          <div class="pin-card-header">
            <span
              class="role-badge"
              class:user={pin.role === "user"}
              class:assistant={pin.role === "assistant"}
            >
              {pin.role === "user" ? "U" : "A"}
            </span>
            <span class="pin-agent">{info.agent}</span>
            <span class="pin-session-name">{truncate(info.name, 60)}</span>
            <span class="pin-ordinal">#{pin.ordinal}</span>
            <span class="pin-time">{formatRelativeTime(pin.created_at)}</span>
          </div>

          {#if preview}
            <div class="pin-content-wrap">
              {#if isExpanded && pin.content}
                <div class="pin-content-full markdown">
                  {@html renderMarkdown(pin.content)}
                </div>
              {:else}
                <div class="pin-content-preview">{preview}</div>
              {/if}
            </div>
          {/if}

          <div class="pin-card-footer">
            <button
              class="pin-card-meta"
              onclick={() => navigateToPin(pin.session_id, pin.ordinal)}
              title="Go to message"
            >
              <ExternalLinkIcon size="10" strokeWidth="2.2" aria-hidden="true" />
              <span>{info.project}</span>
            </button>
            <div class="pin-card-actions">
              {#if hasMore}
                <button
                  class="expand-btn"
                  onclick={() => toggleExpand(pin.id)}
                >
                  {isExpanded ? "Collapse" : "Expand"}
                </button>
              {/if}
              <button
                class="copy-btn"
                title="Copy message"
                onclick={() => handleCopy(pin.id, pin.content)}
              >
                {#if copiedId === pin.id}
                  <CheckIcon size="12" strokeWidth="2.4" aria-hidden="true" />
                {:else}
                  <CopyIcon size="12" strokeWidth="2" aria-hidden="true" />
                {/if}
              </button>
              <button
                class="unpin-btn"
                title="Unpin"
                onclick={() => pins.unpin(pin.session_id, pin.message_id)}
              >
                <XIcon size="12" strokeWidth="2.4" aria-hidden="true" />
              </button>
            </div>
          </div>
        </div>
      {/each}
    </div>
  {/if}
</div>

<style>
  .pinned-page {
    max-width: 1100px;
    margin: 0 auto;
    padding: 40px 24px;
  }

  .pinned-header {
    display: flex;
    align-items: center;
    gap: 10px;
    margin-bottom: 28px;
  }

  :global(.pin-icon) {
    color: var(--accent-blue);
  }

  .pinned-header h2 {
    font-size: 20px;
    font-weight: 600;
    color: var(--text-primary);
    margin: 0;
  }

  .pin-count {
    background: var(--accent-blue);
    color: white;
    font-size: 11px;
    font-weight: 600;
    padding: 1px 7px;
    border-radius: 10px;
  }

  .loading-state {
    text-align: center;
    color: var(--text-muted);
    padding: 40px 0;
    font-size: 13px;
  }

  .empty-state {
    text-align: center;
    padding: 60px 20px;
    color: var(--text-muted);
  }

  :global(.empty-icon) {
    opacity: 0.15;
    margin-bottom: 16px;
  }

  .empty-title {
    font-size: 16px;
    font-weight: 500;
    color: var(--text-secondary);
    margin: 0 0 6px;
  }

  .empty-desc {
    font-size: 13px;
    margin: 0;
  }

  .pin-list {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(340px, 1fr));
    gap: 12px;
  }

  .pin-card {
    background: var(--bg-surface);
    border: 1px solid var(--border-muted);
    border-radius: 8px;
    transition: border-color 0.15s;
  }

  .pin-card:hover {
    border-color: var(--border-default);
  }

  .pin-card-header {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 10px 14px 0;
  }

  .role-badge {
    width: 18px;
    height: 18px;
    border-radius: 50%;
    display: flex;
    align-items: center;
    justify-content: center;
    font-size: 9px;
    font-weight: 700;
    color: white;
    flex-shrink: 0;
    line-height: 1;
    background: var(--accent-purple);
  }

  .role-badge.user {
    background: var(--accent-blue);
  }

  .pin-agent {
    font-size: 9px;
    font-weight: 600;
    text-transform: uppercase;
    color: var(--accent-purple);
    letter-spacing: 0.03em;
    flex-shrink: 0;
  }

  .pin-session-name {
    font-size: 12px;
    font-weight: 500;
    color: var(--text-primary);
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
    flex: 1;
    min-width: 0;
  }

  .pin-ordinal {
    font-size: 10px;
    color: var(--text-muted);
    flex-shrink: 0;
  }

  .pin-time {
    font-size: 10px;
    color: var(--text-muted);
    flex-shrink: 0;
  }

  .pin-content-wrap {
    padding: 8px 14px;
  }

  .pin-content-preview {
    font-size: 12px;
    line-height: 1.6;
    color: var(--text-secondary);
    white-space: pre-wrap;
    word-break: break-word;
  }

  .pin-content-full {
    font-size: 13px;
    line-height: 1.65;
    color: var(--text-primary);
    word-wrap: break-word;
    max-height: 500px;
    overflow-y: auto;
  }

  /* Markdown prose inside expanded pins */
  .pin-content-full :global(p) {
    margin: 0.4em 0;
  }
  .pin-content-full :global(p:first-child) {
    margin-top: 0;
  }
  .pin-content-full :global(p:last-child) {
    margin-bottom: 0;
  }
  .pin-content-full :global(code) {
    font-family: var(--font-mono);
    font-size: 0.85em;
    background: var(--bg-inset);
    border: 1px solid var(--border-muted);
    border-radius: 4px;
    padding: 0.15em 0.4em;
  }
  .pin-content-full :global(pre) {
    background: var(--code-bg);
    color: var(--code-text);
    border-radius: var(--radius-md);
    padding: 10px 14px;
    overflow-x: auto;
    margin: 0.4em 0;
  }
  .pin-content-full :global(pre code) {
    background: none;
    border: none;
    padding: 0;
    font-size: 12px;
    color: inherit;
  }
  .pin-content-full :global(ul),
  .pin-content-full :global(ol) {
    padding-left: 1.4em;
    margin: 0.4em 0;
  }
  .pin-content-full :global(blockquote) {
    border-left: 3px solid var(--border-default);
    margin: 0.4em 0;
    padding: 0.2em 0.8em;
    color: var(--text-secondary);
  }
  .pin-content-full :global(a) {
    color: var(--accent-blue);
    text-decoration: none;
  }
  .pin-content-full :global(a:hover) {
    text-decoration: underline;
  }

  .pin-card-footer {
    display: flex;
    align-items: center;
    justify-content: space-between;
    padding: 6px 14px 10px;
  }

  .pin-card-meta {
    display: flex;
    align-items: center;
    gap: 5px;
    font-size: 10px;
    color: var(--text-muted);
    background: none;
    border: none;
    cursor: pointer;
    padding: 3px 8px;
    border-radius: var(--radius-sm);
    transition: background 0.12s, color 0.12s;
  }

  .pin-card-meta:hover {
    background: var(--bg-surface-hover);
    color: var(--accent-blue);
  }

  .pin-card-actions {
    display: flex;
    align-items: center;
    gap: 4px;
  }

  .expand-btn {
    font-size: 10px;
    font-weight: 500;
    color: var(--accent-blue);
    background: none;
    border: none;
    cursor: pointer;
    padding: 3px 8px;
    border-radius: var(--radius-sm);
    transition: background 0.12s;
  }

  .expand-btn:hover {
    background: color-mix(in srgb, var(--accent-blue) 8%, transparent);
  }

  .unpin-btn {
    display: flex;
    align-items: center;
    justify-content: center;
    width: 26px;
    height: 26px;
    border: none;
    border-radius: var(--radius-sm);
    background: transparent;
    color: var(--text-muted);
    cursor: pointer;
    flex-shrink: 0;
    transition: background 0.15s, color 0.15s;
  }

  .unpin-btn:hover {
    background: color-mix(in srgb, var(--accent-red, #e55) 12%, transparent);
    color: var(--accent-red, #e55);
  }

  .copy-btn {
    display: flex;
    align-items: center;
    justify-content: center;
    width: 26px;
    height: 26px;
    border: none;
    border-radius: var(--radius-sm);
    background: transparent;
    color: var(--text-muted);
    cursor: pointer;
    flex-shrink: 0;
    transition: background 0.15s, color 0.15s;
  }

  .copy-btn:hover {
    background: var(--bg-surface-hover);
    color: var(--text-secondary);
  }

  /* Make expanded cards span full width in grid */
  .pin-card.expanded {
    grid-column: 1 / -1;
  }
</style>
