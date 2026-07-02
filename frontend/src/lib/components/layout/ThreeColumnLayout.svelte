<script lang="ts">
  import type { Snippet } from "svelte";
  import { m } from "../../i18n/index.js";
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
  import {
    ActivityIcon,
    ChartColumnIcon,
    Grid2x2Icon,
    LayoutGridIcon,
    LogsIcon,
    PencilIcon,
    PinIcon,
    TrashIcon,
  } from "../../icons.js";

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
    // eslint-disable-next-line @typescript-eslint/no-unused-expressions -- reactive dependency: re-measure layout when viewport width changes
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
      aria-label={m.nav_close_sidebar()}
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
        <LayoutGridIcon size="12" strokeWidth="2" aria-hidden="true" />
        {m.nav_sessions()}
      </button>
      <button
        class="mobile-nav-btn"
        class:active={router.route === "usage"}
        onclick={() => mobileNav("usage")}
      >
        <Grid2x2Icon size="12" strokeWidth="2" aria-hidden="true" />
        {m.nav_usage()}
      </button>
      <button
        class="mobile-nav-btn"
        class:active={router.route === "activity"}
        onclick={() => mobileNav("activity")}
      >
        <ActivityIcon size="12" strokeWidth="2" aria-hidden="true" />
        {m.nav_activity()}
      </button>
      <button
        class="mobile-nav-btn"
        class:active={router.route === "trends"}
        onclick={() => mobileNav("trends")}
      >
        <ChartColumnIcon size="12" strokeWidth="2" aria-hidden="true" />
        {m.nav_trends()}
      </button>
      <button
        class="mobile-nav-btn"
        class:active={router.route === "pinned"}
        onclick={() => mobileNav("pinned")}
      >
        <PinIcon size="12" strokeWidth="2" aria-hidden="true" />
        {m.nav_pinned()}
      </button>
      <button
        class="mobile-nav-btn"
        class:active={router.route === "insights"}
        onclick={() => mobileNav("insights")}
      >
        <LogsIcon size="12" strokeWidth="2" aria-hidden="true" />
        {m.nav_insights()}
      </button>
      <button
        class="mobile-nav-btn"
        class:active={router.route === "trash"}
        onclick={() => mobileNav("trash")}
      >
        <TrashIcon size="12" strokeWidth="2" aria-hidden="true" />
        {m.nav_trash()}
      </button>
      <button
        class="mobile-nav-btn"
        class:active={router.route === "recent-edits"}
        onclick={() => mobileNav("recent-edits")}
      >
        <PencilIcon size="12" strokeWidth="2" aria-hidden="true" />
        {m.nav_recent_edits()}
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
      aria-label={m.nav_resize_sidebar()}
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
    /* kit-ui-check-ignore: pointer-capture resizer with persisted width and test contract predates kit-ui SplitResizeHandle; swapping (and gaining keyboard resize) is tracked as a follow-up */
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
    /* kit-ui-check-ignore: body-level drag cursor for the sidebar resizer above */
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
      /* kit-ui-check-ignore: scrim behind the mobile sidebar drawer, not a dialog — Modal's centered panel/focus-trap chrome does not apply */
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
