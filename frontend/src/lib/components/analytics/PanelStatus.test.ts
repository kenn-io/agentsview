// @vitest-environment jsdom
import { expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import PanelStatus from "./PanelStatus.svelte";

it("shows loading, or the error with Retry that is withheld while a retry runs", async () => {
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
      expect(document.querySelector('[role="status"]') !== null).toBe(showLoading);
      const alert = document.querySelector('[role="alert"]');
      const button = document.querySelector<HTMLButtonElement>("button");
      if (error) {
        expect(alert!.textContent).toContain("request failed");
        if (pending) {
          expect(button).toBeNull();
        } else {
          expect(button!.textContent).toBe("Retry");
          button!.click();
          expect(retry).toHaveBeenCalledOnce();
        }
      } else {
        expect(alert).toBeNull();
        expect(button).toBeNull();
      }
    } finally {
      await unmount(component);
    }
  }
});
