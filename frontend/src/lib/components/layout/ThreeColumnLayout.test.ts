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
  clampSidebarWidthForLayout,
} from "./sidebar-width.js";
import { ui } from "../../stores/ui.svelte.js";

const sidebarSnippet = createRawSnippet(() => ({
  render: () => '<div data-testid="sidebar-slot">Sidebar</div>',
}));

const contentSnippet = createRawSnippet(() => ({
  render: () => '<div data-testid="content-slot">Content</div>',
}));

const RESIZE_HANDLE_WIDTH = 12;
const SIDEBAR_BORDER_WIDTH = 1;

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

function renderLayout() {
  component = mount(ThreeColumnLayout, {
    target: document.body,
    props: {
      sidebar: sidebarSnippet,
      content: contentSnippet,
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
    '[data-testid="sidebar-resize-handle"]',
  );
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

function createPointerMouseEvent(
  type: string,
  options: {
    clientX: number;
    buttons?: number;
    pointerId: number;
  },
) {
  const event = new MouseEvent(type, {
    bubbles: true,
    clientX: options.clientX,
    buttons: options.buttons,
  });

  Object.defineProperty(event, "pointerId", {
    configurable: true,
    value: options.pointerId,
  });

  return event;
}

async function dragHandle(startX: number, endX: number) {
  const handle = getHandle();
  expect(handle).not.toBeNull();

  handle!.dispatchEvent(
    new MouseEvent("pointerdown", {
      bubbles: true,
      clientX: startX,
    }),
  );
  window.dispatchEvent(
    new MouseEvent("pointermove", {
      bubbles: true,
      clientX: endX,
      buttons: 1,
    }),
  );
  await tick();
  window.dispatchEvent(
    new MouseEvent("pointerup", {
      bubbles: true,
      clientX: endX,
    }),
  );
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
  setViewportWidth(SIDEBAR_DESKTOP_BREAKPOINT);
});

describe("ThreeColumnLayout", () => {
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

    const layout = getLayout();
    const handle = getHandle();
    expect(handle).not.toBeNull();

    handle!.dispatchEvent(
      new MouseEvent("pointerdown", {
        bubbles: true,
        clientX: SIDEBAR_WIDTH_DEFAULT,
      }),
    );
    window.dispatchEvent(
      new MouseEvent("pointermove", {
        bubbles: true,
        clientX: SIDEBAR_WIDTH_DEFAULT + 80,
        buttons: 1,
      }),
    );
    await tick();

    expect(ui.sidebarWidth).toBe(SIDEBAR_WIDTH_DEFAULT + 80);
    expect(getSidebar().style.width).toBe(
      `${SIDEBAR_WIDTH_DEFAULT + 80}px`,
    );
    expect(layout.classList.contains("is-resizing")).toBe(true);
    expect(document.body.classList.contains("sidebar-resizing")).toBe(
      true,
    );

    window.dispatchEvent(
      new MouseEvent("pointerup", {
        bubbles: true,
        clientX: SIDEBAR_WIDTH_DEFAULT + 80,
      }),
    );
    await tick();

    expect(layout.classList.contains("is-resizing")).toBe(false);
    expect(document.body.classList.contains("sidebar-resizing")).toBe(
      false,
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
      new MouseEvent("pointerdown", {
        bubbles: true,
        clientX: expectedWidth,
      }),
    );
    window.dispatchEvent(
      new MouseEvent("pointerup", {
        bubbles: true,
        clientX: expectedWidth,
      }),
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

    const handle = getHandle();
    expect(handle).not.toBeNull();

    handle!.dispatchEvent(
      new MouseEvent("pointerdown", {
        bubbles: true,
        clientX: expectedWidth,
      }),
    );
    window.dispatchEvent(
      new MouseEvent("pointermove", {
        bubbles: true,
        clientX: expectedWidth + 120,
        buttons: 1,
      }),
    );
    await tick();
    window.dispatchEvent(
      new MouseEvent("pointerup", {
        bubbles: true,
        clientX: expectedWidth + 120,
      }),
    );
    await tick();

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

  it("ignores non-primary mouse buttons on the resize handle", async () => {
    setViewportWidth(1280);
    ui.setSidebarWidth(SIDEBAR_WIDTH_DEFAULT);

    renderLayout();
    await tick();

    mockLayoutWidth(1280);

    const handle = getHandle();
    const layout = getLayout();
    expect(handle).not.toBeNull();

    handle!.dispatchEvent(
      new MouseEvent("pointerdown", {
        bubbles: true,
        button: 2,
        buttons: 2,
        clientX: SIDEBAR_WIDTH_DEFAULT,
      }),
    );
    window.dispatchEvent(
      new MouseEvent("pointermove", {
        bubbles: true,
        button: 2,
        buttons: 2,
        clientX: SIDEBAR_WIDTH_DEFAULT + 100,
      }),
    );
    await tick();

    expect(ui.sidebarWidth).toBe(SIDEBAR_WIDTH_DEFAULT);
    expect(layout.classList.contains("is-resizing")).toBe(false);
    expect(document.body.classList.contains("sidebar-resizing")).toBe(
      false,
    );
  });

  it("stops an active resize when the sidebar closes mid-drag", async () => {
    setViewportWidth(1280);
    ui.sidebarOpen = true;
    ui.setSidebarWidth(SIDEBAR_WIDTH_DEFAULT);

    renderLayout();
    await tick();

    mockLayoutWidth(1280);

    const layout = getLayout();
    const handle = getHandle();
    expect(handle).not.toBeNull();

    handle!.dispatchEvent(
      new MouseEvent("pointerdown", {
        bubbles: true,
        clientX: SIDEBAR_WIDTH_DEFAULT,
      }),
    );
    window.dispatchEvent(
      new MouseEvent("pointermove", {
        bubbles: true,
        clientX: SIDEBAR_WIDTH_DEFAULT + 60,
        buttons: 1,
      }),
    );
    await tick();

    expect(layout.classList.contains("is-resizing")).toBe(true);
    expect(document.body.classList.contains("sidebar-resizing")).toBe(
      true,
    );

    ui.sidebarOpen = false;
    await tick();

    expect(getHandle()).toBeNull();
    expect(layout.classList.contains("is-resizing")).toBe(false);
    expect(document.body.classList.contains("sidebar-resizing")).toBe(
      false,
    );

    const widthAfterClose = ui.sidebarWidth;
    window.dispatchEvent(
      new MouseEvent("pointermove", {
        bubbles: true,
        clientX: SIDEBAR_WIDTH_DEFAULT + 140,
        buttons: 1,
      }),
    );
    await tick();

    expect(ui.sidebarWidth).toBe(widthAfterClose);
  });

  it("stops an active drag when a later pointermove reports no buttons pressed", async () => {
    setViewportWidth(1280);
    ui.setSidebarWidth(SIDEBAR_WIDTH_DEFAULT);

    renderLayout();
    await tick();

    mockLayoutWidth(1280);

    const layout = getLayout();
    const handle = getHandle();
    expect(handle).not.toBeNull();

    handle!.dispatchEvent(
      new MouseEvent("pointerdown", {
        bubbles: true,
        clientX: SIDEBAR_WIDTH_DEFAULT,
      }),
    );
    window.dispatchEvent(
      new MouseEvent("pointermove", {
        bubbles: true,
        clientX: SIDEBAR_WIDTH_DEFAULT + 60,
        buttons: 1,
      }),
    );
    await tick();

    expect(ui.sidebarWidth).toBe(
      SIDEBAR_WIDTH_DEFAULT + 60,
    );
    expect(layout.classList.contains("is-resizing")).toBe(true);
    expect(document.body.classList.contains("sidebar-resizing")).toBe(
      true,
    );

    window.dispatchEvent(
      new MouseEvent("pointermove", {
        bubbles: true,
        clientX: SIDEBAR_WIDTH_DEFAULT + 120,
        buttons: 0,
      }),
    );
    await tick();

    expect(ui.sidebarWidth).toBe(
      SIDEBAR_WIDTH_DEFAULT + 60,
    );
    expect(layout.classList.contains("is-resizing")).toBe(false);
    expect(document.body.classList.contains("sidebar-resizing")).toBe(
      false,
    );

    window.dispatchEvent(
      new MouseEvent("pointermove", {
        bubbles: true,
        clientX: SIDEBAR_WIDTH_DEFAULT + 180,
        buttons: 0,
      }),
    );
    await tick();

    expect(ui.sidebarWidth).toBe(
      SIDEBAR_WIDTH_DEFAULT + 60,
    );
    expect(layout.classList.contains("is-resizing")).toBe(false);
    expect(document.body.classList.contains("sidebar-resizing")).toBe(
      false,
    );
  });

  it("ignores non-owning pointer events while resizing", async () => {
    setViewportWidth(1280);
    ui.setSidebarWidth(SIDEBAR_WIDTH_DEFAULT);

    renderLayout();
    await tick();

    mockLayoutWidth(1280);

    const layout = getLayout();
    const handle = getHandle();
    expect(handle).not.toBeNull();

    handle!.dispatchEvent(
      createPointerMouseEvent("pointerdown", {
        clientX: SIDEBAR_WIDTH_DEFAULT,
        pointerId: 1,
      }),
    );
    window.dispatchEvent(
      createPointerMouseEvent("pointermove", {
        clientX: SIDEBAR_WIDTH_DEFAULT + 60,
        buttons: 1,
        pointerId: 1,
      }),
    );
    await tick();

    expect(ui.sidebarWidth).toBe(
      SIDEBAR_WIDTH_DEFAULT + 60,
    );
    expect(layout.classList.contains("is-resizing")).toBe(true);
    expect(document.body.classList.contains("sidebar-resizing")).toBe(
      true,
    );

    window.dispatchEvent(
      createPointerMouseEvent("pointermove", {
        clientX: SIDEBAR_WIDTH_DEFAULT + 180,
        buttons: 1,
        pointerId: 2,
      }),
    );
    window.dispatchEvent(
      createPointerMouseEvent("pointerup", {
        clientX: SIDEBAR_WIDTH_DEFAULT + 180,
        pointerId: 2,
      }),
    );
    window.dispatchEvent(
      createPointerMouseEvent("pointercancel", {
        clientX: SIDEBAR_WIDTH_DEFAULT + 180,
        pointerId: 2,
      }),
    );
    await tick();

    expect(ui.sidebarWidth).toBe(
      SIDEBAR_WIDTH_DEFAULT + 60,
    );
    expect(layout.classList.contains("is-resizing")).toBe(true);
    expect(document.body.classList.contains("sidebar-resizing")).toBe(
      true,
    );

    window.dispatchEvent(
      createPointerMouseEvent("pointerup", {
        clientX: SIDEBAR_WIDTH_DEFAULT + 60,
        pointerId: 1,
      }),
    );
    await tick();

    expect(layout.classList.contains("is-resizing")).toBe(false);
    expect(document.body.classList.contains("sidebar-resizing")).toBe(
      false,
    );
    expect(ui.sidebarWidth).toBe(
      SIDEBAR_WIDTH_DEFAULT + 60,
    );
  });

  it("cleans up active drag listeners and body state on unmount", async () => {
    setViewportWidth(1280);
    ui.setSidebarWidth(SIDEBAR_WIDTH_DEFAULT);

    renderLayout();
    await tick();

    mockLayoutWidth(1280);

    const handle = getHandle();
    expect(handle).not.toBeNull();

    handle!.dispatchEvent(
      new MouseEvent("pointerdown", {
        bubbles: true,
        clientX: SIDEBAR_WIDTH_DEFAULT,
      }),
    );
    await tick();

    expect(document.body.classList.contains("sidebar-resizing")).toBe(
      true,
    );

    unmount(component!);
    component = undefined;
    await tick();

    expect(document.body.classList.contains("sidebar-resizing")).toBe(
      false,
    );

    window.dispatchEvent(
      new MouseEvent("pointermove", {
        bubbles: true,
        clientX: SIDEBAR_WIDTH_DEFAULT + 120,
      }),
    );
    window.dispatchEvent(
      new MouseEvent("pointerup", {
        bubbles: true,
        clientX: SIDEBAR_WIDTH_DEFAULT + 120,
      }),
    );
    await tick();

    expect(ui.sidebarWidth).toBe(
      SIDEBAR_WIDTH_DEFAULT,
    );
    expect(document.body.classList.contains("sidebar-resizing")).toBe(
      false,
    );
  });
});
