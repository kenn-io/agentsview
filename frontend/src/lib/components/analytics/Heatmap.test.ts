// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import { analytics } from "../../stores/analytics.svelte.js";
import Heatmap from "./Heatmap.svelte";

const originalClientWidth = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "clientWidth");

describe("Heatmap", () => {
  afterEach(() => {
    analytics.heatmap = null;
    analytics.summary = null;
    analytics.metric = "messages";
    document.body.innerHTML = "";
    vi.restoreAllMocks();
    if (originalClientWidth) {
      Object.defineProperty(HTMLElement.prototype, "clientWidth", originalClientWidth);
    }
  });

  it("fits a full year of cells inside the card", async () => {
    Object.defineProperty(HTMLElement.prototype, "clientWidth", {
      configurable: true,
      get: () => 600,
    });
    analytics.heatmap = {
      metric: "messages",
      entries_from: "2025-08-21",
      levels: { l1: 1, l2: 2, l3: 3, l4: 4 },
      entries: Array.from({ length: 365 }, (_, index) => ({
        date: new Date(Date.UTC(2025, 7, 21 + index)).toISOString().slice(0, 10),
        value: index % 5,
        level: index % 5,
      })),
    };

    const component = mount(Heatmap, { target: document.body });
    await tick();

    const container = document.querySelector<HTMLElement>(".heatmap-scroll");
    const chart = container?.querySelector<SVGSVGElement>("svg");
    expect(container).not.toBeNull();
    expect(chart).not.toBeNull();
    expect(Number(chart!.getAttribute("width"))).toBeLessThanOrEqual(container!.clientWidth);

    unmount(component);
  });
});
