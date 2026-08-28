<script lang="ts">
  import type { Snippet } from "svelte";
  import {
    SplitResizeHandle,
    type SplitResizeEvent,
  } from "@kenn-io/kit-ui";
  import { m } from "../../i18n/index.js";
  import {
    SIDEBAR_DESKTOP_BREAKPOINT,
    SIDEBAR_WIDTH_DEFAULT,
    SIDEBAR_WIDTH_MIN,
    SIDEBAR_WIDTH_STORAGE_MAX,
    VITALS_WIDTH_DEFAULT,
    VITALS_WIDTH_MIN,
    VITALS_WIDTH_STORAGE_MAX,
    clampSidebarWidthForLayout,
    clampVitalsWidthForLayout,
    isDesktopSidebarLayout,
  } from "./sidebar-width.js";
  import { ui } from "../../stores/ui.svelte.js";
  import { router } from "../../stores/router.svelte.js";
  import type { Route } from "../../stores/router.svelte.js";
  import { sessions } from "../../stores/sessions.svelte.js";
  import {
    ActivityIcon,
    ChartColumnIcon,
    DatabaseIcon,
    Grid2x2Icon,
    LayoutGridIcon,
    LightbulbIcon,
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

  // Rendered width of kit-ui's .kit-split-resize-handle.
  const RESIZE_HANDLE_WIDTH = 4;
  const SIDEBAR_BORDER_WIDTH = 1;

  let { sidebar, content, vitals }: Props = $props();
  let layoutElement = $state<HTMLElement | null>(null);
  let layoutWidth = $state<number | null>(null);
  let viewportWidth = $state(
    typeof window === "undefined"
      ? SIDEBAR_DESKTOP_BREAKPOINT
      : window.innerWidth,
  );
  let resizeStartWidth = 0;
  let vitalsResizeStartWidth = 0;

  const isDesktop = $derived(
    isDesktopSidebarLayout(viewportWidth),
  );
  const currentLayoutWidth = $derived(
    layoutWidth ?? viewportWidth,
  );
  const vitalsVisible = $derived(
    Boolean(vitals) &&
      isDesktop &&
      ui.vitalsOpen &&
      sessions.activeSessionId !== null,
  );
  // The two panes share one width budget with the content column. When space
  // runs out, pane minimums win, then the content minimum, then preferred
  // widths: the sidebar clamp reserves the vitals panel's minimum footprint,
  // and the vitals clamp yields whatever the actual sidebar leaves over.
  function sidebarLayoutWidth(layoutWidthNow: number): number {
    const vitalsReservedWidth = vitalsVisible
      ? VITALS_WIDTH_MIN + RESIZE_HANDLE_WIDTH + SIDEBAR_BORDER_WIDTH
      : 0;

    return Math.max(
      0,
      layoutWidthNow -
        RESIZE_HANDLE_WIDTH -
        SIDEBAR_BORDER_WIDTH -
        vitalsReservedWidth,
    );
  }
  const sidebarWidth = $derived(
    isDesktop
      ? clampSidebarWidthForLayout(
          ui.sidebarWidth,
          sidebarLayoutWidth(currentLayoutWidth),
        )
      : SIDEBAR_WIDTH_DEFAULT,
  );
  const sidebarMaxWidth = $derived(
    clampSidebarWidthForLayout(
      SIDEBAR_WIDTH_STORAGE_MAX,
      sidebarLayoutWidth(currentLayoutWidth),
    ),
  );
  const vitalsAvailableWidth = $derived(
    vitalsLayoutWidth(currentLayoutWidth),
  );
  const vitalsWidth = $derived(
    isDesktop
      ? clampVitalsWidthForLayout(
          ui.vitalsWidth,
          vitalsAvailableWidth,
        )
      : VITALS_WIDTH_DEFAULT,
  );
  const vitalsMaxWidth = $derived(
    clampVitalsWidthForLayout(
      VITALS_WIDTH_STORAGE_MAX,
      vitalsAvailableWidth,
    ),
  );

  // Width left for the content column and vitals panel once the sidebar,
  // its handle, and the pane borders are spoken for.
  function vitalsLayoutWidth(layoutWidthNow: number): number {
    const sidebarTotalWidth = ui.sidebarOpen
      ? sidebarWidth + RESIZE_HANDLE_WIDTH + SIDEBAR_BORDER_WIDTH
      : 0;

    return Math.max(
      0,
      layoutWidthNow -
        sidebarTotalWidth -
        RESIZE_HANDLE_WIDTH -
        SIDEBAR_BORDER_WIDTH,
    );
  }
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

  function handleResizeStart() {
    resizeStartWidth = sidebarWidth;
  }

  function handleResize(event: SplitResizeEvent) {
    const clampedWidth = clampSidebarWidthForLayout(
      resizeStartWidth + event.delta,
      sidebarLayoutWidth(measureLayoutWidth()),
    );

    // Skip persisting when the clamp lands on the rendered width so a wider
    // stored preference survives drags inside the same clamp.
    if (clampedWidth === sidebarWidth) return;
    ui.setSidebarWidth(clampedWidth);
  }

  function handleVitalsResizeStart() {
    vitalsResizeStartWidth = vitalsWidth;
  }

  function handleVitalsResize(event: SplitResizeEvent) {
    // The handle sits on the panel's left edge, so moving it left (negative
    // delta) widens the panel.
    const clampedWidth = clampVitalsWidthForLayout(
      vitalsResizeStartWidth - event.delta,
      vitalsLayoutWidth(measureLayoutWidth()),
    );

    if (clampedWidth === vitalsWidth) return;
    ui.setVitalsWidth(clampedWidth);
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

</script>

<svelte:window bind:innerWidth={viewportWidth} />

<div class="layout" bind:this={layoutElement}>
  {#if ui.isMobileViewport && ui.sidebarOpen}
    <button
      class="sidebar-backdrop"
      aria-label={m.nav_close_sidebar()}
      onclick={handleBackdropClick}
    ></button>
  {/if}

  <aside
    id="session-sidebar"
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
        class:active={router.route === "recall"}
        onclick={() => mobileNav("recall")}
      >
        <LightbulbIcon size="12" strokeWidth="2" aria-hidden="true" />
        {m.nav_recall()}
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
        class:active={router.route === "quality"}
        onclick={() => mobileNav("quality")}
      >
        <LogsIcon size="12" strokeWidth="2" aria-hidden="true" />
        {m.nav_quality()}
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
      <button
        class="mobile-nav-btn"
        class:active={router.route === "data"}
        onclick={() => mobileNav("data")}
      >
        <DatabaseIcon size="12" strokeWidth="2" aria-hidden="true" />
        {m.nav_data()}
      </button>
    </nav>
    {@render sidebar()}
  </aside>

  {#if isDesktop && ui.sidebarOpen}
    <SplitResizeHandle
      ariaLabel={m.nav_resize_sidebar()}
      ariaValueMin={SIDEBAR_WIDTH_MIN}
      ariaValueMax={sidebarMaxWidth}
      ariaValueNow={sidebarWidth}
      onResizeStart={handleResizeStart}
      onResize={handleResize}
      onResizeEnd={handleResize}
    />
  {/if}

  <main class="content">
    {@render content()}
  </main>

  {#if vitalsVisible}
    <SplitResizeHandle
      ariaLabel={m.session_vitals_resize()}
      ariaValueMin={VITALS_WIDTH_MIN}
      ariaValueMax={vitalsMaxWidth}
      ariaValueNow={vitalsWidth}
      onResizeStart={handleVitalsResizeStart}
      onResize={handleVitalsResize}
      onResizeEnd={handleVitalsResize}
    />
    <aside class="vitals" style:width={`${vitalsWidth}px`}>
      {@render vitals?.()}
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

  .content {
    flex: 1;
    min-width: 0;
    overflow: hidden;
    display: flex;
    flex-direction: column;
  }

  .vitals {
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

  @media (max-width: 760px) {
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
