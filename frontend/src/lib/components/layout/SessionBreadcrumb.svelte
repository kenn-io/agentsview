<script lang="ts">
  import DocbankStatus from "../content/DocbankStatus.svelte";
  import {
    ChartColumnIcon,
    CheckIcon,
    EllipsisIcon,
    LightbulbIcon,
    LinkIcon,
    SearchIcon,
  } from "../../icons.js";
  import { MediaQuery } from "svelte/reactivity";
  import type { Session } from "../../api/types.js";
  import { copyToClipboard } from "../../utils/clipboard.js";
  import {
    agentInk,
    agentLabel,
    entrypointBadge,
  } from "../../utils/agents.js";
  import { formatCount } from "../../utils/format.js";
  import { normalizeMessagePreview } from "../../utils/messages.js";
  import { getGradeStyle, getGradeLabel } from "../../utils/grade.js";
  import SessionFilterControl from "../filters/SessionFilterControl.svelte";
  import SidebarToggleButton from "./SidebarToggleButton.svelte";
  import { sessions } from "../../stores/sessions.svelte.js";
  import { router } from "../../stores/router.svelte.js";
  import { insights } from "../../stores/insights.svelte.js";
  import { sync } from "../../stores/sync.svelte.js";
  import { sessionDisplayId } from "../../utils/sessionDisplayId.js";
  import { markWrappedGroups } from "./breadcrumbLayout.js";
  import { SessionUsage } from "./sessionUsage.svelte.js";
  import { SessionOpeners } from "./sessionOpeners.svelte.js";
  import SessionOpenMenu from "./SessionOpenMenu.svelte";
  import SessionSignalPanels from "./SessionSignalPanels.svelte";
  import SessionUsageStats from "./SessionUsageStats.svelte";

  import { inSessionSearch } from "../../stores/inSessionSearch.svelte.js";
  import { messages as messagesStore } from "../../stores/messages.svelte.js";
  import { formatModelEffort } from "../../utils/model.js";
  import { ui } from "../../stores/ui.svelte.js";
  import TranscriptControls from "./TranscriptControls.svelte";
  import { m } from "../../i18n/index.js";

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

  const openers = new SessionOpeners(() => session);
  const usageStats = new SessionUsage(() => session);

  let mainModelInfo = $derived(
    messagesStore.sessionId === session?.id
      ? messagesStore.mainModelInfo
      : { model: "", reasoningEffort: "" },
  );
  let mainModel = $derived(formatModelEffort(mainModelInfo));

  // The entrypoint (for example sdk-ts) is detail, so it lives in the
  // agent's hover text, as on the sidebar row.
  let agentTitle = $derived.by(() => {
    if (!session) return undefined;
    const label = agentLabel(session.agent, session.agent_label);
    const entrypoint = entrypointBadge(session.entrypoint);
    return entrypoint ? `${label} · ${entrypoint}` : label;
  });

  const gradeStyle = $derived(
    getGradeStyle(session?.health_grade),
  );

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

  function handleAgentAnalysis() {
    if (!session) return;
    insights.generateForSession(session);
    router.navigate("recall", { tab: "generated" });
  }

  const insightGenerationAvailable = $derived(
    sync.serverVersion?.insight_generation_available ??
      (sync.serverVersion?.read_only !== true),
  );

  function handleToolEffectiveness() {
    if (!session || !insightGenerationAvailable) return;
    closeMenu();
    insights.generateForSession(session, "tool_effectiveness");
    router.navigate("recall", { tab: "generated" });
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

  function handleKeydown(e: KeyboardEvent) {
    if (e.key === "Escape") {
      if (renaming) {
        cancelRename();
      } else if (menuOpen) {
        closeMenu();
      } else if (openers.menuOpen) {
        openers.menuOpen = false;
        e.preventDefault();
      }
      return;
    }
    if (openers.menuOpen && openers.isLocal) {
      // Number key shortcuts (1-9) for quick selection.
      const num = parseInt(e.key);
      if (num >= 1 && num <= 9) {
        const idx = num - 1;
        if (idx < openers.terminalOpeners.length) {
          e.preventDefault();
          openers.resumeIn(openers.terminalOpeners[idx]!);
        }
      }
    }
  }

  // Above phone width the header is two rows: the title with its actions,
  // then the session metadata. From 1100px the transcript controls join
  // the title row; below that they start the metadata row. The markup
  // follows these rows directly (instead of CSS order) so keyboard focus
  // moves through the header in the order it reads. Keep the widths in
  // step with the media queries in the style block.
  const twoRowQuery = new MediaQuery("min-width: 641px");
  const wideQuery = new MediaQuery("min-width: 1100px");
  const twoRowHeader = $derived(twoRowQuery.current);
  const wideHeader = $derived(wideQuery.current);

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
      openers.menuOpen = false;
    }
  }
</script>

<svelte:document
  onkeydown={handleKeydown}
  onclick={handleClickOutside}
/>

<!-- The session's action groups render in one of two places so the
     document (and Tab) order always follows the visual rows; see
     twoRowHeader. -->
{#snippet sessionActions()}
    <div class="actions-wrapper">
      <button
        class="link-btn"
        class:link-btn--copied={copiedLinkId === session?.id}
        title={m.session_breadcrumb_copy_link_to_session()}
        onclick={copySessionLink}
        aria-label={m.session_breadcrumb_copy_link_to_session()}
      >
        {#if copiedLinkId === session?.id}
          <CheckIcon size="14" aria-hidden="true" />
        {:else}
          <LinkIcon size="14" aria-hidden="true" />
        {/if}
      </button>
      <button
        class="minimap-btn"
        class:minimap-btn--active={ui.vitalsOpen}
        title={ui.vitalsOpen
          ? m.session_breadcrumb_hide_session_analysis()
          : m.session_breadcrumb_show_session_analysis()}
        onclick={() => ui.toggleVitals()}
        aria-label={ui.vitalsOpen
          ? m.session_breadcrumb_hide_session_analysis()
          : m.session_breadcrumb_show_session_analysis()}
      >
        <ChartColumnIcon size="14" aria-hidden="true" />
      </button>
      <button
        class="insight-btn"
        title={m.insights_page_agent_analysis()}
        aria-label={m.insights_page_agent_analysis()}
        onclick={handleAgentAnalysis}
      >
        <LightbulbIcon size="14" aria-hidden="true" />
      </button>
      <button
        class="find-btn"
        class:find-btn--active={inSessionSearch.isOpen}
        title={m.session_breadcrumb_find_in_session_shortcut()}
        onclick={() => inSessionSearch.toggle()}
        aria-label={m.session_breadcrumb_find_in_session()}
      >
        <SearchIcon size="14" aria-hidden="true" />
      </button>
      <button
        class="actions-btn"
        title={m.session_breadcrumb_session_actions()}
        aria-label={m.session_breadcrumb_session_actions()}
        bind:this={menuBtnEl}
        onclick={toggleMenu}
      >
        <!-- The same horizontal dots as the top bar menu, so the two overflow
             menus share one glyph. -->
        <EllipsisIcon size="14" aria-hidden="true" />
      </button>
      {#if menuOpen}
        <div class="actions-menu" bind:this={menuEl}>
          {#if insightGenerationAvailable}
            <button
              class="actions-menu-item"
              onclick={handleToolEffectiveness}
            >
              {m.session_breadcrumb_tool_effectiveness()}
            </button>
          {/if}
          {#if session && !session.health_grade}
            <!-- Graded sessions open this panel from the grade chip; an
                 ungraded session has no chip, so the panel lives here. -->
            <button
              class="actions-menu-item"
              aria-pressed={ui.signalPanelOpen}
              onclick={() => {
                menuOpen = false;
                ui.toggleSignalPanel();
              }}
            >
              {m.session_breadcrumb_session_health_and_tool_sequences()}
            </button>
          {/if}
          <button
            class="actions-menu-item"
            onclick={startRename}
          >
            {m.session_breadcrumb_rename()}
          </button>
          <button
            class="actions-menu-item danger"
            onclick={handleDelete}
          >
            {m.session_breadcrumb_delete()}
          </button>
        </div>
      {/if}
    </div>
{/snippet}

<div class="session-breadcrumb" {@attach markWrappedGroups}>
  {#if !ui.isMobileViewport && !ui.sidebarOpen}
    <div
      class="sidebar-controls"
      data-sidebar-focus-region="content"
    >
      <SidebarToggleButton placement="content" />
      <SessionFilterControl
        showDisplay={false}
        showStarred={false}
        align="left"
      />
    </div>
  {/if}
  <button
    class="breadcrumb-link"
    onclick={onBack}
    title={m.session_breadcrumb_back_to_sessions()}
  >
    {m.session_breadcrumb_sessions()}
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
  {#if twoRowHeader && wideHeader && !ui.isMobileViewport}
    <TranscriptControls />
  {/if}
  {#if session && twoRowHeader}
    <SessionOpenMenu {openers} />
    {@render sessionActions()}
  {/if}
  {#if twoRowHeader}
    <span class="row-break" aria-hidden="true"></span>
  {/if}
  <!-- Outside the session check: the transcript controls must stay usable
       when the session details fail to load but its messages do not. -->
  {#if !ui.isMobileViewport && !(twoRowHeader && wideHeader)}
    <TranscriptControls />
  {/if}
  {#if session}
    <!-- The metadata reads in groups: who ran it (agent and model), when it
         started, what it used, then the grade and ID chips. A middot
         separates groups; items inside a group are spaced only. Groups with
         nothing to show are left out, so no placeholder renders. -->
    <span class="breadcrumb-meta">
      <span class="meta-group meta-identity">
        <span
          class="agent-badge"
          style:--agent-ink={agentInk(session.agent)}
          title={agentTitle}
        >{agentLabel(session.agent, session.agent_label)}</span>
        {#if mainModelInfo.model}
          <span
            class="model-badge"
            class:model-badge--with-effort={mainModelInfo.reasoningEffort}
            title={mainModel}
          ><span class="model-badge__model">{mainModelInfo.model}</span>{#if mainModelInfo.reasoningEffort}{" "}<span class="model-badge__effort">{mainModelInfo.reasoningEffort}</span>{/if}</span>
        {/if}
        {#if session.agent === "antigravity-cli" && session.transcript_fidelity === "summary"}
          <a
            class="summary-badge"
            href="https://github.com/kenn-io/agentsview#antigravity-cli-high-resolution-transcripts"
            target="_blank"
            rel="noopener noreferrer"
            title={m.session_breadcrumb_summary_mode_tooltip()}
          >{m.session_breadcrumb_summary_mode()}</a>
        {/if}
        {#if session.parser_malformed_lines}
          <span
            class="malformed-badge"
            title={m.session_breadcrumb_malformed_lines_tooltip({
              count: session.parser_malformed_lines,
              countLabel: formatCount(session.parser_malformed_lines),
            })}
          >{m.session_breadcrumb_malformed_lines({
            count: session.parser_malformed_lines,
            countLabel: formatCount(session.parser_malformed_lines),
          })}</span>
        {/if}
        {#if (session.agent === "antigravity" || session.agent === "antigravity-cli") && session.decode_confidence === "low"}
          <span
            class="decode-badge"
            title={m.session_breadcrumb_antigravity_decode_confidence_low_tooltip()}
          >{m.session_breadcrumb_antigravity_decode_confidence_low()}</span>
        {/if}
      </span>
      {#if session.started_at}
        <span class="meta-group meta-time">
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
        </span>
      {/if}
      {#if usageStats.hasStats}
        <span class="meta-group meta-stats">
          <SessionUsageStats usage={usageStats} />
        </span>
      {/if}
      {#if session.health_grade || session.id}
        <span class="meta-group meta-ref">
          {#if session.health_grade}
            <button
              class="grade-badge"
              style:--grade-ink={gradeStyle.text}
              onclick={() => ui.toggleSignalPanel()}
              title={m.session_breadcrumb_session_health_and_tool_sequences()}
              aria-expanded={ui.signalPanelOpen}
            >
              {getGradeLabel(session.health_grade)}
            </button>
          {/if}
          {#if session.id}
            {@const rawId = sessionDisplayId(session.id)}
            <button
              class="session-id"
              title={m.session_breadcrumb_copy_session_id_value({ id: rawId })}
              onclick={() => copySessionId(rawId, session.id)}
              aria-label={m.session_breadcrumb_copy_session_id()}
            >
              {copiedSessionId === session.id
                ? m.session_breadcrumb_copied()
                : rawId.slice(0, 8)}
            </button>
          {/if}
        </span>
      {/if}
      {#if !twoRowHeader}
        <SessionOpenMenu {openers} />
      {/if}
      {#if !twoRowHeader}
        {@render sessionActions()}
      {/if}
    </span>
  {/if}
</div>
<DocbankStatus />

{#if ui.signalPanelOpen && session}
  <SessionSignalPanels {session} />
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

  .sidebar-controls {
    position: relative;
    display: flex;
    align-items: center;
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
    gap: var(--space-4);
    margin-left: auto;
    min-width: 0;
    flex-shrink: 1;
  }

  /* The agent name is set in its agent color, the same mix the sidebar
     uses, so it is the one colored word in the strip. */
  .agent-badge {
    display: inline-flex;
    align-items: center;
    font-size: 12px;
    font-weight: 600;
    line-height: 16px;
    color: color-mix(in srgb, var(--agent-ink, var(--text-muted)) 82%, var(--text-primary));
    white-space: nowrap;
    flex-shrink: 0;
  }

  /* The metadata reads in groups: identity (agent and model), start time,
     usage figures, then the grade and ID chips. Items in a group are only
     spaced; a muted middot separates groups. */
  /* A group too wide for the pane wraps its own items; each figure and
     chip keeps nowrap itself, so the menus inside keep normal wrapping. */
  .meta-group {
    display: inline-flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 4px;
    min-width: 0;
    max-width: 100%;
    flex-shrink: 0;
  }

  .meta-identity {
    flex-shrink: 1;
  }

  .meta-identity .agent-badge {
    margin-right: 2px;
  }

  .meta-ref {
    gap: 6px;
  }

  .meta-group + .meta-group::before {
    content: "\00b7";
    width: 6px;
    text-align: center;
    font-size: 12px;
    font-weight: 700;
    line-height: 16px;
    color: var(--text-muted);
    opacity: 0.7;
  }

  /* A group that starts a wrapped line keeps its separator's space (so the
     row does not reflow) but hides it and shifts back to the line start by
     the separator and the group gap. Relative offsets leave layout and
     fixed-position menus alone. */
  .meta-group:global([data-wraps])::before {
    visibility: hidden;
  }

  .meta-group + .meta-group:global([data-wraps]) {
    position: relative;
    left: -10px;
  }

  /* The strip has one chip style, shared by the health grade and the
     session ID: an 11px outlined label on no fill. Figures (date, usage,
     cost, model) are plain muted text, so the title stays the strongest
     element in the header. */
  .grade-badge,
  .session-id {
    display: inline-flex;
    align-items: center;
    padding: 0 5px;
    font-size: 11px;
    font-weight: 500;
    line-height: 16px;
    background: transparent;
    border: 1px solid var(--border-default);
    border-radius: var(--radius-sm);
    white-space: nowrap;
    flex-shrink: 0;
  }

  .grade-badge:hover,
  .session-id:hover {
    background: var(--bg-surface-hover);
  }

  .summary-badge {
    font-size: 9px;
    font-weight: 600;
    padding: 1px 6px;
    border-radius: 8px;
    text-transform: uppercase;
    letter-spacing: 0.03em;
    flex-shrink: 0;
    color: var(--accent-amber, #e0a458);
    background: color-mix(in srgb, var(--accent-amber, #e0a458) 18%, transparent);
    border: 1px solid color-mix(in srgb, var(--accent-amber, #e0a458) 40%, transparent);
    text-decoration: none;
    white-space: nowrap;
  }

  .summary-badge:hover {
    text-decoration: underline;
  }

  .malformed-badge {
    display: inline-flex;
    align-items: center;
    font-size: 9px;
    font-weight: 600;
    padding: 1px 6px;
    border-radius: 8px;
    letter-spacing: 0.02em;
    flex-shrink: 0;
    color: var(--accent-amber, #e0a458);
    background: color-mix(in srgb, var(--accent-amber, #e0a458) 18%, transparent);
    border: 1px solid color-mix(in srgb, var(--accent-amber, #e0a458) 40%, transparent);
    white-space: nowrap;
    cursor: default;
  }

  .decode-badge {
    font-size: 9px;
    font-weight: 600;
    padding: 1px 6px;
    border-radius: 8px;
    text-transform: uppercase;
    letter-spacing: 0.03em;
    flex-shrink: 0;
    color: var(--accent-red, #e55);
    background: color-mix(in srgb, var(--accent-red, #e55) 16%, transparent);
    border: 1px solid color-mix(in srgb, var(--accent-red, #e55) 45%, transparent);
    white-space: nowrap;
    cursor: help;
  }

  /* Figures: one 12px muted size with tabular numerals and no fill. The
     side padding only keeps their spacing. SessionUsageStats gives the
     usage figures the same look. */
  .session-time,
  .model-badge {
    font-size: 12px;
    font-weight: 400;
    color: var(--text-muted);
    font-variant-numeric: tabular-nums;
    background: transparent;
    padding: 0 4px;
    white-space: nowrap;
  }

  /* A model name is an identifier, not a figure. */
  .model-badge {
    font-variant-numeric: normal;
  }

  .session-time {
    flex-shrink: 0;
  }

  /* The grade keeps its ink so the chip still signals health. */
  .grade-badge {
    font-weight: 600;
    color: color-mix(in srgb, var(--grade-ink) 62%, var(--text-primary));
    cursor: pointer;
  }

  /* The grade inks are dark, so dark mode needs a larger primary share
     for the 11px label to stay above 4.5:1 on the hover fill. */
  :global(:root.dark) .grade-badge {
    color: color-mix(in srgb, var(--grade-ink) 50%, var(--text-primary));
  }

  .session-id {
    font-family: var(--font-mono);
    color: var(--text-muted);
    cursor: pointer;
    transition: color 0.15s, background 0.15s;
  }

  .session-id:hover {
    color: var(--text-secondary);
  }

  .model-badge {
    display: inline-flex;
    align-items: center;
    min-width: 0;
    max-width: min(280px, 28vw);
    overflow: hidden;
    flex-shrink: 1;
  }

  .model-badge__model {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .model-badge--with-effort {
    min-width: 39px;
  }

  .model-badge__effort {
    flex-shrink: 0;
    margin-left: 4px;
  }

  .actions-wrapper {
    position: relative;
    display: flex;
    align-items: center;
    gap: 2px;
  }

  /* The session action buttons share one spec with the transcript
     controls beside them (TranscriptControls .control-btn): a 28px ghost
     square, a 14px icon in secondary ink, primary ink on hover, and the
     shared neutral selected fill when their panel is open. */
  .link-btn,
  .minimap-btn,
  .insight-btn,
  .find-btn,
  .actions-btn {
    display: flex;
    align-items: center;
    justify-content: center;
    width: 28px;
    height: 28px;
    border: none;
    border-radius: var(--radius-sm, 4px);
    background: transparent;
    color: var(--text-secondary);
    cursor: pointer;
    transition: background 0.12s, color 0.12s;
    flex-shrink: 0;
  }

  .link-btn:hover,
  .minimap-btn:hover,
  .insight-btn:hover,
  .find-btn:hover,
  .actions-btn:hover {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }

  .link-btn--copied {
    color: var(--accent-green, #2ea043);
  }

  .minimap-btn--active,
  .find-btn--active {
    background: var(--selected-bg);
    box-shadow: var(--selected-edge);
    color: var(--text-primary);
  }

  .actions-menu {
    position: absolute;
    top: 100%;
    right: 0;
    z-index: var(--z-popover);
    margin-top: 4px;
    background: var(--bg-surface);
    border: 1px solid var(--border-default);
    border-radius: 6px;
    box-shadow: var(--shadow-lg);
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

  /* Above phone width the strip becomes two lines: the session title with
     its actions, then the session metadata. The markup already lists the
     items row by row (see twoRowHeader); the meta wrapper dissolves so its
     children wrap as header items, and .row-break ends the first row.
     Phones keep the single scrolling strip. */
  /* With a mouse, header controls stay 26px tall so the two-line header
     stays close to a single toolbar's height. Touch keeps 44px targets. */
  @media (min-width: 641px) and (pointer: fine) {
    .session-breadcrumb > :global(.transcript-controls) {
      --kit-control-height: 26px;
    }

    .session-breadcrumb > :global(.transcript-controls .control-btn) {
      height: 26px;
    }
  }

  @media (min-width: 641px) {
    .session-breadcrumb {
      flex-wrap: wrap;
      height: auto;
      min-height: 0;
      padding-top: 3px;
      padding-bottom: 3px;
      row-gap: 0;
      align-content: center;
    }

    /* The metadata row's figures share the chips' 16px line, so the
       second row costs 18px instead of 20px. */
    .model-badge,
    .session-time {
      line-height: 16px;
    }

    .row-break {
      flex-basis: 100%;
      height: 0;
    }

    /* In a very narrow pane the buttons wrap below the View trigger rather
       than run past the pane's clipped edge. */
    .session-breadcrumb > :global(.transcript-controls) {
      flex-wrap: wrap;
      flex-shrink: 1;
      min-width: 0;
      max-width: 100%;
    }

    .breadcrumb-meta {
      display: contents;
    }

    .breadcrumb-link,
    .breadcrumb-sep {
      font-size: 12px;
    }

    .breadcrumb-current {
      font-size: 15px;
      font-weight: 600;
      letter-spacing: -0.01em;
      line-height: 22px;
    }

    .session-breadcrumb > :global(.transcript-controls) {
      margin-left: auto;
      margin-right: 6px;
    }

    /* The metadata reads as one packed line: the ID follows the date
       cluster after the usual divider instead of jumping to the far edge
       across an empty gap. */
  }

  /* At desktop widths both header rows sit on the transcript's centered
     reading column (MessageList .virtual-row), so the title shares the
     first card's left edge and the stats end at its right edge. The bottom
     rule still spans the pane. A pane narrower than the column keeps the
     usual 14px inset. The transcript's scrollbar (MessageList publishes
     its width) narrows the area the column centers in, so it is left out
     of the centering and added back on the right. */
  /* kit-ui-check-ignore: content breakpoint where the title, transcript controls, and actions stop fitting one row; the script's wideQuery uses the same width */
  @media (min-width: 1100px) {
    .session-breadcrumb {
      --column-inset: calc(
        (100% - var(--transcript-scrollbar, 0px) - var(--reading-column)) / 2
      );
      padding-left: max(14px, var(--column-inset));
      padding-right: max(14px, calc(var(--column-inset) + var(--transcript-scrollbar, 0px)));
    }
  }

  /* Below 1100px the title, transcript controls, and actions do not fit on
     one row. The title and actions keep the first row, and the transcript
     controls start the second, ahead of the metadata. */
  /* kit-ui-check-ignore: content breakpoint where the title, transcript controls, and actions stop fitting one row; the script's wideQuery uses the same width */
  @media (min-width: 641px) and (max-width: 1099px) {
    .session-breadcrumb > :global(.open-group) {
      margin-left: auto;
    }

    .session-breadcrumb > :global(.transcript-controls) {
      margin-left: 0;
      margin-right: 8px;
    }
  }

  @media (max-width: 900px) {
    .breadcrumb-meta {
      gap: 2px;
    }

    /* Hide whole groups whose items are all hidden at this width, so no
       separator is left without a group. */
    .meta-time,
    .meta-ref:not(:has(.grade-badge)),
    .meta-stats:not(:has(:global(.token-badge), :global(.cost-badge), :global(.plan-usage))) {
      display: none;
    }

    .session-id {
      display: none;
    }
  }

  @media (max-width: 640px) {
    .session-breadcrumb {
      gap: 4px;
      padding: 0 6px;
    }

    .breadcrumb-meta {
      gap: 1px;
    }

    .meta-identity > .agent-badge {
      display: none;
    }

    /* The phone header is one fixed-height strip that scrolls sideways,
       so its groups stay on one line. */
    .meta-group {
      flex-wrap: nowrap;
      max-width: none;
    }

    /* The phone strip scrolls sideways and may start with an empty
       identity group, so it drops the separators. */
    .meta-group + .meta-group::before {
      display: none;
    }

    .actions-wrapper {
      gap: 0;
    }
  }
</style>
