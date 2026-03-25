<script lang="ts">
  import type { Snippet } from "svelte";
  import { ui } from "../../stores/ui.svelte.js";
  import { router } from "../../stores/router.svelte.js";
  import type { Route } from "../../stores/router.svelte.js";
  import { sessions } from "../../stores/sessions.svelte.js";

  interface Props {
    sidebar: Snippet;
    content: Snippet;
  }

  let { sidebar, content }: Props = $props();

  function handleBackdropClick() {
    ui.closeSidebar();
  }

  function mobileNav(route: Route) {
    router.navigate(route);
    // Close sidebar for full-page routes; keep open for sessions
    // so the user can select from the list.
    if (route !== "sessions") {
      ui.closeSidebar();
    }
  }
</script>

<div class="layout">
  {#if ui.sidebarOpen}
    <button class="sidebar-backdrop" aria-label="Close sidebar" onclick={handleBackdropClick}></button>
  {/if}
  <aside class="sidebar" class:open={ui.sidebarOpen}>
    <nav class="mobile-nav">
      <button
        class="mobile-nav-btn"
        class:active={router.route === "sessions"}
        onclick={() => mobileNav("sessions")}
      >
        <svg width="12" height="12" viewBox="0 0 16 16" fill="currentColor" aria-hidden="true">
          <path d="M0 1.5A1.5 1.5 0 011.5 0h2A1.5 1.5 0 015 1.5v2A1.5 1.5 0 013.5 5h-2A1.5 1.5 0 010 3.5v-2zm6 0A1.5 1.5 0 017.5 0h2A1.5 1.5 0 0111 1.5v2A1.5 1.5 0 019.5 5h-2A1.5 1.5 0 016 3.5v-2zm5 0A1.5 1.5 0 0112.5 0h2A1.5 1.5 0 0116 1.5v2A1.5 1.5 0 0114.5 5h-2A1.5 1.5 0 0111 3.5v-2zM0 7.5A1.5 1.5 0 011.5 6h2A1.5 1.5 0 015 7.5v2A1.5 1.5 0 013.5 11h-2A1.5 1.5 0 010 9.5v-2zm6 0A1.5 1.5 0 017.5 6h2A1.5 1.5 0 0111 7.5v2A1.5 1.5 0 019.5 11h-2A1.5 1.5 0 016 9.5v-2zm5 0A1.5 1.5 0 0112.5 6h2A1.5 1.5 0 0116 7.5v2a1.5 1.5 0 01-1.5 1.5h-2A1.5 1.5 0 0111 9.5v-2zM0 13.5A1.5 1.5 0 011.5 12h2A1.5 1.5 0 015 13.5v2A1.5 1.5 0 013.5 17h-2A1.5 1.5 0 010 15.5v-2zm6 0A1.5 1.5 0 017.5 12h2a1.5 1.5 0 011.5 1.5v2A1.5 1.5 0 019.5 17h-2A1.5 1.5 0 016 15.5v-2zm5 0a1.5 1.5 0 011.5-1.5h2a1.5 1.5 0 011.5 1.5v2a1.5 1.5 0 01-1.5 1.5h-2a1.5 1.5 0 01-1.5-1.5v-2z"/>
        </svg>
        Sessions
      </button>
      <button
        class="mobile-nav-btn"
        class:active={router.route === "pinned"}
        onclick={() => mobileNav("pinned")}
      >
        <svg width="12" height="12" viewBox="0 0 16 16" fill="currentColor" aria-hidden="true">
          <path d="M4.146.146A.5.5 0 014.5 0h7a.5.5 0 01.5.5c0 .68-.342 1.174-.646 1.479-.126.125-.25.224-.354.298v4.431l.078.048c.203.127.476.314.751.555C12.36 7.775 13 8.527 13 9.5a.5.5 0 01-.5.5H8.5v5.5a.5.5 0 01-1 0V10H3.5a.5.5 0 01-.5-.5c0-.973.64-1.725 1.17-2.189A6 6 0 015 6.708V2.277a3 3 0 01-.354-.298C4.342 1.674 4 1.179 4 .5a.5.5 0 01.146-.354z"/>
        </svg>
        Pinned
      </button>
      <button
        class="mobile-nav-btn"
        class:active={router.route === "insights"}
        onclick={() => mobileNav("insights")}
      >
        <svg width="12" height="12" viewBox="0 0 16 16" fill="currentColor" aria-hidden="true">
          <path d="M14.5 3a.5.5 0 01.5.5v9a.5.5 0 01-.5.5h-13a.5.5 0 01-.5-.5v-9a.5.5 0 01.5-.5h13zm-13-1A1.5 1.5 0 000 3.5v9A1.5 1.5 0 001.5 14h13a1.5 1.5 0 001.5-1.5v-9A1.5 1.5 0 0014.5 2h-13z"/>
          <path d="M3 5.5a.5.5 0 01.5-.5h9a.5.5 0 010 1h-9a.5.5 0 01-.5-.5zM3 8a.5.5 0 01.5-.5h9a.5.5 0 010 1h-9A.5.5 0 013 8zm0 2.5a.5.5 0 01.5-.5h6a.5.5 0 010 1h-6a.5.5 0 01-.5-.5z"/>
        </svg>
        Insights
      </button>
      <button
        class="mobile-nav-btn"
        class:active={router.route === "trash"}
        onclick={() => mobileNav("trash")}
      >
        <svg width="12" height="12" viewBox="0 0 16 16" fill="currentColor" aria-hidden="true">
          <path d="M5.5 5.5A.5.5 0 016 6v6a.5.5 0 01-1 0V6a.5.5 0 01.5-.5zm2.5 0a.5.5 0 01.5.5v6a.5.5 0 01-1 0V6a.5.5 0 01.5-.5zm3 .5a.5.5 0 00-1 0v6a.5.5 0 001 0V6z"/>
          <path fill-rule="evenodd" d="M14.5 3a1 1 0 01-1 1H13v9a2 2 0 01-2 2H5a2 2 0 01-2-2V4h-.5a1 1 0 01-1-1V2a1 1 0 011-1H5.5l1-1h3l1 1h2.5a1 1 0 011 1v1zM4.118 4L4 4.059V13a1 1 0 001 1h6a1 1 0 001-1V4.059L11.882 4H4.118zM2.5 3V2h11v1h-11z"/>
        </svg>
        Trash
      </button>
    </nav>
    {@render sidebar()}
  </aside>
  <main class="content">
    {@render content()}
  </main>
</div>

<style>
  .layout {
    display: flex;
    height: calc(100vh - var(--header-height, 40px) - var(--status-bar-height, 24px));
    overflow: hidden;
    position: relative;
  }

  .sidebar {
    width: 260px;
    flex-shrink: 0;
    border-right: 1px solid var(--border-default);
    overflow: hidden;
    display: flex;
    flex-direction: column;
    background: var(--bg-surface);
  }

  .sidebar:not(.open) {
    display: none;
  }

  .content {
    flex: 1;
    min-width: 0;
    overflow: hidden;
    display: flex;
    flex-direction: column;
  }

  .sidebar-backdrop {
    display: none;
    border: none;
    padding: 0;
  }

  .mobile-nav {
    display: none;
  }

  @media (max-width: 767px) {
    .sidebar {
      position: fixed;
      top: var(--header-height, 40px);
      bottom: var(--status-bar-height, 24px);
      left: 0;
      width: 280px;
      z-index: 50;
      box-shadow: var(--shadow-lg);
      display: flex;
    }

    .sidebar:not(.open) {
      display: none;
    }

    .sidebar-backdrop {
      display: block;
      position: fixed;
      inset: 0;
      background: var(--overlay-bg);
      z-index: 49;
    }

    .mobile-nav {
      display: flex;
      gap: 2px;
      padding: 8px 8px 0;
      border-bottom: 1px solid var(--border-muted);
      flex-shrink: 0;
    }

    .mobile-nav-btn {
      flex: 1;
      display: flex;
      align-items: center;
      justify-content: center;
      gap: 4px;
      padding: 6px 0;
      font-size: 11px;
      font-weight: 500;
      color: var(--text-muted);
      border-radius: var(--radius-sm) var(--radius-sm) 0 0;
      white-space: nowrap;
      transition: background 0.12s, color 0.12s;
    }

    .mobile-nav-btn:hover {
      background: var(--bg-surface-hover);
      color: var(--text-primary);
    }

    .mobile-nav-btn.active {
      color: var(--accent-blue);
      background: color-mix(in srgb, var(--accent-blue) 8%, transparent);
    }
  }
</style>
