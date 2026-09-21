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

  it("renders the last-query duration to the right of the age label", async () => {
    const component = mount(RefreshControl, {
      target: document.body,
      props: {
        lastUpdatedAt: Date.now() - 3 * 60_000,
        queryDurationMs: 2400,
        onRefresh: vi.fn(),
      },
    });
    await tick();

    const age = document.querySelector(".kit-refresh-control__age > .kit-refresh-control__text");
    const detail = document.querySelector(
      ".kit-refresh-control__detail > .kit-refresh-control__text",
    );
    expect(age?.textContent).toBe("Updated 3m ago");
    expect(detail?.textContent).toBe("2 s");
    expect(age!.compareDocumentPosition(detail!) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();

    unmount(component);
    document.body.innerHTML = "";
  });

  it("leaves the duration box empty before the first query completes", async () => {
    const component = mount(RefreshControl, {
      target: document.body,
      props: { lastUpdatedAt: null, onRefresh: vi.fn() },
    });
    await tick();

    const detail = document.querySelector(
      ".kit-refresh-control__detail > .kit-refresh-control__text",
    );
    expect(detail).not.toBeNull();
    expect(detail?.textContent).toBe("");

    unmount(component);
    document.body.innerHTML = "";
  });

  it("reserves width with the widest localized age and duration variants", async () => {
    setLocale("zh-CN");
    const component = mount(RefreshControl, {
      target: document.body,
      props: { lastUpdatedAt: Date.now(), queryDurationMs: 8, onRefresh: vi.fn() },
    });
    await tick();

    const samples = Array.from(document.querySelectorAll(".kit-refresh-control__sample")).map(
      (node) => node.textContent,
    );
    expect(samples).toContain("999 天前更新");
    expect(samples).toContain("99 分 59 秒");
    expect(
      document.querySelector(".kit-refresh-control__age > .kit-refresh-control__text")?.textContent,
    ).toBe("刚刚更新");

    unmount(component);
    setLocale("en");
    document.body.innerHTML = "";
  });

  it("replaces the age with a transient status", async () => {
    const component = mount(RefreshControl, {
      target: document.body,
      props: {
        lastUpdatedAt: Date.now() - 3 * 60_000,
        onRefresh: vi.fn(),
        status: "Processing activity… 120 rows",
      },
    });
    await tick();

    expect(document.body.textContent).toContain("Processing activity… 120 rows");
    expect(document.body.textContent).not.toContain("Updated 3m ago");

    unmount(component);
    document.body.innerHTML = "";
  });
});
