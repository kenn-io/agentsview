<script lang="ts">
  import { ui, ALL_BLOCK_TYPES, type BlockType } from "../../stores/ui.svelte.js";
  import { sessions } from "../../stores/sessions.svelte.js";
  import { sync } from "../../stores/sync.svelte.js";
  import { router } from "../../stores/router.svelte.js";
  import { downloadExport } from "../../api/client.js";
  import ProjectTypeahead from "./ProjectTypeahead.svelte";

  const isMac = navigator.platform.toUpperCase().includes("MAC");
  const modKey = isMac ? "Cmd" : "Ctrl";

  let showBlockFilter = $state(false);
  let filterBtnRef: HTMLButtonElement | undefined =
    $state(undefined);
  let filterDropRef: HTMLDivElement | undefined =
    $state(undefined);

  const BLOCK_LABELS: Record<BlockType, string> = {
    user: "User messages",
    assistant: "Assistant text",
    thinking: "Thinking blocks",
    tool: "Tool calls",
    code: "Code blocks",
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

  const hasActiveSession = $derived(
    sessions.activeSessionId !== null,
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
</script>

<header class="header">
  <div class="header-left">
    <button
      class="header-home"
      onclick={() => {
        sessions.deselectSession();
        router.navigate("sessions");
      }}
      title="Home"
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

    <ProjectTypeahead
      projects={sessions.projects}
      value={sessions.filters.project}
      onselect={(v) => sessions.setProjectFilter(v)}
    />

    <button
      class="nav-btn"
      class:active={router.route === "sessions"}
      onclick={() => {
        sessions.deselectSession();
        router.navigate("sessions");
      }}
      title="Sessions"
    >
      <svg width="12" height="12" viewBox="0 0 16 16" fill="currentColor" aria-hidden="true">
        <path d="M0 1.5A1.5 1.5 0 011.5 0h2A1.5 1.5 0 015 1.5v2A1.5 1.5 0 013.5 5h-2A1.5 1.5 0 010 3.5v-2zm6 0A1.5 1.5 0 017.5 0h2A1.5 1.5 0 0111 1.5v2A1.5 1.5 0 019.5 5h-2A1.5 1.5 0 016 3.5v-2zm5 0A1.5 1.5 0 0112.5 0h2A1.5 1.5 0 0116 1.5v2A1.5 1.5 0 0114.5 5h-2A1.5 1.5 0 0111 3.5v-2zM0 7.5A1.5 1.5 0 011.5 6h2A1.5 1.5 0 015 7.5v2A1.5 1.5 0 013.5 11h-2A1.5 1.5 0 010 9.5v-2zm6 0A1.5 1.5 0 017.5 6h2A1.5 1.5 0 0111 7.5v2A1.5 1.5 0 019.5 11h-2A1.5 1.5 0 016 9.5v-2zm5 0A1.5 1.5 0 0112.5 6h2A1.5 1.5 0 0116 7.5v2a1.5 1.5 0 01-1.5 1.5h-2A1.5 1.5 0 0111 9.5v-2zM0 13.5A1.5 1.5 0 011.5 12h2A1.5 1.5 0 015 13.5v2A1.5 1.5 0 013.5 17h-2A1.5 1.5 0 010 15.5v-2zm6 0A1.5 1.5 0 017.5 12h2a1.5 1.5 0 011.5 1.5v2A1.5 1.5 0 019.5 17h-2A1.5 1.5 0 016 15.5v-2zm5 0a1.5 1.5 0 011.5-1.5h2a1.5 1.5 0 011.5 1.5v2a1.5 1.5 0 01-1.5 1.5h-2a1.5 1.5 0 01-1.5-1.5v-2z"/>
      </svg>
      Sessions
    </button>

    <button
      class="nav-btn"
      class:active={router.route === "pinned"}
      onclick={() => router.navigate("pinned")}
      title="Pinned Messages"
    >
      <svg width="12" height="12" viewBox="0 0 16 16" fill="currentColor" aria-hidden="true">
        <path d="M4.146.146A.5.5 0 014.5 0h7a.5.5 0 01.5.5c0 .68-.342 1.174-.646 1.479-.126.125-.25.224-.354.298v4.431l.078.048c.203.127.476.314.751.555C12.36 7.775 13 8.527 13 9.5a.5.5 0 01-.5.5H8.5v5.5a.5.5 0 01-1 0V10H3.5a.5.5 0 01-.5-.5c0-.973.64-1.725 1.17-2.189A6 6 0 015 6.708V2.277a3 3 0 01-.354-.298C4.342 1.674 4 1.179 4 .5a.5.5 0 01.146-.354z"/>
      </svg>
      Pinned
    </button>

    <button
      class="nav-btn"
      class:active={router.route === "insights"}
      onclick={() => router.navigate("insights")}
      title="Insights"
    >
      <svg width="12" height="12" viewBox="0 0 16 16" fill="currentColor" aria-hidden="true">
        <path d="M14.5 3a.5.5 0 01.5.5v9a.5.5 0 01-.5.5h-13a.5.5 0 01-.5-.5v-9a.5.5 0 01.5-.5h13zm-13-1A1.5 1.5 0 000 3.5v9A1.5 1.5 0 001.5 14h13a1.5 1.5 0 001.5-1.5v-9A1.5 1.5 0 0014.5 2h-13z"/>
        <path d="M3 5.5a.5.5 0 01.5-.5h9a.5.5 0 010 1h-9a.5.5 0 01-.5-.5zM3 8a.5.5 0 01.5-.5h9a.5.5 0 010 1h-9A.5.5 0 013 8zm0 2.5a.5.5 0 01.5-.5h6a.5.5 0 010 1h-6a.5.5 0 01-.5-.5z"/>
      </svg>
      Insights
    </button>

    <button
      class="nav-btn"
      class:active={router.route === "trash"}
      onclick={() => router.navigate("trash")}
      title="Trash"
    >
      <svg width="12" height="12" viewBox="0 0 16 16" fill="currentColor" aria-hidden="true">
        <path d="M5.5 5.5A.5.5 0 016 6v6a.5.5 0 01-1 0V6a.5.5 0 01.5-.5zm2.5 0a.5.5 0 01.5.5v6a.5.5 0 01-1 0V6a.5.5 0 01.5-.5zm3 .5a.5.5 0 00-1 0v6a.5.5 0 001 0V6z"/>
        <path fill-rule="evenodd" d="M14.5 3a1 1 0 01-1 1H13v9a2 2 0 01-2 2H5a2 2 0 01-2-2V4h-.5a1 1 0 01-1-1V2a1 1 0 011-1H5.5l1-1h3l1 1h2.5a1 1 0 011 1v1zM4.118 4L4 4.059V13a1 1 0 001 1h6a1 1 0 001-1V4.059L11.882 4H4.118zM2.5 3V2h11v1h-11z"/>
      </svg>
      Trash
    </button>
  </div>

  <button
    class="search-hint"
    onclick={() => (ui.activeModal = "commandPalette")}
    title="Search sessions ({modKey}+K)"
  >
    <svg width="12" height="12" viewBox="0 0 16 16" fill="currentColor">
      <path d="M11.742 10.344a6.5 6.5 0 10-1.397 1.398h-.001l3.85 3.85a1 1 0 001.415-1.414l-3.85-3.85zm-5.44.656a5 5 0 110-10 5 5 0 010 10z"/>
    </svg>
    <span class="search-hint-text">Search sessions...</span>
    <kbd class="search-hint-kbd">{modKey}+K</kbd>
  </button>

  <div class="header-right">
    {#if hasActiveSession}
      <div class="block-filter-wrap">
        <button
          class="header-btn"
          class:active={ui.hasBlockFilters}
          bind:this={filterBtnRef}
          onclick={() => (showBlockFilter = !showBlockFilter)}
          title="Filter block types"
          aria-label="Filter block types"
        >
          <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
            <polygon points="22 3 2 3 10 12.46 10 19 14 21 14 12.46 22 3"/>
          </svg>
          {#if ui.hasBlockFilters}
            <span class="block-filter-badge">{ui.hiddenBlockCount}</span>
          {/if}
        </button>

        {#if showBlockFilter}
          <div class="block-filter-dropdown" bind:this={filterDropRef}>
            <div class="block-filter-title">Block Visibility</div>
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
                <span class="block-filter-label">{BLOCK_LABELS[bt]}</span>
                <span class="block-filter-check" class:on={visible}>
                  {#if visible}
                    <svg width="10" height="10" viewBox="0 0 16 16" fill="currentColor">
                      <path d="M13.78 4.22a.75.75 0 010 1.06l-7.25 7.25a.75.75 0 01-1.06 0L2.22 9.28a.75.75 0 011.06-1.06L6 10.94l6.72-6.72a.75.75 0 011.06 0z"/>
                    </svg>
                  {/if}
                </span>
              </button>
            {/each}
            {#if ui.hasBlockFilters}
              <button
                class="block-filter-reset"
                onclick={() => ui.showAllBlocks()}
              >
                Show all
              </button>
            {/if}
          </div>
        {/if}
      </div>

      <button
        class="header-btn"
        onclick={() => ui.toggleSort()}
        title="Toggle sort order (o)"
        aria-label="Toggle sort order"
      >
        {#if ui.sortNewestFirst}
          <svg width="14" height="14" viewBox="0 0 16 16" fill="currentColor">
            <path d="M3.5 3a.5.5 0 01.5.5v8.793l2.146-2.147a.5.5 0 01.708.708l-3 3a.5.5 0 01-.708 0l-3-3a.5.5 0 01.708-.708L3 12.293V3.5a.5.5 0 01.5-.5zm4 0h7a.5.5 0 010 1h-7a.5.5 0 010-1zm0 3h5a.5.5 0 010 1h-5a.5.5 0 010-1zm0 3h3a.5.5 0 010 1h-3a.5.5 0 010-1z"/>
          </svg>
        {:else}
          <svg width="14" height="14" viewBox="0 0 16 16" fill="currentColor">
            <path d="M3.5 13a.5.5 0 00.5-.5V3.707l2.146 2.147a.5.5 0 00.708-.708l-3-3a.5.5 0 00-.708 0l-3 3a.5.5 0 00.708.708L3 3.707V12.5a.5.5 0 00.5.5zm4-10h3a.5.5 0 010 1h-3a.5.5 0 010-1zm0 3h5a.5.5 0 010 1h-5a.5.5 0 010-1zm0 3h7a.5.5 0 010 1h-7a.5.5 0 010-1z"/>
          </svg>
        {/if}
      </button>

      <button
        class="header-btn"
        onclick={() => ui.cycleLayout()}
        title="Cycle layout: {ui.messageLayout} (l)"
        aria-label="Cycle message layout"
      >
        {#if ui.messageLayout === "default"}
          <!-- Default: cards/bordered -->
          <svg width="14" height="14" viewBox="0 0 16 16" fill="currentColor">
            <path d="M1.5 2A1.5 1.5 0 000 3.5v2A1.5 1.5 0 001.5 7h13A1.5 1.5 0 0016 5.5v-2A1.5 1.5 0 0014.5 2h-13zm0 1h13a.5.5 0 01.5.5v2a.5.5 0 01-.5.5h-13a.5.5 0 01-.5-.5v-2a.5.5 0 01.5-.5zm0 6A1.5 1.5 0 000 10.5v2A1.5 1.5 0 001.5 14h13a1.5 1.5 0 001.5-1.5v-2A1.5 1.5 0 0014.5 9h-13zm0 1h13a.5.5 0 01.5.5v2a.5.5 0 01-.5.5h-13a.5.5 0 01-.5-.5v-2a.5.5 0 01.5-.5z"/>
          </svg>
        {:else if ui.messageLayout === "compact"}
          <!-- Compact: terminal lines -->
          <svg width="14" height="14" viewBox="0 0 16 16" fill="currentColor">
            <path d="M3 4l4 4-4 4" stroke="currentColor" fill="none" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"/>
            <line x1="9" y1="12" x2="14" y2="12" stroke="currentColor" stroke-width="1.5" stroke-linecap="round"/>
          </svg>
        {:else}
          <!-- Stream: continuous flow -->
          <svg width="14" height="14" viewBox="0 0 16 16" fill="currentColor">
            <rect x="1" y="1" width="14" height="4" rx="1" opacity="0.2"/>
            <rect x="1" y="6" width="14" height="4" rx="1" opacity="0.08"/>
            <rect x="1" y="11" width="14" height="4" rx="1" opacity="0.2"/>
          </svg>
        {/if}
      </button>

      <button
        class="header-btn"
        onclick={handleExport}
        disabled={!sessions.activeSessionId}
        title="Export session (e)"
        aria-label="Export session"
      >
        <svg width="14" height="14" viewBox="0 0 16 16" fill="currentColor">
          <path d="M4.406 1.342A5.53 5.53 0 018 0c2.69 0 4.923 2 5.166 4.579C14.758 4.804 16 6.137 16 7.773 16 9.569 14.502 11 12.687 11H10a.5.5 0 010-1h2.688C13.979 10 15 8.988 15 7.773c0-1.216-1.02-2.228-2.313-2.228h-.5v-.5C12.188 2.825 10.328 1 8 1a4.53 4.53 0 00-2.941 1.1c-.757.652-1.153 1.438-1.153 2.055v.448l-.445.049C2.064 4.805 1 5.952 1 7.318 1 8.785 2.23 10 3.781 10H6a.5.5 0 010 1H3.781C1.708 11 0 9.366 0 7.318c0-1.763 1.266-3.223 2.942-3.593.143-.863.698-1.723 1.464-2.383z"/>
          <path d="M7.646 4.146a.5.5 0 01.708 0l3 3a.5.5 0 01-.708.708L8.5 5.707V14.5a.5.5 0 01-1 0V5.707L5.354 7.854a.5.5 0 11-.708-.708l3-3z"/>
        </svg>
      </button>

      <button
        class="header-btn"
        onclick={() => (ui.activeModal = "publish")}
        disabled={!sessions.activeSessionId}
        title="Publish to Gist (p)"
        aria-label="Publish to Gist"
      >
        <svg width="14" height="14" viewBox="0 0 16 16" fill="currentColor">
          <path d="M3.5 13h9a.5.5 0 010 1h-9a.5.5 0 010-1zm4.854-9.354a.5.5 0 00-.708 0l-3 3a.5.5 0 10.708.708L7.5 5.207V11.5a.5.5 0 001 0V5.207l2.146 2.147a.5.5 0 00.708-.708l-3-3z"/>
        </svg>
      </button>
    {/if}

    <button
      class="header-btn"
      class:syncing={sync.syncing}
      onclick={() => sync.triggerSync()}
      disabled={sync.syncing}
      title="Sync sessions (r)"
      aria-label="Sync sessions"
    >
      <svg width="14" height="14" viewBox="0 0 16 16" fill="currentColor">
        <path d="M8 3a5 5 0 00-4.546 2.914.5.5 0 01-.908-.418A6 6 0 0114 8a.5.5 0 01-1 0 5 5 0 00-5-5zm4.546 7.086a.5.5 0 01.908.418A6 6 0 012 8a.5.5 0 011 0 5 5 0 005 5 5 5 0 004.546-2.914z"/>
      </svg>
    </button>

    <span class="header-divider"></span>

    <button
      class="header-btn"
      onclick={() => ui.toggleTheme()}
      title="Toggle theme"
      aria-label="Toggle theme"
    >
      {#if ui.theme === "light"}
        <svg width="14" height="14" viewBox="0 0 16 16" fill="currentColor">
          <path d="M6 .278a.768.768 0 01.08.858 7.208 7.208 0 00-.878 3.46c0 4.021 3.278 7.277 7.318 7.277.527 0 1.04-.055 1.533-.16a.787.787 0 01.81.316.733.733 0 01-.031.893A8.349 8.349 0 018.344 16C3.734 16 0 12.286 0 7.71 0 4.266 2.114 1.312 5.124.06A.752.752 0 016 .278z"/>
        </svg>
      {:else}
        <svg width="14" height="14" viewBox="0 0 16 16" fill="currentColor">
          <path d="M8 12a4 4 0 100-8 4 4 0 000 8zM8 0a.5.5 0 01.5.5v2a.5.5 0 01-1 0v-2A.5.5 0 018 0zm0 13a.5.5 0 01.5.5v2a.5.5 0 01-1 0v-2A.5.5 0 018 13zm8-5a.5.5 0 01-.5.5h-2a.5.5 0 010-1h2A.5.5 0 0116 8zM3 8a.5.5 0 01-.5.5h-2a.5.5 0 010-1h2A.5.5 0 013 8zm10.657-5.657a.5.5 0 010 .707l-1.414 1.414a.5.5 0 11-.707-.707l1.414-1.414a.5.5 0 01.707 0zm-9.193 9.193a.5.5 0 010 .707L3.05 13.657a.5.5 0 01-.707-.707l1.414-1.414a.5.5 0 01.707 0zm9.193 2.121a.5.5 0 01-.707 0l-1.414-1.414a.5.5 0 01.707-.707l1.414 1.414a.5.5 0 010 .707zM4.464 4.465a.5.5 0 01-.707 0L2.343 3.05a.5.5 0 01.707-.707l1.414 1.414a.5.5 0 010 .708z"/>
        </svg>
      {/if}
    </button>

    <button
      class="header-btn"
      class:active={router.route === "settings"}
      onclick={() => router.navigate("settings")}
      title="Settings"
      aria-label="Settings"
    >
      <svg width="14" height="14" viewBox="0 0 16 16" fill="currentColor">
        <path d="M9.405 1.05c-.413-1.4-2.397-1.4-2.81 0l-.1.34a1.464 1.464 0 01-2.105.872l-.31-.17c-1.283-.698-2.686.705-1.987 1.987l.169.311c.446.82.023 1.841-.872 2.105l-.34.1c-1.4.413-1.4 2.397 0 2.81l.34.1a1.464 1.464 0 01.872 2.105l-.17.31c-.698 1.283.705 2.686 1.987 1.987l.311-.169a1.464 1.464 0 012.105.872l.1.34c.413 1.4 2.397 1.4 2.81 0l.1-.34a1.464 1.464 0 012.105-.872l.31.17c1.283.698 2.686-.705 1.987-1.987l-.169-.311a1.464 1.464 0 01.872-2.105l.34-.1c1.4-.413 1.4-2.397 0-2.81l-.34-.1a1.464 1.464 0 01-.872-2.105l.17-.31c.698-1.283-.705-2.686-1.987-1.987l-.311.169a1.464 1.464 0 01-2.105-.872l-.1-.34zM8 10.93a2.929 2.929 0 110-5.86 2.929 2.929 0 010 5.858z"/>
      </svg>
    </button>

    <button
      class="header-btn"
      onclick={() => (ui.activeModal = "shortcuts")}
      title="Keyboard shortcuts (?)"
    >
      ?
    </button>
  </div>
</header>

<style>
  .header {
    height: 40px;
    display: flex;
    align-items: center;
    justify-content: space-between;
    padding: 0 14px;
    background: var(--bg-surface);
    border-bottom: 1px solid var(--border-default);
    flex-shrink: 0;
    gap: 10px;
  }

  .header-left {
    display: flex;
    align-items: center;
    gap: 12px;
    min-width: 0;
  }

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

.nav-btn {
    height: 26px;
    display: flex;
    align-items: center;
    gap: 5px;
    padding: 0 10px;
    border-radius: var(--radius-sm);
    font-size: 11px;
    font-weight: 500;
    color: var(--text-muted);
    cursor: pointer;
    white-space: nowrap;
    transition: background 0.12s, color 0.12s;
  }

  .nav-btn:hover {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }

  .nav-btn.active {
    color: var(--accent-blue);
    background: color-mix(
      in srgb,
      var(--accent-blue) 8%,
      transparent
    );
  }

  .search-hint {
    height: 26px;
    display: flex;
    align-items: center;
    gap: 6px;
    padding: 0 10px;
    background: var(--bg-inset);
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-md);
    color: var(--text-muted);
    font-size: 11px;
    cursor: pointer;
    white-space: nowrap;
    transition: border-color 0.15s, box-shadow 0.15s;
  }

  .search-hint:hover {
    border-color: var(--border-default);
    box-shadow: var(--shadow-sm);
  }

  .search-hint-text {
    color: var(--text-muted);
  }

  .search-hint-kbd {
    font-size: 10px;
    padding: 0 4px;
    border: 1px solid var(--border-default);
    border-radius: var(--radius-sm);
    color: var(--text-muted);
    background: var(--bg-surface);
    font-family: var(--font-sans);
    line-height: 16px;
  }

  .header-right {
    display: flex;
    align-items: center;
    gap: 2px;
  }

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
  }

  .header-btn:hover:not(:disabled) {
    background: var(--bg-surface-hover);
    color: var(--text-secondary);
  }

  .header-btn.active {
    color: var(--accent-purple);
  }

  .header-btn.syncing {
    animation: spin 1s linear infinite;
  }

  .header-divider {
    width: 1px;
    height: 14px;
    background: var(--border-muted);
    margin: 0 2px;
    flex-shrink: 0;
  }

  @keyframes spin {
    from { transform: rotate(0deg); }
    to { transform: rotate(360deg); }
  }

  .block-filter-wrap {
    position: relative;
  }

  .block-filter-badge {
    position: absolute;
    top: 1px;
    right: 1px;
    width: 12px;
    height: 12px;
    border-radius: 50%;
    background: var(--accent-amber);
    color: white;
    font-size: 8px;
    font-weight: 700;
    display: flex;
    align-items: center;
    justify-content: center;
    line-height: 1;
    pointer-events: none;
  }

  .block-filter-dropdown {
    position: absolute;
    top: 100%;
    right: 0;
    margin-top: 4px;
    width: 190px;
    background: var(--bg-surface);
    border: 1px solid var(--border-default);
    border-radius: var(--radius-md);
    box-shadow: var(--shadow-lg);
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
</style>
