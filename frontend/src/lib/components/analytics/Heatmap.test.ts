// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import { analytics } from "../../stores/analytics.svelte.js";
import Heatmap from "./Heatmap.svelte";

const originalClientWidth = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "clientWidth");

describe("Heatmap", () => {
  afterEach(() => {
    analytics.heatmap = null;
    analytics.errors.heatmap = null;
    analytics.summary = null;
    analytics.metric = "messages";
    document.body.innerHTML = "";
    vi.restoreAllMocks();
    if (originalClientWidth) {
      Object.defineProperty(HTMLElement.prototype, "clientWidth", originalClientWidth);
    }
  });

  it("fits a full year and keeps Output Tokens usable without a summary", async () => {
    Object.defineProperty(HTMLElement.prototype, "clientWidth", {
      configurable: true,
      get: () => 600,
    });
    analytics.summary = null;
    const metric = vi.spyOn(analytics, "setMetric").mockImplementation(() => {});
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

    const data = analytics.heatmap;
    analytics.heatmap = null;
    const component = mount(Heatmap, { target: document.body });
    await tick();
    expect.soft(document.querySelector(".loading")).not.toBeNull();
    analytics.heatmap = data;
    await tick();
    expect.soft(document.querySelector(".loading")).toBeNull();

    const container = document.querySelector<HTMLElement>(".heatmap-scroll");
    const chart = container?.querySelector<SVGSVGElement>("svg");
    expect.soft(container).not.toBeNull();
    expect.soft(chart).not.toBeNull();
    expect
      .soft(Number(chart?.getAttribute("width")))
      .toBeLessThanOrEqual(container?.clientWidth ?? 0);

    const tokens = [...document.querySelectorAll<HTMLButtonElement>("button")].find(
      (button) => button.textContent?.trim() === "Output Tokens",
    );
    expect(tokens).toBeDefined();
    tokens!.click();
    expect(metric).toHaveBeenCalledWith("output_tokens");

    analytics.metric = "output_tokens";
    analytics.heatmap = { ...data!, metric: "output_tokens", entries: [] };
    await tick();
    expect(document.body.textContent).toContain("No data for this period");
    analytics.errors.heatmap = "request failed";
    await tick();
    expect(document.querySelector(".error")).not.toBeNull();
    expect(document.body.textContent).toContain("No data for this period");
    analytics.heatmap = null;
    await tick();
    expect(document.body.textContent).not.toContain("No data for this period");

    unmount(component);
  });
});
