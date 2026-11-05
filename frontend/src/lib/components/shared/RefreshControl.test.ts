// @vitest-environment jsdom
import { describe, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import { setLocale } from "../../i18n/index.js";
// @ts-ignore
import RefreshControl from "./RefreshControl.svelte";
import { LiveQuery } from "../../utils/liveQuery.svelte.js";

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

  it("draws a timeline with an axis, phase segments, and a legend when the label is focused", async () => {
    const component = mount(RefreshControl, {
      target: document.body,
      props: {
        lastUpdatedAt: Date.now(),
        queryDurationMs: 2000,
        querySteps: [
          {
            name: "summary",
            startMs: 0,
            durationMs: 500,
            segments: [
              { phase: "wait", startMs: 0, durationMs: 400 },
              { phase: "download", startMs: 400, durationMs: 80 },
              { phase: "apply", startMs: 480, durationMs: 20 },
            ],
          },
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

    const tooltip = document.querySelector('[role="tooltip"]')!;
    const geometry = (el: Element | null) => {
      const bar = el as HTMLElement | null;
      return `${bar?.style.left} ${bar?.style.width}`;
    };
    // Axis ticks span the 2000 ms query at 500 ms steps.
    expect(
      Array.from(tooltip.querySelectorAll(".query-steps__tick")).map((tick) =>
        tick.textContent?.trim(),
      ),
    ).toEqual(["0", "500 ms", "1 s", "1.5 s", "2 s"]);
    expect(
      Array.from(tooltip.querySelectorAll(".query-steps__name")).map((name) => name.textContent),
    ).toEqual(["Summary", "Top sessions"]);
    const tracks = tooltip.querySelectorAll(".query-steps__track");
    // Segmented step: wait, download, apply placed end to end.
    expect(Array.from(tracks[0]!.querySelectorAll(".query-steps__bar")).map(geometry)).toEqual([
      "0% 20%",
      "20% 4%",
      "24% 1%",
    ]);
    // Unsegmented step: one solid bar at its start offset.
    expect(Array.from(tracks[1]!.querySelectorAll(".query-steps__bar")).map(geometry)).toEqual([
      "25% 75%",
    ]);
    expect(
      tooltip.querySelector(".query-steps__legend")?.textContent?.replace(/\s+/g, " ").trim(),
    ).toBe("Server Transfer Render");
    expect(tooltip.querySelector(".query-steps__total")?.textContent).toBe("2 s");

    unmount(component);
    document.body.innerHTML = "";
  });

  it("draws requests from one dispatch burst flush with the zero line", async () => {
    const component = mount(RefreshControl, {
      target: document.body,
      props: {
        lastUpdatedAt: Date.now(),
        queryDurationMs: 200,
        querySteps: [
          { name: "summary", startMs: 0.4, durationMs: 100 },
          { name: "activity", startMs: 1.6, durationMs: 100 },
          { name: "topSessions", startMs: 100, durationMs: 100 },
        ],
        onRefresh: vi.fn(),
      },
    });
    await tick();
    document
      .querySelector(".kit-tooltip-trigger")!
      .dispatchEvent(new FocusEvent("focusin", { bubbles: true }));
    await tick();

    const lefts = Array.from(
      document.querySelectorAll<HTMLElement>('[role="tooltip"] .query-steps__bar'),
    ).map((bar) => bar.style.left);
    // The axis starts at the first send (0.4 ms). 1.6 ms is within two
    // pixels of it on a 200 px track, so both sit at 0%; the later request
    // keeps its real offset from that origin: 99.6 of 199.6 ms.
    expect(lefts).toEqual(["0%", "0%", "49.9%"]);

    unmount(component);
    document.body.innerHTML = "";
  });

  it("starts the axis at the first request, not at the refresh", async () => {
    const component = mount(RefreshControl, {
      target: document.body,
      props: {
        lastUpdatedAt: Date.now(),
        queryDurationMs: 110,
        // 3 ms of setup before the first send is well past the two-pixel
        // snap on a ~107 ms axis, so only a moved origin puts it at zero.
        querySteps: [{ name: "entries", startMs: 3, durationMs: 100 }],
        onRefresh: vi.fn(),
      },
    });
    await tick();
    document
      .querySelector(".kit-tooltip-trigger")!
      .dispatchEvent(new FocusEvent("focusin", { bubbles: true }));
    await tick();

    const bar = document.querySelector<HTMLElement>('[role="tooltip"] .query-steps__bar')!;
    expect(bar.style.left).toBe("0%");
    // The axis runs 107 ms: from the first send to the recorded total.
    expect(bar.style.width).toBe("93.46%");

    unmount(component);
    document.body.innerHTML = "";
  });

  it("counts a running query up and grows its running steps", async () => {
    vi.useFakeTimers({ toFake: ["performance", "setInterval", "clearInterval"] });
    try {
      const live = new LiveQuery();
      const component = mount(RefreshControl, {
        target: document.body,
        props: {
          lastUpdatedAt: Date.now() - 3 * 60_000,
          queryDurationMs: 17_000,
          querySteps: [{ name: "summary", startMs: 0, durationMs: 17_000 }],
          liveQuery: live,
          onRefresh: vi.fn(),
        },
      });
      const text = () =>
        document.querySelector(".kit-refresh-control__age > .kit-refresh-control__text")
          ?.textContent;
      await tick();
      expect(text()).toBe("Updated 3m ago · 17 s");

      const started = performance.now();
      live.begin(started);
      const summary = live.start("summary", started);
      live.start("tools", started);
      vi.advanceTimersByTime(400);
      await tick();
      expect(text()).toBe("Updated 3m ago · 400 ms");

      live.settle(summary, { name: "summary", startMs: 0, durationMs: 300 });
      vi.advanceTimersByTime(1600);
      await tick();
      expect(text()).toBe("Updated 3m ago · 2 s");

      document
        .querySelector(".kit-tooltip-trigger")!
        .dispatchEvent(new FocusEvent("focusin", { bubbles: true }));
      await tick();
      const tooltip = document.querySelector('[role="tooltip"]')!;
      expect(tooltip.querySelector(".query-steps__at")?.textContent).toBe("Refreshing…");
      const durations = () =>
        Array.from(tooltip.querySelectorAll(".query-steps__duration")).map((d) => d.textContent);
      // The settled step keeps its time; the running one reaches now.
      expect(durations()).toEqual(["300 ms", "2 s"]);
      const toolsBar = () =>
        tooltip
          .querySelectorAll<HTMLElement>(".query-steps__track")[1]!
          .querySelector<HTMLElement>(".query-steps__bar")!.style.width;
      expect(toolsBar()).toBe("100%");

      vi.advanceTimersByTime(2000);
      await tick();
      expect(durations()).toEqual(["300 ms", "4 s"]);
      // The axis grew with the query, so the settled step now covers less of it.
      expect(tooltip.querySelectorAll<HTMLElement>(".query-steps__bar")[0]!.style.width).toBe(
        "7.5%",
      );

      void unmount(component);
    } finally {
      vi.useRealTimers();
      document.body.innerHTML = "";
    }
  });

  it("starts running bars empty at the zero line when a query begins", async () => {
    vi.useFakeTimers({ toFake: ["performance", "setInterval", "clearInterval"] });
    try {
      const live = new LiveQuery();
      const component = mount(RefreshControl, {
        target: document.body,
        props: {
          lastUpdatedAt: Date.now(),
          queryDurationMs: 5000,
          querySteps: [{ name: "summary", startMs: 0, durationMs: 5000 }],
          liveQuery: live,
          onRefresh: vi.fn(),
        },
      });
      await tick();
      document
        .querySelector(".kit-tooltip-trigger")!
        .dispatchEvent(new FocusEvent("focusin", { bubbles: true }));
      await tick();

      // Requests from one dispatch leave a fraction of a millisecond apart,
      // and the first tick lands just after they go out.
      const started = performance.now();
      live.begin(started);
      live.start("summary", started + 0.1);
      live.start("tools", started + 0.4);
      vi.advanceTimersByTime(100);
      await tick();

      const bars = Array.from(
        document.querySelectorAll<HTMLElement>('[role="tooltip"] .query-steps__bar'),
      ).map((bar) => `${bar.style.left} ${bar.style.width}`);
      expect(bars).toEqual(["0% 9.99%", "0% 9.96%"]);

      void unmount(component);
    } finally {
      vi.useRealTimers();
      document.body.innerHTML = "";
    }
  });

  it("appends the running duration to a progress status", async () => {
    vi.useFakeTimers({ toFake: ["performance", "setInterval", "clearInterval"] });
    try {
      const live = new LiveQuery();
      live.begin(performance.now());
      const component = mount(RefreshControl, {
        target: document.body,
        props: {
          lastUpdatedAt: null,
          status: "Loading sessions…",
          liveQuery: live,
          onRefresh: vi.fn(),
        },
      });
      vi.advanceTimersByTime(3000);
      await tick();
      expect(
        document.querySelector(".kit-refresh-control__age > .kit-refresh-control__text")
          ?.textContent,
      ).toBe("Loading sessions… · 3 s");

      void unmount(component);
    } finally {
      vi.useRealTimers();
      document.body.innerHTML = "";
    }
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
