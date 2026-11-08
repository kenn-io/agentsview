// @vitest-environment jsdom
import { afterEach, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import { analytics } from "../../stores/analytics.svelte.js";
import { AnalyticsService } from "../../api/generated/index.js";
import { makeSummary, makeVelocity, makeTools } from "../../test-support/analytics.js";
import AgentComparison from "./AgentComparison.svelte";

let component: ReturnType<typeof mount> | undefined;
afterEach(async () => {
  if (component) await unmount(component);
  component = undefined;
  analytics.cancelInFlightReads();
  for (const panel of ["summary", "velocity", "tools"] as const) {
    analytics[panel] = null;
    analytics.errors[panel] = null;
  }
  vi.restoreAllMocks();
  document.body.innerHTML = "";
});

it.each(["summary", "velocity", "tools"] as const)(
  "waits for comparison %s and retries only its failure",
  async (source) => {
    const summary = {
      ...makeSummary(),
      agents: { claude: { sessions: 3, messages: 30 }, codex: { sessions: 4, messages: 40 } },
    };
    Object.assign(analytics, { summary, velocity: makeVelocity(), tools: makeTools() });
    for (const panel of ["summary", "velocity", "tools"] as const) analytics.errors[panel] = null;
    analytics[source] = null;
    const reads = {
      summary: vi.spyOn(AnalyticsService, "getApiV1AnalyticsSummary").mockResolvedValue(summary),
      velocity: vi
        .spyOn(AnalyticsService, "getApiV1AnalyticsVelocity")
        .mockResolvedValue(makeVelocity()),
      tools: vi.spyOn(AnalyticsService, "getApiV1AnalyticsTools").mockResolvedValue(makeTools()),
    };
    component = mount(AgentComparison, { target: document.body });
    await tick();
    const comparison = () => document.querySelector(".agent-comparison")!;
    expect(comparison().textContent).toContain("Loading");
    expect(comparison().querySelector(".comparison-table")).toBeNull();
    analytics.errors[source] = `${source} failed`;
    await tick();
    comparison().querySelector<HTMLButtonElement>(".error button")!.click();
    await vi.waitFor(() => expect(comparison().querySelector(".comparison-table")).not.toBeNull());
    expect(comparison().textContent).toContain("claude");
    expect(comparison().textContent).toContain("codex");
    expect(comparison().textContent).not.toContain(`${source} failed`);
    for (const panel of ["summary", "velocity", "tools"] as const) {
      expect(reads[panel]).toHaveBeenCalledTimes(panel === source ? 1 : 0);
    }
  },
);
