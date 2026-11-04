<script lang="ts">
  import { m } from "../../i18n/index.js";
  import {
    FitStages,
    KbdBadge,
    Menu,
    MenuContent,
    MenuItem,
    MenuSeparator,
    MenuTrigger,
    Spinner,
    TopBar,
    type TopBarTab,
  } from "@kenn-io/kit-ui";
  import {
    ArrowDownToLineIcon,
    ArrowDownWideNarrowIcon,
    ArrowUpNarrowWideIcon,
    CheckIcon,
    FileDownIcon,
    CopyIcon,
    DatabaseBackupIcon,
    DownloadIcon,
    GlobeIcon,
    LinkIcon,
    LockIcon,
    MenuIcon,
    MoonIcon,
    MoreHorizontalIcon,
    SearchIcon,
    SettingsIcon,
    SunIcon,
  } from "../../icons.js";
  import {
    ui,
  } from "../../stores/ui.svelte.js";
  import { sessions } from "../../stores/sessions.svelte.js";
  import { sync } from "../../stores/sync.svelte.js";
  import { router, type Route } from "../../stores/router.svelte.js";
  import {
    downloadExport,
    getMarkdownExportUrl,
  } from "../../api/client.js";
  import { copyToClipboard } from "../../utils/clipboard.js";
  import { reportTelemetry } from "../../utils/telemetry.js";
  import ProjectTypeahead from "./ProjectTypeahead.svelte";
  import TranscriptViewMenu from "./TranscriptViewMenu.svelte";
  import ImportModal from "../import/ImportModal.svelte";
  import { claudeAISync } from "../../stores/claudeAISync.svelte.js";

  const isMac = navigator.platform.toUpperCase().includes("MAC");
  const modKey = isMac ? "⌘" : "Ctrl";

  let showImportModal = $state(false);
  let copiedMarkdownLink = $state(false);
  let copiedMarkdownLinkTimer:
    | ReturnType<typeof setTimeout>
    | undefined;

  /** True while TopBar has collapsed the nav tabs into its dropdown —
   * side-region snippets read it to drop their labels. */
  let navCollapsed = $state(false);

  const NAV_ROUTES = [
    "sessions",
    "usage",
    "activity",
    "trends",
    "recall",
    "pinned",
    "quality",
    "friction",
    "trash",
    "recent-edits",
    "data",
  ] as const;

  const frictionNavVisible = $derived(sync.serverVersion?.friction_readable === true);

  const tabs: TopBarTab[] = $derived([
    { id: "sessions", label: m.nav_sessions() },
    { id: "usage", label: m.nav_usage() },
    { id: "activity", label: m.nav_activity() },
    { id: "trends", label: m.nav_trends() },
    { id: "recall", label: m.nav_recall() },
    { id: "pinned", label: m.nav_pinned() },
    { id: "quality", label: m.nav_quality() },
    ...(frictionNavVisible ? [{ id: "friction", label: m.nav_friction() }] : []),
    { id: "trash", label: m.nav_trash() },
    { id: "recent-edits", label: m.nav_recent_edits() },
    { id: "data", label: m.nav_data() },
  ]);

  const activeTab = $derived(
    (NAV_ROUTES as readonly string[]).includes(router.route)
      ? router.route
      : "",
  );

  async function handleExport() {
    if (sessions.activeSessionId) {
      try {
        await downloadExport(sessions.activeSessionId);
      } catch (e) {
        console.error("Export failed:", e);
      }
    }
  }

  async function handleCopyMarkdownExportLink() {
    if (!sessions.activeSessionId) return;
    const url = new URL(
      getMarkdownExportUrl(sessions.activeSessionId),
      window.location.origin,
    ).toString();
    const ok = await copyToClipboard(url);
    if (!ok) return;
    reportTelemetry("export_run", { format: "markdown_link" });
    copiedMarkdownLink = true;
    clearTimeout(copiedMarkdownLinkTimer);
    copiedMarkdownLinkTimer = setTimeout(() => {
      copiedMarkdownLink = false;
    }, 1500);
  }

  async function handleCopySourceFilePath() {
    const filePath = sessions.activeSession?.file_path;
    if (!filePath) return;
    const ok = await copyToClipboard(filePath);
    if (!ok) return;
  }

  function openPublish(secret: boolean) {
    const id = sessions.activeSessionId;
    if (!id) return;
    ui.publishSecret = secret;
    ui.setPublishTarget({ kind: "session", id });
    ui.activeModal = "publish";
  }

  function openCommandPalette() {
    ui.activeModal = "commandPalette";
  }

  const hasActiveSession = $derived(
    sessions.activeSessionId !== null,
  );
  const activeSessionFilePath = $derived(
    sessions.activeSession?.file_path ?? "",
  );
</script>

{#snippet searchField()}
  <button
    class="search-hint"
    onclick={openCommandPalette}
    title={m.nav_search_sessions_shortcut({ shortcut: `${modKey} K` })}
  >
    <SearchIcon size="12" strokeWidth="2" aria-hidden="true" />
    <span class="search-hint-text">{m.nav_search_sessions()}</span>
    <KbdBadge keys={[modKey, "K"]} joiner="compact" />
  </button>
{/snippet}

{#snippet searchIconOnly()}
  <button
    class="search-hint search-hint--icon"
    onclick={openCommandPalette}
    title={m.nav_search_sessions_shortcut({ shortcut: `${modKey} K` })}
    aria-label={m.nav_search_sessions()}
  >
    <SearchIcon size="12" strokeWidth="2" aria-hidden="true" />
  </button>
{/snippet}

<TopBar
  {tabs}
  active={activeTab}
  onchange={(id) => router.navigate(id as Route)}
  bind:collapsed={navCollapsed}
  searchMinWidth={navCollapsed ? 48 : 220}
  ariaLabel={m.nav_primary()}
>
  {#snippet left()}
    {#if ui.isMobileViewport}
      <button
        class="hamburger"
        onclick={() => {
          if (router.route !== "sessions") {
            router.navigate("sessions");
            ui.sidebarOpen = true;
          } else {
            ui.toggleSidebar();
          }
        }}
        title={m.nav_toggle_sidebar_shortcut()}
        aria-label={m.nav_toggle_sidebar()}
        aria-expanded={ui.sidebarOpen}
        aria-controls="session-sidebar"
        data-sidebar-focus-target="mobile"
      >
        <MenuIcon size="16" strokeWidth="2" aria-hidden="true" />
      </button>
    {/if}
    <button
      class="header-home"
      onclick={() => router.navigate("sessions")}
      title={m.nav_home()}
    >
      <svg class="header-logo" width="18" height="18" viewBox="0 0 32 32" aria-hidden="true">
        <rect width="32" height="32" rx="6" fill="var(--accent-blue, #3b82f6)"/>
        <rect x="13" y="10" width="6" height="16" rx="2" fill="var(--bg-surface, #fff)"/>
        <rect x="11" y="5" width="10" height="7" rx="2" fill="var(--bg-surface, #fff)"/>
        <circle cx="18" cy="8.5" r="2" fill="var(--accent-blue, #3b82f6)"/>
        <circle cx="18" cy="8.5" r="1" fill="#1d4ed8"/>
      </svg>
      <span class="header-title">AgentsView</span>
    </button>

    <span class="project-picker">
      <ProjectTypeahead
        projects={sessions.projects}
        value={sessions.filters.project}
        onselect={(v) => sessions.setProjectFilter(v)}
      />
    </span>
  {/snippet}

  {#snippet search()}
    <FitStages class="search-fit" stages={[searchField, searchIconOnly]} />
  {/snippet}

  {#snippet right()}
    {#if hasActiveSession}
      <TranscriptViewMenu />
      <!-- Follow latest stays visible: it is live state, and its pressed
           look is the only sign that the view is pinned to new messages. -->
      <button
        class="header-btn"
        class:active={ui.followLatest}
        onclick={() => ui.toggleFollowLatest()}
        title={m.header_actions_follow_latest()}
        aria-label={m.header_actions_follow_latest()}
        aria-pressed={ui.followLatest}
      >
        <ArrowDownToLineIcon size="16" strokeWidth="1.75" aria-hidden="true" />
      </button>
      <button
        class="header-btn"
        onclick={() => ui.toggleSort()}
        title={m.header_actions_toggle_sort()}
        aria-label={m.header_actions_toggle_sort()}
      >
        {#if ui.sortNewestFirst}
          <ArrowDownWideNarrowIcon size="16" strokeWidth="1.75" aria-hidden="true" />
        {:else}
          <ArrowUpNarrowWideIcon size="16" strokeWidth="1.75" aria-hidden="true" />
        {/if}
      </button>
      <span class="header-divider"></span>
    {/if}

    {#if !sync.archiveOnly}
    <button
      class="header-btn sync-btn"
      class:syncing={sync.syncing}
      onclick={() => sync.triggerSync()}
      disabled={sync.syncing}
      title={sync.readOnly ? m.header_actions_refresh_data_shortcut() : m.header_actions_sync_sessions_shortcut()}
      aria-label={sync.readOnly ? m.header_actions_refresh_data() : m.header_actions_sync_sessions()}
    >
      {#if sync.syncing}
        <span class="sync-spinner" aria-hidden="true"><Spinner size={13} /></span>
      {:else}
        <DatabaseBackupIcon size="16" strokeWidth="1.75" aria-hidden="true" />
      {/if}
      <span class="sync-label" class:collapsed={navCollapsed}>{sync.readOnly ? m.header_actions_refresh() : m.header_actions_sync()}</span>
    </button>
    {/if}

    <!-- Less frequent actions live in one labeled menu instead of a row of
         look-alike icons. -->
    <Menu align="end">
      <MenuTrigger
        class="header-btn overflow-btn"
        title={m.header_actions_more_actions()}
        ariaLabel={m.header_actions_more_actions()}
      >
        <MoreHorizontalIcon size="16" strokeWidth="2" aria-hidden="true" />
      </MenuTrigger>
      <MenuContent ariaLabel={m.header_actions_more_actions()}>
        {#if hasActiveSession}
          <MenuItem onselect={handleExport} keyshortcuts="E">
            {#snippet icon()}<FileDownIcon size="13" strokeWidth="2" />{/snippet}
            {#snippet trailing()}<span class="menu-key">E</span>{/snippet}
            {m.header_actions_download_html()}
          </MenuItem>
          <MenuItem onselect={handleCopyMarkdownExportLink}>
            {#snippet icon()}<LinkIcon size="13" strokeWidth="2" />{/snippet}
            {m.header_actions_copy_markdown_link()}
          </MenuItem>
          {#if activeSessionFilePath}
            <MenuItem onselect={handleCopySourceFilePath}>
              {#snippet icon()}<CopyIcon size="13" strokeWidth="2" />{/snippet}
              {m.header_actions_copy_source_path()}
            </MenuItem>
          {/if}
          <MenuSeparator />
          <MenuItem onselect={() => openPublish(false)} keyshortcuts="P">
            {#snippet icon()}<GlobeIcon size="13" strokeWidth="2" />{/snippet}
            {#snippet trailing()}<span class="menu-key">P</span>{/snippet}
            {m.header_actions_publish_public()}
          </MenuItem>
          <MenuItem onselect={() => openPublish(true)}>
            {#snippet icon()}<LockIcon size="13" strokeWidth="2" />{/snippet}
            {m.header_actions_publish_secret()}
          </MenuItem>
          <MenuSeparator />
        {/if}
        <MenuItem
          onselect={() => { showImportModal = true; }}
          disabled={sync.readOnly || sync.archiveOnly}
        >
          {#snippet icon()}<DownloadIcon size="13" strokeWidth="2" />{/snippet}
          {sync.archiveOnly
            ? m.header_actions_import_archive_only()
            : sync.readOnly
            ? m.header_actions_import_unavailable()
            : m.header_actions_import_conversations()}
        </MenuItem>
      </MenuContent>
    </Menu>

    <!-- A running Claude.ai sync keeps a visible Import button so its
         progress is never hidden in the menu. -->
    {#if claudeAISync.running}
    <button
        class="import-btn"
        onclick={() => {
          if (!sync.readOnly && !sync.archiveOnly) showImportModal = true;
        }}
        disabled={sync.readOnly || sync.archiveOnly}
        title={sync.archiveOnly
          ? m.header_actions_import_archive_only()
          : sync.readOnly
          ? m.header_actions_import_unavailable()
          : claudeAISync.running
          ? m.header_actions_import_claude_syncing()
          : m.header_actions_import_conversations()}
        aria-label={claudeAISync.running ? m.header_actions_import_claude_syncing() : m.header_actions_import_conversations()}
      >
        {#if claudeAISync.running}
          <span class="sync-spinner" aria-hidden="true"><Spinner size={12} /></span>
        {:else}
          <DownloadIcon size="12" strokeWidth="2" aria-hidden="true" />
        {/if}
        <span class="import-label" class:collapsed={navCollapsed}>{m.header_actions_import()}</span>
      </button>
    {/if}

    <span class="header-divider"></span>

    <button
      class="header-btn"
      onclick={() => ui.toggleTheme()}
      title={m.header_actions_toggle_theme()}
      aria-label={m.header_actions_toggle_theme()}
    >
      {#if ui.theme === "light"}
        <MoonIcon size="16" strokeWidth="1.75" aria-hidden="true" />
      {:else}
        <SunIcon size="16" strokeWidth="1.75" aria-hidden="true" />
      {/if}
    </button>

    <button
      class="header-btn"
      class:active={router.route === "settings"}
      onclick={() => router.navigate("settings")}
      title={m.header_actions_settings()}
      aria-label={m.header_actions_settings()}
    >
      <SettingsIcon size="16" strokeWidth="1.75" aria-hidden="true" />
    </button>

    <button
      class="header-btn help-btn"
      onclick={() => (ui.activeModal = "shortcuts")}
      title={m.header_actions_keyboard_shortcuts_shortcut()}
      aria-label={m.header_actions_keyboard_shortcuts()}
    >
      ?
    </button>
  {/snippet}
</TopBar>

<ImportModal
  bind:open={showImportModal}
  onclose={() => showImportModal = false}
  onimported={() => {
    sessions.invalidateFilterCaches();
    sessions.load();
  }}
/>

<style>
  .header-home {
    display: flex;
    align-items: center;
    gap: 6px;
    cursor: pointer;
    border-radius: var(--radius-sm);
    padding: 2px 6px 2px 2px;
    transition: background 0.1s;
  }

  .header-home:hover {
    background: var(--bg-surface-hover);
  }

  .header-logo {
    flex-shrink: 0;
  }

  .header-title {
    font-size: 12px;
    font-weight: 650;
    color: var(--text-primary);
    white-space: nowrap;
    letter-spacing: -0.01em;
  }

  .project-picker {
    display: flex;
    min-width: 0;
  }

  /* FitStages' sizing contract: the host is sized by the flexible search
   * region, never by its own content. */
  :global(.search-fit) {
    width: 100%;
    display: flex;
    justify-content: center;
  }

  .search-hint {
    height: 26px;
    display: flex;
    align-items: center;
    justify-content: center;
    gap: 6px;
    padding: 0 10px;
    min-width: 220px;
    width: 100%;
    max-width: 340px;
    background: var(--bg-inset);
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-md);
    color: var(--text-muted);
    font-size: 11px;
    cursor: pointer;
    white-space: nowrap;
    transition: border-color 0.15s, box-shadow 0.15s;
  }

  .search-hint--icon {
    min-width: 0;
    width: auto;
    padding: 0 8px;
  }

  .search-hint:hover {
    border-color: var(--border-default);
    box-shadow: var(--shadow-sm);
  }

  .search-hint-text {
    color: var(--text-muted);
  }

  /* Match parent's border-radius on outer edges */

  /* ── Header icon buttons ── */
  .header-btn {
    width: 28px;
    height: 28px;
    display: flex;
    align-items: center;
    justify-content: center;
    border-radius: var(--radius-sm);
    color: var(--text-secondary);
    font-size: 14px;
    font-weight: 600;
    transition: background 0.12s, color 0.12s;
    flex-shrink: 0;
  }

  .help-btn {
    color: var(--text-muted);
    font-weight: 500;
  }

  .header-btn + .header-btn {
    margin-left: 2px;
  }

  /* kit-ui MenuTrigger renders the overflow button, so scoped
     .header-btn rules cannot reach it. */
  :global(.overflow-btn) {
    width: 28px;
    height: 28px;
    display: flex;
    align-items: center;
    justify-content: center;
    border-radius: var(--radius-sm);
    color: var(--text-secondary);
    flex-shrink: 0;
  }

  :global(.overflow-btn:hover),
  :global(.overflow-btn[aria-expanded="true"]) {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }

  .menu-key {
    font-family: var(--font-mono);
    font-size: 10.5px;
    color: var(--text-muted);
    border: 1px solid var(--border-muted);
    border-radius: 3px;
    padding: 0 4px;
  }

  .header-btn:hover:not(:disabled) {
    background: var(--bg-surface-hover);
    color: var(--text-secondary);
  }

  .header-btn:disabled {
    opacity: var(--opacity-disabled);
    cursor: default;
  }

  .header-btn.active {
    background: var(--selected-bg);
    box-shadow: var(--selected-edge);
    color: var(--text-primary);
  }

  .header-btn.syncing {
    color: var(--text-secondary);
  }

  .sync-spinner {
    display: flex;
    align-items: center;
  }

  .sync-btn {
    width: auto;
    min-width: 56px;
    gap: var(--space-2);
    padding: 0 9px;
    font-size: 11px;
    font-weight: 500;
  }

  /* Labels drop while the nav tabs are collapsed, keeping the side
     regions lean at tight widths (TopBar's side regions never shrink). */
  .sync-label.collapsed,
  .import-label.collapsed {
    display: none;
  }

  /* ── Import button (icon + label) ── */
  .import-btn {
    height: 26px;
    display: flex;
    align-items: center;
    gap: var(--space-2);
    padding: 0 10px;
    border-radius: var(--radius-sm);
    font-size: 11px;
    font-weight: 500;
    color: var(--text-muted);
    white-space: nowrap;
    transition: background 0.12s, color 0.12s;
  }

  .import-btn:hover:not(:disabled) {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }

  .import-btn:disabled {
    opacity: var(--opacity-disabled);
    cursor: default;
  }

  .header-divider {
    width: 1px;
    height: 14px;
    background: var(--border-default);
    margin: 0 6px;
    flex-shrink: 0;
  }

  .hamburger {
    display: flex;
    width: 28px;
    height: 28px;
    align-items: center;
    justify-content: center;
    border-radius: var(--radius-sm);
    color: var(--text-muted);
    transition: background 0.12s, color 0.12s;
  }

  .hamburger:hover {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }

  /* ── Responsive ──
   * Nav tabs and the search field degrade by measurement (TopBar +
   * FitStages); only the app-owned side-region content still uses the
   * shared layout breakpoints. */

  /* 760px: hide the project picker */
  @media (max-width: 760px) {
    .project-picker {
      display: none;
    }
  }

  /* 640px: minimal mode — collapse further */
  @media (max-width: 640px) {
    .header-title {
      display: none;
    }

    .search-hint {
      padding: 0 8px;
    }
  }

  /* Touch targets for coarse pointers */
  @media (pointer: coarse) {
    .header-btn,
    .hamburger,
    .import-btn,
    :global(.overflow-btn),
    :global(.view-trigger) {
      min-width: 44px;
      min-height: 44px;
    }
  }
</style>
