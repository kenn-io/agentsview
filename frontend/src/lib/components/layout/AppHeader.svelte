<script lang="ts">
  import { m } from "../../i18n/index.js";
  import {
    FitStages,
    KbdBadge,
    Spinner,
    TopBar,
    type TopBarTab,
  } from "@kenn-io/kit-ui";
  import {
    AlignJustifyIcon,
    ArrowDownIcon,
    ArrowDownWideNarrowIcon,
    ArrowUpNarrowWideIcon,
    CheckIcon,
    CloudUploadIcon,
    CopyIcon,
    DatabaseBackupIcon,
    DownloadIcon,
    FunnelIcon,
    GlobeIcon,
    LinkIcon,
    ListCollapseIcon,
    LockIcon,
    LogsIcon,
    LayoutListIcon,
    MenuIcon,
    MoonIcon,
    MoreHorizontalIcon,
    SearchIcon,
    SettingsIcon,
    SunIcon,
    UploadIcon,
  } from "../../icons.js";
  import {
    ui,
    ALL_BLOCK_TYPES,
    type BlockType,
  } from "../../stores/ui.svelte.js";
  import { sessions } from "../../stores/sessions.svelte.js";
  import { sync } from "../../stores/sync.svelte.js";
  import { router, type Route } from "../../stores/router.svelte.js";
  import {
    downloadExport,
    getMarkdownExportUrl,
  } from "../../api/client.js";
  import { copyToClipboard } from "../../utils/clipboard.js";
  import ProjectTypeahead from "./ProjectTypeahead.svelte";
  import ImportModal from "../import/ImportModal.svelte";

  const isMac = navigator.platform.toUpperCase().includes("MAC");
  const modKey = isMac ? "⌘" : "Ctrl";

  let showImportModal = $state(false);
  let showBlockFilter = $state(false);
  let showExportMenu = $state(false);
  let showPublishMenu = $state(false);
  let showOverflow = $state(false);
  let copiedMarkdownLink = $state(false);
  let copiedMarkdownLinkTimer:
    | ReturnType<typeof setTimeout>
    | undefined;
  let filterBtnRef: HTMLButtonElement | undefined =
    $state(undefined);
  let filterDropRef: HTMLDivElement | undefined =
    $state(undefined);
  let exportBtnRef: HTMLButtonElement | undefined =
    $state(undefined);
  let exportDropRef: HTMLDivElement | undefined =
    $state(undefined);
  let publishBtnRef: HTMLButtonElement | undefined =
    $state(undefined);
  let publishDropRef: HTMLDivElement | undefined =
    $state(undefined);
  let overflowBtnRef: HTMLButtonElement | undefined =
    $state(undefined);
  let overflowDropRef: HTMLDivElement | undefined =
    $state(undefined);

  /** True while TopBar has collapsed the nav tabs into its dropdown —
   * side-region snippets read it to drop their labels. */
  let navCollapsed = $state(false);

  const NAV_ROUTES = [
    "sessions",
    "usage",
    "activity",
    "trends",
    "pinned",
    "insights",
    "trash",
    "recent-edits",
  ] as const;

  const tabs: TopBarTab[] = $derived([
    { id: "sessions", label: m.nav_sessions() },
    { id: "usage", label: m.nav_usage() },
    { id: "activity", label: m.nav_activity() },
    { id: "trends", label: m.nav_trends() },
    { id: "pinned", label: m.nav_pinned() },
    { id: "insights", label: m.nav_insights() },
    { id: "trash", label: m.nav_trash() },
    { id: "recent-edits", label: m.nav_recent_edits() },
  ]);

  const activeTab = $derived(
    (NAV_ROUTES as readonly string[]).includes(router.route)
      ? router.route
      : "",
  );

  const BLOCK_LABELS: Record<BlockType, () => string> = {
    user: m.header_transcript_blocks_user,
    assistant: m.header_transcript_blocks_assistant,
    thinking: m.header_transcript_blocks_thinking,
    tool: m.header_transcript_blocks_tool,
    code: m.header_transcript_blocks_code,
  };

  const BLOCK_COLORS: Record<BlockType, string> = {
    user: "var(--accent-blue)",
    assistant: "var(--accent-purple)",
    thinking: "var(--accent-purple)",
    tool: "var(--accent-amber)",
    code: "var(--text-muted)",
  };

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
    copiedMarkdownLink = true;
    clearTimeout(copiedMarkdownLinkTimer);
    copiedMarkdownLinkTimer = setTimeout(() => {
      copiedMarkdownLink = false;
    }, 1500);
    showExportMenu = false;
    showOverflow = false;
  }

  async function handleCopySourceFilePath() {
    const filePath = sessions.activeSession?.file_path;
    if (!filePath) return;
    const ok = await copyToClipboard(filePath);
    if (!ok) return;
    showExportMenu = false;
    showOverflow = false;
  }

  function openPublish(secret: boolean) {
    const id = sessions.activeSessionId;
    if (!id) return;
    ui.publishSecret = secret;
    ui.setPublishTarget({ kind: "session", id });
    ui.activeModal = "publish";
    showPublishMenu = false;
    showOverflow = false;
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

  // Close block filter dropdown on outside click
  $effect(() => {
    if (!showBlockFilter) return;
    function onClickOutside(e: MouseEvent) {
      const target = e.target as Node;
      if (
        filterBtnRef?.contains(target) ||
        filterDropRef?.contains(target)
      )
        return;
      showBlockFilter = false;
    }
    document.addEventListener("click", onClickOutside, true);
    return () =>
      document.removeEventListener(
        "click",
        onClickOutside,
        true,
      );
  });

  // Close export menu on outside click
  $effect(() => {
    if (!showExportMenu) return;
    function onClickOutside(e: MouseEvent) {
      const target = e.target as Node;
      if (
        exportBtnRef?.contains(target) ||
        exportDropRef?.contains(target)
      )
        return;
      showExportMenu = false;
    }
    document.addEventListener("click", onClickOutside, true);
    return () =>
      document.removeEventListener(
        "click",
        onClickOutside,
        true,
      );
  });

  // Close publish menu on outside click
  $effect(() => {
    if (!showPublishMenu) return;
    function onClickOutside(e: MouseEvent) {
      const target = e.target as Node;
      if (
        publishBtnRef?.contains(target) ||
        publishDropRef?.contains(target)
      )
        return;
      showPublishMenu = false;
    }
    document.addEventListener("click", onClickOutside, true);
    return () =>
      document.removeEventListener(
        "click",
        onClickOutside,
        true,
      );
  });

  // Close overflow dropdown on outside click
  $effect(() => {
    if (!showOverflow) return;
    function onClickOutside(e: MouseEvent) {
      const target = e.target as Node;
      if (
        overflowBtnRef?.contains(target) ||
        overflowDropRef?.contains(target)
      )
        return;
      showOverflow = false;
    }
    document.addEventListener("click", onClickOutside, true);
    return () =>
      document.removeEventListener(
        "click",
        onClickOutside,
        true,
      );
  });
</script>

{#snippet messageLayoutIcon(size: string)}
  {#if ui.messageLayout === "default"}
    <LayoutListIcon {size} strokeWidth="2" aria-hidden="true" />
  {:else if ui.messageLayout === "compact"}
    <ListCollapseIcon {size} strokeWidth="2" aria-hidden="true" />
  {:else if ui.messageLayout === "stream"}
    <LogsIcon {size} strokeWidth="2" aria-hidden="true" />
  {:else}
    <AlignJustifyIcon {size} strokeWidth="2" aria-hidden="true" />
  {/if}
{/snippet}

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
    <button
      class="hamburger"
      onclick={() => {
        if (ui.isMobileViewport && router.route !== "sessions") {
          router.navigate("sessions");
          ui.sidebarOpen = true;
        } else {
          ui.toggleSidebar();
        }
      }}
      title={m.nav_toggle_sidebar_shortcut()}
      aria-label={m.nav_toggle_sidebar()}
    >
      <MenuIcon size="16" strokeWidth="2" aria-hidden="true" />
    </button>
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
      <!-- Transcript controls: mode pills + filter, grouped visually -->
      <div class="transcript-strip">
        <button
          class="pill"
          class:active={ui.transcriptMode === "normal"}
          onclick={() => ui.setTranscriptMode("normal")}
          title={m.header_transcript_normal_title()}
          aria-label={m.header_transcript_normal_label()}
        >
          <span class="pill-label">{m.header_transcript_normal()}</span>
        </button>
        <button
          class="pill"
          class:active={ui.transcriptMode === "focused"}
          onclick={() => ui.setTranscriptMode("focused")}
          title={m.header_transcript_focused_title()}
          aria-label={m.header_transcript_focused_label()}
        >
          <span class="pill-label">{m.header_transcript_focused()}</span>
        </button>

        <span class="strip-divider"></span>

        <div class="filter-wrap">
          <button
            class="pill pill-icon"
            class:filter-active={ui.hasBlockFilters}
            bind:this={filterBtnRef}
            onclick={() => (showBlockFilter = !showBlockFilter)}
            title={m.header_transcript_filter_title()}
            aria-label={m.header_transcript_filter_label()}
          >
            <FunnelIcon size="12" strokeWidth="2" aria-hidden="true" />
            {#if ui.hasBlockFilters}
              <span class="filter-badge">{ui.hiddenBlockCount}</span>
            {/if}
          </button>

          {#if showBlockFilter}
            <div class="block-filter-dropdown kit-popover-card" bind:this={filterDropRef}>
              <div class="block-filter-title">{m.header_transcript_visibility()}</div>
              {#each ALL_BLOCK_TYPES as bt}
                {@const visible = ui.isBlockVisible(bt)}
                <button
                  class="block-filter-item"
                  class:active={visible}
                  onclick={() => ui.toggleBlock(bt)}
                >
                  <span
                    class="block-filter-dot"
                    style:background={visible ? BLOCK_COLORS[bt] : "var(--border-muted)"}
                  ></span>
                  <span class="block-filter-label">{BLOCK_LABELS[bt]()}</span>
                  <span class="block-filter-check" class:on={visible}>
                    {#if visible}
                      <CheckIcon size="10" strokeWidth="2.4" aria-hidden="true" />
                    {/if}
                  </span>
                </button>
              {/each}
              {#if ui.hasBlockFilters}
                <button
                  class="block-filter-reset"
                  onclick={() => ui.showAllBlocks()}
                >
                  {m.header_transcript_show_all()}
                </button>
              {/if}
            </div>
          {/if}
        </div>
      </div>

      <button
        class="header-btn"
        class:active={ui.followLatest}
        onclick={() => ui.toggleFollowLatest()}
        title={m.header_actions_follow_latest()}
        aria-label={m.header_actions_follow_latest()}
        aria-pressed={ui.followLatest}
      >
        <ArrowDownIcon size="14" strokeWidth="2" aria-hidden="true" />
      </button>

      <button
        class="header-btn"
        onclick={() => ui.toggleSort()}
        title={m.header_actions_toggle_sort()}
        aria-label={m.header_actions_toggle_sort()}
      >
        {#if ui.sortNewestFirst}
          <ArrowDownWideNarrowIcon size="14" strokeWidth="2" aria-hidden="true" />
        {:else}
          <ArrowUpNarrowWideIcon size="14" strokeWidth="2" aria-hidden="true" />
        {/if}
      </button>

      <!-- Layout, export, publish: collapse into overflow at narrow widths -->
      <button
        class="header-btn collapsible"
        onclick={() => ui.cycleLayout()}
        title={m.header_actions_cycle_layout({ layout: ui.messageLayout })}
        aria-label={m.header_actions_cycle_layout_label()}
      >
        {@render messageLayoutIcon("14")}
      </button>

      <div class="export-wrap collapsible">
        <button
          class="header-btn"
          bind:this={exportBtnRef}
          onclick={() => {
            showExportMenu = !showExportMenu;
            showOverflow = false;
          }}
          disabled={!sessions.activeSessionId}
          title={m.header_actions_export_options()}
          aria-label={m.header_actions_export_session()}
          aria-expanded={showExportMenu}
        >
          <CloudUploadIcon size="14" strokeWidth="2" aria-hidden="true" />
        </button>

        {#if showExportMenu}
          <div class="export-dropdown kit-popover-card" bind:this={exportDropRef}>
            <button
              class="overflow-item"
              onclick={() => {
                handleExport();
                showExportMenu = false;
              }}
            >
              <CloudUploadIcon size="13" strokeWidth="2" aria-hidden="true" />
              <span>{m.header_actions_download_html()}</span>
            </button>
            <button
              class="overflow-item"
              onclick={handleCopyMarkdownExportLink}
            >
              {#if copiedMarkdownLink}
                <CheckIcon size="13" strokeWidth="2.4" aria-hidden="true" />
              {:else}
                <LinkIcon size="13" strokeWidth="2" aria-hidden="true" />
              {/if}
              <span>
                {#if copiedMarkdownLink}
                  {m.header_actions_copied_markdown_link()}
                {:else}
                  {m.header_actions_copy_markdown_link()}
                {/if}
              </span>
            </button>
            {#if activeSessionFilePath}
              <button
                class="overflow-item"
                onclick={handleCopySourceFilePath}
              >
                <CopyIcon size="13" strokeWidth="2" aria-hidden="true" />
                <span>{m.header_actions_copy_source_path()}</span>
              </button>
            {/if}
          </div>
        {/if}
      </div>

      <div class="export-wrap collapsible">
        <button
          class="header-btn"
          bind:this={publishBtnRef}
          onclick={() => {
            showPublishMenu = !showPublishMenu;
            showExportMenu = false;
            showOverflow = false;
          }}
          disabled={!sessions.activeSessionId}
          title={m.header_actions_publish_title()}
          aria-label={m.header_actions_publish_label()}
          aria-expanded={showPublishMenu}
        >
          <UploadIcon size="14" strokeWidth="2" aria-hidden="true" />
        </button>

        {#if showPublishMenu}
          <div class="export-dropdown kit-popover-card" bind:this={publishDropRef}>
            <button
              class="overflow-item"
              onclick={() => openPublish(false)}
            >
              <GlobeIcon size="13" strokeWidth="2" aria-hidden="true" />
              <span>{m.header_actions_publish_public()}</span>
            </button>
            <button
              class="overflow-item"
              onclick={() => openPublish(true)}
            >
              <LockIcon size="13" strokeWidth="2" aria-hidden="true" />
              <span>{m.header_actions_publish_secret()}</span>
            </button>
          </div>
        {/if}
      </div>

      <!-- Overflow menu (visible only at narrow widths) -->
      <div class="overflow-wrap">
        <button
          class="header-btn overflow-btn"
          bind:this={overflowBtnRef}
          onclick={() => (showOverflow = !showOverflow)}
          title={m.header_actions_more_actions()}
          aria-label={m.header_actions_more_actions()}
        >
          <MoreHorizontalIcon size="14" strokeWidth="2.4" aria-hidden="true" />
        </button>

        {#if showOverflow}
          <div class="overflow-dropdown kit-popover-card" bind:this={overflowDropRef}>
            <button
              class="overflow-item"
              onclick={() => { ui.cycleLayout(); showOverflow = false; }}
            >
              {@render messageLayoutIcon("13")}
              <span>{m.header_actions_layout({ layout: ui.messageLayout })}</span>
            </button>
            <button
              class="overflow-item"
              onclick={() => { handleExport(); showOverflow = false; }}
            >
              <CloudUploadIcon size="13" strokeWidth="2" aria-hidden="true" />
              <span>{m.header_actions_download_html()}</span>
            </button>
            <button
              class="overflow-item"
              onclick={handleCopyMarkdownExportLink}
            >
              {#if copiedMarkdownLink}
                <CheckIcon size="13" strokeWidth="2.4" aria-hidden="true" />
              {:else}
                <LinkIcon size="13" strokeWidth="2" aria-hidden="true" />
              {/if}
              <span>
                {#if copiedMarkdownLink}
                  {m.header_actions_copied_markdown_link()}
                {:else}
                  {m.header_actions_copy_markdown_link()}
                {/if}
              </span>
            </button>
            {#if activeSessionFilePath}
              <button
                class="overflow-item"
                onclick={handleCopySourceFilePath}
              >
                <CopyIcon size="13" strokeWidth="2" aria-hidden="true" />
                <span>{m.header_actions_copy_source_path()}</span>
              </button>
            {/if}
            <button
              class="overflow-item"
              onclick={() => openPublish(false)}
            >
              <UploadIcon size="13" strokeWidth="2" aria-hidden="true" />
              <span>{m.header_actions_publish_public()}</span>
            </button>
            <button
              class="overflow-item"
              onclick={() => openPublish(true)}
            >
              <LockIcon size="13" strokeWidth="2" aria-hidden="true" />
              <span>{m.header_actions_publish_secret()}</span>
            </button>
          </div>
        {/if}
      </div>
    {/if}

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
        <DatabaseBackupIcon size="14" strokeWidth="2" aria-hidden="true" />
      {/if}
      <span class="sync-label" class:collapsed={navCollapsed}>{sync.readOnly ? m.header_actions_refresh() : m.header_actions_sync()}</span>
    </button>

    <button
      class="import-btn"
      onclick={() => {
        if (!sync.readOnly) showImportModal = true;
      }}
      disabled={sync.readOnly}
      title={sync.readOnly
        ? m.header_actions_import_unavailable()
        : m.header_actions_import_conversations()}
      aria-label={m.header_actions_import_conversations()}
    >
      <DownloadIcon size="12" strokeWidth="2" aria-hidden="true" />
      <span class="import-label" class:collapsed={navCollapsed}>{m.header_actions_import()}</span>
    </button>

    <span class="header-divider"></span>

    <button
      class="header-btn"
      onclick={() => ui.toggleTheme()}
      title={m.header_actions_toggle_theme()}
      aria-label={m.header_actions_toggle_theme()}
    >
      {#if ui.theme === "light"}
        <MoonIcon size="14" strokeWidth="2" aria-hidden="true" />
      {:else}
        <SunIcon size="14" strokeWidth="2" aria-hidden="true" />
      {/if}
    </button>

    <button
      class="header-btn"
      class:active={router.route === "settings"}
      onclick={() => router.navigate("settings")}
      title={m.header_actions_settings()}
      aria-label={m.header_actions_settings()}
    >
      <SettingsIcon size="14" strokeWidth="2" aria-hidden="true" />
    </button>

    <button
      class="header-btn"
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

  /* ── Transcript strip: mode pills + filter ── */
  .transcript-strip {
    display: flex;
    align-items: stretch;
    height: 26px;
    border: 1px solid var(--border-default);
    border-radius: var(--radius-sm);
    margin-right: 4px;
    flex-shrink: 0;
  }

  .filter-wrap {
    position: relative;
    display: flex;
  }

  .pill {
    height: 100%;
    display: flex;
    align-items: center;
    justify-content: center;
    padding: 0 9px;
    font-size: 11px;
    font-weight: 500;
    color: var(--text-muted);
    background: transparent;
    transition: background 0.1s, color 0.1s;
    white-space: nowrap;
    cursor: pointer;
    border: none;
    border-radius: 0;
  }

  .pill:hover {
    background: var(--bg-surface-hover);
    color: var(--text-secondary);
  }

  .pill.active {
    background: color-mix(
      in srgb,
      var(--accent-blue) 12%,
      transparent
    );
    color: var(--accent-blue);
    font-weight: 600;
  }

  /* Match parent's border-radius on outer edges */
  .pill:first-child {
    border-radius: var(--radius-sm) 0 0 var(--radius-sm);
  }

  .pill-icon {
    padding: 0 7px;
    position: relative;
  }

  .filter-wrap:last-child .pill {
    border-radius: 0 var(--radius-sm) var(--radius-sm) 0;
  }

  .pill.filter-active {
    color: var(--accent-purple);
  }

  .strip-divider {
    width: 1px;
    height: 14px;
    background: var(--border-default);
    flex-shrink: 0;
    align-self: center;
  }

  .filter-badge {
    position: absolute;
    top: 0px;
    right: 0px;
    width: 11px;
    height: 11px;
    border-radius: 50%;
    background: var(--accent-amber);
    color: var(--accent-amber-foreground);
    font-size: 7px;
    font-weight: 700;
    display: flex;
    align-items: center;
    justify-content: center;
    line-height: 1;
    pointer-events: none;
  }

  /* ── Block filter dropdown ── */
  .block-filter-dropdown {
    position: absolute;
    top: 100%;
    right: 0;
    margin-top: 4px;
    width: 190px;
    /* card chrome comes from the shared kit-popover-card class */
    padding: 6px 0;
    z-index: 100;
    animation: dropdown-in 0.12s ease-out;
    transform-origin: top right;
  }

  @keyframes dropdown-in {
    from {
      opacity: 0;
      transform: scale(0.95) translateY(-2px);
    }
    to {
      opacity: 1;
      transform: scale(1) translateY(0);
    }
  }

  .block-filter-title {
    padding: 4px 12px 6px;
    font-size: 9px;
    font-weight: 600;
    color: var(--text-muted);
    text-transform: uppercase;
    letter-spacing: 0.04em;
  }

  .block-filter-item {
    display: flex;
    align-items: center;
    gap: 8px;
    width: 100%;
    padding: 5px 12px;
    font-size: 12px;
    color: var(--text-secondary);
    text-align: left;
    transition: background 0.08s;
  }

  .block-filter-item:hover {
    background: var(--bg-surface-hover);
  }

  .block-filter-item:not(.active) {
    opacity: 0.5;
  }

  .block-filter-dot {
    width: 6px;
    height: 6px;
    border-radius: 50%;
    flex-shrink: 0;
    transition: background 0.1s;
  }

  .block-filter-label {
    flex: 1;
  }

  .block-filter-check {
    width: 14px;
    height: 14px;
    display: flex;
    align-items: center;
    justify-content: center;
    color: var(--accent-green);
    flex-shrink: 0;
  }

  .block-filter-reset {
    display: block;
    width: calc(100% - 16px);
    margin: 6px 8px 2px;
    padding: 4px 8px;
    font-size: 10px;
    color: var(--text-muted);
    text-align: center;
    border-top: 1px solid var(--border-muted);
    padding-top: 8px;
    transition: color 0.1s;
  }

  .block-filter-reset:hover {
    color: var(--text-primary);
  }

  /* ── Header icon buttons ── */
  .header-btn {
    width: 28px;
    height: 28px;
    display: flex;
    align-items: center;
    justify-content: center;
    border-radius: var(--radius-sm);
    color: var(--text-muted);
    font-size: 12px;
    font-weight: 600;
    transition: background 0.12s, color 0.12s;
    flex-shrink: 0;
  }

  .header-btn:hover:not(:disabled) {
    background: var(--bg-surface-hover);
    color: var(--text-secondary);
  }

  .header-btn:disabled {
    opacity: 0.55;
    cursor: default;
  }

  .header-btn.active {
    color: var(--accent-purple);
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
    opacity: 0.55;
    cursor: default;
  }

  .header-divider {
    width: 1px;
    height: 14px;
    background: var(--border-muted);
    margin: 0 2px;
    flex-shrink: 0;
  }

  .export-wrap {
    position: relative;
    display: flex;
  }

  .export-dropdown {
    position: absolute;
    top: 100%;
    right: 0;
    margin-top: 4px;
    width: 220px;
    /* card chrome comes from the shared kit-popover-card class */
    padding: 4px 0;
    z-index: 100;
    animation: dropdown-in 0.12s ease-out;
    transform-origin: top right;
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

  /* ── Overflow menu (narrow viewports) ── */
  .overflow-wrap {
    position: relative;
    display: none;
  }

  .overflow-dropdown {
    position: absolute;
    top: 100%;
    right: 0;
    margin-top: 4px;
    width: 180px;
    /* card chrome comes from the shared kit-popover-card class */
    padding: 4px 0;
    z-index: 100;
    animation: dropdown-in 0.12s ease-out;
    transform-origin: top right;
  }

  .overflow-item {
    display: flex;
    align-items: center;
    gap: 8px;
    width: 100%;
    padding: 6px 12px;
    font-size: 12px;
    color: var(--text-secondary);
    text-align: left;
    transition: background 0.08s;
    white-space: nowrap;
  }

  .overflow-item:hover {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }

  .overflow-item :global(svg) {
    flex-shrink: 0;
    color: var(--text-muted);
  }

  /* ── Responsive ──
   * Nav tabs and the search field degrade by measurement (TopBar +
   * FitStages); only the app-owned side-region content still uses the
   * shared layout breakpoints. */

  /* 760px: hide the project picker; collapse layout/export/publish into
   * the overflow menu; shrink mode pills to single letters */
  @media (max-width: 760px) {
    .project-picker {
      display: none;
    }

    .collapsible {
      display: none;
    }

    .overflow-wrap {
      display: block;
    }

    .pill-label {
      font-size: 0;
    }

    /* Show first letter only via data attrs */
    .pill:nth-child(1) .pill-label::after {
      content: "N";
      font-size: 11px;
    }

    .pill:nth-child(2) .pill-label::after {
      content: "F";
      font-size: 11px;
    }

    .pill {
      padding: 0 7px;
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
    .import-btn {
      min-width: 44px;
      min-height: 44px;
    }

    .transcript-strip {
      min-height: 44px;
    }

    .pill {
      min-height: 44px;
    }
  }
</style>
