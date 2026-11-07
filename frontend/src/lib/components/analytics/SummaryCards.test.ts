// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import { analytics } from "../../stores/analytics.svelte.js";
import SummaryCards from "./SummaryCards.svelte";

let component: ReturnType<typeof mount> | undefined;

afterEach(() => {
  if (component) unmount(component);
  component = undefined;
  document.body.innerHTML = "";
  analytics.summary = null;
  analytics.summaryFilterParams = null;
  analytics.from = "";
  analytics.to = "";
  analytics.querying.summary = false;
});

describe("SummaryCards", () => {
  it("keeps the caption on the last accepted summary", async () => {
    analytics.summary = null;
    analytics.from = "2025-11-07";
    analytics.to = "2026-11-06";
    const data = {
      total_sessions: 29,
      total_messages: 100,
      total_output_tokens: 0,
      token_reporting_sessions: 0,
      models: [],
      active_projects: 3,
      active_days: 5,
      avg_messages: 10,
      median_messages: 8,
      p90_messages: 20,
      most_active_project: "project-a",
      concentration: 0.5,
      agents: {},
    };
    component = mount(SummaryCards, { target: document.body });
    await tick();
    const card = () => document.querySelector(".summary-cards .card")!;
    const caption = () =>
      card().querySelector(".card-sub")?.textContent?.replace(/\s/g, " ").trim();
    expect(caption()).toBeUndefined();
    analytics.summary = data;
    analytics.summaryFilterParams = { from: "2025-11-07", to: "2026-11-06" };
    await tick();
    expect(caption()).toBe("Nov 7, 2025 – Nov 6, 2026 · incl. subagents");

    analytics.from = "2026-11-01";
    analytics.querying.summary = true;
    await tick();
    expect(caption()).toBe("Nov 7, 2025 – Nov 6, 2026 · incl. subagents");
    analytics.querying.summary = false;
    analytics.summaryFilterParams = { from: "2026-11-01", to: "2026-11-06" };
    await tick();
    expect(caption()).toBe("Nov 1, 2026 – Nov 6, 2026 · incl. subagents");
    analytics.summaryFilterParams = { from: "2026-11-03", to: "2026-11-03" };
    await tick();
    expect(caption()).toBe("Nov 3, 2026 · incl. subagents");
  });
});
