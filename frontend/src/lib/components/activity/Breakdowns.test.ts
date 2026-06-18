// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vitest";
import { mount, tick, unmount } from "svelte";
import Breakdowns from "./Breakdowns.svelte";
import type { Report } from "../../api/types.js";

function makeReport(): Report {
  return {
    peak: { agents: 0, at: null },
    totals: {
      active_minutes: 0, idle_minutes: 0, agent_minutes: 0, sessions: 0,
      untimed_sessions: 0, distinct_projects: 0, distinct_models: 0,
      output_tokens: 0, cost: 0,
      automated_agent_minutes: 0, interactive_agent_minutes: 0,
      automated_cost: 0, interactive_cost: 0,
    },
    partial: false,
    as_of: null,
    timezone: "UTC",
    range_start: "2026-06-16T00:00:00Z",
    range_end: "2026-06-17T00:00:00Z",
    bucket_unit: "minute",
    effective_end: "2026-06-17T00:00:00Z",
    bucket_seconds: 300,
    bucket_count: 0,
    elapsed_bucket_count: 0,
    buckets: [],
    by_project: [
      { key: "alpha", agent_minutes: 30 },
      { key: "beta", agent_minutes: 10 },
    ],
    by_model: [],
    by_agent: [],
    by_session: [],
    intervals: [],
  } as Report;
}

describe("Breakdowns", () => {
  afterEach(() => {
    document.body.innerHTML = "";
  });

  it("shows a tooltip with the key and share-of-total on bar hover", async () => {
    const target = document.createElement("div");
    document.body.appendChild(target);
    const c = mount(Breakdowns, { target, props: { report: makeReport() } });
    await tick();
    const row = target.querySelector(".bar-row") as HTMLElement; // first project row = alpha (30 of 40)
    expect(row).toBeTruthy();
    row.dispatchEvent(new MouseEvent("mouseenter", { bubbles: true }));
    await tick();
    const tip = target.querySelector(".tooltip");
    expect(tip).toBeTruthy();
    expect(tip!.textContent).toContain("alpha");
    expect(tip!.textContent).toContain("75%");
    row.dispatchEvent(new MouseEvent("mouseleave", { bubbles: true }));
    await tick();
    expect(target.querySelector(".tooltip")).toBeNull();
    unmount(c);
    target.remove();
  });

  it("filters cost-only rows from the default agent-minutes view", async () => {
    const report = makeReport();
    // The backend emits rows with cost but zero agent-minutes for untimed
    // usage; they must not render as empty "0" bars in the minutes view.
    report.by_project = [
      { key: "timed", agent_minutes: 30, cost: 1 },
      { key: "costonly", agent_minutes: 0, cost: 5 },
    ] as Report["by_project"];
    const target = document.createElement("div");
    document.body.appendChild(target);
    const c = mount(Breakdowns, { target, props: { report } });
    await tick();
    const labels = [...target.querySelectorAll(".bar-label")].map(
      (el) => el.textContent?.trim() ?? "",
    );
    expect(labels).toContain("timed");
    expect(labels).not.toContain("costonly");
    unmount(c);
    target.remove();
  });

  it("switches to cost, revealing cost-only rows ranked by cost", async () => {
    const report = makeReport();
    report.by_project = [
      { key: "timed", agent_minutes: 30, cost: 1 },
      { key: "costonly", agent_minutes: 0, cost: 5 },
    ] as Report["by_project"];
    const target = document.createElement("div");
    document.body.appendChild(target);
    const c = mount(Breakdowns, { target, props: { report } });
    await tick();
    const costBtn = [...target.querySelectorAll(".metric-btn")].find(
      (b) => b.textContent?.trim() === "Cost",
    ) as HTMLButtonElement | undefined;
    expect(costBtn).toBeTruthy();
    costBtn!.dispatchEvent(new MouseEvent("click", { bubbles: true }));
    await tick();
    const labels = [...target.querySelectorAll(".bar-label")].map(
      (el) => el.textContent?.trim() ?? "",
    );
    // The cost-only row appears and outranks the lower-cost timed row.
    expect(labels[0]).toBe("costonly");
    expect(labels).toContain("timed");
    const values = [...target.querySelectorAll(".bar-value")].map(
      (el) => el.textContent?.trim() ?? "",
    );
    expect(values.some((v) => v.includes("$5.00"))).toBe(true);
    unmount(c);
    target.remove();
  });
});
