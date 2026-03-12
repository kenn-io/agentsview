<script lang="ts">
  import { onMount } from "svelte";
  import type { Session } from "../../api/types.js";
  import {
    resumeSession,
    openSession,
    getSessionDirectory,
    listOpeners,
    type Opener,
  } from "../../api/client.js";
  import { copyToClipboard } from "../../utils/clipboard.js";
  import { agentColor } from "../../utils/agents.js";
  import { sessions } from "../../stores/sessions.svelte.js";
  import {
    supportsResume,
    buildResumeCommand,
  } from "../../utils/resume.js";

  import { inSessionSearch } from "../../stores/inSessionSearch.svelte.js";

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
  let showOpenMenu = $state(false);
  let openers: Opener[] = $state([]);
  let openFeedback = $state("");
  let feedbackTimer: ReturnType<typeof setTimeout> | undefined;
  let sessionDir = $state<string | null>(null);

  onMount(() => {
    listOpeners()
      .then((res) => { openers = res.openers; })
      .catch(() => {});
  });

  $effect(() => {
    sessionDir = null;
    if (!session) return;
    const id = session.id;
    getSessionDirectory(id)
      .then(({ path }) => {
        if (session?.id === id) sessionDir = path || null;
      })
      .catch(() => {});
  });

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

  function showFeedback(msg: string) {
    openFeedback = msg;
    clearTimeout(feedbackTimer);
    feedbackTimer = setTimeout(() => { openFeedback = ""; }, 2000);
  }

  async function handleResumeIn(opener: Opener) {
    if (!session) return;
    showOpenMenu = false;
    try {
      const resp = await resumeSession(session.id, {
        opener_id: opener.id,
      });
      if (resp.launched) {
        showFeedback(`Resumed in ${resp.terminal ?? opener.name}`);
        return;
      }
      // Launch failed — fall back to clipboard copy.
      if (resp.command) {
        const ok = await copyToClipboard(resp.command);
        showFeedback(ok ? "Command copied!" : "Failed");
        return;
      }
    } catch {
      // Fall back to local command build.
    }
    const cmd = buildResumeCommand(session.agent, session.id);
    if (cmd) {
      const ok = await copyToClipboard(cmd);
      showFeedback(ok ? "Command copied!" : "Failed");
    } else {
      showFeedback("Not supported");
    }
  }

  async function handleCopyResumeCommand() {
    if (!session) return;
    showOpenMenu = false;
    try {
      const resp = await resumeSession(session.id, { command_only: true });
      if (resp.command) {
        const ok = await copyToClipboard(resp.command);
        showFeedback(ok ? "Command copied!" : "Failed");
        return;
      }
    } catch {
      // Fall back to local build.
    }
    const cmd = buildResumeCommand(session.agent, session.id);
    if (cmd) {
      const ok = await copyToClipboard(cmd);
      showFeedback(ok ? "Command copied!" : "Failed");
    } else {
      showFeedback("Not supported");
    }
  }

  async function handleCopyFilePath() {
    showOpenMenu = false;
    if (!sessionDir) {
      showFeedback("No path available");
      return;
    }
    const ok = await copyToClipboard(sessionDir);
    showFeedback(ok ? "Path copied!" : "Failed");
  }

  async function handleOpenIn(opener: Opener) {
    if (!session) return;
    showOpenMenu = false;
    try {
      await openSession(session.id, opener.id);
      showFeedback(`Opened in ${opener.name}`);
    } catch {
      showFeedback("Failed to open");
    }
  }

  async function handleResumeDefault() {
    if (!session) return;
    showOpenMenu = false;
    try {
      const resp = await resumeSession(session.id, {});
      if (resp.launched) {
        showFeedback(
          `Resumed in ${resp.terminal ?? "terminal"}`,
        );
        return;
      }
      if (resp.command) {
        const ok = await copyToClipboard(resp.command);
        showFeedback(ok ? "Command copied!" : "Failed");
        return;
      }
    } catch {
      // Fall back to local command build.
    }
    const cmd = buildResumeCommand(session.agent, session.id);
    if (cmd) {
      const ok = await copyToClipboard(cmd);
      showFeedback(ok ? "Command copied!" : "Failed");
    } else {
      showFeedback("Not supported");
    }
  }

  const canResume = $derived(
    session ? supportsResume(session.agent) : false,
  );

  const terminalOpeners = $derived(
    openers.filter((o) => o.kind === "terminal"),
  );

  const editorOpeners = $derived(
    openers.filter((o) => o.kind === "editor"),
  );

  const fileOpeners = $derived(
    openers.filter((o) => o.kind === "files"),
  );

  const showDropdown = $derived(
    canResume ||
    editorOpeners.length > 0 ||
    fileOpeners.length > 0 ||
    sessionDir !== null,
  );

  function handleKeydown(e: KeyboardEvent) {
    if (e.key === "Escape") {
      if (renaming) {
        cancelRename();
      } else if (menuOpen) {
        closeMenu();
      } else if (showOpenMenu) {
        showOpenMenu = false;
        e.preventDefault();
      }
      return;
    }
    if (showOpenMenu) {
      // Number key shortcuts (1-9) for quick selection.
      const num = parseInt(e.key);
      if (num >= 1 && num <= 9) {
        const idx = num - 1;
        if (idx < terminalOpeners.length) {
          e.preventDefault();
          handleResumeIn(terminalOpeners[idx]!);
        }
      }
    }
  }

  function handleClickOutside(e: MouseEvent) {
    const target = e.target as Node;
    // Close actions menu
    if (menuOpen) {
      if (
        !menuEl?.contains(target) &&
        !menuBtnEl?.contains(target)
      ) {
        closeMenu();
      }
    }
    // Close open menu
    if (!(target as HTMLElement).closest?.(".open-group")) {
      showOpenMenu = false;
    }
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
      {#if showDropdown}
        <span class="open-group">
          <button
            class="resume-btn"
            class:has-feedback={openFeedback !== ""}
            onclick={(e) => { e.stopPropagation(); showOpenMenu = !showOpenMenu; }}
            title={canResume ? "Resume session in terminal" : "Session actions"}
            aria-label={canResume ? "Resume session" : "Session actions"}
          >
            {#if openFeedback}
              <svg width="11" height="11" viewBox="0 0 16 16" fill="currentColor" aria-hidden="true">
                <path d="M13.78 4.22a.75.75 0 010 1.06l-7.25 7.25a.75.75 0 01-1.06 0L2.22 9.28a.75.75 0 011.06-1.06L6 10.94l6.72-6.72a.75.75 0 011.06 0z"/>
              </svg>
              {openFeedback}
            {:else}
              {canResume ? "Resume" : "Open"}
              <svg width="8" height="8" viewBox="0 0 16 16" fill="currentColor" aria-hidden="true">
                <path d="M4.427 7.427l3.396 3.396a.25.25 0 00.354 0l3.396-3.396A.25.25 0 0011.396 7H4.604a.25.25 0 00-.177.427z"/>
              </svg>
            {/if}
          </button>
          {#if showOpenMenu}
            <div class="open-menu">
              {#if canResume}
                {#each terminalOpeners as opener, i (opener.id)}
                  <button
                    class="open-menu-item"
                    onclick={() => handleResumeIn(opener)}
                  >
                    <span class="open-menu-num">{i + 1}</span>
                    <span class="open-menu-name">{opener.name}</span>
                  </button>
                {/each}
                <button class="open-menu-item" onclick={handleResumeDefault}>
                  <span class="open-menu-num">
                    <svg width="10" height="10" viewBox="0 0 16 16" fill="currentColor">
                      <path d="M0 1.75C0 .784.784 0 1.75 0h12.5C15.216 0 16 .784 16 1.75v3.585a.746.746 0 010 .83v3.585a.747.747 0 010 .83v3.67A1.75 1.75 0 0114.25 16H1.75A1.75 1.75 0 010 14.25V1.75zM1.5 6.5v3h13v-3h-13zm0 4.5v3.25c0 .138.112.25.25.25h12.5a.25.25 0 00.25-.25V11h-13zm13-5.5v-3.25a.25.25 0 00-.25-.25H1.75a.25.25 0 00-.25.25V5.5h13z"/>
                    </svg>
                  </span>
                  <span class="open-menu-name">Default terminal</span>
                </button>
                <div class="open-menu-divider"></div>
                <button class="open-menu-item" onclick={handleCopyResumeCommand}>
                  <span class="open-menu-num">
                    <svg width="10" height="10" viewBox="0 0 16 16" fill="currentColor">
                      <path d="M0 6.75C0 5.784.784 5 1.75 5h1.5a.75.75 0 010 1.5h-1.5a.25.25 0 00-.25.25v7.5c0 .138.112.25.25.25h7.5a.25.25 0 00.25-.25v-1.5a.75.75 0 011.5 0v1.5A1.75 1.75 0 019.25 16h-7.5A1.75 1.75 0 010 14.25v-7.5z"/>
                      <path d="M5 1.75C5 .784 5.784 0 6.75 0h7.5C15.216 0 16 .784 16 1.75v7.5A1.75 1.75 0 0114.25 11h-7.5A1.75 1.75 0 015 9.25v-7.5zm1.75-.25a.25.25 0 00-.25.25v7.5c0 .138.112.25.25.25h7.5a.25.25 0 00.25-.25v-7.5a.25.25 0 00-.25-.25h-7.5z"/>
                    </svg>
                  </span>
                  <span class="open-menu-name">Copy command</span>
                </button>
              {/if}
              <button class="open-menu-item" onclick={handleCopyFilePath}>
                <span class="open-menu-num">
                  <svg width="10" height="10" viewBox="0 0 16 16" fill="currentColor">
                    <path fill-rule="evenodd" d="M3.75 1.5a.25.25 0 00-.25.25v12.5c0 .138.112.25.25.25h9.5a.25.25 0 00.25-.25V6H9.75A1.75 1.75 0 018 4.25V1.5H3.75zm5.75.56v2.19c0 .138.112.25.25.25h2.19L9.5 2.06zM2 1.75C2 .784 2.784 0 3.75 0h5.086c.464 0 .909.184 1.237.513l3.414 3.414c.329.328.513.773.513 1.237v9.086A1.75 1.75 0 0112.25 16h-8.5A1.75 1.75 0 012 14.25V1.75z"/>
                  </svg>
                </span>
                <span class="open-menu-name">Copy directory path</span>
              </button>
              {#if editorOpeners.length > 0 || fileOpeners.length > 0}
                <div class="open-menu-divider"></div>
                <div class="open-menu-section">Open in</div>
                {#each editorOpeners as opener (opener.id)}
                  <button
                    class="open-menu-item"
                    onclick={() => handleOpenIn(opener)}
                  >
                    <span class="open-menu-num">
                      <svg width="10" height="10" viewBox="0 0 16 16" fill="currentColor">
                        <path d="M4.708 5.578L2.061 8.224l2.647 2.646-.708.708L.94 8.578V7.87L4 4.87l.708.708zm7.292 0l2.647 2.646-2.647 2.646.708.708L15.768 8.578V7.87L12.708 4.87 12 5.578z"/>
                        <path d="M5.708 13.578L9.258 2.578l.984.344-3.55 11-.984-.344z"/>
                      </svg>
                    </span>
                    <span class="open-menu-name">{opener.name}</span>
                  </button>
                {/each}
                {#each fileOpeners as opener (opener.id)}
                  <button
                    class="open-menu-item"
                    onclick={() => handleOpenIn(opener)}
                  >
                    <span class="open-menu-num">
                      <svg width="10" height="10" viewBox="0 0 16 16" fill="currentColor">
                        <path d="M1.75 1A1.75 1.75 0 000 2.75v10.5C0 14.216.784 15 1.75 15h12.5A1.75 1.75 0 0016 13.25v-8.5A1.75 1.75 0 0014.25 3H7.5a.25.25 0 01-.2-.1l-.9-1.2C6.07 1.26 5.55 1 5 1H1.75z"/>
                      </svg>
                    </span>
                    <span class="open-menu-name">{opener.name}</span>
                  </button>
                {/each}
              {/if}
            </div>
          {/if}
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
          class="find-btn"
          class:find-btn--active={inSessionSearch.isOpen}
          title="Find in session (/)"
          onclick={() => inSessionSearch.toggle()}
          aria-label="Find in session"
        >
          <svg width="13" height="13" viewBox="0 0 16 16" fill="currentColor">
            <path d="M11.742 10.344a6.5 6.5 0 10-1.397 1.398h-.001c.03.04.062.078.098.115l3.85 3.85a1 1 0 001.415-1.414l-3.85-3.85a1.007 1.007 0 00-.115-.099zm-5.242 1.156a5.5 5.5 0 110-11 5.5 5.5 0 010 11z"/>
          </svg>
        </button>
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

  .open-group {
    position: relative;
    display: flex;
    align-items: center;
    flex-shrink: 0;
  }

  .resume-btn {
    display: flex;
    align-items: center;
    gap: 4px;
    font-size: 10px;
    font-weight: 500;
    color: var(--text-muted);
    padding: 1px 8px;
    border-radius: 4px;
    background: var(--bg-tertiary);
    cursor: pointer;
    white-space: nowrap;
    flex-shrink: 0;
    transition: color 0.15s, background 0.15s;
  }

  .resume-btn:hover {
    color: var(--text-secondary);
    background: var(--bg-surface-hover);
  }

  .resume-btn.has-feedback {
    color: var(--accent-green, #2ea043);
  }

  .open-menu {
    position: absolute;
    top: 100%;
    right: 0;
    margin-top: 4px;
    background: var(--bg-primary);
    border: 1px solid var(--border-default);
    border-radius: 8px;
    padding: 4px;
    min-width: 200px;
    z-index: 100;
    box-shadow: 0 8px 24px rgba(0, 0, 0, 0.2);
  }

  .open-menu-item {
    display: flex;
    align-items: center;
    gap: 10px;
    width: 100%;
    padding: 6px 10px;
    font-size: 13px;
    color: var(--text-primary);
    border-radius: 5px;
    cursor: pointer;
    transition: background 0.1s;
  }

  .open-menu-item:hover {
    background: var(--bg-surface-hover);
  }

  .open-menu-num {
    display: flex;
    align-items: center;
    justify-content: center;
    width: 18px;
    font-size: 11px;
    font-weight: 500;
    color: var(--text-muted);
    flex-shrink: 0;
  }

  .open-menu-name {
    flex: 1;
    font-weight: 500;
  }

  .open-menu-divider {
    height: 1px;
    background: var(--border-muted);
    margin: 4px 0;
  }

  .open-menu-section {
    padding: 4px 10px 2px;
    font-size: 10px;
    font-weight: 600;
    color: var(--text-muted);
    text-transform: uppercase;
    letter-spacing: 0.04em;
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
    background: var(--bg-surface-hover);
  }

  .actions-wrapper {
    position: relative;
    display: flex;
    align-items: center;
    gap: 2px;
  }

  .find-btn {
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

  .find-btn:hover {
    background: var(--bg-surface-hover);
    color: var(--accent-blue);
  }

  .find-btn--active {
    color: var(--accent-blue);
    background: color-mix(in srgb, var(--accent-blue) 12%, transparent);
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
