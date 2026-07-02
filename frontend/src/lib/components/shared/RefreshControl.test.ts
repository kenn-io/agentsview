// @vitest-environment jsdom
import { describe, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import { setLocale } from "../../i18n/index.js";
// @ts-ignore
import RefreshControl from "./RefreshControl.svelte";

// The wrapper's whole job is injecting the app's localized age formatter
// into kit-ui's RefreshControl (whose built-in default is English).
describe("RefreshControl", () => {
  it("localizes the never-updated label through the app formatter", async () => {
    setLocale("zh-CN");
    const component = mount(RefreshControl, {
      target: document.body,
      props: { lastUpdatedAt: null, onRefresh: vi.fn() },
    });
    await tick();

    expect(document.body.textContent).toContain("未更新");

    unmount(component);
    setLocale("en");
    document.body.innerHTML = "";
  });

  it("renders the age through the app's formatRefreshAge", async () => {
    const component = mount(RefreshControl, {
      target: document.body,
      props: {
        lastUpdatedAt: Date.now() - 3 * 60_000,
        onRefresh: vi.fn(),
      },
    });
    await tick();

    expect(document.body.textContent).toContain("Updated 3m ago");

    unmount(component);
    document.body.innerHTML = "";
  });
});
