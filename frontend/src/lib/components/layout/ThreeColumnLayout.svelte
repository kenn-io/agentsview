<script lang="ts">
  import type { Snippet } from "svelte";
  import {
    SIDEBAR_DESKTOP_BREAKPOINT,
    SIDEBAR_WIDTH_DEFAULT,
    SIDEBAR_WIDTH_MIN,
    SIDEBAR_WIDTH_STORAGE_MAX,
    clampSidebarWidthForLayout,
    isDesktopSidebarLayout,
  } from "./sidebar-width.js";
  import { ui } from "../../stores/ui.svelte.js";
  import { router } from "../../stores/router.svelte.js";
  import type { Route } from "../../stores/router.svelte.js";
  import { sessions } from "../../stores/sessions.svelte.js";

  interface Props {
    sidebar: Snippet;
    content: Snippet;
    vitals?: Snippet;
  }

  const RESIZE_HANDLE_WIDTH = 12;
  const SIDEBAR_BORDER_WIDTH = 1;

  let { sidebar, content, vitals }: Props = $props();
  let layoutElement = $state<HTMLElement | null>(null);
  let resizeHandleElement = $state<HTMLElement | null>(null);
  let layoutWidth = $state<number | null>(null);
  let viewportWidth = $state(
    typeof window === "undefined"
      ? SIDEBAR_DESKTOP_BREAKPOINT
      : window.innerWidth,
  );
  let isResizing = $state(false);
  let dragState = $state<{
    startX: number;
    startWidth: number;
  } | null>(null);
  let didDragMove = $state(false);
  let activePointerId = $state<number | null>(null);

  const isDesktop = $derived(
    isDesktopSidebarLayout(viewportWidth),
  );
  const currentLayoutWidth = $derived(
    layoutWidth ?? viewportWidth,
  );
  const clampedLayoutWidth = $derived(
    isDesktop
      ? Math.max(
          0,
          currentLayoutWidth -
            RESIZE_HANDLE_WIDTH -
            SIDEBAR_BORDER_WIDTH,
        )
      : currentLayoutWidth,
  );
  const sidebarWidth = $derived(
    isDesktop
      ? clampSidebarWidthForLayout(
          ui.sidebarWidth,
          clampedLayoutWidth,
        )
      : SIDEBAR_WIDTH_DEFAULT,
  );

  function handleBackdropClick() {
    ui.closeSidebar();
  }

  function mobileNav(route: Route) {
    router.navigate(route);
    if (route !== "sessions") {
      ui.closeSidebar();
    }
  }

  function measureLayoutWidth(): number {
    const measuredWidth =
      layoutElement?.getBoundingClientRect().width ??
      layoutElement?.clientWidth ??
      viewportWidth;

    const nextLayoutWidth =
      measuredWidth > 0 ? measuredWidth : viewportWidth;

    layoutWidth = nextLayoutWidth;
    return nextLayoutWidth;
  }

  function updateSidebarWidth(clientX: number) {
    if (!dragState) return;

    const desiredWidth =
      dragState.startWidth + (clientX - dragState.startX);
    const clampedWidth = clampSidebarWidthForLayout(
      desiredWidth,
      Math.max(
        0,
        measureLayoutWidth() -
          RESIZE_HANDLE_WIDTH -
          SIDEBAR_BORDER_WIDTH,
      ),
    );

    if (clampedWidth === sidebarWidth) return;
    ui.setSidebarWidth(clampedWidth);
  }

  function isActiveDragPointer(event: PointerEvent) {
    return (
      activePointerId === null ||
      event.pointerId === activePointerId
    );
  }

  function stopResizing() {
    if (
      resizeHandleElement &&
      activePointerId !== null &&
      typeof resizeHandleElement.releasePointerCapture ===
        "function"
    ) {
      try {
        resizeHandleElement.releasePointerCapture(
          activePointerId,
        );
      } catch {
        // Ignore release failures when capture is absent.
      }
    }

    if (typeof window !== "undefined") {
      window.removeEventListener(
        "pointermove",
        handlePointerMove,
      );
      window.removeEventListener(
        "pointerup",
        handlePointerUp,
      );
      window.removeEventListener(
        "pointercancel",
        handlePointerCancel,
      );
    }

    isResizing = false;
    dragState = null;
    didDragMove = false;
    activePointerId = null;
  }

  function handlePointerMove(event: PointerEvent) {
    if (!dragState) return;
    if (!isActiveDragPointer(event)) return;

    if (event.buttons === 0) {
      stopResizing();
      return;
    }

    const hasMoved =
      didDragMove || event.clientX !== dragState.startX;
    if (!hasMoved) return;

    event.preventDefault();
    didDragMove = true;
    updateSidebarWidth(event.clientX);
  }

  function handlePointerUp(event: PointerEvent) {
    if (!dragState || !isActiveDragPointer(event)) return;

    if (didDragMove) {
      updateSidebarWidth(event.clientX);
    }

    stopResizing();
  }

  function handlePointerCancel(event: PointerEvent) {
    if (!dragState || !isActiveDragPointer(event)) return;
    stopResizing();
  }

  function handlePointerDown(event: PointerEvent) {
    if (!isDesktop || !ui.sidebarOpen || dragState || event.button !== 0) {
      return;
    }

    event.preventDefault();
    dragState = {
      startX: event.clientX,
      startWidth: sidebarWidth,
    };
    didDragMove = false;
    activePointerId =
      typeof event.pointerId === "number"
        ? event.pointerId
        : null;
    isResizing = true;

    if (
      resizeHandleElement &&
      activePointerId !== null &&
      typeof resizeHandleElement.setPointerCapture ===
        "function"
    ) {
      try {
        resizeHandleElement.setPointerCapture(
          activePointerId,
        );
      } catch {
        // Ignore capture failures and keep window listeners as fallback.
      }
    }

    window.addEventListener("pointermove", handlePointerMove);
    window.addEventListener("pointerup", handlePointerUp);
    window.addEventListener("pointercancel", handlePointerCancel);
  }

  $effect(() => {
    if (!layoutElement) return;
    viewportWidth;
    measureLayoutWidth();
  });

  $effect(() => {
    if (
      !layoutElement ||
      typeof ResizeObserver === "undefined"
    ) {
      return;
    }

    const observer = new ResizeObserver(() => {
      measureLayoutWidth();
    });
    observer.observe(layoutElement);

    return () => {
      observer.disconnect();
    };
  });

  $effect(() => {
    return () => {
      stopResizing();
    };
  });

  $effect(() => {
    if ((!isDesktop || !ui.sidebarOpen) && isResizing) {
      stopResizing();
    }
  });

  $effect(() => {
    if (typeof document === "undefined") return;

    document.body.classList.toggle(
      "sidebar-resizing",
      isResizing,
    );

    return () => {
      document.body.classList.remove("sidebar-resizing");
    };
  });
</script>

<svelte:window bind:innerWidth={viewportWidth} />

<div
  class="layout"
  class:is-resizing={isResizing}
  bind:this={layoutElement}
>
  {#if ui.isMobileViewport && ui.sidebarOpen}
    <button
      class="sidebar-backdrop"
      aria-label="Close sidebar"
      onclick={handleBackdropClick}
    ></button>
  {/if}

  <aside
    class="sidebar"
    class:open={ui.sidebarOpen}
    style:width={isDesktop ? `${sidebarWidth}px` : undefined}
  >
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
        class:active={router.route === "usage"}
        onclick={() => mobileNav("usage")}
      >
        <svg width="12" height="12" viewBox="0 0 16 16" fill="currentColor" aria-hidden="true">
          <path d="M1 2.5A1.5 1.5 0 012.5 1h3A1.5 1.5 0 017 2.5v3A1.5 1.5 0 015.5 7h-3A1.5 1.5 0 011 5.5v-3zM2.5 2a.5.5 0 00-.5.5v3a.5.5 0 00.5.5h3a.5.5 0 00.5-.5v-3a.5.5 0 00-.5-.5h-3zm6.5.5A1.5 1.5 0 0110.5 1h3A1.5 1.5 0 0115 2.5v3A1.5 1.5 0 0113.5 7h-3A1.5 1.5 0 019 5.5v-3zm1.5-.5a.5.5 0 00-.5.5v3a.5.5 0 00.5.5h3a.5.5 0 00.5-.5v-3a.5.5 0 00-.5-.5h-3zM1 10.5A1.5 1.5 0 012.5 9h3A1.5 1.5 0 017 10.5v3A1.5 1.5 0 015.5 15h-3A1.5 1.5 0 011 13.5v-3zm1.5-.5a.5.5 0 00-.5.5v3a.5.5 0 00.5.5h3a.5.5 0 00.5-.5v-3a.5.5 0 00-.5-.5h-3zm6.5.5A1.5 1.5 0 0110.5 9h3a1.5 1.5 0 011.5 1.5v3a1.5 1.5 0 01-1.5 1.5h-3A1.5 1.5 0 019 13.5v-3zm1.5-.5a.5.5 0 00-.5.5v3a.5.5 0 00.5.5h3a.5.5 0 00.5-.5v-3a.5.5 0 00-.5-.5h-3z"/>
        </svg>
        Usage
      </button>
      <button
        class="mobile-nav-btn"
        class:active={router.route === "trends"}
        onclick={() => mobileNav("trends")}
      >
        <svg width="12" height="12" viewBox="0 0 16 16" fill="currentColor" aria-hidden="true">
          <path d="M2 13.5a.5.5 0 01-.5-.5V2a.5.5 0 011 0v10.5H14a.5.5 0 010 1H2z"/>
          <path d="M3.35 10.35a.5.5 0 010-.7l2.5-2.5a.5.5 0 01.7 0L8.5 9.09l3.15-4.44a.5.5 0 01.82.58l-3.5 4.94a.5.5 0 01-.76.06L6.2 8.21l-2.15 2.14a.5.5 0 01-.7 0z"/>
        </svg>
        Trends
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

  {#if isDesktop && ui.sidebarOpen}
    <div
      class="resize-handle"
      bind:this={resizeHandleElement}
      data-testid="sidebar-resize-handle"
      role="separator"
      aria-label="Resize sidebar"
      aria-orientation="vertical"
      aria-valuemin={SIDEBAR_WIDTH_MIN}
      aria-valuemax={SIDEBAR_WIDTH_STORAGE_MAX}
      aria-valuenow={sidebarWidth}
      onpointerdown={handlePointerDown}
      style:width={`${RESIZE_HANDLE_WIDTH}px`}
    ></div>
  {/if}

  <main class="content">
    {@render content()}
  </main>

  {#if vitals && isDesktop && ui.vitalsOpen && sessions.activeSessionId}
    <aside class="vitals">
      {@render vitals()}
    </aside>
  {/if}
</div>

<style>
  .layout {
    display: flex;
    height: calc(
      100vh - var(--header-height, 40px) - var(--status-bar-height, 24px)
    );
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

  .resize-handle {
    position: relative;
    flex-shrink: 0;
    cursor: col-resize;
    touch-action: none;
    transition: background-color 120ms ease;
  }

  .resize-handle::before {
    content: "";
    position: absolute;
    top: 0;
    bottom: 0;
    left: 50%;
    width: 1px;
    background: var(--border-default);
    transform: translateX(-50%);
  }

  .resize-handle::after {
    content: "";
    position: absolute;
    top: 50%;
    left: 50%;
    width: 3px;
    height: 52px;
    border-radius: 999px;
    background: var(--text-muted);
    opacity: 0.6;
    transform: translate(-50%, -50%);
    transition: opacity 120ms ease;
  }

  .resize-handle:hover,
  .layout.is-resizing .resize-handle {
    background: color-mix(
      in srgb,
      var(--accent-blue) 10%,
      transparent
    );
  }

  .resize-handle:hover::after,
  .layout.is-resizing .resize-handle::after {
    opacity: 1;
  }

  .content {
    flex: 1;
    min-width: 0;
    overflow: hidden;
    display: flex;
    flex-direction: column;
  }

  .vitals {
    width: 320px;
    flex-shrink: 0;
    border-left: 1px solid var(--border-default);
    overflow: hidden;
    display: flex;
    flex-direction: column;
    background: var(--bg-surface);
  }

  .sidebar-backdrop {
    display: none;
    border: none;
    padding: 0;
  }

  .mobile-nav {
    display: none;
  }

  :global(body.sidebar-resizing) {
    cursor: col-resize;
    user-select: none;
    -webkit-user-select: none;
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
      display: grid;
      grid-template-columns: repeat(3, minmax(0, 1fr));
      gap: 6px;
      padding: 8px;
      border-bottom: 1px solid var(--border-muted);
      flex-shrink: 0;
    }

    .mobile-nav-btn {
      display: flex;
      align-items: center;
      justify-content: center;
      gap: 4px;
      min-width: 0;
      padding: 6px 4px;
      font-size: 11px;
      font-weight: 500;
      color: var(--text-muted);
      border-radius: var(--radius-sm);
      white-space: nowrap;
      transition: background 0.12s, color 0.12s;
    }

    .mobile-nav-btn:hover {
      background: var(--bg-surface-hover);
      color: var(--text-primary);
    }

    .mobile-nav-btn.active {
      color: var(--accent-blue);
      background: color-mix(
        in srgb,
        var(--accent-blue) 8%,
        transparent
      );
    }
  }
</style>
