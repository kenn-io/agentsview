// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vitest";
import { mount, tick } from "svelte";
import SummaryCards from "./SummaryCards.svelte";
import type { Report } from "../../api/types.js";
import { testMoney } from "../../test/money.js";

function makeReport(totals: Partial<Report["totals"]> = {}): Report {
  return {
    timezone: "UTC",
    range_start: "2026-06-16T00:00:00Z",
    range_end: "2026-06-17T00:00:00Z",
    bucket_unit: "minute",
    bucket_seconds: 300,
    bucket_count: 0,
    partial: false,
    as_of: null,
    effective_end: "2026-06-17T00:00:00Z",
    elapsed_bucket_count: 0,
    buckets: [],
    peak: { agents: 0, at: null },
    totals: {
      active_minutes: 0,
      idle_minutes: 0,
      agent_minutes: 0,
      sessions: 3,
      untimed_sessions: 0,
      distinct_projects: 0,
      distinct_models: 0,
      output_tokens: 0,
      cost: testMoney(0),
      automated_agent_minutes: 0,
      interactive_agent_minutes: 0,
      automated_cost: testMoney(0),
      interactive_cost: testMoney(0),
      automated_sessions: 0,
      interactive_sessions: 3,
      ...totals,
    },
    by_project: [],
    by_model: [],
    by_agent: [],
    by_session: [],
    intervals: [],
    projects: {},
  } as Report;
}

// Read the sub-label text of the Sessions card.
function sessionsSub(target: HTMLElement): string {
  const card = [...target.querySelectorAll(".card")].find(
    (c) => c.querySelector(".card-label")?.textContent?.trim() === "Sessions",
  );
  return card?.querySelector(".card-sub")?.textContent?.trim() ?? "";
}

async function render(report: Report): Promise<HTMLElement> {
  const target = document.createElement("div");
  document.body.appendChild(target);
  mount(SummaryCards, { target, props: { report } });
  await tick();
  return target;
}

describe("SummaryCards", () => {
  afterEach(() => {
    document.body.innerHTML = "";
  });

  it("shows the interactive/automated split when automated sessions exist", async () => {
    const target = await render(
      makeReport({
        sessions: 3,
        interactive_sessions: 1,
        automated_sessions: 2,
      }),
    );
    expect(sessionsSub(target)).toBe("1 interactive / 2 automated");
  });

  it("omits the split and keeps untimed when there are no automated sessions", async () => {
    const target = await render(
      makeReport({
        sessions: 3,
        interactive_sessions: 3,
        automated_sessions: 0,
        untimed_sessions: 1,
      }),
    );
    expect(sessionsSub(target)).toBe("1 untimed");
  });

  it("combines the split and the untimed count", async () => {
    const target = await render(
      makeReport({
        sessions: 4,
        interactive_sessions: 1,
        automated_sessions: 2,
        untimed_sessions: 1,
      }),
    );
    expect(sessionsSub(target)).toBe("1 interactive / 2 automated, 1 untimed");
  });
});
