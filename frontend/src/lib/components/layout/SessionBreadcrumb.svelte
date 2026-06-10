<script lang="ts">
  import {
    ChartColumnIcon,
    CheckIcon,
    ChevronDownIcon,
    CirclePlayIcon,
    CodeIcon,
    CopyIcon,
    EllipsisVerticalIcon,
    FileTextIcon,
    FolderIcon,
    LinkIcon,
    SearchIcon,
    SquareTerminalIcon,
  } from "../../icons.js";
  import { onMount } from "svelte";
  import type { Session } from "../../api/types.js";
  import {
    OpenersService,
    SessionsService,
    type ResumeRequest,
    type ResumeResponse,
  } from "../../api/generated/index";
  import { configureGeneratedClient } from "../../api/runtime.js";
  import { copyToClipboard } from "../../utils/clipboard.js";
  import { agentColor, agentLabel } from "../../utils/agents.js";
  import { formatCost, formatTokenUsage } from "../../utils/format.js";
  import { normalizeMessagePreview } from "../../utils/messages.js";
  import { getGradeStyle, getGradeLabel } from "../../utils/grade.js";
  import SignalPanel from "../content/SignalPanel.svelte";
  import { sessions } from "../../stores/sessions.svelte.js";
  import { router } from "../../stores/router.svelte.js";
  import {
    supportsResume,
    buildResumeCommand,
    formatResumeResponseCommand,
  } from "../../utils/resume.js";

  import { inSessionSearch } from "../../stores/inSessionSearch.svelte.js";
  import { messages as messagesStore } from "../../stores/messages.svelte.js";
  import { ui } from "../../stores/ui.svelte.js";

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

  interface Opener {
    id: string;
    name: string;
    kind: "editor" | "terminal" | "files" | "action";
    bin: string;
  }

  interface OpenersResponse {
    openers: Opener[];
  }

  interface SessionDirectoryResponse {
    path: string;
  }

  onMount(() => {
    configureGeneratedClient();
    OpenersService.getApiV1Openers()
      .then((res) => {
        openers = (res as unknown as OpenersResponse).openers;
      })
      .catch(() => {});
  });

  let resolvedSessionDirId: string | null = null;
  $effect(() => {
    if (!session) {
      sessionDir = null;
      resolvedSessionDirId = null;
      return;
    }
    const id = session.id;
    if (id === resolvedSessionDirId) return;
    sessionDir = null;
    configureGeneratedClient();
    SessionsService.getApiV1SessionsIdDirectory({ id })
      .then(({ path }) => {
        if (session?.id === id) {
          sessionDir = (path as SessionDirectoryResponse["path"]) || null;
          resolvedSessionDirId = id;
        }
      })
      .catch(() => {
        // Don't cache the ID on failure so the next
        // session refresh retries the lookup.
      });
  });

  let sessionCost = $state<number | null>(null);
  // Key of the last successful usage fetch. Cost depends on more
  // than output tokens (input/cache tokens and explicit usage-event
  // costs), so the key includes every cost-affecting field present
  // in API session responses. A resync that changes none of these
  // (e.g. a cost-only usage event) keeps a stale cost until the
  // next keyed field moves; closing that would need a freshness
  // marker in the session API.
  let costFetchKey: string | null = null;
  let costSessionId: string | null = null;
  let costRequestSeq = 0;
  $effect(() => {
    if (!session) {
      sessionCost = null;
      costFetchKey = null;
      costSessionId = null;
      costRequestSeq++;
      return;
    }
    const id = session.id;
    const key = [
      id,
      session.total_output_tokens ?? 0,
      session.peak_context_tokens ?? 0,
      session.has_total_output_tokens ?? "",
      session.has_peak_context_tokens ?? "",
      session.message_count ?? 0,
      session.ended_at ?? "",
    ].join("\n");
    if (id !== costSessionId) {
      // Entering a different session invalidates both the displayed
      // cost and the fetch cache; the cached key must never satisfy
      // the early return below while another session's request is
      // still in flight.
      sessionCost = null;
      costFetchKey = null;
    }
    if (key === costFetchKey) return;
    costSessionId = id;
    const seq = ++costRequestSeq;
    configureGeneratedClient();
    SessionsService.getApiV1SessionsIdUsage({ id })
      .then((res) => {
        if (seq !== costRequestSeq) return;
        costFetchKey = key;
        sessionCost = res.has_cost ? res.cost_usd : null;
      })
      .catch(() => {
        // Leave the fetch key unset so the next
        // session refresh retries the lookup.
      });
  });

  let sessionCostLabel = $derived(
    sessionCost !== null ? formatCost(sessionCost) : null,
  );

  let sessionContextTokens = $derived(session?.peak_context_tokens ?? 0);
  let sessionOutputTokens = $derived(session?.total_output_tokens ?? 0);
  let sessionHasContextTokens = $derived(
    session
      ? (session.has_peak_context_tokens ?? session.peak_context_tokens > 0)
      : false,
  );
  let sessionHasOutputTokens = $derived(
    session
      ? (session.has_total_output_tokens ?? session.total_output_tokens > 0)
      : false,
  );
  let sessionTokenSummary = $derived(
    session
      ? formatTokenUsage(
          sessionContextTokens,
          sessionHasContextTokens,
          sessionOutputTokens,
          sessionHasOutputTokens,
        )
      : null,
  );

  let mainModel = $derived(
    messagesStore.sessionId === session?.id
      ? messagesStore.mainModel
      : "",
  );

  const gradeStyle = $derived(
    getGradeStyle(session?.health_grade),
  );

  $effect(() => {
    if (ui.signalPanelOpen && session?.id) {
      sessions.fetchSignalDetail(session.id);
    }
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


  let copiedLinkId = $state("");
  let copiedLinkTimer: ReturnType<typeof setTimeout> | undefined;

  async function copySessionLink() {
    if (!session) return;
    const id = session.id;
    const href = router.buildSessionHref(id);
    const url = window.location.origin + href;
    const ok = await copyToClipboard(url);
    if (!ok) return;
    copiedLinkId = id;
    clearTimeout(copiedLinkTimer);
    copiedLinkTimer = setTimeout(() => {
      if (copiedLinkId === id) copiedLinkId = "";
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
      session.display_name
      ?? normalizeMessagePreview(session.first_message)
      ?? "";
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
      configureGeneratedClient();
      const resp =
        await SessionsService.postApiV1SessionsIdResume({
          id: session.id,
          requestBody: {
            opener_id: opener.id,
          } satisfies ResumeRequest,
        }) as ResumeResponse;
      if (resp.launched) {
        showFeedback(`Resumed in ${resp.terminal ?? opener.name}`);
        return;
      }
      // Launch failed — fall back to clipboard copy.
      if (resp.command) {
        const cmd = formatResumeResponseCommand(session.agent, resp);
        const ok = cmd ? await copyToClipboard(cmd) : false;
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
      configureGeneratedClient();
      const resp =
        await SessionsService.postApiV1SessionsIdResume({
          id: session.id,
          requestBody: { command_only: true } satisfies ResumeRequest,
        }) as ResumeResponse;
      if (resp.command) {
        const cmd = formatResumeResponseCommand(session.agent, resp);
        const ok = cmd ? await copyToClipboard(cmd) : false;
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
      configureGeneratedClient();
      await SessionsService.postApiV1SessionsIdOpen({
        id: session.id,
        requestBody: { opener_id: opener.id },
      });
      showFeedback(`Opened in ${opener.name}`);
    } catch {
      showFeedback("Failed to open");
    }
  }

  async function handleResumeDefault() {
    if (!session) return;
    showOpenMenu = false;
    try {
      configureGeneratedClient();
      const resp =
        await SessionsService.postApiV1SessionsIdResume({
          id: session.id,
          requestBody: {},
        }) as ResumeResponse;
      if (resp.launched) {
        showFeedback(
          `Resumed in ${resp.terminal ?? "terminal"}`,
        );
        return;
      }
      if (resp.command) {
        const cmd = formatResumeResponseCommand(session.agent, resp);
        const ok = cmd ? await copyToClipboard(cmd) : false;
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

  // Remote sessions have host-prefixed IDs (host~rawID).
  const isLocal = $derived(
    !session?.id.includes("~"),
  );

  const canResume = $derived(
    session
      ? supportsResume(session.agent) && isLocal
      : false,
  );

  const terminalOpeners = $derived(
    openers.filter((o) => o.kind === "terminal"),
  );

  const claudeDesktopOpener = $derived(
    session?.agent === "claude"
      ? openers.find((o) => o.id === "claude-desktop") ?? null
      : null,
  );

  const editorOpeners = $derived(
    openers.filter((o) => o.kind === "editor"),
  );

  const fileOpeners = $derived(
    openers.filter((o) => o.kind === "files"),
  );

  const showDropdown = $derived(
    canResume ||
    (isLocal && (
      editorOpeners.length > 0 ||
      fileOpeners.length > 0 ||
      (sessionDir !== null && !!session?.file_path)
    )),
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
    if (showOpenMenu && isLocal) {
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
  <button
    class="breadcrumb-link"
    onclick={onBack}
    title="Back to sessions"
  >
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
      {session?.display_name ?? session?.project ?? ""}
    </span>
  {/if}
  {#if session}
    <span class="breadcrumb-meta">
      <span
        class="agent-badge"
        style:background={agentColor(session.agent)}
      >{agentLabel(session.agent)}</span>
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
      <button
        class="grade-badge"
        style:background={gradeStyle.bg}
        style:color={gradeStyle.text}
        style:border-color={gradeStyle.border}
        onclick={() => ui.toggleSignalPanel()}
        title="Session health"
      >
        {getGradeLabel(session.health_grade)}
      </button>
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
              <CheckIcon size="11" strokeWidth="2.4" aria-hidden="true" />
              {openFeedback}
            {:else}
              {canResume ? "Resume" : "Open"}
              <ChevronDownIcon size="8" strokeWidth="2.6" aria-hidden="true" />
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
                    <SquareTerminalIcon size="10" strokeWidth="2" aria-hidden="true" />
                  </span>
                  <span class="open-menu-name">Default terminal</span>
                </button>
                <div class="open-menu-divider"></div>
                <button class="open-menu-item" onclick={handleCopyResumeCommand}>
                  <span class="open-menu-num">
                    <CopyIcon size="10" strokeWidth="2" aria-hidden="true" />
                  </span>
                  <span class="open-menu-name">Copy command</span>
                </button>
              {/if}
              {#if isLocal}
              <button class="open-menu-item" onclick={handleCopyFilePath}>
                <span class="open-menu-num">
                  <FileTextIcon size="10" strokeWidth="2" aria-hidden="true" />
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
                      <CodeIcon size="10" strokeWidth="2" aria-hidden="true" />
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
                      <FolderIcon size="10" strokeWidth="2" aria-hidden="true" />
                    </span>
                    <span class="open-menu-name">{opener.name}</span>
                  </button>
                {/each}
              {/if}
              {/if}
              {#if canResume && claudeDesktopOpener}
                <div class="open-menu-divider"></div>
                <button
                  class="open-menu-item"
                  onclick={() => handleResumeIn(claudeDesktopOpener)}
                >
                  <span class="open-menu-num">
                    <CirclePlayIcon size="10" strokeWidth="2" aria-hidden="true" />
                  </span>
                  <span class="open-menu-name">Claude Desktop</span>
                </button>
              {/if}
            </div>
          {/if}
        </span>
      {/if}
      {#if session.id}
        {@const rawId = sessionDisplayId(session.id)}
        <button
          class="session-id"
          title="Copy session ID: {rawId}"
          onclick={() => copySessionId(rawId, session.id)}
          aria-label="Copy session ID"
        >
          {copiedSessionId === session.id
            ? "Copied!"
            : rawId.slice(0, 8)}
        </button>
      {/if}
      {#if sessionTokenSummary}
        <span class="token-badge token-badge--desktop">
          {sessionTokenSummary}
        </span>
        <span
          class="token-badge token-badge--mobile"
          title={sessionTokenSummary}
        >
          {sessionTokenSummary}
        </span>
      {/if}
      {#if sessionCostLabel}
        <span class="cost-badge" title="Estimated session cost">
          {sessionCostLabel}
        </span>
      {/if}
      {#if mainModel}
        <span class="model-badge" title={mainModel}>{mainModel}</span>
      {/if}
      <div class="actions-wrapper">
        <button
          class="link-btn"
          class:link-btn--copied={copiedLinkId === session?.id}
          title="Copy link to session"
          onclick={copySessionLink}
          aria-label="Copy link to session"
        >
          {#if copiedLinkId === session?.id}
            <CheckIcon size="13" strokeWidth="2.4" aria-hidden="true" />
          {:else}
            <LinkIcon size="13" strokeWidth="2" aria-hidden="true" />
          {/if}
        </button>
        <button
          class="minimap-btn"
          class:minimap-btn--active={ui.vitalsOpen}
          title={ui.vitalsOpen
            ? "Hide session analysis"
            : "Show session analysis"}
          onclick={() => ui.toggleVitals()}
          aria-label={ui.vitalsOpen
            ? "Hide session analysis"
            : "Show session analysis"}
        >
          <ChartColumnIcon size="13" strokeWidth="2" aria-hidden="true" />
        </button>
        <button
          class="find-btn"
          class:find-btn--active={inSessionSearch.isOpen}
          title="Find in session (/)"
          onclick={() => inSessionSearch.toggle()}
          aria-label="Find in session"
        >
          <SearchIcon size="13" strokeWidth="2" aria-hidden="true" />
        </button>
        <button
          class="actions-btn"
          title="Session actions"
          aria-label="Session actions"
          bind:this={menuBtnEl}
          onclick={toggleMenu}
        >
          <EllipsisVerticalIcon size="14" strokeWidth="2.4" aria-hidden="true" />
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

{#if ui.signalPanelOpen && session}
  <SignalPanel {session} />
{/if}

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

  .grade-badge {
    display: inline-flex;
    align-items: center;
    padding: 1px 6px;
    border-radius: 4px;
    font-size: 11px;
    font-weight: 700;
    border: 1px solid;
    cursor: pointer;
    line-height: 1.4;
  }

  .grade-badge:hover {
    opacity: 0.85;
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

  .token-badge {
    font-size: 10px;
    font-variant-numeric: tabular-nums;
    color: var(--text-muted);
    padding: 1px 5px;
    border-radius: 4px;
    background: var(--bg-tertiary);
    white-space: nowrap;
    flex-shrink: 0;
  }

  .token-badge--mobile {
    display: none;
    white-space: nowrap;
  }

  .cost-badge {
    font-size: 10px;
    font-variant-numeric: tabular-nums;
    color: var(--text-muted);
    padding: 1px 5px;
    border-radius: 4px;
    background: var(--bg-tertiary);
    white-space: nowrap;
    flex-shrink: 0;
  }

  .model-badge {
    font-size: 10px;
    color: var(--text-muted);
    padding: 1px 5px;
    border-radius: 4px;
    background: var(--bg-tertiary);
    white-space: nowrap;
    flex-shrink: 0;
  }

  .actions-wrapper {
    position: relative;
    display: flex;
    align-items: center;
    gap: 2px;
  }

  .link-btn {
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

  .link-btn:hover {
    background: var(--bg-surface-hover);
    color: var(--accent-blue);
  }

  .link-btn--copied {
    color: var(--accent-green, #2ea043);
  }

  .minimap-btn {
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

  .minimap-btn:hover {
    background: var(--bg-surface-hover);
    color: var(--accent-blue);
  }

  .minimap-btn--active {
    color: var(--accent-blue);
    background: color-mix(
      in srgb,
      var(--accent-blue) 12%,
      transparent
    );
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

  @media (max-width: 767px) {
    .breadcrumb-meta {
      gap: 4px;
    }

    .session-time {
      display: none;
    }

    .token-badge--desktop {
      display: none;
    }

    .token-badge--mobile {
      display: inline-flex;
      font-size: 9px;
      padding: 1px 4px;
      max-width: 110px;
      overflow: hidden;
      text-overflow: ellipsis;
    }

    .session-id {
      display: none;
    }
  }
</style>
