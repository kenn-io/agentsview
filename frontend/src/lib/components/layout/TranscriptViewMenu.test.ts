// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import { setLocale } from "../../i18n/index.js";
import { ui } from "../../stores/ui.svelte.js";
import { sessions } from "../../stores/sessions.svelte.js";
import { registerShortcuts } from "../../utils/keyboard.js";
import TranscriptViewMenu from "./TranscriptViewMenu.svelte";

function trigger(): HTMLButtonElement {
  return document.querySelector<HTMLButtonElement>(".view-trigger")!;
}

async function open(): Promise<void> {
  trigger().click();
  await tick();
}

function item(role: string, text: string): HTMLElement {
  const found = Array.from(document.querySelectorAll<HTMLElement>(`[role="${role}"]`)).find(
    (element) => element.textContent?.includes(text),
  );
  if (!found) throw new Error(`no ${role} containing "${text}"`);
  return found;
}

describe("TranscriptViewMenu", () => {
  let component: ReturnType<typeof mount> | undefined;

  beforeEach(() => {
    setLocale("en");
    ui.setLayout("default");
    ui.setTranscriptMode("normal");
    ui.showAllBlocks();
    component = mount(TranscriptViewMenu, { target: document.body });
  });

  afterEach(() => {
    if (component) unmount(component);
    component = undefined;
    document.body.innerHTML = "";
    ui.setLayout("default");
    ui.setTranscriptMode("normal");
    ui.showAllBlocks();
  });

  it("names the current layout on the trigger", async () => {
    await tick();
    expect(trigger().textContent).toContain("View: Default");
    ui.setLayout("skim");
    await tick();
    expect(trigger().textContent).toContain("View: Skim");
  });

  it("lists every layout with a description and switches layout", async () => {
    await open();
    for (const name of ["Default", "Compact", "Stream", "Skim"]) {
      expect(item("menuitemradio", name)).toBeDefined();
    }
    expect(item("menuitemradio", "Skim").textContent).toContain("One line per message and tool call");
    expect(item("menuitemradio", "Default").getAttribute("aria-checked")).toBe("true");

    item("menuitemradio", "Skim").click();
    await tick();
    expect(ui.messageLayout).toBe("skim");
    // Picking a layout closes the menu.
    expect(document.querySelector('[role="menu"]')).toBeNull();
  });

  it("switches to prompts and final answers", async () => {
    await open();
    item("menuitemradio", "Prompts and final answers").click();
    await tick();
    expect(ui.transcriptMode).toBe("focused");
  });

  it("toggles a block type without closing the menu", async () => {
    await open();
    item("menuitem", "Tool calls").click();
    await tick();
    expect(ui.isBlockVisible("tool")).toBe(false);
    expect(item("menuitem", "Tool calls").querySelector(".kit-menu__icon svg")).toBeNull();
    expect(item("menuitem", "Show all")).toBeDefined();

    item("menuitem", "Show all").click();
    await tick();
    expect(ui.isBlockVisible("tool")).toBe(true);
  });

  it("names prompts-only mode on the trigger and marks hidden blocks", async () => {
    await tick();
    expect(trigger().textContent).not.toContain("Prompts and answers");
    ui.setTranscriptMode("focused");
    await tick();
    expect(trigger().textContent).toContain("View: Default · Prompts and answers");
    expect(trigger().classList.contains("filtered")).toBe(false);
    ui.toggleBlock("thinking");
    await tick();
    expect(trigger().classList.contains("filtered")).toBe(true);
    expect(trigger().querySelector(".view-filter-dot")).not.toBeNull();
  });

  it("closes on Escape without closing the open session", async () => {
    const unregister = registerShortcuts({ navigateMessage: () => {}, navigateUserPrompt: () => {} });
    sessions.activeSessionId = "s1";
    try {
      await open();
      expect(document.querySelector('[role="menu"]')).not.toBeNull();
      (document.activeElement ?? document.body).dispatchEvent(
        new KeyboardEvent("keydown", { key: "Escape", bubbles: true, cancelable: true }),
      );
      await tick();
      expect(document.querySelector('[role="menu"]')).toBeNull();
      expect(sessions.activeSessionId).toBe("s1");
    } finally {
      unregister();
      sessions.activeSessionId = null;
    }
  });

  it("keeps Show all mounted while nothing is hidden", async () => {
    await open();
    expect(item("menuitem", "Show all").getAttribute("aria-disabled")).toBe("true");
  });
});
