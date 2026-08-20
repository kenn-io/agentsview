// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import { analytics } from "../../stores/analytics.svelte.js";
import ActivityTimeline from "./ActivityTimeline.svelte";

describe("ActivityTimeline", () => {
  afterEach(() => {
    analytics.activity = null;
    analytics.granularity = "day";
    analytics.errors.activity = null;
    document.body.innerHTML = "";
  });

  it("keeps daily bars usable across a full-year range", async () => {
    analytics.granularity = "day";
    analytics.activity = {
      granularity: "day",
      series: Array.from({ length: 365 }, (_, index) => {
        const date = new Date(Date.UTC(2025, 0, 1 + index))
          .toISOString()
          .slice(0, 10);
        return {
          date,
          sessions: 1,
          messages: 2,
          user_messages: 1,
          assistant_messages: 1,
          tool_calls: 0,
          thinking_messages: 0,
          by_agent: {},
        };
      }),
    };

    const component = mount(ActivityTimeline, { target: document.body });
    await tick();

    const firstBar = document.querySelector<SVGRectElement>("rect.bar");
    expect(firstBar).not.toBeNull();
    expect(Number(firstBar!.getAttribute("width"))).toBeGreaterThanOrEqual(5);

    unmount(component);
  });
});
