<script lang="ts">
  import type { Session } from "../../api/types.js";
  import { sessions, isRecentlyActive } from "../../stores/sessions.svelte.js";
  import { starred } from "../../stores/starred.svelte.js";
  import { formatRelativeTime, truncate } from "../../utils/format.js";
  import { agentColor as getAgentColor } from "../../utils/agents.js";

  interface Props {
    session: Session;
    continuationCount?: number;
    groupSessionIds?: string[];
    hideAgent?: boolean;
  }

  let {
    session,
    continuationCount = 1,
    groupSessionIds,
    hideAgent = false,
  }: Props = $props();

  let isActive = $derived(
    groupSessionIds
      ? groupSessionIds.includes(
          sessions.activeSessionId ?? "",
        )
      : sessions.activeSessionId === session.id,
  );

  let recentlyActive = $derived(isRecentlyActive(session));

  let agentColor = $derived(
    getAgentColor(session.agent),
  );

  let displayName = $derived(
    session.display_name
      ? truncate(session.display_name, 50)
      : session.first_message
        ? truncate(session.first_message, 50)
        : truncate(session.project, 30),
  );

  let timeStr = $derived(
    formatRelativeTime(session.ended_at ?? session.started_at),
  );

  let isStarred = $derived(starred.isStarred(session.id));

  function handleStar(e: MouseEvent) {
    e.stopPropagation();
    starred.toggle(session.id);
  }

  // Context menu state
  let contextMenu: { x: number; y: number } | null = $state(null);

  // Rename state
  let renaming = $state(false);
  let renameValue = $state("");
  let renameInput: HTMLInputElement | undefined = $state(undefined);

  /**
   * Svelte action: portal — moves a DOM node to document.body,
   * escaping overflow/transform stacking contexts.
   */
  function portal(node: HTMLElement) {
    document.body.appendChild(node);
    return {
      destroy() {
        node.remove();
      },
    };
  }

  function handleContextMenu(e: MouseEvent) {
    e.preventDefault();
    contextMenu = { x: e.clientX, y: e.clientY };
  }

  function closeContextMenu() {
    contextMenu = null;
  }

  function startRename() {
    renameValue = session.display_name ?? session.first_message ?? "";
    renaming = true;
    closeContextMenu();
    requestAnimationFrame(() => renameInput?.select());
  }

  async function submitRename() {
    // Guard against blur firing after Escape already cancelled.
    if (!renaming) return;
    renaming = false;
    const name = renameValue.trim() || null;
    try {
      await sessions.renameSession(session.id, name);
    } catch {
      // silently fail — name reverts in UI
    }
  }

  async function handleDelete() {
    closeContextMenu();
    try {
      await sessions.deleteSession(session.id);
    } catch {
      // silently fail
    }
  }

  function handleDblClick(e: MouseEvent) {
    e.preventDefault();
    startRename();
  }

  // Close context menu on outside click
  $effect(() => {
    if (!contextMenu) return;
    function handler() {
      contextMenu = null;
    }
    // Use setTimeout to avoid closing from the same right-click event.
    const id = setTimeout(() => {
      document.addEventListener("click", handler, { once: true });
      document.addEventListener("contextmenu", handler, {
        once: true,
      });
    }, 0);
    return () => {
      clearTimeout(id);
      document.removeEventListener("click", handler);
      document.removeEventListener("contextmenu", handler);
    };
  });

  // Close context menu on Escape
  $effect(() => {
    if (!contextMenu) return;
    function handler(e: KeyboardEvent) {
      if (e.key === "Escape") contextMenu = null;
    }
    document.addEventListener("keydown", handler);
    return () => document.removeEventListener("keydown", handler);
  });
</script>

<!-- svelte-ignore a11y_no_static_element_interactions -->
<div
  class="session-item"
  class:active={isActive}
  data-session-id={session.id}
  role="button"
  tabindex="0"
  onclick={() => sessions.selectSession(session.id)}
  onkeydown={(e) => { if (e.target !== e.currentTarget) return; if (e.key === "Enter" || e.key === " ") { e.preventDefault(); sessions.selectSession(session.id); } }}
  oncontextmenu={handleContextMenu}
>
  {#if !hideAgent}
    <div class="agent-indicator" style:--agent-c={agentColor}>
      <span
        class="agent-dot"
        class:recently-active={recentlyActive}
      ></span>
      <span class="agent-label">{session.agent}</span>
    </div>
  {:else if recentlyActive}
    <span class="agent-dot recently-active" style:background={agentColor}></span>
  {/if}
  <div class="session-info">
    {#if renaming}
      <!-- svelte-ignore a11y_autofocus -->
      <input
        bind:this={renameInput}
        bind:value={renameValue}
        class="rename-input"
        autofocus
        onclick={(e) => e.stopPropagation()}
        onblur={submitRename}
        onkeydown={(e) => {
          if (e.key === "Enter") {
            e.stopPropagation();
            submitRename();
          }
          if (e.key === "Escape") {
            e.stopPropagation();
            renaming = false;
          }
        }}
      />
    {:else}
      <!-- svelte-ignore a11y_no_static_element_interactions -->
      <div class="session-name" ondblclick={handleDblClick}>{displayName}</div>
    {/if}
    <div class="session-meta">
      <span class="session-project">{session.project}</span>
      <span class="session-time">{timeStr}</span>
      <span class="session-count">{session.user_message_count}</span>
      {#if continuationCount > 1}
        <span class="continuation-badge">x{continuationCount}</span>
      {/if}
    </div>
  </div>
  <button
    class="star-btn"
    class:starred={isStarred}
    onclick={handleStar}
    title={isStarred ? "Unstar session" : "Star session"}
    aria-label={isStarred ? "Unstar session" : "Star session"}
  >
    {#if isStarred}
      <svg width="12" height="12" viewBox="0 0 16 16" fill="currentColor" aria-hidden="true">
        <path d="M8 .25a.75.75 0 01.673.418l1.882 3.815 4.21.612a.75.75 0 01.416 1.279l-3.046 2.97.719 4.192a.75.75 0 01-1.088.791L8 12.347l-3.766 1.98a.75.75 0 01-1.088-.79l.72-4.194L.818 6.374a.75.75 0 01.416-1.28l4.21-.611L7.327.668A.75.75 0 018 .25z"/>
      </svg>
    {:else}
      <svg width="12" height="12" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.2" aria-hidden="true">
        <path d="M8 1.5l1.88 3.81 4.21.61-3.05 2.97.72 4.19L8 11.1l-3.77 1.98.72-4.19L1.9 5.92l4.21-.61L8 1.5z"/>
      </svg>
    {/if}
  </button>
</div>

{#if contextMenu}
  <div
    class="context-menu"
    use:portal
    style="left: {contextMenu.x}px; top: {contextMenu.y}px;"
  >
    <button class="context-menu-item" onclick={startRename}>
      Rename
    </button>
    <button class="context-menu-item danger" onclick={handleDelete}>
      Delete
    </button>
  </div>
{/if}

<style>
  .session-item {
    display: flex;
    align-items: center;
    gap: 10px;
    width: 100%;
    height: 42px;
    padding: 0 14px;
    text-align: left;
    border-left: 2px solid transparent;
    transition: background 0.1s;
    user-select: none;
    -webkit-user-select: none;
    cursor: pointer;
  }

  .session-item:hover {
    background: var(--bg-surface-hover);
  }

  .session-item.active {
    background: var(--bg-surface-hover);
    border-left-color: var(--accent-blue);
  }

  .agent-indicator {
    display: flex;
    align-items: center;
    gap: 4px;
    flex-shrink: 0;
    max-width: 72px;
  }

  .agent-dot {
    width: 5px;
    height: 5px;
    border-radius: 50%;
    background: var(--agent-c);
    flex-shrink: 0;
  }

  .agent-dot.recently-active {
    animation: pulse-glow 3s ease-in-out infinite;
    will-change: box-shadow;
  }

  @keyframes pulse-glow {
    0%,
    100% {
      box-shadow: 0 0 0 0 transparent;
    }
    50% {
      box-shadow: 0 0 6px 3px color-mix(
        in srgb,
        var(--accent-green) 40%,
        transparent
      );
    }
  }

  .agent-label {
    font-size: 9px;
    font-weight: 550;
    color: var(--agent-c);
    text-transform: capitalize;
    letter-spacing: 0.01em;
    line-height: 1;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .session-info {
    min-width: 0;
    flex: 1;
  }

  .session-name {
    font-size: 12px;
    font-weight: 450;
    color: var(--text-primary);
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
    line-height: 1.3;
    letter-spacing: -0.005em;
  }

  .rename-input {
    font-size: 12px;
    font-weight: 450;
    color: var(--text-primary);
    background: var(--bg-surface-hover);
    border: 1px solid var(--accent-blue);
    border-radius: 3px;
    padding: 1px 4px;
    width: 100%;
    outline: none;
    line-height: 1.3;
  }

  .session-meta {
    display: flex;
    align-items: center;
    gap: 6px;
    font-size: 10px;
    color: var(--text-muted);
    line-height: 1.3;
    letter-spacing: 0.01em;
  }

  .session-project {
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
    max-width: 100px;
  }

  .session-time {
    white-space: nowrap;
    flex-shrink: 0;
  }

  .session-count {
    white-space: nowrap;
    flex-shrink: 0;
  }

  .session-count::before {
    content: "\2022 ";
  }

  .continuation-badge {
    font-size: 9px;
    font-weight: 600;
    color: var(--accent-blue);
    white-space: nowrap;
    flex-shrink: 0;
  }

  .star-btn {
    width: 20px;
    height: 20px;
    display: flex;
    align-items: center;
    justify-content: center;
    border-radius: var(--radius-sm);
    color: var(--text-muted);
    flex-shrink: 0;
    opacity: 0;
    transition: opacity 0.12s, color 0.12s, background 0.12s;
  }

  .session-item:hover .star-btn,
  .session-item:focus-within .star-btn,
  .star-btn:focus-visible,
  .star-btn.starred {
    opacity: 1;
  }

  .star-btn:hover {
    background: var(--bg-surface-hover);
    color: var(--text-secondary);
  }

  .star-btn.starred {
    color: var(--accent-amber);
  }

  .star-btn.starred:hover {
    color: var(--accent-amber);
    background: var(--bg-surface-hover);
  }

  /* Context menu uses :global since it's portaled to document.body */
  :global(.context-menu) {
    position: fixed;
    z-index: 9999;
    background: var(--bg-surface);
    border: 1px solid var(--border-default);
    border-radius: 6px;
    box-shadow: 0 4px 16px rgba(0, 0, 0, 0.25);
    padding: 4px 0;
    min-width: 120px;
  }

  :global(.context-menu .context-menu-item) {
    display: block;
    width: 100%;
    padding: 6px 14px;
    font-size: 12px;
    color: var(--text-primary);
    text-align: left;
    background: none;
    border: none;
    cursor: pointer;
    font-family: var(--font-sans);
  }

  :global(.context-menu .context-menu-item:hover) {
    background: var(--bg-surface-hover);
  }

  :global(.context-menu .context-menu-item.danger) {
    color: var(--accent-red, #e55);
  }

  :global(.context-menu .context-menu-item.danger:hover) {
    background: color-mix(in srgb, var(--accent-red, #e55) 10%, transparent);
  }
</style>
