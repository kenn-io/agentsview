import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { mount, tick, unmount } from "svelte";
import AttributionPanel from "./AttributionPanel.svelte";
import { usage } from "../../stores/usage.svelte.js";
import { branchFilterToken } from "../../branchFilters.js";
import type { UsageSummaryResponse } from "../../api/types/usage.js";

function usageSummary(): UsageSummaryResponse {
  return {
    from: "2024-01-01",
    to: "2024-01-31",
    totals: {
      inputTokens: 100,
      outputTokens: 50,
      cacheCreationTokens: 0,
      cacheReadTokens: 0,
      totalCost: 12,
    },
    daily: [],
    projectTotals: [],
    modelTotals: [],
    agentTotals: [],
    branchTotals: [
      {
        project: "alpha",
        branch: "main",
        inputTokens: 60,
        outputTokens: 30,
        cacheCreationTokens: 0,
        cacheReadTokens: 0,
        cost: 8,
      },
      {
        project: "alpha",
        branch: "dev",
        inputTokens: 40,
        outputTokens: 20,
        cacheCreationTokens: 0,
        cacheReadTokens: 0,
        cost: 4,
      },
    ],
    sessionCounts: {
      total: 2,
      byProject: { alpha: 2 },
      byAgent: {},
    },
    cacheStats: {
      cacheReadTokens: 0,
      cacheCreationTokens: 0,
      uncachedInputTokens: 100,
      outputTokens: 50,
      hitRate: 0,
      savingsVsUncached: 0,
    },
  };
}

describe("AttributionPanel branch mode", () => {
  beforeEach(() => {
    usage.summary = usageSummary();
    usage.toggles.attribution.groupBy = "branch";
    usage.toggles.attribution.view = "list";
  });

  afterEach(() => {
    usage.summary = null;
    usage.toggles.attribution.groupBy = "project";
    usage.excludedGitBranch = "";
    document.body.innerHTML = "";
    vi.restoreAllMocks();
  });

  it("routes a branch row click into the branch exclusion toggle", async () => {
    const spy = vi
      .spyOn(usage, "toggleBranch")
      .mockImplementation(() => {});
    const component = mount(AttributionPanel, { target: document.body });
    await tick();

    const row = Array.from(
      document.querySelectorAll<HTMLDivElement>(".list-row"),
    ).find((r) => r.textContent?.includes("alpha/dev"));
    expect(row).toBeTruthy();
    row?.click();
    await tick();

    expect(spy).toHaveBeenCalledWith(branchFilterToken("alpha", "dev"));
    unmount(component);
  });

  it("shows the click-to-hide hint in branch mode", async () => {
    const component = mount(AttributionPanel, { target: document.body });
    await tick();

    expect(document.querySelector(".hint")).toBeTruthy();
    unmount(component);
  });
});
