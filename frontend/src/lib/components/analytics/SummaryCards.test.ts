// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import { analytics } from "../../stores/analytics.svelte.js";
import { AnalyticsService } from "../../api/generated/index.js";
import SummaryCards from "./SummaryCards.svelte";

let component: ReturnType<typeof mount> | undefined;

afterEach(() => {
  if (component) unmount(component);
  component = undefined;
  vi.restoreAllMocks();
  document.body.innerHTML = "";
  analytics.summary = null;
  analytics.summaryFilterParams = null;
  analytics.from = "";
  analytics.to = "";
  analytics.isPinned = false;
  analytics.selectedDate = null;
});

describe("SummaryCards", () => {
  it("keeps the caption on the last accepted summary", async () => {
    analytics.summary = null;
    analytics.errors.summary = null;
    analytics.from = "2025-11-07";
    analytics.to = "2026-11-06";
    analytics.isPinned = true;
    const data = {
      total_sessions: 29, total_messages: 100, total_output_tokens: 0,
      token_reporting_sessions: 0, models: [], active_projects: 3,
      active_days: 5, avg_messages: 10, median_messages: 8, p90_messages: 20,
      most_active_project: "project-a", concentration: 0.5, agents: {},
    };
    let resolve!: (value: typeof data) => void;
    let reject!: (reason: Error) => void;
    const service = vi.spyOn(AnalyticsService, "getApiV1AnalyticsSummary")
      .mockImplementation(() => new Promise((res, rej) => { resolve = res; reject = rej; }));
    component = mount(SummaryCards, { target: document.body });
    await tick();
    const card = () => document.querySelector(".summary-cards .card")!;
    const caption = () => card().querySelector(".card-sub")?.textContent?.replace(/\s/g, " ").trim();
    expect(caption()).toBeUndefined();
    const annual = analytics.fetchSummary();
    resolve(data);
    await annual;
    await tick();
    expect(caption()).toBe("Nov 7, 2025 – Nov 6, 2026 · incl. subagents");

    analytics.from = "2026-11-01";
    const pending = analytics.fetchSummary();
    await tick();
    expect(caption()).toBe("Nov 7, 2025 – Nov 6, 2026 · incl. subagents");
    reject(new Error("summary unavailable"));
    await pending;
    await tick();
    expect(caption()).toBe("Nov 7, 2025 – Nov 6, 2026 · incl. subagents");

    service.mockResolvedValue({ ...data, total_sessions: 7 });
    await analytics.fetchSummary();
    await tick();
    expect(caption()).toBe("Nov 1, 2026 – Nov 6, 2026 · incl. subagents");
    analytics.selectedDate = "2026-11-03";
    await analytics.fetchSummary();
    await tick();
    expect(caption()).toBe("Nov 3, 2026 · incl. subagents");
  });

});
