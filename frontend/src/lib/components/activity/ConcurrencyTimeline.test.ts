// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render } from "@testing-library/svelte";
import { mount, tick, unmount } from "svelte";
import ConcurrencyTimeline from "./ConcurrencyTimeline.svelte";
import type { Bucket, Report } from "../../api/types.js";
import { testMoney } from "../../test/money.js";

class ResizeObserverMock {
  observe = vi.fn();
  unobserve = vi.fn();
  disconnect = vi.fn();
}

type PeakSplit = "interactive_at_peak" | "automated_at_peak";
type BucketFixture = Omit<Bucket, PeakSplit> & Partial<Pick<Bucket, PeakSplit>>;
type ReportOverrides = Partial<Omit<Report, "buckets">> & { buckets?: BucketFixture[] | null };

function makeReport(overrides: ReportOverrides = {}): Report {
  // idx 2 (peak 3) carries a mixed split (2 interactive / 1 automated) for the
  // stacking and split-tooltip tests; idx 3 (peak 1) is all-interactive.
  const buckets = [
    {
      start: "2026-06-16T00:00:00Z",
      end: "2026-06-16T03:00:00Z",
      max_agents: 0,
      agent_minutes: 0,
      output_tokens: 0,
      cost: testMoney(0),
      interactive_at_peak: 0,
      automated_at_peak: 0,
    },
    {
      start: "2026-06-16T03:00:00Z",
      end: "2026-06-16T06:00:00Z",
      max_agents: 2,
      agent_minutes: 12,
      output_tokens: 4000,
      cost: testMoney(0.4),
      interactive_at_peak: 1,
      automated_at_peak: 1,
    },
    {
      start: "2026-06-16T06:00:00Z",
      end: "2026-06-16T09:00:00Z",
      max_agents: 3,
      agent_minutes: 30,
      input_tokens: 120000,
      output_tokens: 9000,
      cost: testMoney(0.9),
      interactive_at_peak: 2,
      automated_at_peak: 1,
    },
    {
      start: "2026-06-16T09:00:00Z",
      end: "2026-06-16T12:00:00Z",
      max_agents: 1,
      agent_minutes: 8,
      output_tokens: 2000,
      cost: testMoney(0.2),
      interactive_at_peak: 1,
      automated_at_peak: 0,
    },
    {
      start: "2026-06-16T12:00:00Z",
      end: "2026-06-16T15:00:00Z",
      max_agents: 0,
      agent_minutes: 0,
      output_tokens: 0,
      cost: testMoney(0),
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
      cost: testMoney(1.5),
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
    sessions_total: 0,
    projects: {},
    ...overrides,
  } as Report;
  // Backfill the peak-automation split onto any bucket literal that omits it
  // (most fixtures only set max_agents), so the stacked bars get real geometry
  // instead of NaN. Unspecified buckets default to all-interactive.
  report.buckets = (overrides.buckets ?? buckets).map((b) => ({
    ...b,
    interactive_at_peak: b.interactive_at_peak ?? b.max_agents,
    automated_at_peak: b.automated_at_peak ?? 0,
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
        cost: testMoney(0),
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
        cost: testMoney(0),
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
        cost: testMoney(0),
        output_tokens: 0,
        first_active: "2026-06-16T10:01:00Z",
        last_active: "2026-06-16T10:03:00Z",
        timing_quality: "timed",
      },
    ] as Report["by_session"],
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
        cost: testMoney(0),
      },
      {
        start: "2026-06-16T00:05:00Z",
        end: "2026-06-16T00:10:00Z",
        max_agents: 2,
        agent_minutes: 5,
        output_tokens: 20,
        cost: testMoney(0),
      },
      {
        start: "2026-06-16T00:10:00Z",
        end: "2026-06-16T00:15:00Z",
        max_agents: 1,
        agent_minutes: 5,
        output_tokens: 5,
        cost: testMoney(0),
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

async function dragRange(target: HTMLElement, start: number, end: number) {
  const hits = target.querySelectorAll<SVGRectElement>(".slot-hit");
  await fireEvent.pointerDown(hits[start]!, { button: 0 });
  if (end !== start) {
    const endX = Number(hits[end]!.getAttribute("x"));
    const endWidth = Number(hits[end]!.getAttribute("width"));
    await fireEvent.pointerMove(target.querySelector(".timeline-body")!, {
      clientX: endX + endWidth / 2,
    });
  }
  await fireEvent.pointerUp(window);
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

  it("renders no hit target for future buckets and clamps keyboard nav to live ones", async () => {
    // The last of the three minute buckets starts at the effective end, so it
    // is entirely in the future: it must not be hoverable, tooltippable, or
    // reachable, while its bar geometry still renders.
    const report = minuteReport({
      elapsed_bucket_count: 2,
      partial: true,
      as_of: "2026-06-16T00:10:00Z",
      effective_end: "2026-06-16T00:10:00Z",
    });
    report.buckets![2] = {
      ...report.buckets![2]!,
      max_agents: 0,
      agent_minutes: 0,
      output_tokens: 0,
    };
    const onSelectRange = vi.fn();
    const target = document.createElement("div");
    document.body.appendChild(target);
    const c = mount(ConcurrencyTimeline, {
      target,
      props: { report, onSelectRange },
    });
    await tick();

    const hits = target.querySelectorAll(".slot-hit");
    expect(hits.length).toBe(2);
    expect(target.querySelector('[data-concurrency-bucket-index="2"]')).toBeNull();
    // The future bucket keeps its (zero-height) bar segments.
    expect(target.querySelectorAll(".concurrency-seg.interactive").length).toBe(3);

    // Shift+ArrowRight from the last live slot clamps to the live range
    // instead of extending the selection into the future bucket.
    hits[1]!.dispatchEvent(
      new KeyboardEvent("keydown", {
        key: "ArrowRight",
        shiftKey: true,
        bubbles: true,
      }),
    );
    await tick();
    expect(onSelectRange).toHaveBeenCalledWith(expect.objectContaining({ start: 1, end: 2 }));

    unmount(c);
    target.remove();
  });

  it("clamps the hover tooltip inside the viewport at the top-left edge", async () => {
    // jsdom reports zero-size boxes, so mock the tooltip's measured size. The
    // hovered slot's rect is all zeros, anchoring the tooltip at x=0, y=-4.
    // Hand-computed from viewport geometry (innerWidth 1024, pad 8): the
    // unclamped box would sit at left -100, top -104; both clamp to 8.
    vi.spyOn(HTMLElement.prototype, "offsetWidth", "get").mockReturnValue(200);
    vi.spyOn(HTMLElement.prototype, "offsetHeight", "get").mockReturnValue(100);
    const target = document.createElement("div");
    document.body.appendChild(target);
    const c = mount(ConcurrencyTimeline, { target, props: { report: makeReport() } });
    await tick();
    const hit = target.querySelectorAll(".slot-hit")[2] as SVGRectElement;
    hit.dispatchEvent(new MouseEvent("mouseenter", { bubbles: true }));
    await tick();
    const tip = target.querySelector<HTMLDivElement>(".tooltip");
    expect(tip).toBeTruthy();
    expect(tip!.style.left).toBe("8px");
    expect(tip!.style.top).toBe("8px");
    unmount(c);
    target.remove();
  });

  it("centers the tooltip above an anchor away from viewport edges", async () => {
    vi.spyOn(HTMLElement.prototype, "offsetWidth", "get").mockReturnValue(200);
    vi.spyOn(HTMLElement.prototype, "offsetHeight", "get").mockReturnValue(100);
    const target = document.createElement("div");
    document.body.appendChild(target);
    const c = mount(ConcurrencyTimeline, { target, props: { report: makeReport() } });
    await tick();
    const hit = target.querySelectorAll(".slot-hit")[2] as SVGRectElement;
    vi.spyOn(hit, "getBoundingClientRect").mockReturnValue({
      left: 400,
      top: 300,
      width: 40,
      height: 180,
      right: 440,
      bottom: 480,
      x: 400,
      y: 300,
      toJSON: () => ({}),
    } as DOMRect);
    hit.dispatchEvent(new MouseEvent("mouseenter", { bubbles: true }));
    await tick();
    const tip = target.querySelector<HTMLDivElement>(".tooltip");
    // Anchor center x = 420, anchor y = 300 - 4 = 296; a 200x100 box centered
    // above lands at left 320, top 196 with no clamping needed.
    expect(tip!.style.left).toBe("320px");
    expect(tip!.style.top).toBe("196px");
    unmount(c);
    target.remove();
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
    expect(tip!.querySelector(".tooltip-metrics > div")?.textContent).toContain("Peak Concurrency");
    hit.dispatchEvent(new MouseEvent("mouseleave", { bubbles: true }));
    await tick();
    expect(target.querySelector(".tooltip")).toBeNull();
    unmount(c);
    target.remove();
  });

  it("shows structured compact metrics with cost in the tooltip", async () => {
    const target = document.createElement("div");
    document.body.appendChild(target);
    const report = makeReport();
    report.buckets![2]!.agent_minutes = 7500;
    const c = mount(ConcurrencyTimeline, { target, props: { report } });
    await tick();
    const hit = target.querySelectorAll(".slot-hit")[2] as SVGRectElement;
    hit.dispatchEvent(new MouseEvent("mouseenter", { bubbles: true }));
    await tick();
    const tip = target.querySelector(".tooltip");
    expect(tip).toBeTruthy();
    const rows = Array.from(tip!.querySelectorAll(".tooltip-metrics > div"));
    expect(rows.map((row) => row.textContent?.replace(/\s+/g, " ").trim())).toEqual([
      "Peak Concurrency 3 (2 int / 1 auto)",
      "Agent-min 7.5K",
      "Input Tokens 120K",
      "Output Tokens 9K",
      "Cost $0.90",
    ]);
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
    const rows = tip!.querySelectorAll(".tooltip-metrics > div");
    expect(rows[0]!.textContent?.replace(/\s+/g, " ").trim()).toBe(
      "Peak Concurrency 3 (2 int / 1 auto)",
    );
    // agent-minutes stays a single combined figure, not split by automation.
    expect(rows[1]!.textContent?.replace(/\s+/g, " ").trim()).toBe("Agent-min 30");
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
    const peakRow = tip!.querySelector(".tooltip-metrics > div");
    expect(peakRow?.textContent?.replace(/\s+/g, " ").trim()).toBe("Peak Concurrency 1");
    unmount(c);
    target.remove();
  });

  it("emits a one-bucket range for server-side membership paging", async () => {
    const onSelectRange = vi.fn();
    const target = document.createElement("div");
    document.body.appendChild(target);
    const c = mount(ConcurrencyTimeline, {
      target,
      props: { report: popoverReport(), onSelectRange },
    });
    await tick();
    await dragRange(target, 0, 0);
    expect(onSelectRange).toHaveBeenCalledWith({ start: 0, end: 1, label: "10:00–10:05" });
    unmount(c);
    target.remove();
  });

  it("emits an idle one-bucket range without computing membership locally", async () => {
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
          cost: testMoney(0),
        },
        {
          start: "2026-06-16T10:05:00Z",
          end: "2026-06-16T10:10:00Z",
          max_agents: 0,
          agent_minutes: 0,
          output_tokens: 0,
          cost: testMoney(0),
        },
      ],
      by_session: [] as Report["by_session"],
    });
    const onSelectRange = vi.fn();
    const target = document.createElement("div");
    document.body.appendChild(target);
    const c = mount(ConcurrencyTimeline, {
      target,
      props: { report, onSelectRange },
    });
    await tick();
    const hits = target.querySelectorAll(".slot-hit");
    expect(hits.length).toBe(2);
    await dragRange(target, 1, 1);
    expect(onSelectRange).toHaveBeenCalledWith({ start: 1, end: 2, label: "10:05–10:10" });
    unmount(c);
    target.remove();
  });

  it("clears the selection when the selected range is clicked again", async () => {
    const onSelectRange = vi.fn();
    const target = document.createElement("div");
    document.body.appendChild(target);
    const c = mount(ConcurrencyTimeline, {
      target,
      props: {
        report: popoverReport(),
        selectedRange: { start: 0, end: 1 },
        onSelectRange,
      },
    });
    await tick();
    await dragRange(target, 0, 0);
    expect(onSelectRange).toHaveBeenCalledWith(null);
    unmount(c);
    target.remove();
  });

  it("shows a clear-selection button for a brushed range", async () => {
    const onSelectRange = vi.fn();
    const target = document.createElement("div");
    document.body.appendChild(target);
    const c = mount(ConcurrencyTimeline, {
      target,
      props: {
        report: popoverReport(),
        selectedRange: { start: 0, end: 1 },
        onSelectRange,
      },
    });
    await tick();

    const clear = [...target.querySelectorAll<HTMLButtonElement>("button")].find(
      (button) => button.textContent?.trim() === "Clear selection",
    );
    expect(clear).toBeDefined();
    clear!.click();
    expect(onSelectRange).toHaveBeenCalledExactlyOnceWith(null);

    unmount(c);
    target.remove();
  });

  it("extends a keyboard selection with Shift+ArrowRight", async () => {
    const onSelectRange = vi.fn();
    const target = document.createElement("div");
    document.body.appendChild(target);
    const c = mount(ConcurrencyTimeline, {
      target,
      props: { report: minuteReport(), onSelectRange },
    });
    await tick();

    const first = target.querySelector<SVGRectElement>(".slot-hit");
    first!.dispatchEvent(
      new KeyboardEvent("keydown", {
        key: "ArrowRight",
        shiftKey: true,
        bubbles: true,
      }),
    );
    await tick();

    expect(onSelectRange).toHaveBeenCalledWith(expect.objectContaining({ start: 0, end: 2 }));
    unmount(c);
    target.remove();
  });

  it("selects a slot with a keyboard Enter", async () => {
    const onSelectRange = vi.fn();
    const target = document.createElement("div");
    document.body.appendChild(target);
    const c = mount(ConcurrencyTimeline, {
      target,
      props: { report: popoverReport(), onSelectRange },
    });
    await tick();
    const hit = target.querySelector(".slot-hit") as SVGRectElement;
    hit.dispatchEvent(new KeyboardEvent("keydown", { key: "Enter", bubbles: true }));
    await tick();
    expect(onSelectRange).toHaveBeenCalledWith(expect.objectContaining({ start: 0, end: 1 }));
    unmount(c);
    target.remove();
  });

  it("marks the selected range and brightens its segments", async () => {
    const target = document.createElement("div");
    document.body.appendChild(target);
    const c = mount(ConcurrencyTimeline, {
      target,
      props: { report: popoverReport(), selectedRange: { start: 0, end: 1 } },
    });
    await tick();
    const selection = target.querySelector(".range-selection");
    const selectedSegment = target.querySelector(".concurrency-seg.selected");
    const stripCell = target.querySelector(".strip-cell");
    const hitTarget = target.querySelector(".slot-hit");
    expect(selection).toBeTruthy();
    expect(selectedSegment).toBeTruthy();
    expect(stripCell).toBeTruthy();
    expect(hitTarget).toBeTruthy();
    expect(
      selection!.compareDocumentPosition(stripCell!) & Node.DOCUMENT_POSITION_PRECEDING,
    ).toBeTruthy();
    expect(
      selection!.compareDocumentPosition(hitTarget!) & Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();
    unmount(c);
    target.remove();
  });

  it("drags across buckets to emit a half-open range", async () => {
    const onSelectRange = vi.fn();
    const target = document.createElement("div");
    document.body.appendChild(target);
    const c = mount(ConcurrencyTimeline, {
      target,
      props: { report: minuteReport(), onSelectRange },
    });
    await tick();

    await dragRange(target, 0, 2);

    expect(onSelectRange).toHaveBeenCalledWith({
      start: 0,
      end: 3,
      label: "00:00–00:15",
    });
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
          cost: testMoney(0),
        },
        {
          start: "2026-06-16T00:00:00Z",
          end: "2026-06-17T00:00:00Z",
          max_agents: 1,
          agent_minutes: 10,
          output_tokens: 1,
          cost: testMoney(0),
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
          cost: testMoney(0),
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
