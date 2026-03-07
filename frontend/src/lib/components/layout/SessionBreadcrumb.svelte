<script lang="ts">
  import type { Session } from "../../api/types.js";
  import { copyToClipboard } from "../../utils/clipboard.js";
  import { agentColor } from "../../utils/agents.js";
  import { sessions } from "../../stores/sessions.svelte.js";

  interface Props {
    session: Session | undefined;
    onBack: () => void;
  }

  let { session, onBack }: Props = $props();
  let copiedSessionId = $state("");
  let menuOpen = $state(false);
  let renaming = $state(false);
  let renameValue = $state("");
  let renameInput = $state<HTMLInputElement | null>(null);
  let menuBtnEl = $state<HTMLButtonElement | null>(null);
  let menuEl = $state<HTMLDivElement | null>(null);

  function sessionDisplayId(id: string): string {
    const idx = id.indexOf(":");
    return idx >= 0 ? id.slice(idx + 1) : id;
  }

  async function copySessionId(
    rawId: string,
    sessionId: string,
  ) {
    const ok = await copyToClipboard(rawId);
    if (!ok) return;

    copiedSessionId = sessionId;
    setTimeout(() => {
      if (copiedSessionId === sessionId) copiedSessionId = "";
    }, 1500);
  }

  function toggleMenu() {
    menuOpen = !menuOpen;
  }

  function closeMenu() {
    menuOpen = false;
  }

  function startRename() {
    if (!session) return;
    renameValue =
      session.display_name ?? session.first_message ?? "";
    renaming = true;
    closeMenu();
    requestAnimationFrame(() => renameInput?.select());
  }

  async function submitRename() {
    if (!renaming || !session) return;
    renaming = false;
    const name = renameValue.trim() || null;
    try {
      await sessions.renameSession(session.id, name);
    } catch {
      // name reverts in UI
    }
  }

  function cancelRename() {
    renaming = false;
  }

  async function handleDelete() {
    if (!session) return;
    closeMenu();
    try {
      await sessions.deleteSession(session.id);
    } catch {
      // silently fail
    }
  }

  function handleKeydown(e: KeyboardEvent) {
    if (e.key === "Escape") {
      if (renaming) {
        cancelRename();
      } else if (menuOpen) {
        closeMenu();
      }
    }
  }

  function handleClickOutside(e: MouseEvent) {
    if (!menuOpen) return;
    const target = e.target as Node;
    if (
      menuEl?.contains(target) ||
      menuBtnEl?.contains(target)
    ) {
      return;
    }
    closeMenu();
  }
</script>

<svelte:document
  onkeydown={handleKeydown}
  onclick={handleClickOutside}
/>

<div class="session-breadcrumb">
  <button class="breadcrumb-link" onclick={onBack}>
    Sessions
  </button>
  <span class="breadcrumb-sep">/</span>
  {#if renaming}
    <input
      class="rename-input"
      type="text"
      bind:value={renameValue}
      bind:this={renameInput}
      onkeydown={(e) => {
        if (e.key === "Enter") submitRename();
        if (e.key === "Escape") cancelRename();
      }}
      onblur={submitRename}
    />
  {:else}
    <span class="breadcrumb-current">
      {session?.display_name || session?.project || ""}
    </span>
  {/if}
  {#if session}
    <span class="breadcrumb-meta">
      <span
        class="agent-badge"
        style:background={agentColor(session.agent)}
      >{session.agent}</span>
      {#if session.started_at}
        <span class="session-time">
          {new Date(session.started_at).toLocaleDateString(
            undefined,
            { month: "short", day: "numeric" },
          )}
          {new Date(session.started_at).toLocaleTimeString(
            undefined,
            { hour: "2-digit", minute: "2-digit" },
          )}
        </span>
      {/if}
      {#if session.id}
        {@const rawId = sessionDisplayId(session.id)}
        <button
          class="session-id"
          title={rawId}
          onclick={() => copySessionId(rawId, session.id)}
        >
          {copiedSessionId === session.id
            ? "Copied!"
            : rawId.slice(0, 8)}
        </button>
      {/if}
      <div class="actions-wrapper">
        <button
          class="actions-btn"
          title="Session actions"
          bind:this={menuBtnEl}
          onclick={toggleMenu}
        >
          <svg
            width="14"
            height="14"
            viewBox="0 0 16 16"
            fill="currentColor"
          >
            <circle cx="8" cy="2.5" r="1.5" />
            <circle cx="8" cy="8" r="1.5" />
            <circle cx="8" cy="13.5" r="1.5" />
          </svg>
        </button>
        {#if menuOpen}
          <div class="actions-menu" bind:this={menuEl}>
            <button
              class="actions-menu-item"
              onclick={startRename}
            >
              Rename
            </button>
            <button
              class="actions-menu-item danger"
              onclick={handleDelete}
            >
              Delete
            </button>
          </div>
        {/if}
      </div>
    </span>
  {/if}
</div>

<style>
  .session-breadcrumb {
    display: flex;
    align-items: center;
    gap: 6px;
    height: 32px;
    padding: 0 14px;
    border-bottom: 1px solid var(--border-muted);
    flex-shrink: 0;
    font-size: 11px;
    color: var(--text-muted);
  }

  .breadcrumb-link {
    color: var(--text-muted);
    font-size: 11px;
    font-weight: 500;
    cursor: pointer;
    transition: color 0.12s;
  }

  .breadcrumb-link:hover {
    color: var(--accent-blue);
  }

  .breadcrumb-sep {
    opacity: 0.3;
    font-size: 10px;
  }

  .breadcrumb-current {
    color: var(--text-primary);
    font-weight: 500;
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
    flex: 1;
    min-width: 0;
  }

  .rename-input {
    flex: 1;
    min-width: 0;
    font-size: 11px;
    font-weight: 500;
    color: var(--text-primary);
    background: var(--bg-surface);
    border: 1px solid var(--accent-blue);
    border-radius: 4px;
    padding: 2px 6px;
    outline: none;
    font-family: inherit;
  }

  .breadcrumb-meta {
    display: flex;
    align-items: center;
    gap: 6px;
    margin-left: auto;
    flex-shrink: 0;
  }

  .agent-badge {
    font-size: 9px;
    font-weight: 600;
    padding: 1px 6px;
    border-radius: 8px;
    text-transform: uppercase;
    letter-spacing: 0.03em;
    color: white;
    flex-shrink: 0;
    background: var(--text-muted);
  }

  .session-time {
    font-size: 10px;
    color: var(--text-muted);
    font-variant-numeric: tabular-nums;
    white-space: nowrap;
    flex-shrink: 0;
  }

  .session-id {
    font-size: 10px;
    font-family: "SF Mono", "Menlo", "Consolas", monospace;
    color: var(--text-muted);
    cursor: pointer;
    padding: 1px 5px;
    border-radius: 4px;
    background: var(--bg-tertiary);
    transition: color 0.15s, background 0.15s;
    white-space: nowrap;
    flex-shrink: 0;
  }

  .session-id:hover {
    color: var(--text-secondary);
    background: var(--bg-hover);
  }

  .actions-wrapper {
    position: relative;
  }

  .actions-btn {
    display: flex;
    align-items: center;
    justify-content: center;
    width: 22px;
    height: 22px;
    border: none;
    border-radius: var(--radius-sm, 4px);
    background: transparent;
    color: var(--text-muted);
    cursor: pointer;
    transition: background 0.15s, color 0.15s;
    flex-shrink: 0;
  }

  .actions-btn:hover {
    background: var(--bg-surface-hover);
    color: var(--text-secondary);
  }

  .actions-menu {
    position: absolute;
    top: 100%;
    right: 0;
    z-index: 9999;
    margin-top: 4px;
    background: var(--bg-surface);
    border: 1px solid var(--border-default);
    border-radius: 6px;
    box-shadow: 0 4px 16px rgba(0, 0, 0, 0.25);
    padding: 4px 0;
    min-width: 120px;
  }

  .actions-menu-item {
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

  .actions-menu-item:hover {
    background: var(--bg-surface-hover);
  }

  .actions-menu-item.danger {
    color: var(--accent-red, #e55);
  }

  .actions-menu-item.danger:hover {
    background: color-mix(
      in srgb,
      var(--accent-red, #e55) 10%,
      transparent
    );
  }
</style>
