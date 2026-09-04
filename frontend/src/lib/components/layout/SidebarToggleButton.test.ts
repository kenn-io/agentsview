import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { mount, tick, unmount } from "svelte";
import SidebarToggleButton from "./SidebarToggleButton.svelte";
import { setLocale } from "../../i18n/index.js";
import { ui } from "../../stores/ui.svelte.js";

describe("SidebarToggleButton keyboard focus", () => {
  beforeEach(() => {
    setLocale("en");
    ui.sidebarOpen = true;
  });

  afterEach(() => {
    document.body.innerHTML = "";
    ui.sidebarOpen = true;
  });

  it("moves focus to the visible counterpart after collapse and reopen", async () => {
    const sidebarToggle = mount(SidebarToggleButton, {
      target: document.body,
      props: { placement: "sidebar" },
    });
    const contentToggle = mount(SidebarToggleButton, {
      target: document.body,
      props: { placement: "content" },
    });

    const sidebarButton = document.querySelector<HTMLButtonElement>(
      ".sidebar-panel-control--sidebar",
    );
    const contentButton = document.querySelector<HTMLButtonElement>(
      ".sidebar-panel-control--content",
    );

    expect(sidebarButton).not.toBeNull();
    expect(contentButton).not.toBeNull();
    expect(sidebarButton?.getAttribute("aria-controls")).toBe("session-sidebar");
    expect(sidebarButton?.getAttribute("aria-expanded")).toBe("true");

    sidebarButton!.focus();
    sidebarButton!.click();
    await tick();

    expect(ui.sidebarOpen).toBe(false);
    await vi.waitFor(() => {
      expect(document.activeElement).toBe(contentButton);
    });
    expect(contentButton?.getAttribute("aria-expanded")).toBe("false");

    contentButton!.click();
    await tick();

    expect(ui.sidebarOpen).toBe(true);
    await vi.waitFor(() => {
      expect(document.activeElement).toBe(sidebarButton);
    });

    await unmount(contentToggle);
    await unmount(sidebarToggle);
  });
});
