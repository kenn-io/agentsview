// @vitest-environment jsdom
import { expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import PanelStatus from "./PanelStatus.svelte";

it("keeps Retry labelled and unavailable while its request runs", async () => {
  const retry = vi.fn();
  const component = mount(PanelStatus, {
    target: document.body,
    props: { hasData: true, pending: true, error: "Month failed", onRetry: retry },
  });
  try {
    await tick();
    const button = document.querySelector<HTMLButtonElement>("button")!;
    expect(button.getAttribute("aria-busy")).toBe("true");
    expect(button.disabled || button.getAttribute("aria-disabled") === "true").toBe(true);
    expect(button.querySelector(".kit-button__label")!.textContent).toBe("Retry");
    button.click();
    expect(retry).not.toHaveBeenCalled();
  } finally {
    await unmount(component);
  }
});
