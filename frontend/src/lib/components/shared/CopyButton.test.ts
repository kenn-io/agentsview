// @vitest-environment jsdom
import { describe, expect, it, vi } from "vite-plus/test";
import { mount, unmount } from "svelte";
// @ts-ignore
import CopyButton from "./CopyButton.svelte";

describe("CopyButton", () => {
  it("renders the copy icon button and forwards clicks", () => {
    const onclick = vi.fn();
    const component = mount(CopyButton, {
      target: document.body,
      props: {
        copied: false,
        ariaLabel: "Copy message",
        copiedAriaLabel: "Copied message",
        title: "Copy message",
        copiedTitle: "Copied!",
        onclick,
      },
    });

    const button = document.querySelector<HTMLButtonElement>(
      'button.copy-btn[aria-label="Copy message"]',
    );
    expect(button).not.toBeNull();
    expect(button!.getAttribute("title")).toBe("Copy message");
    expect(button!.querySelector("svg")).not.toBeNull();
    expect(button!.textContent?.trim()).toBe("");

    button!.click();
    expect(onclick).toHaveBeenCalledOnce();

    unmount(component);
  });

  it("renders the copied icon and labels", () => {
    const component = mount(CopyButton, {
      target: document.body,
      props: {
        copied: true,
        ariaLabel: "Copy code block",
        copiedAriaLabel: "Copied code block",
        title: "Copy code",
        copiedTitle: "Copied!",
      },
    });

    const button = document.querySelector<HTMLButtonElement>(
      'button.copy-btn[aria-label="Copied code block"]',
    );
    expect(button).not.toBeNull();
    expect(button!.getAttribute("title")).toBe("Copied!");
    expect(button!.querySelector("svg")).not.toBeNull();
    expect(button!.textContent?.trim()).toBe("");

    unmount(component);
  });
});
