// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { fireEvent, render } from "@testing-library/svelte";
import { mount, tick, unmount } from "svelte";
import ConcurrencyTimeline from "./ConcurrencyTimeline.svelte";
import type { Report } from "../../api/types.js";

class ResizeObserverMock {
  observe = vi.fn();
  disconnect = vi.fn();
}

function makeReport(overrides: Partial<Report> = {}): Report {
  // idx 2 (peak 3) carries a mixed split (2 interactive / 1 automated) for the
  // stacking and split-tooltip tests; idx 3 (peak 1) is all-interactive.
  const buckets = [
    {
      start: "2026-06-16T00:00:00Z",
      end: "2026-06-16T03:00:00Z",
      max_agents: 0,
      agent_minutes: 0,
      output_tokens: 0,
      cost: 0,
      interactive_at_peak: 0,
      automated_at_peak: 0,
    },
    {
      start: "2026-06-16T03:00:00Z",
      end: "2026-06-16T06:00:00Z",
      max_agents: 2,
      agent_minutes: 12,
      output_tokens: 4000,
      cost: 0.4,
      interactive_at_peak: 1,
      automated_at_peak: 1,
    },
    {
      start: "2026-06-16T06:00:00Z",
      end: "2026-06-16T09:00:00Z",
      max_agents: 3,
      agent_minutes: 30,
      output_tokens: 9000,
      cost: 0.9,
      interactive_at_peak: 2,
      automated_at_peak: 1,
    },
    {
      start: "2026-06-16T09:00:00Z",
      end: "2026-06-16T12:00:00Z",
      max_agents: 1,
      agent_minutes: 8,
      output_tokens: 2000,
      cost: 0.2,
      interactive_at_peak: 1,
      automated_at_peak: 0,
    },
    {
      start: "2026-06-16T12:00:00Z",
      end: "2026-06-16T15:00:00Z",
      max_agents: 0,
      agent_minutes: 0,
      output_tokens: 0,
      cost: 0,
      interactive_at_peak: 0,
      automated_at_peak: 0,
    },
  ];
  const report = {
    peak: { agents: 3, at: "2026-06-16T06:00:00Z" },
    totals: {
      active_minutes: 50,
      idle_minutes: 10,
      agent_minutes: 50,
      sessions: 4,
      untimed_sessions: 0,
      distinct_projects: 2,
      distinct_models: 1,
      output_tokens: 15000,
      cost: 1.5,
    },
    partial: false,
    as_of: null,
    timezone: "UTC",
    range_start: "2026-06-16T00:00:00Z",
    range_end: "2026-06-17T00:00:00Z",
    bucket_unit: "hour",
    // Five 3h buckets (00:00-15:00) have elapsed of the eight-bucket day, so the
    // effective end is 15:00 and 15:00-24:00 is the future region.
    effective_end: "2026-06-16T15:00:00Z",
    bucket_seconds: 10800,
    bucket_count: 8,
    elapsed_bucket_count: 5,
    buckets,
    by_project: null,
    by_model: null,
    by_agent: null,
    by_session: null,
    intervals: [],
    ...overrides,
  } as Report;
  // Backfill the peak-automation split onto any bucket literal that omits it
  // (most fixtures only set max_agents), so the stacked bars get real geometry
  // instead of NaN. Unspecified buckets default to all-interactive.
  report.buckets = (report.buckets ?? []).map((b) => ({
    interactive_at_peak: b.max_agents,
    automated_at_peak: 0,
    ...b,
  }));
  return report;
}

function popoverReport(): Report {
  return makeReport({
    bucket_unit: "minute",
    bucket_seconds: 300,
    bucket_count: 2,
    elapsed_bucket_count: 1,
    buckets: [
      {
        start: "2026-06-16T10:00:00Z",
        end: "2026-06-16T10:05:00Z",
        max_agents: 2,
        agent_minutes: 4,
        output_tokens: 0,
        cost: 0,
      },
    ],
    by_session: [
      {
        session_id: "a",
        title: "Alpha",
        project: "p",
        agent: "claude",
        primary_model: "m",
        models: ["m"],
        agent_minutes: 2,
        cost: 0,
        output_tokens: 0,
        first_active: "2026-06-16T10:00:00Z",
        last_active: "2026-06-16T10:02:00Z",
        timing_quality: "timed",
      },
      {
        session_id: "b",
        title: "Beta",
        project: "p",
        agent: "claude",
        primary_model: "m",
        models: ["m"],
        agent_minutes: 2,
        cost: 0,
        output_tokens: 0,
        first_active: "2026-06-16T10:01:00Z",
        last_active: "2026-06-16T10:03:00Z",
        timing_quality: "timed",
      },
    ] as Report["by_session"],
    intervals: [
      { session_id: "a", start: "2026-06-16T10:00:00Z", end: "2026-06-16T10:01:00Z" },
      { session_id: "a", start: "2026-06-16T10:01:00Z", end: "2026-06-16T10:02:00Z" },
      { session_id: "b", start: "2026-06-16T10:01:00Z", end: "2026-06-16T10:03:00Z" },
    ] as Report["intervals"],
  });
}

// A minute-bucketed quarter-hour range used by the per-bucket geometry tests.
function minuteReport(overrides: Partial<Report> = {}): Report {
  return makeReport({
    range_start: "2026-06-16T00:00:00Z",
    range_end: "2026-06-16T00:15:00Z",
    bucket_unit: "minute",
    bucket_seconds: 300,
    bucket_count: 3,
    elapsed_bucket_count: 3,
    effective_end: "2026-06-16T00:15:00Z",
    buckets: [
      {
        start: "2026-06-16T00:00:00Z",
        end: "2026-06-16T00:05:00Z",
        max_agents: 1,
        agent_minutes: 5,
        output_tokens: 10,
        cost: 0,
      },
      {
        start: "2026-06-16T00:05:00Z",
        end: "2026-06-16T00:10:00Z",
        max_agents: 2,
        agent_minutes: 5,
        output_tokens: 20,
        cost: 0,
      },
      {
        start: "2026-06-16T00:10:00Z",
        end: "2026-06-16T00:15:00Z",
        max_agents: 1,
        agent_minutes: 5,
        output_tokens: 5,
        cost: 0,
      },
    ] as Report["buckets"],
    ...overrides,
  });
}

async function chooseOverlayMetric(target: HTMLElement, label: string) {
  const trigger = target.querySelector<HTMLButtonElement>(
    ".overlay-toggle .kit-typeahead__trigger",
  );
  expect(trigger).toBeTruthy();
  await fireEvent.click(trigger!);
  await tick();

  const option = Array.from(
    target.querySelectorAll<HTMLElement>(".overlay-toggle .kit-typeahead__option"),
  ).find((el) => el.textContent?.trim() === label);
  expect(option).toBeTruthy();
  await fireEvent.mouseDown(option!);
  await tick();
}

describe("ConcurrencyTimeline", () => {
  let originalResizeObserver: typeof ResizeObserver | undefined;

  beforeEach(() => {
    originalResizeObserver = globalThis.ResizeObserver;
    Object.defineProperty(globalThis, "ResizeObserver", {
      configurable: true,
      writable: true,
      value: ResizeObserverMock,
    });
  });

  afterEach(() => {
    Object.defineProperty(globalThis, "ResizeObserver", {
      configurable: true,
      writable: true,
      value: originalResizeObserver,
    });
    document.body.innerHTML = "";
    vi.restoreAllMocks();
  });

  it("renders one interactive and one automated segment per bucket", async () => {
    const report = makeReport();
    const c = mount(ConcurrencyTimeline, {
      target: document.body,
      props: { report },
    });
    await tick();

    const interactive = document.querySelectorAll(".concurrency-seg.interactive");
    const automated = document.querySelectorAll(".concurrency-seg.automated");
    expect(interactive.length).toBe(report.buckets!.length);
    expect(automated.length).toBe(report.buckets!.length);

    unmount(c);
  });

  it("stacks a taller interactive base under a shorter automated cap", async () => {
    const report = makeReport();
    const c = mount(ConcurrencyTimeline, {
      target: document.body,
      props: { report },
    });
    await tick();
    // Bucket idx 2 peaks at 3 (2 interactive + 1 automated).
    const interactive = document.querySelectorAll(
      ".concurrency-seg.interactive",
    )[2] as SVGRectElement;
    const automated = document.querySelectorAll(".concurrency-seg.automated")[2] as SVGRectElement;
    const h = (el: SVGRectElement) => Number(el.getAttribute("height"));
    const y = (el: SVGRectElement) => Number(el.getAttribute("y"));
    // The automated cap has real height and sits above (smaller y) the taller
    // interactive base.
    expect(h(automated)).toBeGreaterThan(0);
    expect(h(interactive)).toBeGreaterThan(h(automated));
    expect(y(automated)).toBeLessThan(y(interactive));

    unmount(c);
  });

  it("draws a future region when elapsed_bucket_count < bucket_count", async () => {
    const report = makeReport();
    expect(report.elapsed_bucket_count).toBeLessThan(report.bucket_count);
    const c = mount(ConcurrencyTimeline, {
      target: document.body,
      props: { report },
    });
    await tick();

    const future = document.querySelector(".concurrency-future");
    expect(future).toBeTruthy();

    unmount(c);
  });

  it("omits the future region for a complete day", async () => {
    const report = makeReport({
      bucket_count: 5,
      elapsed_bucket_count: 5,
      effective_end: "2026-06-17T00:00:00Z",
    });
    const c = mount(ConcurrencyTimeline, {
      target: document.body,
      props: { report },
    });
    await tick();

    expect(document.querySelector(".concurrency-future")).toBeNull();

    unmount(c);
  });

  it("shades the active/idle strip cell only when max_agents > 0", async () => {
    const report = makeReport();
    const c = mount(ConcurrencyTimeline, {
      target: document.body,
      props: { report },
    });
    await tick();

    const cells = document.querySelectorAll(".strip-cell");
    expect(cells.length).toBe(report.buckets!.length);
    const active = document.querySelectorAll(".strip-cell.active");
    expect(active.length).toBe(3);

    unmount(c);
  });

  it("shows a tooltip on slot hover and clears it on leave", async () => {
    const target = document.createElement("div");
    document.body.appendChild(target);
    const c = mount(ConcurrencyTimeline, { target, props: { report: makeReport() } });
    await tick();
    const hits = target.querySelectorAll(".slot-hit");
    expect(hits.length).toBe(makeReport().buckets!.length);
    const hit = hits[2] as SVGRectElement; // bucket idx 2 has max_agents 3
    hit.dispatchEvent(new MouseEvent("mouseenter", { bubbles: true }));
    await tick();
    const tip = target.querySelector(".tooltip");
    expect(tip).toBeTruthy();
    expect(tip!.textContent).toContain("peak 3");
    hit.dispatchEvent(new MouseEvent("mouseleave", { bubbles: true }));
    await tick();
    expect(target.querySelector(".tooltip")).toBeNull();
    unmount(c);
    target.remove();
  });

  it("labels the token count as output and surfaces cost in the tooltip", async () => {
    const target = document.createElement("div");
    document.body.appendChild(target);
    const c = mount(ConcurrencyTimeline, { target, props: { report: makeReport() } });
    await tick();
    const hit = target.querySelectorAll(".slot-hit")[2] as SVGRectElement; // 9000 tokens, $0.90
    hit.dispatchEvent(new MouseEvent("mouseenter", { bubbles: true }));
    await tick();
    const tip = target.querySelector(".tooltip");
    expect(tip).toBeTruthy();
    // Disambiguates input vs output tokens and shows the overlay's cost value.
    expect(tip!.textContent).toContain("output tokens");
    expect(tip!.textContent).toContain("9,000");
    expect(tip!.textContent).toContain("$0.90");
    unmount(c);
    target.remove();
  });

  it("splits only the peak count in the tooltip, leaving agent-min combined", async () => {
    const target = document.createElement("div");
    document.body.appendChild(target);
    const c = mount(ConcurrencyTimeline, { target, props: { report: makeReport() } });
    await tick();
    const hit = target.querySelectorAll(".slot-hit")[2] as SVGRectElement; // peak 3 = 2 int / 1 auto
    hit.dispatchEvent(new MouseEvent("mouseenter", { bubbles: true }));
    await tick();
    const tip = target.querySelector(".tooltip");
    expect(tip!.textContent).toContain("peak 3 (2 int / 1 auto)");
    // agent-minutes stays a single combined figure, not split by automation.
    expect(tip!.textContent).toContain("30.0 agent-min");
    unmount(c);
    target.remove();
  });

  it("omits the peak split when the bucket has no automated agent", async () => {
    const target = document.createElement("div");
    document.body.appendChild(target);
    const c = mount(ConcurrencyTimeline, { target, props: { report: makeReport() } });
    await tick();
    const hit = target.querySelectorAll(".slot-hit")[3] as SVGRectElement; // peak 1, all interactive
    hit.dispatchEvent(new MouseEvent("mouseenter", { bubbles: true }));
    await tick();
    const tip = target.querySelector(".tooltip");
    expect(tip!.textContent).toContain("peak 1");
    expect(tip!.textContent).not.toContain("int /");
    unmount(c);
    target.remove();
  });

  it("emits the active session ids for a clicked slot", async () => {
    const onSelectBucket = vi.fn();
    const target = document.createElement("div");
    document.body.appendChild(target);
    const c = mount(ConcurrencyTimeline, {
      target,
      props: { report: popoverReport(), onSelectBucket },
    });
    await tick();
    (target.querySelector(".slot-hit") as SVGRectElement).dispatchEvent(
      new MouseEvent("click", { bubbles: true }),
    );
    await tick();
    // 3 intervals, "a" deduped to one row, sorted by earliest overlap then id.
    expect(onSelectBucket).toHaveBeenCalledWith(
      expect.objectContaining({ idx: 0, sessionIds: ["a", "b"] }),
    );
    unmount(c);
    target.remove();
  });

  it("emits an empty session list for an idle slot", async () => {
    const report = makeReport({
      bucket_unit: "minute",
      bucket_seconds: 300,
      bucket_count: 3,
      elapsed_bucket_count: 2,
      buckets: [
        {
          start: "2026-06-16T10:00:00Z",
          end: "2026-06-16T10:05:00Z",
          max_agents: 1,
          agent_minutes: 1,
          output_tokens: 0,
          cost: 0,
        },
        {
          start: "2026-06-16T10:05:00Z",
          end: "2026-06-16T10:10:00Z",
          max_agents: 0,
          agent_minutes: 0,
          output_tokens: 0,
          cost: 0,
        },
      ],
      by_session: [] as Report["by_session"],
      intervals: [] as Report["intervals"],
    });
    const onSelectBucket = vi.fn();
    const target = document.createElement("div");
    document.body.appendChild(target);
    const c = mount(ConcurrencyTimeline, {
      target,
      props: { report, onSelectBucket },
    });
    await tick();
    const hits = target.querySelectorAll(".slot-hit");
    expect(hits.length).toBe(2);
    (hits[1] as SVGRectElement).dispatchEvent(new MouseEvent("click", { bubbles: true }));
    await tick();
    expect(onSelectBucket).toHaveBeenCalledWith(
      expect.objectContaining({ idx: 1, sessionIds: [] }),
    );
    unmount(c);
    target.remove();
  });

  it("clears the selection when the already selected slot is clicked again", async () => {
    const onSelectBucket = vi.fn();
    const target = document.createElement("div");
    document.body.appendChild(target);
    // selectedBucket=0 marks slot 0 active; clicking it again toggles it off.
    const c = mount(ConcurrencyTimeline, {
      target,
      props: { report: popoverReport(), selectedBucket: 0, onSelectBucket },
    });
    await tick();
    (target.querySelector(".slot-hit") as SVGRectElement).dispatchEvent(
      new MouseEvent("click", { bubbles: true }),
    );
    await tick();
    expect(onSelectBucket).toHaveBeenCalledWith(null);
    unmount(c);
    target.remove();
  });

  it("selects a slot with a keyboard Enter", async () => {
    const onSelectBucket = vi.fn();
    const target = document.createElement("div");
    document.body.appendChild(target);
    const c = mount(ConcurrencyTimeline, {
      target,
      props: { report: popoverReport(), onSelectBucket },
    });
    await tick();
    const hit = target.querySelector(".slot-hit") as SVGRectElement;
    hit.dispatchEvent(new KeyboardEvent("keydown", { key: "Enter", bubbles: true }));
    await tick();
    expect(onSelectBucket).toHaveBeenCalledWith(expect.objectContaining({ idx: 0 }));
    unmount(c);
    target.remove();
  });

  it("marks the selected bucket with an outline and brightened segments", async () => {
    const target = document.createElement("div");
    document.body.appendChild(target);
    const c = mount(ConcurrencyTimeline, {
      target,
      props: { report: popoverReport(), selectedBucket: 0 },
    });
    await tick();
    expect(target.querySelector(".concurrency-outline")).toBeTruthy();
    expect(target.querySelector(".concurrency-seg.selected")).toBeTruthy();
    unmount(c);
    target.remove();
  });

  it("renders one bar per bucket with widths from bucket bounds", () => {
    render(ConcurrencyTimeline, { report: minuteReport() });
    const bars = document.querySelectorAll("rect[data-bucket-bar]");
    expect(bars.length).toBe(3);
  });

  it("shades a future region when the range is partial", () => {
    const r = minuteReport({
      bucket_count: 3,
      elapsed_bucket_count: 2,
      partial: true,
      as_of: "2026-06-16T00:10:00Z",
      effective_end: "2026-06-16T00:10:00Z",
    });
    render(ConcurrencyTimeline, { report: r });
    expect(document.querySelector("rect[data-future]")).toBeTruthy();
  });

  it("formats a date-range tooltip for daily buckets", async () => {
    const r = makeReport({
      bucket_unit: "day",
      range_start: "2026-06-15T00:00:00Z",
      range_end: "2026-06-17T00:00:00Z",
      bucket_seconds: 86400,
      bucket_count: 2,
      elapsed_bucket_count: 2,
      effective_end: "2026-06-17T00:00:00Z",
      buckets: [
        {
          start: "2026-06-15T00:00:00Z",
          end: "2026-06-16T00:00:00Z",
          max_agents: 1,
          agent_minutes: 10,
          output_tokens: 1,
          cost: 0,
        },
        {
          start: "2026-06-16T00:00:00Z",
          end: "2026-06-17T00:00:00Z",
          max_agents: 1,
          agent_minutes: 10,
          output_tokens: 1,
          cost: 0,
        },
      ] as Report["buckets"],
    });
    render(ConcurrencyTimeline, { report: r });
    const bar = document.querySelector("rect[data-bucket-bar]") as Element;
    await fireEvent.mouseEnter(bar);
    // Tooltip text should be a date label, not an HH:MM time.
    expect(document.body.textContent).toMatch(/Jun/);
  });

  it("formats a DST-safe week tooltip with the inclusive last day", async () => {
    const r = makeReport({
      bucket_unit: "week",
      range_start: "2026-06-15T00:00:00Z",
      range_end: "2026-06-22T00:00:00Z",
      bucket_seconds: 604800,
      bucket_count: 1,
      elapsed_bucket_count: 1,
      effective_end: "2026-06-22T00:00:00Z",
      buckets: [
        {
          start: "2026-06-15T00:00:00Z",
          end: "2026-06-22T00:00:00Z",
          max_agents: 2,
          agent_minutes: 20,
          output_tokens: 100,
          cost: 0,
        },
      ] as Report["buckets"],
    });
    const target = document.createElement("div");
    document.body.appendChild(target);
    const c = mount(ConcurrencyTimeline, { target, props: { report: r } });
    await tick();
    const hit = target.querySelector(".slot-hit") as SVGRectElement;
    hit.dispatchEvent(new MouseEvent("mouseenter", { bubbles: true }));
    await tick();
    const tip = target.querySelector(".tooltip");
    expect(tip).toBeTruthy();
    expect(tip!.textContent).toContain("Jun 15");
    expect(tip!.textContent).toContain("Jun 21"); // end - 1ms = inclusive last day
    expect(tip!.textContent).not.toContain("Jun 22"); // not the exclusive end
    unmount(c);
    target.remove();
  });

  it("labels minute-unit ticks with real local times", () => {
    render(ConcurrencyTimeline, { report: minuteReport() });
    const labels = Array.from(document.querySelectorAll("text.x-label")).map(
      (el) => el.textContent?.trim() ?? "",
    );
    expect(labels.length).toBeGreaterThan(0);
    // Every label is HH:MM, and a sub-hour range yields non-:00 minutes
    // rather than the old hardcoded 00/06/12/18/24 hour marks.
    expect(labels.every((l) => /^\d{2}:\d{2}$/.test(l))).toBe(true);
    expect(labels.some((l) => !l.endsWith(":00"))).toBe(true);
  });

  it("overlays the selected metric line and hides it when None", async () => {
    const target = document.createElement("div");
    document.body.appendChild(target);
    const c = mount(ConcurrencyTimeline, { target, props: { report: makeReport() } });
    await tick();
    // Default metric is "none": no overlay line.
    expect(target.querySelector(".overlay-line")).toBeNull();
    await chooseOverlayMetric(target, "Cost");
    expect(target.querySelector(".overlay-line")).toBeTruthy();
    unmount(c);
    target.remove();
  });

  it("labels the overlay scale on the right y-axis", async () => {
    const target = document.createElement("div");
    document.body.appendChild(target);
    const c = mount(ConcurrencyTimeline, { target, props: { report: makeReport() } });
    await tick();

    await chooseOverlayMetric(target, "Cost");

    const labels = Array.from(target.querySelectorAll("text.overlay-y-label")).map(
      (el) => el.textContent?.trim() ?? "",
    );
    expect(labels).toContain("$0.90");
    expect(labels).toContain("$0.00");

    await chooseOverlayMetric(target, "Tokens");

    const tokenLabels = Array.from(target.querySelectorAll("text.overlay-y-label")).map(
      (el) => el.textContent?.trim() ?? "",
    );
    expect(tokenLabels).toContain("9k");

    unmount(c);
    target.remove();
  });
});
