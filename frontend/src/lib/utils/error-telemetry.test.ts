// @vitest-environment jsdom
import { expect, it, onTestFinished, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import { reportErrorShown } from "./telemetry.js";
import ErrorDisplay from "./__fixtures__/ErrorDisplay.svelte";
vi.mock("./telemetry.js", () => ({ reportErrorShown: vi.fn() }));
it("counts display lifetimes and identities rather than text updates", async () => {
  const view = mount(ErrorDisplay, { target: document.body });
  onTestFinished(() => unmount(view));
  let detail = 0;
  for (const [visible, identity, count] of [
    [true, "first", 1],
    [true, "first", 1],
    [false, "first", 1],
    [true, "first", 2],
    [true, "second", 3],
  ] as const) {
    view.update(visible, identity, String(detail++));
    await tick();
    expect(reportErrorShown).toHaveBeenCalledTimes(count);
  }
  expect(reportErrorShown).toHaveBeenLastCalledWith("search");
});
