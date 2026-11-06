// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { createRawSnippet, mount, tick, unmount } from "svelte";
// @ts-ignore
import MissingData from "./MissingData.svelte";

let component: ReturnType<typeof mount> | null = null;

afterEach(() => {
  if (component) unmount(component);
  component = null;
  document.body.innerHTML = "";
});

async function render(props: { title: string } & Record<string, unknown>) {
  component = mount(MissingData, { target: document.body, props });
  await tick();
  return document.body.querySelector(".missing-data") as HTMLElement;
}

describe("MissingData", () => {
  it("shows the title and the explanation", async () => {
    const root = await render({
      title: "No pinned messages",
      description: "Pin messages from any session.",
    });
    const paragraphs = Array.from(root.querySelectorAll("p")).map((p) => p.textContent);
    expect(paragraphs).toEqual(["No pinned messages", "Pin messages from any session."]);
  });

  it("renders only the title when nothing else is given", async () => {
    const root = await render({ title: "No results" });
    expect(root.textContent?.trim()).toBe("No results");
    expect(root.querySelector('[aria-hidden="true"]')).toBeNull();
    expect(root.querySelector("button")).toBeNull();
    expect(root.getAttribute("role")).toBeNull();
  });

  it("keeps the icon out of the accessible text", async () => {
    const icon = createRawSnippet(() => ({ render: () => "<svg data-testid=\"glyph\"></svg>" }));
    const root = await render({ title: "Trash is empty", icon });
    const glyph = root.querySelector('[data-testid="glyph"]')!;
    expect(glyph.closest('[aria-hidden="true"]')).not.toBeNull();
  });

  it("renders a working action under the text", async () => {
    const onRetry = vi.fn();
    const action = createRawSnippet(() => ({
      render: () => "<button>Retry</button>",
      setup: (button) => {
        button.addEventListener("click", onRetry);
      },
    }));
    const root = await render({ title: "Could not load", action, role: "status" });
    expect(root.getAttribute("role")).toBe("status");
    const button = root.querySelector("button")!;
    // The action follows the text, so a screen reader hears why before what.
    expect(root.lastElementChild?.contains(button)).toBe(true);
    button.click();
    expect(onRetry).toHaveBeenCalledOnce();
  });
});
