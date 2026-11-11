// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
// @ts-ignore
import ShortcutsModal from "./ShortcutsModal.svelte";

let component: ReturnType<typeof mount> | undefined;

afterEach(() => {
  if (component) unmount(component);
  component = undefined;
  document.body.innerHTML = "";
});

async function render(): Promise<void> {
  component = mount(ShortcutsModal, { target: document.body });
  await tick();
}

function groupsByLabel(): Map<string, string[]> {
  const result = new Map<string, string[]>();
  for (const group of document.querySelectorAll<HTMLElement>('[role="group"]')) {
    const labelId = group.getAttribute("aria-labelledby");
    const label = labelId ? document.getElementById(labelId)?.textContent?.trim() : undefined;
    const actions = [...group.querySelectorAll(".shortcut-action")].map(
      (el) => el.textContent?.trim() ?? "",
    );
    result.set(label ?? "", actions);
  }
  return result;
}

describe("ShortcutsModal", () => {
  it("labels each shortcut family", async () => {
    await render();

    const groups = groupsByLabel();
    expect([...groups.keys()]).toEqual(["Navigation", "Messages", "Sessions", "Actions"]);
    expect(groups.get("Navigation")).toContain("Open command palette");
    expect(groups.get("Messages")).toEqual([
      "Next message",
      "Previous message",
      "Next user prompt",
      "Previous user prompt",
    ]);
    expect(groups.get("Sessions")).toEqual(["Next session", "Previous session"]);
    expect(groups.get("Actions")).toContain("Show this modal");
  });
});
