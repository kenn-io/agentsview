// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import RateLimitHistoryChart from "./RateLimitHistoryChart.svelte";
import type { RateLimitWindow } from "../../stores/ratelimits.svelte.js";

class ImmediateResizeObserver implements ResizeObserver {
  constructor(private readonly cb: ResizeObserverCallback) {}
  observe(target: Element): void {
    this.cb([{ target, contentRect: { width: 400, height: 72 } }] as unknown as ResizeObserverEntry[], this);
  }
  unobserve(): void {}
  disconnect(): void {}
}

function snapshot(observedAt: string, usedPercent: number): RateLimitWindow {
  return { vendor: "codex", machine: "laptop", limitId: "codex", planType: "pro", windowKind: "primary", usedPercent, windowMinutes: 10080, resetsAt: 0, creditsHas: false, creditsUnlimited: false, observedAt };
}
// x coordinates of every point on the spline's "d" attribute ("M x,y" / "L x,y").
function splineX(): number[] {
  const el = document.querySelector<SVGPathElement>(".rate-limit-history-chart path.lc-path");
  return [...(el?.getAttribute("d") ?? "").matchAll(/[ML]\s*(-?[\d.]+)/g)].map((m) => Number(m[1]));
}

let component: ReturnType<typeof mount> | undefined;

describe("RateLimitHistoryChart", () => {
  beforeEach(() => {
    globalThis.ResizeObserver = ImmediateResizeObserver as typeof ResizeObserver;
  });
  afterEach(() => {
    if (component) void unmount(component);
    component = undefined;
    document.body.innerHTML = "";
  });
  // A ~1-year gap must render far wider than a 1-minute gap: points space by elapsed time, not index.
  it("spaces points proportionally to elapsed time, not by index", async () => {
    const snapshots = [
      snapshot("2026-01-01T00:00:00Z", 10),
      snapshot("2026-01-01T00:01:00Z", 20),
      snapshot("2026-12-31T00:01:00Z", 30),
    ];
    component = mount(RateLimitHistoryChart, { target: document.body, props: { snapshots, color: "var(--accent-blue)" } });
    await tick();
    await tick();
    const xs = splineX();
    expect(xs).toHaveLength(3);
    expect(Math.abs(xs[2]! - xs[1]!)).toBeGreaterThan(Math.abs(xs[1]! - xs[0]!) * 10);
  });
});
