<script lang="ts">
  import {
    sessions,
    type SessionGroupInput,
  } from "../../stores/sessions.svelte.js";
  import { starred } from "../../stores/starred.svelte.js";
  import { formatRelativeTime, truncate } from "../../utils/format.js";
  import { agentColor as getAgentColor, agentLabel } from "../../utils/agents.js";
  import {
    normalizeMessagePreview,
    previewMessage,
  } from "../../utils/messages.js";
  import {
    ChevronDownIcon,
    ChevronRightIcon,
    StarIcon,
    UserRoundIcon,
    UsersRoundIcon,
  } from "../../icons.js";
  import StatusDot from "../common/StatusDot.svelte";

  interface Props {
    session: SessionGroupInput;
    continuationCount?: number;
    groupSessionIds?: string[];
    /** Optional full session objects in this row's group. When
     * provided, the status dot uses the group's freshest activity
     * for the time-based tier — so a parent in tool_call_pending
     * with a subagent currently writing stays green/working
     * instead of decaying to stale. The parent's parser status
     * still wins over freshness for awaiting_user (a fork running
     * in parallel doesn't change that the parent is waiting). */
    groupSessions?: SessionGroupInput[];
    hideAgent?: boolean;
    hideProject?: boolean;
    /** Render in compact mode (smaller, used for child sessions). */
    compact?: boolean;
    /** Whether this item's continuation chain is expanded. */
    expanded?: boolean;
    /** Callback to toggle continuation chain expand/collapse. */
    onToggleExpand?: () => void;
    /** Nesting depth: 0 = root, 1 = child, 2 = grandchild. */
    depth?: number;
    /** Whether this is the last sibling at its depth level. */
    isLastChild?: boolean;
    /** Whether the group contains subagent children. */
    hasSubagents?: boolean;
    /** Whether the group contains teammate children. */
    hasTeammates?: boolean;
  }

  let {
    session,
    continuationCount = 1,
    groupSessionIds,
    groupSessions,
    hideAgent = false,
    hideProject = false,
    compact = false,
    expanded = false,
    onToggleExpand,
    depth = 0,
    isLastChild = false,
    hasSubagents = false,
    hasTeammates = false,
  }: Props = $props();

  let isActive = $derived.by(() => {
    const aid = sessions.activeSessionId;
    if (!aid) return false;
    // Direct match (child rows, or root with no group).
    if (aid === session.id) return true;
    // Parent row: only highlight when the chain is collapsed
    // (i.e. the child is not visible as its own row).
    if (groupSessionIds && !expanded) {
      return groupSessionIds.includes(aid);
    }
    return false;
  });

  let agentColor = $derived(
    getAgentColor(session.agent),
  );

  let showMachine = $derived(
    !compact &&
    !!session.machine &&
    session.machine !== "local",
  );

  /** Whether this session is a team member (received a <teammate-message>). */
  let isTeamSession = $derived(
    session.is_teammate
      ?? session.first_message?.includes("<teammate-message")
      ?? false,
  );

  /**
   * Clean display name: for teammate sessions, extract the unique task
   * description (e.g. "Task #2: Align ROADMAP.md...") instead of the
   * repetitive "You are a teammate on..." boilerplate.
   */
  let displayLabel = $derived.by((): { text: string; isShell: boolean } => {
    const name = session.display_name ?? null;
    if (name) {
      return { text: truncate(name, 50), isShell: false };
    }
    let msg = session.first_message ?? "";
    if (msg.includes("<teammate-message")) {
      msg = msg
        .replace(/<teammate-message[^>]*>/g, "")
        .replace(/<\/teammate-message>/g, "")
        .trim();
      // Extract "Task #N: description" from the boilerplate.
      const taskMatch = msg.match(/Task\s*#?\d+[:\s]+(.+?)(?:\s+\d+\.|$)/s);
      if (taskMatch) {
        return { text: truncate(taskMatch[1]!.trim(), 50), isShell: false };
      }
      // Fallback: skip the "You are a teammate on ..." boilerplate.
      const afterTeam = msg.match(/team[."]\s*[^.]*?[.]\s+(.+)/s)
        ?? msg.match(/You are a teammate[^.]*\.\s+(.+)/s);
      if (afterTeam) {
        return { text: truncate(afterTeam[1]!.trim(), 50), isShell: false };
      }
    }
    const p = previewMessage(msg);
    if (p.text) return { text: truncate(p.text, 50), isShell: p.isShell };
    return { text: truncate(session.project, 30), isShell: false };
  });

  let timeStr = $derived(
    formatRelativeTime(session.ended_at ?? session.started_at),
  );

  let isStarred = $derived(starred.isStarred(session.id));

  let childCount = $derived(
    continuationCount > 1 ? continuationCount - 1 : 0,
  );

  let hasChildren = $derived(childCount > 0 && !!onToggleExpand);

  /** Whether this is an orphaned teammate showing at root level. */
  let isOrphanedTeammate = $derived(
    depth === 0 && isTeamSession,
  );

  function handleStar(e: MouseEvent) {
    e.stopPropagation();
    starred.toggle(session.id);
  }

  function handleToggle(e: MouseEvent) {
    e.stopPropagation();
    onToggleExpand?.();
  }

  // Context menu state
  let contextMenu: { x: number; y: number } | null = $state(null);

  // Rename state
  let renaming = $state(false);
  let renameValue = $state("");
  let renameInput: HTMLInputElement | undefined = $state(undefined);

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
    renameValue =
      session.display_name
      ?? normalizeMessagePreview(session.first_message)
      ?? "";
    renaming = true;
    closeContextMenu();
    requestAnimationFrame(() => renameInput?.select());
  }

  async function submitRename() {
    if (!renaming) return;
    renaming = false;
    const name = renameValue.trim() || null;
    try {
      await sessions.renameSession(session.id, name);
    } catch {
      // silently fail
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

  $effect(() => {
    if (!contextMenu) return;
    function handler() {
      contextMenu = null;
    }
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
<!-- svelte-ignore a11y_no_static_element_interactions -->
<div
  class="session-item"
  class:active={isActive}
  class:compact
  class:depth-1={depth === 1}
  class:depth-2={depth >= 2}
  class:orphaned-teammate={isOrphanedTeammate}
  data-session-id={session.id}
  role="button"
  tabindex="0"
  style:padding-left="{8 + depth * 16}px"
  onclick={() => sessions.selectSession(session.id)}
  onkeydown={(e) => { if (e.target !== e.currentTarget) return; if (e.key === "Enter" || e.key === " ") { e.preventDefault(); sessions.selectSession(session.id); } }}
  oncontextmenu={handleContextMenu}
>
  <!-- Tree expand/collapse or connector -->
  {#if hasChildren}
    <button
      type="button"
      class="tree-toggle"
      onclick={handleToggle}
      tabindex="-1"
      aria-label={expanded ? "Collapse" : "Expand"}
    >
      {#if expanded}
        <ChevronDownIcon class="tree-arrow" size="10" strokeWidth="2.5" aria-hidden="true" />
      {:else}
        <ChevronRightIcon class="tree-arrow" size="10" strokeWidth="2.5" aria-hidden="true" />
      {/if}
    </button>
  {:else if depth > 0}
    <span class="tree-dash"></span>
  {:else}
    <span class="tree-spacer"></span>
  {/if}

  <StatusDot {session} {groupSessions} size={6} />


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
      <div
        class="session-name"
        class:shell={displayLabel.isShell}
        ondblclick={handleDblClick}
      >
        {#if displayLabel.isShell}
          <code>{displayLabel.text}</code>
        {:else}
          {displayLabel.text}
        {/if}
      </div>
    {/if}
    <div class="session-meta">
      {#if !hideProject}
        <span class="session-project">{session.project}</span>
      {/if}
      <span class="session-time">{timeStr}</span>
      <span class="session-count">{session.user_message_count}</span>
      {#if hasSubagents}
        <UserRoundIcon class="group-hint-icon" size="9" strokeWidth="2" aria-hidden="true" />
      {/if}
      {#if hasTeammates}
        <UsersRoundIcon class="group-hint-icon" size="11" strokeWidth="2" aria-hidden="true" />
      {/if}
      {#if childCount > 0 && !onToggleExpand}
        <span class="continuation-badge">x{continuationCount}</span>
      {/if}
    </div>
  </div>

  {#if !compact}
    <button
      class="star-btn"
      class:starred={isStarred}
      onclick={handleStar}
      title={isStarred ? "Unstar session" : "Star session"}
      aria-label={isStarred ? "Unstar session" : "Star session"}
    >
      {#if isStarred}
        <StarIcon size="12" fill="currentColor" strokeWidth="0" aria-hidden="true" />
      {:else}
        <StarIcon size="12" strokeWidth="1.4" aria-hidden="true" />
      {/if}
    </button>
  {/if}
  {#if !compact && (!hideAgent || showMachine)}
    <div class="side-meta">
      {#if !hideAgent}
        <span class="agent-tag" style:color={agentColor}>{agentLabel(session.agent)}</span>
      {/if}
      {#if showMachine}
        <span class="machine-tag" title={session.machine}>
          {truncate(session.machine, 18)}
        </span>
      {/if}
    </div>
  {/if}
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
    gap: 5px;
    width: 100%;
    height: 42px;
    padding: 0 10px;
    padding-right: 10px;
    text-align: left;
    transition: background 0.1s;
    user-select: none;
    -webkit-user-select: none;
    cursor: pointer;
    position: relative;
  }

  .session-item.compact {
    height: 34px;
    gap: 4px;
  }

  .session-item.depth-1,
  .session-item.depth-2 {
    background: transparent;
  }

  .session-item:hover {
    background: var(--bg-surface-hover);
  }

  .session-item.active {
    background: var(--bg-surface-hover);
  }

  /* Orphaned teammate at root level — dim it slightly */
  .session-item.orphaned-teammate {
    opacity: 0.6;
  }

  /* Tree toggle (▶/▼) */
  .tree-toggle {
    all: unset;
    display: flex;
    align-items: center;
    justify-content: center;
    width: 16px;
    height: 100%;
    flex-shrink: 0;
    cursor: pointer;
    color: var(--text-muted);
    transition: color 0.1s;
  }

  .tree-toggle:hover {
    color: var(--text-primary);
  }

  :global(.tree-arrow) {
    flex-shrink: 0;
  }

  /* Spacer for leaf nodes — same width as toggle to align text */
  .tree-dash {
    width: 16px;
    flex-shrink: 0;
  }

  /* Empty spacer for root items without children */
  .tree-spacer {
    width: 16px;
    flex-shrink: 0;
  }

  .side-meta {
    display: flex;
    flex-direction: column;
    align-items: flex-end;
    gap: 3px;
    min-width: 0;
    flex-shrink: 0;
    margin-left: 4px;
  }

  /* Agent tag on the right side */
  .agent-tag {
    font-size: 8px;
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: 0.02em;
    line-height: 1;
    opacity: 0.7;
    white-space: nowrap;
    max-width: 52px;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .machine-tag {
    font-size: 9px;
    line-height: 1;
    color: var(--text-muted);
    opacity: 0.9;
    white-space: nowrap;
    max-width: 74px;
    overflow: hidden;
    text-overflow: ellipsis;
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

  .session-name.shell > code {
    font-family: var(--font-mono);
    font-size: 0.95em;
    background: transparent;
    border: none;
    padding: 0;
    color: var(--text-secondary);
    letter-spacing: 0;
  }

  .compact .session-name {
    font-size: 11px;
    color: var(--text-secondary);
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

  .compact .session-meta {
    font-size: 9px;
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

  :global(.group-hint-icon) {
    flex-shrink: 0;
    color: var(--text-muted);
    opacity: 0.5;
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
