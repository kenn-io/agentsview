// @vitest-environment jsdom
import { expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import PanelStatus from "./PanelStatus.svelte";

it("shows loading or a labelled Retry and marks pending retries busy", async () => {
  for (const [hasData, pending, error, showLoading] of [
    [false, false, null, true],
    [false, true, null, true],
    [true, false, null, false],
    [false, false, "request failed", false],
    [true, false, "request failed", false],
    [true, true, "request failed", false],
  ] as const) {
    const retry = vi.fn();
    const component = mount(PanelStatus, {
      target: document.body,
      props: { hasData, pending, error, onRetry: retry },
    });
    try {
      await tick();
      expect(document.querySelector(".loading") !== null).toBe(showLoading);
      const button = document.querySelector<HTMLButtonElement>("button");
      if (error) {
        expect(document.querySelector(".error")!.textContent).toContain("request failed");
        expect(button!.querySelector(".kit-button__label")!.textContent).toBe("Retry");
        expect(button!.getAttribute("aria-busy")).toBe(pending ? "true" : null);
        if (!pending) {
          button!.click();
          expect(retry).toHaveBeenCalledOnce();
        }
      } else {
        expect(button).toBeNull();
      }
    } finally {
      await unmount(component);
    }
  }
});
