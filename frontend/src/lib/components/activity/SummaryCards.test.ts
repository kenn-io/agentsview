// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vite-plus/test";
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
    interactive_peak: { agents: 0, at: null },
    subagent_peak: { agents: 0, at: null },
    automated_peak: { agents: 0, at: null },
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
      subagent_agent_minutes: 0,
      automated_agent_minutes: 0,
      interactive_agent_minutes: 0,
      subagent_cost: testMoney(0),
      automated_cost: testMoney(0),
      interactive_cost: testMoney(0),
      automated_sessions: 0,
      interactive_sessions: 3,
      subagent_sessions: 0,
      ...totals,
    },
    by_project: [],
    by_model: [],
    by_agent: [],
    by_session: [],
    sessions_total: 0,
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

  it("features interactive concurrency instead of the combined peak", async () => {
    const report = makeReport();
    report.peak = { agents: 102, at: "2026-06-16T07:00:00Z" };
    report.interactive_peak = { agents: 2, at: "2026-06-16T06:00:00Z" };
    const target = await render(report);
    const featured = target.querySelector(".card.featured");
    expect(featured?.querySelector(".card-value")?.textContent).toBe("2");
    expect(featured?.querySelector(".card-label")?.textContent).toBe("Interactive peak");
    expect(featured?.querySelector(".card-sub")?.textContent).toBe("at 06:00");
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

  it.each([
    [1, 0, "2 interactive / 1 subagent / 0 automated"],
    [4, 1, "2 interactive / 4 subagents / 1 automated"],
  ])(
    "shows %i subagents separately with %i automated sessions",
    async (subagents, automated, expected) => {
      const target = await render(
        makeReport({
          sessions: 2 + subagents + automated,
          interactive_sessions: 2,
          subagent_sessions: subagents,
          automated_sessions: automated,
        }),
      );
      expect(sessionsSub(target)).toBe(expected);
    },
  );

  it("combines the split and the untimed count", async () => {
    const target = await render(
      makeReport({
        sessions: 3,
        interactive_sessions: 1,
        automated_sessions: 2,
        untimed_sessions: 1,
      }),
    );
    expect(sessionsSub(target)).toBe("1 interactive / 2 automated, 1 untimed");
  });
});
