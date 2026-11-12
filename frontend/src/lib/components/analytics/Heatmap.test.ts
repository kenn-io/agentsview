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

  it("fits a full year, enables Output Tokens without a summary, and guards empty results", async () => {
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
    const component = mount(Heatmap, { target: document.body });
    await tick();

    expect(document.querySelector(".loading")).toBeNull();
    const container = document.querySelector<HTMLElement>(".heatmap-scroll");
    const chart = container?.querySelector<SVGSVGElement>("svg");
    expect(container).not.toBeNull();
    expect(chart).not.toBeNull();
    expect(Number(chart?.getAttribute("width"))).toBeLessThanOrEqual(container?.clientWidth ?? 0);

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

  it("lists active days in a table and keeps day clicks as a pointer shortcut", async () => {
    const select = vi.spyOn(analytics, "selectDate").mockImplementation(() => {});
    analytics.heatmap = {
      metric: "messages",
      entries_from: "2026-03-01",
      levels: { l1: 1, l2: 2, l3: 3, l4: 4 },
      entries: [
        { date: "2026-03-01", value: 0, level: 0 },
        { date: "2026-03-02", value: 1200, level: 4 },
        { date: "2026-03-03", value: 3, level: 1 },
      ],
    };
    const component = mount(Heatmap, { target: document.body });
    await tick();

    const table = document.querySelector("table")!;
    expect(table.querySelector("caption")?.textContent).toBe("Activity");
    expect(Array.from(table.querySelectorAll("thead th"), (cell) => cell.textContent)).toEqual([
      "Date",
      "Messages",
    ]);
    // Days without activity are left out; counts read as the hover tooltip does.
    expect(
      Array.from(table.querySelectorAll("tbody tr"), (row) =>
        Array.from(row.children, (cell) => cell.textContent),
      ),
    ).toEqual([
      ["Mar 2, 2026", "1.2K"],
      ["Mar 3, 2026", "3"],
    ]);

    // The squares are hidden from assistive technology, so none poses as a
    // control it cannot reach.
    const cells = [...document.querySelectorAll<SVGRectElement>(".heatmap-cell")];
    expect(cells).toHaveLength(3);
    expect(cells.some((cell) => cell.hasAttribute("role") || cell.hasAttribute("tabindex"))).toBe(
      false,
    );
    expect(cells[0]!.closest('[aria-hidden="true"]')).not.toBeNull();
    cells[0]!.dispatchEvent(new MouseEvent("click", { bubbles: true }));
    cells[1]!.dispatchEvent(new MouseEvent("click", { bubbles: true }));
    expect(select.mock.calls).toEqual([["2026-03-02"]]);

    unmount(component);
  });
});
