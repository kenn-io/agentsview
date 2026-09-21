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

  it("renders the last-query duration as part of the age label", async () => {
    const component = mount(RefreshControl, {
      target: document.body,
      props: {
        lastUpdatedAt: Date.now() - 3 * 60_000,
        queryDurationMs: 2400,
        onRefresh: vi.fn(),
      },
    });
    await tick();

    const label = document.querySelector(".kit-refresh-control__age > .kit-refresh-control__text");
    expect(label?.textContent).toBe("Updated 3m ago · 2 s");

    unmount(component);
    document.body.innerHTML = "";
  });

  it("shows nothing for the duration before the first query completes", async () => {
    const component = mount(RefreshControl, {
      target: document.body,
      props: { lastUpdatedAt: null, onRefresh: vi.fn() },
    });
    await tick();

    const label = document.querySelector(".kit-refresh-control__age > .kit-refresh-control__text");
    expect(label?.textContent).toBe("Not updated");

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
    expect(samples).toContain("999 天前更新 · 99 分 59 秒");
    expect(
      document.querySelector(".kit-refresh-control__age > .kit-refresh-control__text")?.textContent,
    ).toBe("刚刚更新 · 8 毫秒");

    unmount(component);
    setLocale("en");
    document.body.innerHTML = "";
  });

  it("draws each query step on a shared time axis when the label is focused", async () => {
    const component = mount(RefreshControl, {
      target: document.body,
      props: {
        lastUpdatedAt: Date.now(),
        queryDurationMs: 2000,
        querySteps: [
          { name: "summary", startMs: 0, durationMs: 500 },
          { name: "topSessions", startMs: 500, durationMs: 1500 },
        ],
        onRefresh: vi.fn(),
      },
    });
    await tick();
    expect(document.querySelector('[role="tooltip"]')).toBeNull();

    document
      .querySelector(".kit-tooltip-trigger")!
      .dispatchEvent(new FocusEvent("focusin", { bubbles: true }));
    await tick();

    const rows = Array.from(document.querySelectorAll('[role="tooltip"] [role="row"]')).map(
      (row) => ({
        name: row.querySelector(".query-steps__name")?.textContent,
        duration: row.querySelector(".query-steps__duration")?.textContent?.trim(),
        bar: (() => {
          const bar = row.querySelector<HTMLElement>(".query-steps__bar");
          return `${bar?.style.left} ${bar?.style.width}`;
        })(),
      }),
    );
    // Bars sit at their start offset and span their duration against the
    // 2000 ms query, so the second step starts where the first ends.
    expect(rows).toEqual([
      { name: "Summary", duration: "500 ms", bar: "0% 25%" },
      { name: "Top sessions", duration: "2 s", bar: "25% 75%" },
    ]);

    unmount(component);
    document.body.innerHTML = "";
  });

  it("keeps the plain timestamp title when there are no steps", async () => {
    const component = mount(RefreshControl, {
      target: document.body,
      props: { lastUpdatedAt: Date.now(), onRefresh: vi.fn() },
    });
    await tick();

    expect(document.querySelector(".kit-tooltip-trigger")).toBeNull();
    expect(document.querySelector(".kit-refresh-control__age")?.getAttribute("title")).toBeTruthy();

    unmount(component);
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
