// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import {
  createRawSnippet,
  mount,
  tick,
  unmount,
} from "svelte";
// @ts-ignore
import ThreeColumnLayout from "./ThreeColumnLayout.svelte";
import { m } from "../../i18n/index.js";
import { router } from "../../stores/router.svelte.js";
import {
  SIDEBAR_DESKTOP_BREAKPOINT,
  SIDEBAR_WIDTH_DEFAULT,
  SIDEBAR_WIDTH_MIN,
  SIDEBAR_WIDTH_STORAGE_MAX,
  VITALS_WIDTH_DEFAULT,
  clampSidebarWidthForLayout,
  clampVitalsWidthForLayout,
} from "./sidebar-width.js";
import { ui } from "../../stores/ui.svelte.js";
import { sync } from "../../stores/sync.svelte.js";
import { sessions } from "../../stores/sessions.svelte.js";
import { settings } from "../../stores/settings.svelte.js";

const sidebarSnippet = createRawSnippet(() => ({
  render: () => '<div data-testid="sidebar-slot">Sidebar</div>',
}));

const contentSnippet = createRawSnippet(() => ({
  render: () => '<div data-testid="content-slot">Content</div>',
}));

const vitalsSnippet = createRawSnippet(() => ({
  render: () => '<div data-testid="vitals-slot">Vitals</div>',
}));

// Rendered width of kit-ui's .kit-split-resize-handle.
const RESIZE_HANDLE_WIDTH = 4;
const SIDEBAR_BORDER_WIDTH = 1;
const KEYBOARD_RESIZE_STEP = 24;

let component: ReturnType<typeof mount> | undefined;
let restoreMeasuredLayoutWidth:
  | (() => void)
  | undefined;

function setViewportWidth(width: number) {
  Object.defineProperty(window, "innerWidth", {
    configurable: true,
    writable: true,
    value: width,
  });
  window.dispatchEvent(new Event("resize"));
}

function renderLayout(withVitals = false) {
  component = mount(ThreeColumnLayout, {
    target: document.body,
    props: {
      sidebar: sidebarSnippet,
      content: contentSnippet,
      ...(withVitals ? { vitals: vitalsSnippet } : {}),
    },
  });
  return component;
}

function getLayout() {
  const layout = document.querySelector<HTMLElement>(".layout");
  expect(layout).not.toBeNull();
  return layout!;
}

function getSidebar() {
  const sidebar = document.querySelector<HTMLElement>(".sidebar");
  expect(sidebar).not.toBeNull();
  return sidebar!;
}

function getHandle() {
  return document.querySelector<HTMLElement>(
    `[aria-label="${m.nav_resize_sidebar()}"]`,
  );
}

function getVitalsHandle() {
  return document.querySelector<HTMLElement>(
    `[aria-label="${m.session_vitals_resize()}"]`,
  );
}

function getVitalsPanel() {
  const panel = document.querySelector<HTMLElement>(".vitals");
  expect(panel).not.toBeNull();
  return panel!;
}

function getClampedSidebarWidthForLayout(
  desiredWidth: number,
  layoutWidth: number,
) {
  return clampSidebarWidthForLayout(
    desiredWidth,
    layoutWidth -
      RESIZE_HANDLE_WIDTH -
      SIDEBAR_BORDER_WIDTH,
  );
}

function mockLayoutWidth(width: number) {
  const layout = getLayout();

  Object.defineProperty(layout, "getBoundingClientRect", {
    configurable: true,
    value: () => ({
      width,
      height: 0,
      top: 0,
      right: width,
      bottom: 0,
      left: 0,
      x: 0,
      y: 0,
      toJSON: () => ({}),
    }),
  });
}

function mockLayoutWidthOnRender(width: number) {
  const original =
    HTMLElement.prototype.getBoundingClientRect;

  Object.defineProperty(
    HTMLElement.prototype,
    "getBoundingClientRect",
    {
      configurable: true,
      value: function () {
        if (
          this instanceof HTMLElement &&
          this.classList.contains("layout")
        ) {
          return {
            width,
            height: 0,
            top: 0,
            right: width,
            bottom: 0,
            left: 0,
            x: 0,
            y: 0,
            toJSON: () => ({}),
          };
        }

        return original.call(this);
      },
    },
  );

  restoreMeasuredLayoutWidth = () => {
    Object.defineProperty(
      HTMLElement.prototype,
      "getBoundingClientRect",
      {
        configurable: true,
        value: original,
      },
    );
    restoreMeasuredLayoutWidth = undefined;
  };
}

function pointerEvent(
  type: string,
  clientX: number,
) {
  return new PointerEvent(type, {
    bubbles: true,
    clientX,
    pointerId: 1,
  });
}

async function dragHandle(startX: number, endX: number) {
  const handle = getHandle();
  expect(handle).not.toBeNull();

  handle!.dispatchEvent(pointerEvent("pointerdown", startX));
  handle!.dispatchEvent(pointerEvent("pointermove", endX));
  await tick();
  handle!.dispatchEvent(pointerEvent("pointerup", endX));
  await tick();
}

afterEach(() => {
  if (component) {
    unmount(component);
    component = undefined;
  }

  document.body.className = "";
  document.body.innerHTML = "";
  restoreMeasuredLayoutWidth?.();
  ui.sidebarOpen = true;
  ui.isMobileViewport = false;
  ui.setSidebarWidth(SIDEBAR_WIDTH_DEFAULT);
  ui.vitalsOpen = false;
  ui.setVitalsWidth(VITALS_WIDTH_DEFAULT);
  sessions.activeSessionId = null;
  sync.serverVersion = null;
  settings.loaded = false;
  settings.readOnly = false;
  settings.error = null;
  setViewportWidth(SIDEBAR_DESKTOP_BREAKPOINT);
});

describe("ThreeColumnLayout", () => {
  it("exposes Recall in mobile navigation when the backend supports it", async () => {
    setViewportWidth(SIDEBAR_DESKTOP_BREAKPOINT - 1);
    ui.isMobileViewport = true;
    sync.serverVersion = {
      version: "dev",
      commit: "unknown",
      build_date: "",
      read_only: false,
    };
    renderLayout();
    await tick();

    const recall = Array.from(
      document.querySelectorAll<HTMLButtonElement>(
        ".mobile-nav .mobile-nav-btn",
      ),
    ).find((button) => button.textContent?.trim() === m.nav_recall());
    expect(recall).not.toBeUndefined();

    const navigate = vi
      .spyOn(router, "navigate")
      .mockImplementation(() => true);
    recall!.click();
    expect(navigate).toHaveBeenCalledWith("recall");
    expect(ui.sidebarOpen).toBe(false);
    navigate.mockRestore();
  });

  it("keeps Recall in mobile navigation for read-only backends", async () => {
    setViewportWidth(SIDEBAR_DESKTOP_BREAKPOINT - 1);
    ui.isMobileViewport = true;
    sync.serverVersion = {
      version: "dev",
      commit: "unknown",
      build_date: "",
      read_only: true,
    };
    renderLayout();
    await tick();

    const labels = Array.from(
      document.querySelectorAll<HTMLButtonElement>(
        ".mobile-nav .mobile-nav-btn",
      ),
    ).map((button) => button.textContent?.trim());
    expect(labels).toContain(m.nav_recall());
    expect(labels).toContain("Quality");
  });

  it("exposes Recent Edits in the mobile nav, reachable below the header breakpoint", async () => {
    renderLayout();
    await tick();

    const navButtons = Array.from(
      document.querySelectorAll<HTMLButtonElement>(
        ".mobile-nav .mobile-nav-btn",
      ),
    );
    const recentEdits = navButtons.find(
      (btn) => btn.textContent?.trim() === m.nav_recent_edits(),
    );
    expect(recentEdits).not.toBeUndefined();

    // The header More menu that also hosts Recent Edits is display:none under    // the medium breakpoint, so the mobile nav button is the only way mobile users reach it.
    const navigate = vi
      .spyOn(router, "navigate")
      .mockImplementation(() => true);
    recentEdits!.click();
    expect(navigate).toHaveBeenCalledWith("recent-edits");
    navigate.mockRestore();
  });

  it("exposes Data in the mobile nav, reachable below the header breakpoint", async () => {
    renderLayout();
    await tick();

    const navButtons = Array.from(
      document.querySelectorAll<HTMLButtonElement>(
        ".mobile-nav .mobile-nav-btn",
      ),
    );
    const data = navButtons.find(
      (btn) => btn.textContent?.trim() === m.nav_data(),
    );
    expect(data).not.toBeUndefined();

    const navigate = vi
      .spyOn(router, "navigate")
      .mockImplementation(() => true);
    data!.click();
    expect(navigate).toHaveBeenCalledWith("data");
    navigate.mockRestore();
  });

  it("renders the resize handle at the desktop layout breakpoint", async () => {
    const expectedWidth = getClampedSidebarWidthForLayout(
      320,
      SIDEBAR_DESKTOP_BREAKPOINT,
    );

    setViewportWidth(SIDEBAR_DESKTOP_BREAKPOINT);
    ui.sidebarOpen = true;
    ui.setSidebarWidth(320);

    renderLayout();
    await tick();

    expect(getHandle()).not.toBeNull();
    expect(getSidebar().style.width).toBe(
      `${expectedWidth}px`,
    );
  });

  it("renders handle on desktop layouts", async () => {
    setViewportWidth(1280);
    ui.setSidebarWidth(320);

    renderLayout();
    await tick();

    expect(getHandle()).not.toBeNull();
    expect(getSidebar().style.width).toBe("320px");
  });

  it("hides handle below breakpoint", async () => {
    setViewportWidth(SIDEBAR_DESKTOP_BREAKPOINT - 1);
    ui.setSidebarWidth(360);

    renderLayout();
    await tick();

    expect(getHandle()).toBeNull();
    expect(getSidebar().style.width).toBe("");
  });

  it("renders a clamped width on mount while preserving the stored preference", async () => {
    const layoutWidth = 760;
    const expectedWidth =
      getClampedSidebarWidthForLayout(
        SIDEBAR_WIDTH_STORAGE_MAX,
        layoutWidth,
      );

    setViewportWidth(1280);
    ui.setSidebarWidth(SIDEBAR_WIDTH_STORAGE_MAX);
    mockLayoutWidthOnRender(layoutWidth);

    renderLayout();
    await tick();

    expect(getSidebar().style.width).toBe(
      `${expectedWidth}px`,
    );
    expect(ui.sidebarWidth).toBe(
      SIDEBAR_WIDTH_STORAGE_MAX,
    );
  });

  it("dragging updates ui.sidebarWidth", async () => {
    setViewportWidth(1280);
    ui.setSidebarWidth(SIDEBAR_WIDTH_DEFAULT);

    renderLayout();
    await tick();

    mockLayoutWidth(1280);

    const handle = getHandle();
    expect(handle).not.toBeNull();

    handle!.dispatchEvent(
      pointerEvent("pointerdown", SIDEBAR_WIDTH_DEFAULT),
    );
    handle!.dispatchEvent(
      pointerEvent("pointermove", SIDEBAR_WIDTH_DEFAULT + 80),
    );
    await tick();

    expect(ui.sidebarWidth).toBe(SIDEBAR_WIDTH_DEFAULT + 80);
    expect(getSidebar().style.width).toBe(
      `${SIDEBAR_WIDTH_DEFAULT + 80}px`,
    );

    handle!.dispatchEvent(
      pointerEvent("pointerup", SIDEBAR_WIDTH_DEFAULT + 80),
    );
    await tick();

    expect(ui.sidebarWidth).toBe(SIDEBAR_WIDTH_DEFAULT + 80);
    expect(getSidebar().style.width).toBe(
      `${SIDEBAR_WIDTH_DEFAULT + 80}px`,
    );
  });

  it("dragging clamps to the computed minimum/max using mocked layout width", async () => {
    const layoutWidth = 760;

    setViewportWidth(1280);
    ui.setSidebarWidth(SIDEBAR_WIDTH_DEFAULT);

    renderLayout();
    await tick();

    mockLayoutWidth(layoutWidth);

    await dragHandle(
      SIDEBAR_WIDTH_DEFAULT,
      SIDEBAR_WIDTH_DEFAULT + 240,
    );

    expect(ui.sidebarWidth).toBe(
      getClampedSidebarWidthForLayout(
        SIDEBAR_WIDTH_DEFAULT + 240,
        layoutWidth,
      ),
    );

    await dragHandle(280, -200);

    expect(ui.sidebarWidth).toBe(
      getClampedSidebarWidthForLayout(
        SIDEBAR_WIDTH_MIN - 420,
        layoutWidth,
      ),
    );
  });

  it("does not persist a clamped width for a click without drag movement", async () => {
    const layoutWidth = 700;
    const expectedWidth =
      getClampedSidebarWidthForLayout(
        SIDEBAR_WIDTH_STORAGE_MAX,
        layoutWidth,
      );

    setViewportWidth(1280);
    ui.setSidebarWidth(SIDEBAR_WIDTH_STORAGE_MAX);
    mockLayoutWidthOnRender(layoutWidth);

    renderLayout();
    await tick();

    const handle = getHandle();
    expect(handle).not.toBeNull();

    handle!.dispatchEvent(
      pointerEvent("pointerdown", expectedWidth),
    );
    handle!.dispatchEvent(
      pointerEvent("pointerup", expectedWidth),
    );
    await tick();

    expect(getSidebar().style.width).toBe(
      `${expectedWidth}px`,
    );
    expect(ui.sidebarWidth).toBe(
      SIDEBAR_WIDTH_STORAGE_MAX,
    );
  });

  it("preserves the stored preferred width when a drag stays inside the same clamp", async () => {
    const layoutWidth = 760;
    const expectedWidth =
      getClampedSidebarWidthForLayout(
        SIDEBAR_WIDTH_STORAGE_MAX,
        layoutWidth,
      );

    setViewportWidth(1280);
    ui.setSidebarWidth(SIDEBAR_WIDTH_STORAGE_MAX);
    mockLayoutWidthOnRender(layoutWidth);

    renderLayout();
    await tick();

    await dragHandle(expectedWidth, expectedWidth + 120);

    expect(getSidebar().style.width).toBe(
      `${expectedWidth}px`,
    );
    expect(ui.sidebarWidth).toBe(
      SIDEBAR_WIDTH_STORAGE_MAX,
    );
  });

  it("accounts for the resize handle gutter when clamping near the content minimum", async () => {
    const layoutWidth = 1000;
    const expectedWidth =
      getClampedSidebarWidthForLayout(
        SIDEBAR_WIDTH_STORAGE_MAX,
        layoutWidth,
      );

    setViewportWidth(1280);
    ui.setSidebarWidth(SIDEBAR_WIDTH_STORAGE_MAX);
    mockLayoutWidthOnRender(layoutWidth);

    renderLayout();
    await tick();

    expect(getSidebar().style.width).toBe(
      `${expectedWidth}px`,
    );
    expect(
      layoutWidth -
        expectedWidth -
        RESIZE_HANDLE_WIDTH -
        SIDEBAR_BORDER_WIDTH,
    ).toBe(480);
  });

  it("removes the handle and keeps the dragged width when the sidebar closes mid-drag", async () => {
    setViewportWidth(1280);
    ui.sidebarOpen = true;
    ui.setSidebarWidth(SIDEBAR_WIDTH_DEFAULT);

    renderLayout();
    await tick();

    mockLayoutWidth(1280);

    const handle = getHandle();
    expect(handle).not.toBeNull();

    handle!.dispatchEvent(
      pointerEvent("pointerdown", SIDEBAR_WIDTH_DEFAULT),
    );
    handle!.dispatchEvent(
      pointerEvent("pointermove", SIDEBAR_WIDTH_DEFAULT + 60),
    );
    await tick();

    expect(ui.sidebarWidth).toBe(SIDEBAR_WIDTH_DEFAULT + 60);

    ui.sidebarOpen = false;
    await tick();

    expect(getHandle()).toBeNull();

    handle!.dispatchEvent(
      pointerEvent("pointermove", SIDEBAR_WIDTH_DEFAULT + 140),
    );
    await tick();

    expect(ui.sidebarWidth).toBe(SIDEBAR_WIDTH_DEFAULT + 60);
  });

  it("resizes and persists the width from arrow keys on the handle", async () => {
    setViewportWidth(1280);
    ui.setSidebarWidth(SIDEBAR_WIDTH_DEFAULT);

    renderLayout();
    await tick();

    mockLayoutWidth(1280);

    const handle = getHandle();
    expect(handle).not.toBeNull();

    handle!.dispatchEvent(
      new KeyboardEvent("keydown", {
        bubbles: true,
        key: "ArrowRight",
      }),
    );
    await tick();

    expect(ui.sidebarWidth).toBe(
      SIDEBAR_WIDTH_DEFAULT + KEYBOARD_RESIZE_STEP,
    );
    expect(getSidebar().style.width).toBe(
      `${SIDEBAR_WIDTH_DEFAULT + KEYBOARD_RESIZE_STEP}px`,
    );

    handle!.dispatchEvent(
      new KeyboardEvent("keydown", {
        bubbles: true,
        key: "ArrowLeft",
      }),
    );
    await tick();

    expect(ui.sidebarWidth).toBe(SIDEBAR_WIDTH_DEFAULT);
  });

  it("renders a resizable vitals panel when open with an active session", async () => {
    setViewportWidth(1280);
    ui.vitalsOpen = true;
    sessions.activeSessionId = "session-1";

    renderLayout(true);
    await tick();

    expect(getVitalsHandle()).not.toBeNull();
    expect(getVitalsPanel().style.width).toBe(
      `${VITALS_WIDTH_DEFAULT}px`,
    );
  });

  it("dragging the vitals handle left widens the panel and persists the width", async () => {
    setViewportWidth(1280);
    ui.vitalsOpen = true;
    sessions.activeSessionId = "session-1";
    ui.setVitalsWidth(VITALS_WIDTH_DEFAULT);

    renderLayout(true);
    await tick();

    mockLayoutWidth(1280);

    const handle = getVitalsHandle();
    expect(handle).not.toBeNull();

    handle!.dispatchEvent(pointerEvent("pointerdown", 900));
    handle!.dispatchEvent(pointerEvent("pointermove", 820));
    await tick();

    expect(ui.vitalsWidth).toBe(VITALS_WIDTH_DEFAULT + 80);
    expect(getVitalsPanel().style.width).toBe(
      `${VITALS_WIDTH_DEFAULT + 80}px`,
    );

    handle!.dispatchEvent(pointerEvent("pointerup", 820));
    await tick();

    expect(ui.vitalsWidth).toBe(VITALS_WIDTH_DEFAULT + 80);
  });

  it("resizes the vitals panel from arrow keys, widening toward the content", async () => {
    setViewportWidth(1280);
    ui.vitalsOpen = true;
    sessions.activeSessionId = "session-1";
    ui.setVitalsWidth(VITALS_WIDTH_DEFAULT);

    renderLayout(true);
    await tick();

    mockLayoutWidth(1280);

    const handle = getVitalsHandle();
    expect(handle).not.toBeNull();

    handle!.dispatchEvent(
      new KeyboardEvent("keydown", {
        bubbles: true,
        key: "ArrowLeft",
      }),
    );
    await tick();

    expect(ui.vitalsWidth).toBe(
      VITALS_WIDTH_DEFAULT + KEYBOARD_RESIZE_STEP,
    );

    handle!.dispatchEvent(
      new KeyboardEvent("keydown", {
        bubbles: true,
        key: "ArrowRight",
      }),
    );
    await tick();

    expect(ui.vitalsWidth).toBe(VITALS_WIDTH_DEFAULT);
  });

  it("clamps the vitals panel to the layout and keeps a wider stored preference", async () => {
    const layoutWidth = 1100;
    // Space left of the vitals panel: the sidebar, its handle, and both
    // 1px pane borders plus the vitals handle.
    const vitalsAvailable =
      layoutWidth -
      (SIDEBAR_WIDTH_DEFAULT +
        RESIZE_HANDLE_WIDTH +
        SIDEBAR_BORDER_WIDTH) -
      RESIZE_HANDLE_WIDTH -
      SIDEBAR_BORDER_WIDTH;
    const expectedWidth = clampVitalsWidthForLayout(
      560,
      vitalsAvailable,
    );

    setViewportWidth(1280);
    ui.vitalsOpen = true;
    sessions.activeSessionId = "session-1";
    ui.setVitalsWidth(560);
    mockLayoutWidthOnRender(layoutWidth);

    renderLayout(true);
    await tick();

    expect(getVitalsPanel().style.width).toBe(
      `${expectedWidth}px`,
    );

    const handle = getVitalsHandle();
    expect(handle).not.toBeNull();

    handle!.dispatchEvent(pointerEvent("pointerdown", 900));
    handle!.dispatchEvent(pointerEvent("pointermove", 780));
    await tick();
    handle!.dispatchEvent(pointerEvent("pointerup", 780));
    await tick();

    expect(getVitalsPanel().style.width).toBe(
      `${expectedWidth}px`,
    );
    expect(ui.vitalsWidth).toBe(560);
  });

  it("reserves the vitals minimum when widening the sidebar with both panes open", async () => {
    setViewportWidth(1280);
    ui.vitalsOpen = true;
    sessions.activeSessionId = "session-1";
    ui.setSidebarWidth(SIDEBAR_WIDTH_DEFAULT);
    ui.setVitalsWidth(320);

    renderLayout(true);
    await tick();

    mockLayoutWidth(1280);

    await dragHandle(
      SIDEBAR_WIDTH_DEFAULT,
      SIDEBAR_WIDTH_DEFAULT + 400,
    );

    // 1280 minus both 4px handles and both 1px pane borders leaves 990 for
    // the sidebar clamp once the vitals minimum footprint (280 + 5) is
    // reserved; the 480 content minimum caps the sidebar at 510 and the
    // vitals panel yields to 280, keeping the content column at exactly 480.
    expect(ui.sidebarWidth).toBe(510);
    expect(getSidebar().style.width).toBe("510px");
    expect(getVitalsPanel().style.width).toBe("280px");
  });

  it("keeps the content minimum when both stored pane widths are maxed", async () => {
    setViewportWidth(1280);
    ui.vitalsOpen = true;
    sessions.activeSessionId = "session-1";
    ui.setSidebarWidth(SIDEBAR_WIDTH_STORAGE_MAX);
    ui.setVitalsWidth(560);

    renderLayout(true);
    await tick();

    expect(getSidebar().style.width).toBe("510px");
    expect(getVitalsPanel().style.width).toBe("280px");

    // The separators advertise the layout-effective maxima, not the
    // storage caps the current window cannot grant.
    expect(getHandle()!.getAttribute("aria-valuemax")).toBe("510");
    expect(
      getVitalsHandle()!.getAttribute("aria-valuemax"),
    ).toBe("280");
  });
});
