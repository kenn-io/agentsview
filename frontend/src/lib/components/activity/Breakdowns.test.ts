// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vitest";
import { mount, tick, unmount } from "svelte";
import Breakdowns from "./Breakdowns.svelte";
import type { Report } from "../../api/types.js";
import { BRANCH_TOKEN_SEP } from "../../branchFilters.js";

function makeReport(): Report {
  return {
    peak: { agents: 0, at: null },
    totals: {
      active_minutes: 0, idle_minutes: 0, agent_minutes: 0, sessions: 0,
      untimed_sessions: 0, distinct_projects: 0, distinct_models: 0,
      output_tokens: 0, cost: 0,
      automated_agent_minutes: 0, interactive_agent_minutes: 0,
      automated_cost: 0, interactive_cost: 0,
      automated_sessions: 0, interactive_sessions: 0,
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
      {
        key: "alpha", project_key: "pl1:sha256:alpha", agent_minutes: 30, cost: 0,
        interactive_agent_minutes: 20, automated_agent_minutes: 10,
        interactive_cost: 0, automated_cost: 0,
      },
      {
        key: "beta", project_key: "pl1:sha256:beta", agent_minutes: 10, cost: 0,
        interactive_agent_minutes: 10, automated_agent_minutes: 0,
        interactive_cost: 0, automated_cost: 0,
      },
    ],
    by_model: [],
    by_agent: [],
    by_branch: [],
    by_session: [],
    intervals: [],
    projects: {},
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

  it("stacks interactive and automated segments and shows the split in the tooltip", async () => {
    const target = document.createElement("div");
    document.body.appendChild(target);
    const c = mount(Breakdowns, { target, props: { report: makeReport() } });
    await tick();
    // First project row = alpha (20 interactive + 10 automated agent-minutes).
    const row = target.querySelector(".bar-row") as HTMLElement;
    const interactive = row.querySelector(".bar-seg.interactive") as HTMLElement;
    const automated = row.querySelector(".bar-seg.automated") as HTMLElement;
    expect(interactive).toBeTruthy();
    expect(automated).toBeTruthy();
    // The interactive share (20) is wider than the automated share (10).
    const width = (el: HTMLElement) => Number.parseFloat(el.style.width);
    expect(width(interactive)).toBeGreaterThan(width(automated));

    row.dispatchEvent(new MouseEvent("mouseenter", { bubbles: true }));
    await tick();
    const tip = target.querySelector(".tooltip");
    expect(tip!.textContent).toContain("int 20");
    expect(tip!.textContent).toContain("auto 10");
    unmount(c);
    target.remove();
  });

  it("filters cost-only rows from the default agent-minutes view", async () => {
    const report = makeReport();
    // The backend emits rows with cost but zero agent-minutes for untimed
    // usage; they must not render as empty "0" bars in the minutes view.
    report.by_project = [
      {
        key: "timed", agent_minutes: 30, cost: 1,
        interactive_agent_minutes: 30, automated_agent_minutes: 0,
        interactive_cost: 1, automated_cost: 0,
      },
      {
        key: "costonly", agent_minutes: 0, cost: 5,
        interactive_agent_minutes: 0, automated_agent_minutes: 0,
        interactive_cost: 5, automated_cost: 0,
      },
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
      {
        key: "timed", agent_minutes: 30, cost: 1,
        interactive_agent_minutes: 30, automated_agent_minutes: 0,
        interactive_cost: 1, automated_cost: 0,
      },
      {
        key: "costonly", agent_minutes: 0, cost: 5,
        interactive_agent_minutes: 0, automated_agent_minutes: 0,
        interactive_cost: 5, automated_cost: 0,
      },
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

  it("renders distinct project identities that share a display label", async () => {
    const report = makeReport();
    report.by_project = [
      { ...report.by_project![0], key: "same", project_key: "pl1:sha256:a" },
      { ...report.by_project![1], key: "same", project_key: "pl1:sha256:b" },
    ] as Report["by_project"];
    const target = document.createElement("div");
    document.body.appendChild(target);
    const component = mount(Breakdowns, { target, props: { report } });
    await tick();

    expect(target.querySelectorAll(".bar-row")).toHaveLength(2);
    unmount(component);
    target.remove();
  });

  it("renders branch rows as project/branch labels, never raw tokens", async () => {
    const report = makeReport();
    report.by_branch = [
      {
        project: "alpha", branch: "main", agent_minutes: 12, cost: 0,
        interactive_agent_minutes: 12, automated_agent_minutes: 0,
        interactive_cost: 0, automated_cost: 0,
      },
      {
        project: "alpha", branch: "", agent_minutes: 3, cost: 0,
        interactive_agent_minutes: 3, automated_agent_minutes: 0,
        interactive_cost: 0, automated_cost: 0,
      },
    ];
    const target = document.createElement("div");
    document.body.appendChild(target);
    const c = mount(Breakdowns, { target, props: { report } });
    await tick();
    const labels = [...target.querySelectorAll(".bar-label")].map(
      (el) => el.textContent?.trim() ?? "",
    );
    expect(labels).toContain("alpha/main");
    expect(labels).toContain("alpha/(no branch)");
    expect(labels.some((l) => l.includes(BRANCH_TOKEN_SEP))).toBe(false);
    unmount(c);
    target.remove();
  });
});
