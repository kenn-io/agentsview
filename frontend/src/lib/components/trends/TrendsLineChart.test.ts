// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import TrendsLineChart from "./TrendsLineChart.svelte";

describe("TrendsLineChart", () => {
  afterEach(() => {
    document.body.innerHTML = "";
  });

  it("keeps the expanded line target for pointer hover", async () => {
    const onHover = vi.fn();
    const component = mount(TrendsLineChart, {
      target: document.body,
      props: {
        buckets: [
          { date: "2026-08-10", message_count: 10 },
          { date: "2026-08-17", message_count: 20 },
        ],
        series: [
          {
            term: "alpha",
            variants: [],
            total: 5,
            points: [
              { date: "2026-08-10", count: 2 },
              { date: "2026-08-17", count: 3 },
            ],
          },
        ],
        colorFor: () => "#1f77b4",
        activeTerm: null,
        normalized: false,
        onHover,
      },
    });
    await tick();

    const hitTarget = document.querySelector<SVGPathElement>(
      '[data-trend-hit="alpha"]',
    );
    expect(hitTarget).not.toBeNull();
    expect(Number(hitTarget!.getAttribute("stroke-width"))).toBe(16);

    hitTarget!.dispatchEvent(new MouseEvent("mouseenter", { bubbles: true }));
    expect(onHover).toHaveBeenLastCalledWith("alpha");
    hitTarget!.dispatchEvent(new MouseEvent("mouseleave", { bubbles: true }));
    expect(onHover).toHaveBeenLastCalledWith(null);

    unmount(component);
  });

  it("keeps active point markers out of pointer hit testing", async () => {
    const component = mount(TrendsLineChart, {
      target: document.body,
      props: {
        buckets: [
          { date: "2026-08-10", message_count: 10 },
          { date: "2026-08-17", message_count: 20 },
        ],
        series: [
          {
            term: "alpha",
            variants: [],
            total: 5,
            points: [
              { date: "2026-08-10", count: 2 },
              { date: "2026-08-17", count: 3 },
            ],
          },
        ],
        colorFor: () => "#1f77b4",
        activeTerm: "alpha",
        normalized: false,
        onHover: vi.fn(),
      },
    });
    await tick();

    const markers = document.querySelectorAll<SVGCircleElement>("circle");
    expect(markers).toHaveLength(2);
    expect([...markers].every((marker) =>
      marker.getAttribute("pointer-events") === "none"
    )).toBe(true);

    unmount(component);
  });
});
